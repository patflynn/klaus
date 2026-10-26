//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// TestSharedGitConfigChange: every worktree of a clone shares its git
// config. A worker that adds a setting there has its run flagged at
// finalize: agent:shared-git-changed names the file, and klaus status shows
// the run needs-attention with a line naming it. A worker whose only write
// there is its branch's upstream, as git push -u makes, isn't flagged.
func TestSharedGitConfigChange(t *testing.T) {
	for _, tc := range []struct {
		name, write string
		flagged     bool
	}{
		{"setting", `git config --local e2e.marker set`, true},
		{"upstream", `b=$(git branch --show-current); git config "branch.$b.remote" origin; git config "branch.$b.merge" "refs/heads/$b"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.WriteStub("claude", strings.Replace(h.claudeStubScript(), `cat "$dir/claude.output.jsonl"`,
				tc.write+"\ncat \"$dir/claude.output.jsonl\"", 1))
			st := launchOne(t, h, "small task")
			if st.SharedGit == nil || st.SharedGit.Files["config"] == "" {
				t.Fatalf("launch recorded no snapshot of the clone's config: %+v", st.SharedGit)
			}
			h.WaitForClaudeStart(30 * time.Second)
			releaseBackendWorkers(t, h)

			final, err := h.ReadState(st.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(final.SharedGit.Changed); tc.flagged != (got == "[config]") || !tc.flagged && got != "[]" {
				t.Fatalf("changed shared files = %s, want flagged %v", got, tc.flagged)
			}
			var flagged bool
			for _, e := range h.runEvents(st.ID) {
				if e.Type == event.AgentSharedGitChanged {
					flagged = true
					if fmt.Sprint(e.Data["files"]) != "[config]" {
						t.Errorf("event files = %v, want [config]", e.Data["files"])
					}
				}
			}
			if flagged != tc.flagged {
				t.Errorf("agent:shared-git-changed emitted = %v, want %v", flagged, tc.flagged)
			}

			res := h.RunKlaus("status")
			if res.ExitCode != 0 {
				t.Fatalf("status: %s%s", res.Stdout, res.Stderr)
			}
			var row string
			for _, l := range strings.Split(res.Stdout, "\n") {
				if row == "" && strings.HasPrefix(l, st.ID+" ") {
					row = l
				}
			}
			line := st.ID + " changed its clone's shared git files while it ran: config"
			if tc.flagged != strings.Contains(row, "needs-attention") || tc.flagged != strings.Contains(res.Stdout, line) {
				t.Errorf("status (flagged %v):\n%s", tc.flagged, res.Stdout)
			}
		})
	}
}
