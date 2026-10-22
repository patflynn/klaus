//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// TestReadyAtFinalizeOffLeavesDraftForKlausReady turns ready_at_finalize off,
// in the project's config or in ~/.klaus/config.json, and lets the real
// finalize run against a fake valley: it marks the pushed head ready and
// returns the change to draft at once, records the draft and emits
// agent:change-draft, not agent:change-ready. klaus status and the dashboard
// show the change awaiting review, with no approve command. klaus ready, by
// the change's handle, marks the change's head ready, clears the draft on
// the run and emits agent:change-ready; status then shows it awaiting
// approval. While a follow-up runs on the change, klaus ready refuses. The
// project config lives only in the host checkout, uncommitted, so finalize
// reads it there, not from the agent's worktree.
func TestReadyAtFinalizeOffLeavesDraftForKlausReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
	}{
		{name: "project config"},
		{name: "global config", global: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.installValleyStub()
			h.joinValley()
			if err := os.WriteFile(filepath.Join(h.E2EDir, "valley.has-unready"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			h.AmendRepoConfig(map[string]any{
				"dashboard": map[string]any{"approve_command": "valley approve {handle} --head {head}"},
			})
			if tc.global {
				if err := os.MkdirAll(filepath.Join(h.Home, ".klaus"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.Home, ".klaus", "config.json"), []byte(`{"ready_at_finalize": false}`), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				h.AmendRepoConfig(map[string]any{"ready_at_finalize": false})
			}

			res := h.RunKlaus("launch", "add a health check endpoint")
			if res.ExitCode != 0 {
				t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			id := h.RunIDs()[0]
			h.WaitForClaudeStart(30 * time.Second)
			st, err := h.ReadState(id)
			if err != nil {
				t.Fatal(err)
			}
			worktree, branch := st.Worktree, st.Branch
			if err := os.WriteFile(filepath.Join(worktree, "health.go"), []byte("package health\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.git(worktree, "add", "-A")
			h.git(worktree, "commit", "-qm", "Add a health check")
			h.git(worktree, "push", "-q", "origin", branch)
			head := h.GitOutput(worktree, "rev-parse", "HEAD")

			h.ReleaseClaude()
			st = h.WaitForState(id, func(s *run.State) bool {
				return s.DurationMS != nil && s.TmuxPane == nil && s.Worktree == ""
			}, 60*time.Second)

			// Finalize readies the head, then returns the change to draft.
			calls := append(h.valleyCalls("ready"), h.valleyCalls("unready")...)
			want := []string{
				fmt.Sprintf("%s: ready --change %s --head %s %s", worktree, st.ChangeID, head, branch),
				fmt.Sprintf("%s: unready %s", worktree, st.ChangeID),
			}
			if strings.Join(calls, "\n") != strings.Join(want, "\n") {
				t.Fatalf("valley calls = %q, want %q", calls, want)
			}
			if st.Valley == nil || *st.Valley != (run.Valley{Repo: "widget", Handle: "nxvp", Version: 1, Title: "Add a health check", Draft: true}) {
				t.Fatalf("recorded valley = %+v, want a draft of widget nxvp v1", st.Valley)
			}
			var types []string
			var draft map[string]any
			for _, e := range h.runEvents(id) {
				types = append(types, e.Type)
				if e.Type == event.AgentChangeDraft {
					draft = e.Data
				}
			}
			wantTypes := []string{event.AgentCompleted, event.AgentBranchPushed, event.AgentChangeDraft, event.PhaseCompleted}
			if len(types) < len(wantTypes) || strings.Join(types[len(types)-len(wantTypes):], ",") != strings.Join(wantTypes, ",") {
				t.Errorf("events = %v, want them to end with %v", types, wantTypes)
			}
			for _, typ := range types {
				if typ == event.AgentChangeReady {
					t.Errorf("a draft must not emit %s: events %v", typ, types)
				}
			}
			if draft["handle"] != "nxvp" || draft["head"] != head || draft["version"] != float64(1) || draft["change_id"] != st.ChangeID {
				t.Errorf("agent:change-draft data = %v", draft)
			}

			// The valley lists the change as a draft at the run's head.
			listing := func(state string) {
				v := version(1, head, "Add a health check", "not-landed")
				v["branch"] = branch
				h.writeValleyChanges(map[string]any{
					"schema": "valley-changes/v1",
					"host":   "classic-laddie",
					"repos": []any{map[string]any{
						"repo": "widget", "url": "git@classic-laddie:widget.git", "error": nil,
						"changes":  []any{change(st, state, 1, v)},
						"branches": []any{},
					}},
				})
			}
			listing("draft")
			res = h.RunKlaus("status")
			if res.ExitCode != 0 {
				t.Fatalf("status exited %d; stderr: %s", res.ExitCode, res.Stderr)
			}
			if got := statusOf(t, res.Stdout, id); got != "awaiting-review" {
				t.Errorf("status of the draft = %q, want awaiting-review:\n%s", got, res.Stdout)
			}

			if !tc.global {
				// The dashboard: REVIEW with the pipeline, no approve
				// command, and the klaus ready that readies it.
				_, screen := h.startDashboard(170)
				s := screen("1 pipeline", "REVIEW  v1@"+head[:7], "klaus ready nxvp")
				if strings.Contains(s, "approve: ") || strings.Contains(s, "READY") {
					t.Errorf("a draft awaiting review must offer no approve command:\n%s", s)
				}
			}

			res = h.RunKlaus("ready", "nxvp")
			if res.ExitCode != 0 {
				t.Fatalf("klaus ready exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			if got, want := strings.TrimSpace(res.Stdout), "widget nxvp v1 ready (run "+id+")"; got != want {
				t.Errorf("klaus ready printed %q, want %q", got, want)
			}
			readies := h.valleyCalls("ready")
			if want := fmt.Sprintf("%s: ready --change %s --head %s %s", h.RepoDir, st.ChangeID, head, branch); len(readies) != 2 || readies[1] != want {
				t.Errorf("valley ready calls = %q, want the second to be %q", readies, want)
			}
			st, err = h.ReadState(id)
			if err != nil {
				t.Fatal(err)
			}
			if st.Valley == nil || *st.Valley != (run.Valley{Repo: "widget", Handle: "nxvp", Version: 1, Title: "Add a health check"}) {
				t.Errorf("after klaus ready, recorded valley = %+v, want widget nxvp v1 ready", st.Valley)
			}
			var ready map[string]any
			for _, e := range h.runEvents(id) {
				if e.Type == event.AgentChangeReady {
					ready = e.Data
				}
			}
			if ready == nil || ready["via"] != "klaus ready" || ready["handle"] != "nxvp" || ready["head"] != head ||
				ready["version"] != float64(1) || ready["change_id"] != st.ChangeID || ready["title"] != "Add a health check" {
				t.Errorf("agent:change-ready data = %v", ready)
			}

			listing("ready")
			res = h.RunKlaus("status")
			if got := statusOf(t, res.Stdout, id); got != "awaiting-approval" {
				t.Errorf("status once ready = %q, want awaiting-approval:\n%s", got, res.Stdout)
			}
			if tc.global {
				return
			}

			// While a follow-up runs on the change, klaus ready refuses,
			// naming it, and asks the valley nothing.
			for _, f := range []string{"claude.started", "claude.release"} {
				if err := os.Remove(filepath.Join(h.E2EDir, f)); err != nil {
					t.Fatal(err)
				}
			}
			h.RegisterProject("acme/widget", h.RepoDir)
			waitNextSecond()
			follow, _ := h.launchRun("--repo", "widget", "--change", id, "test the health check")
			h.WaitForClaudeStart(30 * time.Second)
			res = h.RunKlaus("ready", "nxvp")
			if res.ExitCode == 0 || !strings.Contains(res.Stderr, "run "+follow.ID+" is still running on change nxvp") {
				t.Errorf("klaus ready during a follow-up exited %d, want a refusal naming run %s\nstdout:\n%s\nstderr:\n%s",
					res.ExitCode, follow.ID, res.Stdout, res.Stderr)
			}
			if n := len(h.valleyCalls("ready")); n != 2 {
				t.Errorf("a refused klaus ready must not run valley ready: %d calls in all", n)
			}
		})
	}
}
