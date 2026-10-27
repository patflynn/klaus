// Package valley drives the `valley` CLI of a valley, the self-hosted
// integration host that direct-push repos deliver to: marking a pushed head
// ready as a version of its change, returning a change to draft, and reading
// the valley's view of every change.
package valley

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/git"
)

const (
	// ReadyTimeout bounds `valley ready`.
	ReadyTimeout = 30 * time.Second
	// UnreadyTimeout bounds `valley unready`.
	UnreadyTimeout = 30 * time.Second
	// ListTimeout bounds `valley changes`.
	ListTimeout = time.Minute
	// helpTimeout bounds `valley help`.
	helpTimeout = 10 * time.Second
	// gitTimeout bounds each git command detection runs.
	gitTimeout = 30 * time.Second
	// maxReason caps the length of a failure's reason.
	maxReason = 200
)

// Error is a valley command that failed: it refused (exited non-zero), timed
// out, couldn't start, or printed something klaus can't read. Reason is
// short enough to record on a run.
type Error struct {
	Verb   string
	Reason string
	// Conflict is set when valley exited with status 3, a conflict: for a
	// knot write, another write changed what it depends on.
	Conflict bool
	exited   bool // valley ran and exited non-zero: Reason is its own message
}

func (e *Error) Error() string {
	return "valley " + e.Verb + ": " + e.Reason
}

// Readied is what `valley ready` reports: the repo, the change's handle and
// the version the head became.
type Readied struct {
	Repo    string
	Handle  string
	Version int
}

// readyLine is the line `valley ready` prints on success, e.g.
// "cosmo nxvp v2 ready".
var readyLine = regexp.MustCompile(`^(\S+) (\S+) v(\d+) ready$`)

// Ready runs `valley ready --change <changeID> --head <head> <branch>` in dir,
// marking head ready as a version of the change. The valley identifies the
// pusher from git config valley.principal, which the host sets, not klaus.
func Ready(ctx context.Context, dir, changeID, head, branch string) (Readied, error) {
	out, err := command(ctx, dir, ReadyTimeout, "ready", "--change", changeID, "--head", head, branch)
	if err != nil {
		return Readied{}, err
	}
	return parseReady(out)
}

func parseReady(out []byte) (Readied, error) {
	for _, line := range strings.Split(string(out), "\n") {
		m := readyLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[3]); err == nil {
			return Readied{Repo: m[1], Handle: m[2], Version: n}, nil
		}
	}
	return Readied{}, &Error{Verb: "ready", Reason: "unexpected output: " + shortReason(out)}
}

// Unreadied is what `valley unready` reports. Draft is false when the
// change wasn't ready (already a draft, or the valley has no record of it),
// and Note then says what the valley printed.
type Unreadied struct {
	Repo   string
	Handle string
	Draft  bool
	Note   string
}

// draftLine is the line `valley unready` prints on success, e.g.
// "cosmo nxvp v1 draft", naming the version that was ready. A valley that
// names no version prints "cosmo nxvp draft".
var draftLine = regexp.MustCompile(`^(\S+) (\S+)(?: v\d+)? draft$`)

// notReady matches a message that only says the change wasn't ready to
// begin with, which leaves it as unready asked. The valley exits 1 for this
// refusal as for every other, so only its wording tells them apart: it says
// "is a draft already; ready it to resume it".
var notReady = regexp.MustCompile(`(?i)\bnot ready\b|\balready (a )?draft\b|\bdraft already\b|\bno (change )?record\b`)

// Unready runs `valley unready <changeID>` in dir, returning a ready change
// to draft so approve refuses it while an agent continues it. A change that
// wasn't ready isn't an error, whether valley says so or refuses for that
// reason: it is reported with Draft false. Any other output is an error,
// since the change may still be approvable.
func Unready(ctx context.Context, dir, changeID string) (Unreadied, error) {
	out, err := command(ctx, dir, UnreadyTimeout, "unready", changeID)
	var e *Error
	if errors.As(err, &e) && e.exited && notReady.MatchString(e.Reason) {
		return Unreadied{Note: e.Reason}, nil
	}
	if err != nil {
		return Unreadied{}, err
	}
	return parseUnready(out)
}

