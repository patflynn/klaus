//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// Harness is a fully isolated environment for driving the real klaus binary.
//
// Isolation guarantees (so tests never touch the developer's real tmux server
// or ~/.klaus state):
//   - tmux: a private server on a per-test unix socket (Sock). klaus' own tmux
//     calls are pointed at it via the $TMUX env var; the test's control-plane
//     queries use `tmux -S <Sock>`.
//   - state: HOME is a per-test temp dir, so all ~/.klaus/sessions state lands
//     under Home.
//   - externals: a per-test bin dir (BinDir) is prepended to PATH and holds the
//     klaus binary plus fake `claude`/`gh` stubs. git and tmux stay real.
//
// A unique socket name + temp HOME per test keeps tests parallel-safe.
type Harness struct {
	t *testing.T

	Home      string // temp HOME ($HOME for all klaus invocations)
	BinDir    string // dir prepended to PATH; holds klaus + stubs
	E2EDir    string // scratch dir for stub coordination/log files
	RepoDir   string // sandbox repo working tree (CWD for klaus)
	OriginDir string // bare "origin" remote backing RepoDir

	Sock        string // tmux server socket path (isolated server)
	ServerPID   string // isolated tmux server pid (for the $TMUX value)
	InitialPane string // the session's first pane id (klaus splits from here)

	SessionID string // KLAUS_SESSION_ID for all invocations
	shellWrap string // path to the no-profile shell wrapper for tmux panes
}

// RunResult is the outcome of a klaus invocation.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// NewHarness builds nothing (the binary is built once in TestMain) but wires up
// a complete isolated environment: temp HOME, a bin dir with the klaus binary
// and fake claude/gh stubs, a sandbox git repo with a bare origin, and an
// isolated tmux server. It registers cleanup to kill the tmux server; temp dirs
// are removed automatically by t.TempDir.
func NewHarness(t *testing.T) *Harness {
	t.Helper()

	if klausBinary == "" {
		t.Fatal("klaus binary not built (TestMain did not run)")
	}

	root := t.TempDir()
	h := &Harness{
		t:         t,
		Home:      filepath.Join(root, "home"),
		BinDir:    filepath.Join(root, "bin"),
		E2EDir:    filepath.Join(root, "e2e"),
		RepoDir:   filepath.Join(root, "repo"),
		OriginDir: filepath.Join(root, "origin.git"),
		Sock:      filepath.Join(root, "tmux.sock"),
		SessionID: "session-e2e-" + filepath.Base(root),
	}

	for _, d := range []string{h.Home, h.BinDir, h.E2EDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	h.installKlausBinary()
	h.installDefaultStubs()
	h.setupGitConfig()
	h.setupSandboxRepo()
	h.writeRepoConfig()
	h.startTmuxServer()

	return h
}

// installKlausBinary copies the prebuilt klaus binary into BinDir as "klaus"
// so the pane pipeline (which runs `klaus _format-stream`/`klaus _finalize`)
// resolves it from PATH.
func (h *Harness) installKlausBinary() {
	h.t.Helper()
	data, err := os.ReadFile(klausBinary)
	if err != nil {
		h.t.Fatalf("reading klaus binary: %v", err)
	}
	dst := filepath.Join(h.BinDir, "klaus")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		h.t.Fatalf("installing klaus binary: %v", err)
	}
}

// WriteStub writes an executable script into BinDir, shadowing any real binary
// of the same name for klaus invocations in this harness.
func (h *Harness) WriteStub(name, script string) {
	h.t.Helper()
	p := filepath.Join(h.BinDir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		h.t.Fatalf("writing stub %s: %v", name, err)
	}
}

// installDefaultStubs writes the fake claude and gh binaries. The claude stub
// blocks until released (see ReleaseClaude) so tests can deterministically
// observe the live pane before _finalize tears it down. Tests may override
// either stub with WriteStub before launching.
func (h *Harness) installDefaultStubs() {
	h.t.Helper()
	h.WriteDefaultClaudeOutput(defaultClaudeOutput)
	h.WriteStub("claude", h.claudeStubScript())
	h.WriteStub("gh", h.ghStubScript())
}

