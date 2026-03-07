package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWatchPaneCommand(t *testing.T) {
	cmd := buildWatchPaneCommand(
		"", "/tmp/wt", "claude -p 'test'", "/tmp/log.jsonl",
		"klaus", "abc123", "42", "/tmp/baseline.txt", 120,
	)

	// The command should contain the review polling loop structure
	checks := []struct {
		name    string
		substr  string
	}{
		{"initial baseline save", "_save-review-baseline"},
		{"while loop", "while true; do"},
		{"agent command", "claude -p 'test'"},
		{"tee to log", "tee"},
		{"format stream", "_format-stream"},
		{"poll reviews", "_poll-reviews"},
		{"wait flag", "--wait 120"},
		{"break on no comments", "|| break"},
		{"finalize", "_finalize"},
		{"exit message", "Press Enter to close"},
		{"pr number in poll", "'42'"},
		{"baseline file in poll", "baseline.txt"},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(cmd, c.substr) {
				t.Errorf("pane command missing %q\ngot: %s", c.substr, cmd)
			}
		})
	}
}

func TestBuildWatchPaneCommandZeroWait(t *testing.T) {
	cmd := buildWatchPaneCommand(
		"", "/tmp/wt", "claude -p 'test'", "/tmp/log.jsonl",
		"klaus", "abc123", "42", "/tmp/baseline.txt", 0,
	)

	// Even with 0 wait, the structure should still be valid
	if !strings.Contains(cmd, "--wait 0") {
		t.Errorf("expected --wait 0 in command, got: %s", cmd)
	}
}

func TestBuildWatchPaneCommandWithEnvPrefix(t *testing.T) {
	cmd := buildWatchPaneCommand(
		"export KLAUS_SESSION_ID='sess1'; ", "/tmp/wt", "claude -p 'test'", "/tmp/log.jsonl",
		"klaus", "abc123", "42", "/tmp/baseline.txt", 60,
	)

	if !strings.HasPrefix(cmd, "export KLAUS_SESSION_ID='sess1'; ") {
		t.Errorf("expected env prefix at start of command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "--wait 60") {
		t.Errorf("expected --wait 60, got: %s", cmd)
	}
}

func TestWriteAndReadIDsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ids.txt")

	// Write IDs
	ids := []int64{100, 200, 300}
	if err := writeIDsToFile(path, ids); err != nil {
		t.Fatalf("writeIDsToFile: %v", err)
	}

	// Read back
	got, err := readIDsFromFile(path)
	if err != nil {
		t.Fatalf("readIDsFromFile: %v", err)
	}

	for _, id := range ids {
		if !got[id] {
			t.Errorf("expected ID %d in result", id)
		}
	}
	if len(got) != len(ids) {
		t.Errorf("got %d IDs, want %d", len(got), len(ids))
	}
}

func TestReadIDsFromFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readIDsFromFile(path)
	if err != nil {
		t.Fatalf("readIDsFromFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestReadIDsFromFileMissing(t *testing.T) {
	_, err := readIDsFromFile("/nonexistent/file.txt")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadIDsFromFileWithJunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "junk.txt")
	content := "100\nnot-a-number\n200\n\n300\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readIDsFromFile(path)
	if err != nil {
		t.Fatalf("readIDsFromFile: %v", err)
	}

	// Should have parsed 100, 200, 300 and skipped the junk
	if len(got) != 3 {
		t.Errorf("expected 3 IDs, got %d", len(got))
	}
	for _, id := range []int64{100, 200, 300} {
		if !got[id] {
			t.Errorf("expected ID %d", id)
		}
	}
}

func TestPollForNewCommentsZeroWait(t *testing.T) {
	// With zero wait, should return immediately with no results
	baseline := map[int64]bool{1: true}
	result := pollForNewComments("999", baseline, 0)
	if len(result) != 0 {
		t.Errorf("expected no results with 0 wait, got %d", len(result))
	}
}
