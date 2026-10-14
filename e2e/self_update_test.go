//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/selfupdate"
)

// selfUpdateEnv is a harness whose host config turns self-update on against
// the sandbox's bare origin, with real git and a stub nix: nix build makes a
// fake store path whose bin/klaus reports the requested commit, and the
// profile is a file holding the klaus element's store path.
type selfUpdateEnv struct {
	*Harness
	store   string // the stub's store
	profile string // the stub profile's klaus element
}

func newSelfUpdateEnv(t *testing.T) *selfUpdateEnv {
	h := NewHarness(t)
	e := &selfUpdateEnv{Harness: h, store: filepath.Join(h.E2EDir, "store"), profile: filepath.Join(h.E2EDir, "profile.klaus")}
	e.writeHostConfig(map[string]any{"self_update": map[string]any{"repo": h.OriginDir, "branch": "main"}})
	h.WriteStub("nix", fmt.Sprintf(`#!/usr/bin/env bash
dir=%q store=%q profile=%q
{ printf 'nix'; for a in "$@"; do printf ' %%s' "$a"; done; printf '\n'; } >> "$dir/nix.argv"
case "$1 $2" in
"build --no-link")
  : > "$dir/build.started"
  while [ -e "$dir/build.hold" ]; do sleep 0.05; done
  [ -e "$dir/build.fail" ] && { echo "error: builder for klaus failed with exit code 1" >&2; exit 1; }
  ref="${@: -1}"; rev="${ref##*rev=}"; rev="${rev%%%%#*}"; short="${rev:0:7}"
  out="$store/$rev-klaus-0.4.1-$short"
  mkdir -p "$out/bin"
  if [ -e "$dir/status.fail" ]; then status='echo "error: cannot read run state" >&2; exit 1'; else status='echo "No runs found."'; fi
  printf '#!/usr/bin/env bash\ncase "$1" in\n--version) echo "klaus version 0.4.1-%%s";;\nstatus) %%s;;\nesac\n' "$short" "$status" > "$out/bin/klaus"
  chmod +x "$out/bin/klaus"
  echo "$out";;
"profile list")
  k=$(cat "$profile" 2>/dev/null)
  if [ -n "$k" ]; then printf '{"elements":{"htop":{"storePaths":["/nix/store/h-htop-3"]},"klaus":{"storePaths":["%%s"]}},"version":3}\n' "$k"
  else printf '{"elements":{"htop":{"storePaths":["/nix/store/h-htop-3"]}},"version":3}\n'; fi;;
"profile remove") echo "remove $3" >> "$dir/profile.log"; : > "$profile";;
"profile add") echo "add $3" >> "$dir/profile.log"; printf '%%s' "$3" > "$profile";;
*) echo "nix stub: unexpected $*" >&2; exit 2;;
esac
`, h.E2EDir, e.store, e.profile))
	return e
}