// defaultClaudeOutput is the stream-json the fake agent emits. It contains a PR
// URL (matched by hidden.go's prURLExtractRegex) and a result event carrying
// cost/duration/session_id so _finalize can populate run state.
const defaultClaudeOutput = `{"type":"system","subtype":"init","model":"claude-e2e"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Opened PR: https://github.com/acme/widget/pull/4242"}]}}
{"type":"result","subtype":"success","total_cost_usd":0.1234,"duration_ms":4242,"session_id":"11111111-2222-4333-8444-555555555555"}
`

// WriteDefaultClaudeOutput sets the JSONL the fake claude prints once released.
func (h *Harness) WriteDefaultClaudeOutput(jsonl string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "claude.output.jsonl"), []byte(jsonl), 0o644); err != nil {
		h.t.Fatalf("writing claude output: %v", err)
	}
}

func (h *Harness) claudeStubScript() string {
	// Records argv + cwd, signals start, blocks until a release file appears
	// (max ~60s), then emits the canned stream-json. Paths are baked in so the
	// stub is independent of the pane's environment.
	return fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ for a in "$@"; do printf '%%s\n' "$a"; done; } > "$dir/claude.argv"
printf '%%s' "$PWD" > "$dir/claude.cwd"
: > "$dir/claude.started"
for _ in {1..600}; do
  [ -f "$dir/claude.release" ] && break
  sleep 0.1
done
cat "$dir/claude.output.jsonl"
`, h.E2EDir)
}

func (h *Harness) ghStubScript() string {
	// Logs every invocation so tests can prove how (or whether) gh was called,
	// and returns benign output. The launch path avoids gh entirely (pr_reviewer
	// is configured), so this is mostly a safety net against hitting real gh.
	return fmt.Sprintf(`#!/usr/bin/env bash