func parseUnready(out []byte) (Unreadied, error) {
	for _, line := range strings.Split(string(out), "\n") {
		if m := draftLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return Unreadied{Repo: m[1], Handle: m[2], Draft: true}, nil
		}
	}
	if msg := shortReason(out); notReady.MatchString(msg) {
		return Unreadied{Note: msg}, nil
	}
	return Unreadied{}, &Error{Verb: "unready", Reason: "unexpected output: " + shortReason(out)}
}

// List runs `valley changes --all --json` in dir: one process for every repo
// the valley serves.
func List(ctx context.Context, dir string) (*Changes, error) {
	out, err := command(ctx, dir, ListTimeout, "changes", "--all", "--json")
	if err != nil {
		return nil, err
	}
	return Decode(out)
}

// ListRepo runs `valley changes --json` in dir: the valley's view of the
// repo in dir alone.
func ListRepo(ctx context.Context, dir string) (*Changes, error) {
	out, err := command(ctx, dir, ListTimeout, "changes", "--json")
	if err != nil {
		return nil, err
	}
	return Decode(out)
}

// changeVerbs are the verbs klaus needs to mark changes ready and read them.
var changeVerbs = []string{"ready", "changes"}

// Supported reports whether the valley on PATH has the verbs klaus uses: its
// help lists both ready and changes. A valley from before change records
// lacks them, and klaus treats its repos as plain direct-push repos.
func Supported(ctx context.Context) bool {
	return hasVerbs(ctx, changeVerbs...)
}

// HasUnready reports whether the valley on PATH lists unready, which a
// valley from before drafts lacks.
func HasUnready(ctx context.Context) bool {
	return hasVerbs(ctx, "unready")
}

// hasVerbs reports whether the valley on PATH lists every one of verbs in
// its help.
func hasVerbs(ctx context.Context, verbs ...string) bool {
	if _, err := exec.LookPath("valley"); err != nil {
		return false
	}
	// Some CLIs exit non-zero from help; the listing is what counts.
	stdout, stderr, _ := execValley(ctx, "", helpTimeout, nil, "help")
	help := append(stdout, stderr...)
	for _, v := range verbs {
		if !hasVerb(help, v) {
			return false
		}
	}
	return true
}

// hasVerb reports whether help lists verb: a line that starts with it, after
// at most a little indentation and an optional "usage: valley". Prose that
// merely uses the word sits deeper in the text and doesn't count.
func hasVerb(help []byte, verb string) bool {
	re := regexp.MustCompile(`(?m)^[ \t]{0,4}(?:usage:[ \t]+)?(?:valley[ \t]+)?` + regexp.QuoteMeta(verb) + `\b`)
	return re.Match(help)
}

// Detect reports whether the repo in dir delivers to a valley that klaus can
// mark changes ready on: its origin isn't GitHub, the pushed head of branch
// carries the valley's policy (HasPolicy), and the valley on PATH is
// Supported. A head the repo lacks, as when a sandbox pushed it, is fetched
// from origin first. The cheap checks come first, so a repo without the
// policy never runs valley.
func Detect(ctx context.Context, dir, branch, head string) bool {
	return detect(ctx, dir, branch, head, changeVerbs...)
}

// DetectUnready is Detect for a launch that continues a change, with head
// the commit its worktree starts from: it also requires the valley on PATH
// to list unready, which a valley from before drafts lacks.
func DetectUnready(ctx context.Context, dir, head string) bool {
	return detect(ctx, dir, "", head, append(changeVerbs, "unready")...)
}