func (e *selfUpdateEnv) writeHostConfig(cfg map[string]any) {
	e.t.Helper()
	dir := filepath.Join(e.Home, ".klaus")
	data, _ := json.Marshal(cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// advance commits to main and pushes it to the origin, returning the new tip.
func (e *selfUpdateEnv) advance(msg string) string {
	e.t.Helper()
	e.git(e.RepoDir, "commit", "-q", "--allow-empty", "-m", msg)
	e.git(e.RepoDir, "push", "-q", "origin", "main")
	return e.GitOutput(e.RepoDir, "rev-parse", "HEAD")
}

func (e *selfUpdateEnv) tip() string { return e.GitOutput(e.RepoDir, "rev-parse", "HEAD") }

func (e *selfUpdateEnv) out(sha string) string {
	return filepath.Join(e.store, sha+"-klaus-0.4.1-"+sha[:7])
}

func (e *selfUpdateEnv) state() selfupdate.State {
	e.t.Helper()
	st, err := selfupdate.Load(filepath.Join(e.Home, ".local", "state", "klaus"))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *selfUpdateEnv) builds() int {
	data, _ := os.ReadFile(filepath.Join(e.E2EDir, "nix.argv"))
	return strings.Count(string(data), "nix build ")
}

func (e *selfUpdateEnv) profileKlaus() string {
	data, _ := os.ReadFile(e.profile)
	return string(data)
}

func (e *selfUpdateEnv) profileLog() string {
	data, _ := os.ReadFile(filepath.Join(e.E2EDir, "profile.log"))
	return string(data)
}

func (e *selfUpdateEnv) flag(name string, on bool) {
	e.t.Helper()
	p := filepath.Join(e.E2EDir, name)
	if on {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	_ = os.Remove(p)
}

func (e *selfUpdateEnv) selfUpdate(wantExit int, args ...string) RunResult {
	e.t.Helper()
	res := e.RunKlausIn(e.E2EDir, append([]string{"self-update"}, args...)...)
	if res.ExitCode != wantExit {
		e.t.Fatalf("klaus self-update %v exited %d, want %d\nstdout:\n%s\nstderr:\n%s", args, res.ExitCode, wantExit, res.Stdout, res.Stderr)
	}
	return res
}

func TestSelfUpdateRefusesWithoutConfig(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	e.writeHostConfig(map[string]any{})
	res := e.selfUpdate(1)
	if !strings.Contains(res.Stderr, "self-update is off") || !strings.Contains(res.Stderr, `"self_update"`) {
		t.Errorf("want a refusal that shows how to enable self_update, got:\n%s", res.Stderr)
	}
	if e.builds() != 0 {
		t.Error("built without config")
	}
}

// TestSelfUpdateRefusesExtRepo: an ext:: repo, whose command git would run
// here since git config allows the transport, is refused and runs nothing.
func TestSelfUpdateRefusesExtRepo(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	marker := filepath.Join(e.E2EDir, "pwned")
	e.WriteStub("pwn", fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker))
	f, err := os.OpenFile(filepath.Join(e.Home, ".gitconfig"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, "[protocol \"ext\"]\n\tallow = always\n")
	f.Close()

	for _, repo := range []string{"ext::pwn", "ext::sh -c pwn"} {
		e.writeHostConfig(map[string]any{"self_update": map[string]any{"repo": repo}})
		res := e.selfUpdate(1)
		if !strings.Contains(res.Stderr, fmt.Sprintf("self_update.repo %q is not a repo self-update accepts", repo)) {
			t.Errorf("repo %q: want a refusal naming it, got:\n%s", repo, res.Stderr)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("repo %q: the ext:: command ran", repo)
		}
	}
	if e.builds() != 0 {
		t.Error("built from a refused repo")
	}
}

// TestSelfUpdateInstallsMovesAndRollsBack: install the tip, stay put while
// it is installed, install the next tip when main moves, and roll back and
// forward again.
func TestSelfUpdateInstallsMovesAndRollsBack(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	first := e.tip()

	res := e.selfUpdate(0)
	for _, want := range []string{
		"main is at " + first[:7],
		"building git+file://" + e.OriginDir + "?ref=refs/heads/main&rev=" + first + "#packages.",
		"built " + e.out(first),
		"smoke test passed: klaus version 0.4.1-" + first[:7] + "; klaus status exits 0",
		"installed " + first[:7] + " into the nix profile, replacing nothing",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("first run: missing %q in:\n%s", want, res.Stdout)
		}
	}
	if got := e.profileKlaus(); got != e.out(first) {
		t.Fatalf("profile klaus = %q, want %q", got, e.out(first))
	}
	if st := e.state(); st.Current == nil || st.Current.Commit != first || st.Previous != nil {
		t.Fatalf("state after first install: %+v", st)
	}

	// Up to date: no build, and nothing said unless -v.
	if res := e.selfUpdate(0); res.Stdout != "" {
		t.Errorf("up-to-date run printed:\n%s", res.Stdout)
	}
	if res := e.selfUpdate(0, "-v"); !strings.Contains(res.Stdout, "main is at "+first[:7]+": klaus is up to date") {
		t.Errorf("-v up-to-date run printed:\n%s", res.Stdout)
	}
	if n := e.builds(); n != 1 {
		t.Fatalf("%d builds, want 1: up-to-date runs must not build", n)
	}

	second := e.advance("second")
	res = e.selfUpdate(0)
	if !strings.Contains(res.Stdout, "installed "+second[:7]+" into the nix profile, replacing "+first[:7]) {
		t.Errorf("second run:\n%s", res.Stdout)
	}
	if got := e.profileKlaus(); got != e.out(second) {
		t.Fatalf("profile klaus = %q, want %q", got, e.out(second))
	}
	st := e.state()
	if st.Current == nil || st.Current.Commit != second || st.Current.StorePath != e.out(second) ||
		st.Previous == nil || st.Previous.Commit != first || st.Previous.StorePath != e.out(first) {
		t.Fatalf("state after the branch moved: current %+v previous %+v", st.Current, st.Previous)
	}
	if log := e.profileLog(); log != "add "+e.out(first)+"\nremove klaus\nadd "+e.out(second)+"\n" {
		t.Errorf("profile operations:\n%s", log)
	}

	// A klaus removed from the profile by hand is reinstalled, and the
	// rollback target stays.
	if err := os.WriteFile(e.profile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := e.selfUpdate(0); !strings.Contains(res.Stdout, "the nix profile's klaus is no longer "+second[:7]+"; reinstalling it") {
		t.Errorf("run after the profile lost klaus:\n%s", res.Stdout)
	}
	if got := e.profileKlaus(); got != e.out(second) {
		t.Fatalf("profile klaus = %q, want it reinstalled: %q", got, e.out(second))
	}
	if st := e.state(); st.Current.Commit != second || st.Previous == nil || st.Previous.Commit != first {
		t.Fatalf("state after the reinstall: current %+v previous %+v; want the rollback target kept", st.Current, st.Previous)
	}

	res = e.selfUpdate(0, "--rollback")
	if !strings.Contains(res.Stdout, "rolled back to "+first[:7]) || !strings.Contains(res.Stdout, "--rollback again returns to "+second[:7]) {
		t.Errorf("rollback:\n%s", res.Stdout)
	}
	if got := e.profileKlaus(); got != e.out(first) {
		t.Errorf("after rollback profile klaus = %q, want %q", got, e.out(first))
	}
	if st := e.state(); st.Current.Commit != first || st.Previous.Commit != second {
		t.Errorf("state after rollback: current %+v previous %+v", st.Current, st.Previous)
	}
	e.selfUpdate(0, "--rollback")
	if got := e.profileKlaus(); got != e.out(second) {
		t.Errorf("after a second rollback profile klaus = %q, want %q", got, e.out(second))
	}
	if st := e.state(); st.Current.Commit != second || st.Previous.Commit != first {
		t.Errorf("state after the second rollback: current %+v previous %+v", st.Current, st.Previous)
	}
}

// TestSelfUpdateSkipsAFailedCommit: a commit that fails its smoke test
// installs nothing and is skipped until the branch moves or --retry.
func TestSelfUpdateSkipsAFailedCommit(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	bad := e.tip()
	e.flag("status.fail", true)

	res := e.selfUpdate(1)
	if !strings.Contains(res.Stderr, "smoke test: klaus status") || !strings.Contains(res.Stdout, bad[:7]+" failed; nothing installed") {
		t.Errorf("failed run:\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	if e.profileLog() != "" {
		t.Fatalf("a failed build touched the profile:\n%s", e.profileLog())
	}
	if st := e.state(); st.Failed == nil || st.Failed.Commit != bad || !strings.Contains(st.Failed.Error, "cannot read run state") || st.Current != nil {
		t.Fatalf("state after the failure: %+v", st)
	}

	res = e.selfUpdate(0)
	if !strings.Contains(res.Stdout, "skipping "+bad[:7]+": it failed at") || !strings.Contains(res.Stdout, "cannot read run state") {
		t.Errorf("rerun should skip the failed commit, naming the failure:\n%s", res.Stdout)
	}
	if n := e.builds(); n != 1 {
		t.Fatalf("%d builds, want 1: a known failure must not rebuild", n)
	}

	// --retry builds it again; it still fails.
	e.selfUpdate(1, "--retry")
	if n := e.builds(); n != 2 {
		t.Fatalf("%d builds after --retry, want 2", n)
	}

	// The branch moves to a fix: it builds and installs, and the failure clears.
	e.flag("status.fail", false)
	fixed := e.advance("fix")
	e.selfUpdate(0)
	if got := e.profileKlaus(); got != e.out(fixed) {
		t.Errorf("profile klaus = %q, want the fix %q", got, e.out(fixed))
	}
	if st := e.state(); st.Failed != nil || st.Current.Commit != fixed {
		t.Errorf("state after the fix: %+v", st)
	}
}

func TestSelfUpdatePin(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	first := e.tip()
	e.selfUpdate(0)

	if res := e.selfUpdate(0, "--pin"); !strings.Contains(res.Stdout, "pinned at "+first[:7]) {
		t.Errorf("--pin:\n%s", res.Stdout)
	}
	e.advance("second")
	if res := e.selfUpdate(0); !strings.Contains(res.Stdout, "pinned at "+first[:7]+": leaving it alone until klaus self-update --unpin") {
		t.Errorf("pinned run:\n%s", res.Stdout)
	}
	if n := e.builds(); n != 1 {
		t.Fatalf("%d builds, want 1: a pinned run must not build", n)
	}

	e.selfUpdate(0, "--unpin")
	e.selfUpdate(0)
	if got := e.profileKlaus(); got != e.out(e.tip()) {
		t.Errorf("after --unpin profile klaus = %q, want %q", got, e.out(e.tip()))
	}
}

// TestSelfUpdateConcurrentRuns: a run that starts while another builds
// exits 0 without touching anything.
func TestSelfUpdateConcurrentRuns(t *testing.T) {
	t.Parallel()
	e := newSelfUpdateEnv(t)
	e.flag("build.hold", true)

	first := exec.Command(klausBinary, "self-update")
	first.Dir, first.Env = e.E2EDir, e.env()
	var out bytes.Buffer
	first.Stdout, first.Stderr = &out, &out
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(e.E2EDir, "build.started")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first run never started building:\n%s", out.String())
		}
	}

	res := e.selfUpdate(0)
	if strings.TrimSpace(res.Stdout) != "another self-update is in progress; leaving it to finish" {
		t.Errorf("second run:\n%s", res.Stdout)
	}

	e.flag("build.hold", false)
	if err := first.Wait(); err != nil {
		t.Fatalf("first run: %v\n%s", err, out.String())
	}
	if n := e.builds(); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
	if got := e.profileKlaus(); got != e.out(e.tip()) {
		t.Errorf("profile klaus = %q, want %q", got, e.out(e.tip()))
	}
}

// TestDashboardRestartsOntoNewKlaus runs the real dashboard in a tmux pane,
// with a nix profile's bin ahead of the klaus it runs on PATH. When a new
// klaus appears there, the dashboard shuts down, releasing its webhook
// port, and execs it with the same arguments and environment.
func TestDashboardRestartsOntoNewKlaus(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	h.AmendRepoConfig(map[string]any{"webhook": map[string]any{"port": port}})
	repo, merged := "acme/widget", time.Now().Add(-time.Minute).Format(time.RFC3339)
	h.SeedState(&run.State{ID: "20261014-1200-aaaa0001", Type: "launch", Prompt: "seeded", TargetRepo: &repo,
		CreatedAt: merged, MergedAt: &merged})

	profileBin := filepath.Join(h.E2EDir, "profile", "bin")
	if err := os.MkdirAll(profileBin, 0o755); err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(h.tmux("new-session", "-d", "-s", "dash", "-x", "120", "-y", "20", "-c", h.RepoDir, "-P", "-F", "#{pane_id}",
		fmt.Sprintf("env KLAUS_SESSION_ID=%s PATH=%s:$PATH %s dashboard", h.SessionID, profileBin, filepath.Join(h.BinDir, "klaus"))))
	screen := func(timeout time.Duration, want string) string {
		t.Helper()
		var last string
		for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if last = h.tmux("capture-pane", "-p", "-t", pane); strings.Contains(last, want) {
				return last
			}
		}
		t.Fatalf("screen never showed %q; last screen:\n%s", want, last)
		return ""
	}
	screen(20*time.Second, "webhook: listening on 127.0.0.1:")
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		l.Close()
		t.Fatal("the dashboard is not holding its webhook port")
	}

	// A new klaus lands in the profile, as self-update installs it.
	store := filepath.Join(h.E2EDir, "store", "new-klaus", "bin")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
[ "$1" = --version ] && { echo "klaus version 9.9.9-feedbee"; exit 0; }
dir=%q
printf '%%s\n' "$@" > "$dir/new.argv"
printf '%%s' "$KLAUS_SESSION_ID" > "$dir/new.session"
echo "new klaus running: $*"
exec sleep 600
`, h.E2EDir)
	if err := os.WriteFile(filepath.Join(store, "klaus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "klaus"), filepath.Join(profileBin, "klaus")); err != nil {
		t.Fatal(err)
	}

	// The check runs every 30s.
	screen(60*time.Second, "new klaus running: dashboard")
	argv, _ := os.ReadFile(filepath.Join(h.E2EDir, "new.argv"))
	if string(argv) != "dashboard\n" {
		t.Errorf("new klaus argv = %q, want the dashboard's", argv)
	}
	if sid, _ := os.ReadFile(filepath.Join(h.E2EDir, "new.session")); string(sid) != h.SessionID {
		t.Errorf("new klaus KLAUS_SESSION_ID = %q, want %q", sid, h.SessionID)
	}
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Errorf("webhook port still held after the restart: %v", err)
	} else {
		l.Close()
	}
}