dir=%q
{ printf 'gh'; for a in "$@"; do printf ' %%q' "$a"; done; printf '\n'; } >> "$dir/gh.argv"
exit 0
`, h.E2EDir)
}

// ReleaseClaude unblocks the fake claude so the pane pipeline proceeds to
// _format-stream and _finalize.
func (h *Harness) ReleaseClaude() {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.E2EDir, "claude.release"), nil, 0o644); err != nil {
		h.t.Fatalf("releasing claude: %v", err)
	}
}

// WaitForClaudeStart blocks until the fake claude has started running in the
// pane (or the timeout elapses).
func (h *Harness) WaitForClaudeStart(timeout time.Duration) {
	h.t.Helper()
	started := filepath.Join(h.E2EDir, "claude.started")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("fake claude did not start within %s", timeout)
}

// ClaudeArgv returns the argv the fake claude was invoked with (one entry per
// line as recorded), or "" if it has not run yet.
func (h *Harness) ClaudeArgv() string {
	b, _ := os.ReadFile(filepath.Join(h.E2EDir, "claude.argv"))
	return string(b)
}

// GHArgv returns the logged gh invocations (one per line), or "" if gh was
// never called.
func (h *Harness) GHArgv() string {
	b, _ := os.ReadFile(filepath.Join(h.E2EDir, "gh.argv"))
	return string(b)
}

// setupGitConfig writes a global gitconfig under the temp HOME so git has a
// committer identity (needed by data-ref commits) without touching the real
// user config.
func (h *Harness) setupGitConfig() {
	h.t.Helper()
	cfg := "[user]\n\tname = klaus-e2e\n\temail = klaus-e2e@example.com\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n[safe]\n\tdirectory = *\n"
	if err := os.WriteFile(filepath.Join(h.Home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		h.t.Fatalf("writing gitconfig: %v", err)
	}
}

// setupSandboxRepo creates a bare origin and a working clone with one commit on
// main, pushed to origin. klaus' worktree/branch/push operations run against
// these real repos with no network.
func (h *Harness) setupSandboxRepo() {
	h.t.Helper()
	h.git("", "init", "-q", "--bare", "-b", "main", h.OriginDir)
	h.git("", "init", "-q", "-b", "main", h.RepoDir)
	if err := os.WriteFile(filepath.Join(h.RepoDir, "README.md"), []byte("# widget\n"), 0o644); err != nil {
		h.t.Fatalf("seeding repo: %v", err)
	}
	h.git(h.RepoDir, "add", "-A")
	h.git(h.RepoDir, "commit", "-qm", "init")
	h.git(h.RepoDir, "remote", "add", "origin", h.OriginDir)
	h.git(h.RepoDir, "push", "-q", "origin", "main")
}

// writeRepoConfig writes .klaus/config.json pointing WorktreeBase at the temp
// root and setting pr_reviewer (so launch's prompt render does not shell out to
// gh). It is committed so worktrees see the same config.
func (h *Harness) writeRepoConfig() {
	h.t.Helper()
	cfgDir := filepath.Join(h.RepoDir, ".klaus")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		h.t.Fatalf("mkdir .klaus: %v", err)
	}
	cfg := map[string]any{
		"worktree_base":  filepath.Join(filepath.Dir(h.RepoDir), "worktrees"),
		"default_budget": "5.00",
		"default_branch": "main",
		"data_ref":       "refs/klaus/data",
		"pr_reviewer":    "acme-bot",
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), append(data, '\n'), 0o644); err != nil {
		h.t.Fatalf("writing repo config: %v", err)
	}
	h.git(h.RepoDir, "add", "-A")
	h.git(h.RepoDir, "commit", "-qm", "add klaus config")
	h.git(h.RepoDir, "push", "-q", "origin", "main")
}

// startTmuxServer launches an isolated tmux server on h.Sock and points its
// panes at a no-profile shell wrapper so they inherit BinDir-first PATH and the
// temp HOME (the user's login shell would otherwise reset PATH via the Nix
// profile). Registers cleanup to kill the server.
func (h *Harness) startTmuxServer() {
	h.t.Helper()

	// A wrapper shell that forces a deterministic PATH/HOME and refuses to
	// source the user's profile. tmux runs pane commands via the default-shell;
	// without this, a login shell would reset PATH and our stubs/binary would
	// not be found.
	h.shellWrap = filepath.Join(filepath.Dir(h.BinDir), "shellwrap")
	wrap := fmt.Sprintf("#!/bin/sh\nexport HOME=%q\nexport PATH=%q\nexec %q --noprofile --norc \"$@\"\n",
		h.Home, h.BinDir+":"+os.Getenv("PATH"), mustBash(h.t))
	if err := os.WriteFile(h.shellWrap, []byte(wrap), 0o755); err != nil {
		h.t.Fatalf("writing shell wrapper: %v", err)
	}

	// Start the server and set the global default-shell *before* creating the
	// session, so even the session's initial pane uses the no-profile wrapper
	// rather than the system default shell (which could source login profiles).
	// h.tmux passes -f /dev/null, so the server starts ignoring any
	// user/system tmux.conf for full isolation. exit-empty must be turned off
	// in the same command that starts the server, otherwise the freshly started
	// (session-less) server exits before we can set default-shell.
	h.tmux("start-server", ";", "set-option", "-g", "exit-empty", "off")
	h.tmux("set-option", "-g", "default-shell", h.shellWrap)
	h.tmux("new-session", "-d", "-s", "main", "-x", "200", "-y", "50")
	h.t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", h.Sock, "-f", "/dev/null", "kill-server").Run()
	})

	h.ServerPID = strings.TrimSpace(h.tmux("display-message", "-p", "#{pid}"))
	panes := h.ListPanes()
	if len(panes) == 0 {
		h.t.Fatal("isolated tmux server has no panes")
	}
	h.InitialPane = panes[0]
}

// env returns the environment for a klaus invocation: isolated HOME, BinDir-first
// PATH, the session id, and $TMUX/$TMUX_PANE pointing at the isolated server.
func (h *Harness) env() []string {
	return []string{
		"HOME=" + h.Home,
		"PATH=" + h.BinDir + ":" + os.Getenv("PATH"),
		"KLAUS_SESSION_ID=" + h.SessionID,
		"TMUX=" + h.Sock + "," + h.ServerPID + ",0",
		"TMUX_PANE=" + h.InitialPane,
		"TERM=xterm",
	}
}

// RunKlaus runs the real klaus binary with the isolated environment, with CWD
// set to the sandbox repo. It returns stdout/stderr/exit and never fails the
// test on a non-zero exit (callers assert as needed).
func (h *Harness) RunKlaus(args ...string) RunResult {
	h.t.Helper()
	cmd := exec.Command(klausBinary, args...)
	cmd.Dir = h.RepoDir
	cmd.Env = h.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("running klaus %v: %v", args, err)
	}
	return res
}

// tmux runs a control-plane tmux command against the isolated server and
// returns trimmed stdout, failing the test on error.
func (h *Harness) tmux(args ...string) string {
	h.t.Helper()
	full := append([]string{"-S", h.Sock, "-f", "/dev/null"}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	if err != nil {
		h.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

// ListPanes returns all pane ids on the isolated server.
func (h *Harness) ListPanes() []string {
	h.t.Helper()
	out := h.tmux("list-panes", "-a", "-F", "#{pane_id}")
	var panes []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			panes = append(panes, s)
		}
	}
	return panes
}

// PaneExists reports whether the given pane id is alive on the isolated server.
func (h *Harness) PaneExists(paneID string) bool {
	for _, p := range h.ListPanes() {
		if p == paneID {
			return true
		}
	}
	return false
}

// PaneTitle returns the title of the given pane.
func (h *Harness) PaneTitle(paneID string) string {
	h.t.Helper()
	return strings.TrimSpace(h.tmux("display-message", "-p", "-t", paneID, "#{pane_title}"))
}

// RunsDir is the directory holding this session's run-state JSON files.
func (h *Harness) RunsDir() string {
	return filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "runs")
}

// RunIDs returns the run ids (state file basenames) currently on disk.
func (h *Harness) RunIDs() []string {
	entries, err := os.ReadDir(h.RunsDir())
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	return ids
}

// ReadState reads and decodes a run's state JSON from the isolated HOME.
func (h *Harness) ReadState(runID string) (*run.State, error) {
	data, err := os.ReadFile(filepath.Join(h.RunsDir(), runID+".json"))
	if err != nil {
		return nil, err
	}
	var st run.State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// WaitForState polls a run's state until pred is satisfied or the timeout
// elapses, returning the final state. It fails the test on timeout.
func (h *Harness) WaitForState(runID string, pred func(*run.State) bool, timeout time.Duration) *run.State {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last *run.State
	for time.Now().Before(deadline) {
		st, err := h.ReadState(runID)
		if err == nil {
			last = st
			if pred(st) {
				return st
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("state predicate not satisfied for %s within %s (last: %+v)", runID, timeout, last)
	return last
}

// SeedState writes a run-state JSON directly into the session store, for tests
// that exercise read paths (e.g. status) without launching an agent.
func (h *Harness) SeedState(st *run.State) {
	h.t.Helper()
	if err := os.MkdirAll(h.RunsDir(), 0o755); err != nil {
		h.t.Fatalf("mkdir runs: %v", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		h.t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.RunsDir(), st.ID+".json"), data, 0o644); err != nil {
		h.t.Fatalf("writing seeded state: %v", err)
	}
}

func (h *Harness) git(dir string, args ...string) {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "HOME="+h.Home, "GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func mustBash(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash not found: %v", err)
	}
	return p
}