func detect(ctx context.Context, dir, branch, head string, verbs ...string) bool {
	if dir == "" || head == "" {
		return false
	}
	origin, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil || origin == "" || git.IsGitHubRemote(origin) {
		return false
	}
	if _, err := gitOutput(ctx, dir, "cat-file", "-e", head+"^{commit}"); err != nil && branch != "" {
		_, _ = gitOutput(ctx, dir, "fetch", "--quiet", "--no-tags", "origin",
			"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	}
	return HasPolicy(ctx, dir, head) && hasVerbs(ctx, verbs...)
}

// HasPolicy reports whether commit head, in the repo at dir, carries
// policy/valley or a policy/instance/ directory. A head the repo doesn't have
// carries neither.
func HasPolicy(ctx context.Context, dir, head string) bool {
	out, err := gitOutput(ctx, dir, "ls-tree", head, "--", "policy/valley", "policy/instance")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode, type, object
		switch {
		case path == "policy/valley":
			return true
		case path == "policy/instance" && len(f) == 3 && f[1] == "tree":
			return true
		}
	}
	return false
}

// Principal returns the name the valley knows the repo in dir's pusher by:
// its git config valley.principal, which the host sets. "" when unset.
func Principal(ctx context.Context, dir string) string {
	p, err := gitOutput(ctx, dir, "config", "--get", "valley.principal")
	if err != nil {
		return ""
	}
	return p
}

// OriginRefs reads where each of refs is on the origin of the repo in dir,
// with one ls-remote. A ref origin lacks is missing from the map.
func OriginRefs(ctx context.Context, dir string, refs ...string) (map[string]string, error) {
	out, err := gitOutput(ctx, dir, append([]string{"ls-remote", "origin"}, refs...)...)
	if err != nil {
		return nil, fmt.Errorf("reading origin's refs: %w", err)
	}
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r] = true
	}
	got := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		oid, ref, ok := strings.Cut(line, "\t")
		if ok && want[ref] {
			got[ref] = oid
		}
	}
	return got, nil
}

// command runs valley and returns its stdout, or an *Error.
func command(ctx context.Context, dir string, timeout time.Duration, args ...string) ([]byte, error) {
	return commandInput(ctx, dir, timeout, nil, args...)
}

// commandInput is command with stdin, for a verb that reads it.
func commandInput(ctx context.Context, dir string, timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	stdout, stderr, err := execValley(ctx, dir, timeout, stdin, args...)
	if err == nil {
		return stdout, nil
	}
	e := &Error{Verb: args[0]}
	if args[0] == "knot" && len(args) > 1 {
		e.Verb += " " + args[1]
	}
	var exitErr *exec.ExitError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Reason = fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &exitErr):
		// A refusal: the valley's message, from stderr or else stdout.
		e.exited = true
		e.Conflict = exitErr.ExitCode() == 3
		e.Reason = shortReason(stderr)
		if e.Reason == "" {
			e.Reason = shortReason(stdout)
		}
		if e.Reason == "" {
			e.Reason = err.Error()
		}
	default:
		e.Reason = err.Error()
	}
	return nil, e
}

// execValley runs valley in dir (the current directory when empty), with
// stdin on its standard input. It never prompts: stdin is all valley reads,
// git's terminal prompt is off, and the command runs in a new session with no
// controlling terminal, so ssh can't ask for a passphrase on the dashboard's
// screen. A timeout reports
// context.DeadlineExceeded, and kills valley and everything it started.
func execValley(ctx context.Context, dir string, timeout time.Duration, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "valley", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0") // Environ sets PWD to Dir
	isolate(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.Bytes(), errOut.Bytes(), err
}

// shortReason joins a message's non-empty lines and caps its length.
func shortReason(msg []byte) string {
	var lines []string
	for _, l := range strings.Split(string(msg), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	r := []rune(strings.Join(lines, "; "))
	if len(r) > maxReason {
		return string(r[:maxReason-1]) + "…"
	}
	return string(r)
}

// gitOutput runs git in dir and returns its trimmed stdout. Like valley, it
// never prompts.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	isolate(cmd)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// isolate runs cmd, made with exec.CommandContext, in a new session with no
// controlling terminal. That makes cmd the leader of its own process group
// (Setpgid on top would fail: a session leader can't change group), and when
// the context ends the whole group is killed, so a child of cmd, such as the
// ssh or git carrying out a ready, can't finish the work after klaus has
// recorded that it timed out.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// Bounds the wait for output pipes held open by anything that left the
	// group.
	cmd.WaitDelay = 5 * time.Second
}
