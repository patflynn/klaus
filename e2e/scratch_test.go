//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// TestWorkerScratchDir: a launched worker gets KLAUS_SCRATCH, an existing
// directory outside its worktree, and builds its binary there as its standing
// rules say. With nothing left in the worktree there is nothing to salvage,
// and finalize removes the scratch directory with the worktree.
func TestWorkerScratchDir(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.01,"duration_ms":10,"session_id":"11111111-2222-4333-8444-555555555555"}` + "\n")
	h.WriteStub("claude", strings.Replace(h.claudeStubScript(), `cat "$dir/claude.output.jsonl"`,
		`printf '%s\n' "${KLAUS_SCRATCH-unset}" > "$dir/worker.scratch"
if [ -d "${KLAUS_SCRATCH-}" ]; then echo yes > "$dir/worker.scratch-exists"; fi
head -c 2097152 /dev/zero > "$KLAUS_SCRATCH/restack"
cat "$dir/claude.output.jsonl"`, 1))

	st := launchOne(t, h, "fix the restack command")
	h.WaitForClaudeStart(30 * time.Second)
	releaseBackendWorkers(t, h)
	final, err := h.ReadState(st.ID)
	if err != nil {
		t.Fatal(err)
	}

	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(h.E2EDir, name))
		return strings.TrimSpace(string(b))
	}
	scratch := filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "scratch", st.ID)
	if got := read("worker.scratch"); got != scratch {
		t.Fatalf("worker's KLAUS_SCRATCH = %q, want %q", got, scratch)
	}
	if rel, err := filepath.Rel(st.Worktree, scratch); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("scratch directory %s is inside the worktree %s", scratch, st.Worktree)
	}
	if read("worker.scratch-exists") != "yes" {
		t.Errorf("scratch directory %s did not exist while the worker ran", scratch)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("finalize left the scratch directory %s: stat err = %v", scratch, err)
	}

	if final.NeedsAttention != nil {
		t.Errorf("a binary built in scratch made the run need attention: %q", *final.NeedsAttention)
	}
	for _, e := range h.runEvents(st.ID) {
		if e.Type == event.AgentNeedsAttention {
			t.Errorf("unexpected agent:needs-attention: %v", e.Data)
		}
	}
	if refs := h.GitOutput(h.OriginDir, "for-each-ref", "refs/heads/"+st.Branch); refs != "" {
		t.Errorf("nothing to salvage, but %s was pushed: %s", st.Branch, refs)
	}
}
