package prose

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSettle: an edit is applied when the worktree, or the head commit's
// message, has its new text and not its old; skipped edits get no outcome.
func TestSettle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("Install it first.\nRun it from main only.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "Say where it runs"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	r := &Result{Edits: []Edit{
		{File: "README.md", Old: "In order to install it, do so first.", New: "Install it first."},
		{File: "README.md", Old: "Run it from main.", New: "Run it anywhere."},
		{File: CommitFile, Old: "Say where the tool runs", New: "Say where it runs"},
		{File: "README.md", Old: "x", New: "Run it from main only.", Skipped: "no change"},
		{File: "README.md", Old: "Install it first.", New: "Run it from main only."},
	}}
	Settle(dir, r)
	for i, want := range []string{"applied", "rejected", "applied", "", "rejected"} {
		if got := r.Edits[i].Outcome; got != want {
			t.Errorf("edit %d outcome = %q, want %q", i+1, got, want)
		}
	}
}

// TestBuildSplicesFromTheOriginal: each edit replaces its own text in the
// original, not text an earlier edit wrote, and an overlapping edit is left
// out.
func TestBuildSplicesFromTheOriginal(t *testing.T) {
	in := &input{message: "one two three four"}
	r := &Result{Edits: []Edit{
		{File: CommitFile, Old: "one", New: "four"},
		{File: CommitFile, Old: "four", New: "five"},
		{File: CommitFile, Old: "two three", New: "2 3"},
		{File: CommitFile, Old: "three", New: "3"},
	}}
	if _, msg := build("", in, r); msg != "four 2 3 five\n" {
		t.Errorf("message = %q, want %q", msg, "four 2 3 five\n")
	}
	if r.Edits[3].Skipped == "" {
		t.Error("the overlapping edit wasn't skipped")
	}
}
