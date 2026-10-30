//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDashboardReportsChangeTransitions runs the real klaus against a fake
// valley. klaus status reads where a valley change stands and reports
// nothing. The dashboard's first check on a session records where the
// change stands, approved, without reporting it; once the change lands, the
// next dashboard reports change:integrated, under the change's handle, and
// no later start reports it again. klaus notifications lists the landing.
func TestDashboardReportsChangeTransitions(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	sha := h.pushBranch("agent/20261030-0900-0001", "health.txt")
	st := h.valleyRun("20261030-0900-0001", "prompt one", "nxvp", "Add a health check", 1, sha, time.Now().Add(-5*time.Minute))
	v1 := version(1, sha, "Add a health check", "not-landed")
	v1["request"] = map[string]any{"ref": "refs/valley/requests/nxvp", "commit": strings.Repeat("a", 40)}
	listing := func(v map[string]any) {
		h.writeValleyChanges(map[string]any{"schema": "valley-changes/v1", "host": "classic-laddie",
			"repos": []any{map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
				"changes": []any{change(st, "ready", 1, v)}, "branches": []any{}}}})
	}
	listing(v1)
	sessionDir := filepath.Join(h.Home, ".klaus", "sessions", h.SessionID)
	ledger := filepath.Join(sessionDir, "change-transitions.json")
	// klaus session creates the events file when it starts a session.
	if err := os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	changeEvents := func() []string {
		var out []string
		for _, e := range h.runEvents("nxvp") {
			out = append(out, e.Type)
		}
		return out
	}

	if res := h.RunKlaus("status"); res.ExitCode != 0 {
		t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	if got := changeEvents(); len(got) != 0 {
		t.Errorf("klaus status reported %q; it only reads", got)
	}
	if _, err := os.Stat(ledger); err == nil {
		t.Error("klaus status wrote the change ledger; it only reads")
	}

	_, screen := h.startDashboard(170)
	screen("APPROVED")
	if got := changeEvents(); len(got) != 0 {
		t.Errorf("the first dashboard start reported %q; it should record the change as it stands", got)
	}
	if _, err := os.Stat(ledger); err != nil {
		t.Errorf("the first check should record where the change stands: %v", err)
	}
	h.tmux("kill-session", "-t", "dash")

	h.git(h.RepoDir, "push", "-q", "origin", sha+":refs/heads/main")
	landed := version(1, sha, "Add a health check", "landed")
	landed["request"], landed["landed_commit"] = v1["request"], sha
	listing(landed)
	for i := 0; i < 2; i++ {
		_, screen = h.startDashboard(170)
		screen("INTEGRATED")
		h.tmux("kill-session", "-t", "dash")
		if got := changeEvents(); len(got) != 1 || got[0] != "change:integrated" {
			t.Fatalf("dashboard start %d: change events %q, want change:integrated once", i+2, got)
		}
	}
	if e := h.runEvents("nxvp")[0]; e.Data["landed_commit"] != sha || e.Data["repo"] != "widget" || e.Data["title"] != "Add a health check" {
		t.Errorf("change:integrated carries %v", e.Data)
	}

	res := h.RunKlaus("notifications", "--all")
	if res.ExitCode != 0 {
		t.Fatalf("notifications exited %d; stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "1 change(s) integrated: widget nxvp v1\n") {
		t.Errorf("notifications should list the landing:\n%s", res.Stdout)
	}
}

// TestDashboardIgnoresAnOlderListing runs the real dashboard against a fake
// valley that serves a listing, then one read before it. The first check
// records the restacker's version 2. The older listing shows version 1
// approved and gone stale, which is old news: the dashboard shows it, and
// reports nothing. A newer listing's approval and rejection of version 2
// are reported.
func TestDashboardIgnoresAnOlderListing(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.installValleyStub()
	sha := h.pushBranch("agent/20261030-0900-0001", "health.txt")
	st := h.valleyRun("20261030-0900-0001", "prompt one", "nxvp", "Add a health check", 1, sha, time.Now().Add(-5*time.Minute))
	request := map[string]any{"ref": "refs/valley/requests/nxvp", "commit": strings.Repeat("a", 40)}
	v1 := version(1, sha, "Add a health check", "not-landed")
	v2 := version(2, strings.Repeat("b", 40), "Add a health check", "not-landed")
	v2["uploader"] = "restacker"
	listing := func(active int, vs ...map[string]any) {
		h.writeValleyChanges(map[string]any{"schema": "valley-changes/v1", "host": "classic-laddie",
			"repos": []any{map[string]any{"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
				"changes": []any{change(st, "ready", active, vs...)}, "branches": []any{}}}})
	}
	decided := func(v map[string]any, decision string) map[string]any {
		d := make(map[string]any, len(v))
		for k, x := range v {
			d[k] = x
		}
		d["request"], d["outcome"] = request, map[string]any{"decision": decision, "head": v["head"], "tip": sha}
		return d
	}
	sessionDir := filepath.Join(h.Home, ".klaus", "sessions", h.SessionID)
	// klaus session creates the events file when it starts a session.
	if err := os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	changeEvents := func() []string {
		var out []string
		for _, e := range h.runEvents("nxvp") {
			out = append(out, fmt.Sprintf("%s v%v", e.Type, e.Data["version"]))
		}
		return out
	}

	listing(2, v1, v2)
	_, screen := h.startDashboard(170)
	screen("READY  v2@bbbbbbb")
	h.tmux("kill-session", "-t", "dash")

	listing(1, decided(v1, "stale"))
	_, screen = h.startDashboard(170)
	screen("STALE  v1@" + sha[:7])
	h.tmux("kill-session", "-t", "dash")
	if got := changeEvents(); len(got) != 0 {
		t.Errorf("an older listing reported %q; version 1 was superseded", got)
	}

	listing(2, decided(v1, "stale"), decided(v2, "rejected"))
	_, screen = h.startDashboard(170)
	screen("REJECTED  v2@bbbbbbb")
	h.tmux("kill-session", "-t", "dash")
	if got, want := changeEvents(), []string{"change:approved v2", "change:rejected v2"}; !slices.Equal(got, want) {
		t.Errorf("change events %q, want %q", got, want)
	}
}
