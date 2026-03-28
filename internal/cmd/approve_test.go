package cmd

import (
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestApproveByPRNumbers(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/owner/repo/pull/42"
	prURL99 := "https://github.com/owner/repo/pull/99"

	states := []*run.State{
		{ID: "20260101-0000-aaaa", Prompt: "fix bug", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "20260101-0000-bbbb", Prompt: "add feature", Branch: "b2", PRURL: &prURL99, CreatedAt: "2026-01-01T00:01:00Z"},
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	allStates, _ := store.List()
	err := approveByPRNumbers([]string{"42"}, allStates, store)
	if err != nil {
		t.Fatalf("approveByPRNumbers() error = %v", err)
	}

	// Reload and verify
	s42, err := store.Load("20260101-0000-aaaa")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s42.Approved == nil || !*s42.Approved {
		t.Error("PR #42 should be approved")
	}
	if s42.ApprovedAt == nil {
		t.Error("PR #42 should have ApprovedAt set")
	}

	// PR #99 should not be approved
	s99, err := store.Load("20260101-0000-bbbb")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s99.Approved != nil {
		t.Error("PR #99 should not be approved")
	}
}

func TestApproveAll(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/owner/repo/pull/42"
	prURL99 := "https://github.com/owner/repo/pull/99"
	mergedAt := "2026-01-01T00:00:00Z"

	states := []*run.State{
		{ID: "20260101-0000-aaaa", Prompt: "fix bug", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "20260101-0000-bbbb", Prompt: "add feature", Branch: "b2", PRURL: &prURL99, CreatedAt: "2026-01-01T00:01:00Z", MergedAt: &mergedAt},
		{ID: "20260101-0000-cccc", Prompt: "no pr", Branch: "b3", CreatedAt: "2026-01-01T00:02:00Z"},
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	allStates, _ := store.List()
	err := approveAll(allStates, store)
	if err != nil {
		t.Fatalf("approveAll() error = %v", err)
	}

	// PR #42 should be approved (has PR URL, not merged)
	s42, _ := store.Load("20260101-0000-aaaa")
	if s42.Approved == nil || !*s42.Approved {
		t.Error("PR #42 should be approved")
	}

	// PR #99 should NOT be approved (already merged)
	s99, _ := store.Load("20260101-0000-bbbb")
	if s99.Approved != nil {
		t.Error("already-merged PR #99 should not be approved")
	}

	// No-PR run should not be approved
	scc, _ := store.Load("20260101-0000-cccc")
	if scc.Approved != nil {
		t.Error("run without PR should not be approved")
	}
}

func TestFindRunByPR(t *testing.T) {
	prURL42 := "https://github.com/owner/repo/pull/42"
	states := []*run.State{
		{ID: "20260101-0000-aaaa", Prompt: "fix bug", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"},
	}

	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	store.EnsureDirs()
	store.Save(states[0])

	// Should find by PR number
	found, _, err := findRunByPR("42", states, store)
	if err != nil {
		t.Fatalf("findRunByPR() error = %v", err)
	}
	if found.ID != "20260101-0000-aaaa" {
		t.Errorf("found wrong run: %s", found.ID)
	}

	// Should handle # prefix
	found, _, err = findRunByPR("#42", states, store)
	if err != nil {
		t.Fatalf("findRunByPR() error = %v", err)
	}
	if found.ID != "20260101-0000-aaaa" {
		t.Errorf("found wrong run: %s", found.ID)
	}

	// Should return error for missing PR
	_, _, err = findRunByPR("999", states, store)
	if err == nil {
		t.Error("expected error for missing PR")
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"20260101-0000-aaaa", "aaaa"},
		{"abc", "abc"},
		{"a-b-c-d", "d"},
	}
	for _, tt := range tests {
		got := shortID(tt.id)
		if got != tt.want {
			t.Errorf("shortID(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestMarkApproved(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	store.EnsureDirs()

	s := &run.State{ID: "20260101-0000-aaaa", Prompt: "fix", Branch: "b1", CreatedAt: "2026-01-01T00:00:00Z"}
	store.Save(s)

	err := markApproved(s, store)
	if err != nil {
		t.Fatalf("markApproved() error = %v", err)
	}
	if s.Approved == nil || !*s.Approved {
		t.Error("should be approved")
	}
	if s.ApprovedAt == nil {
		t.Error("should have ApprovedAt")
	}

	// Verify persisted
	reloaded, _ := store.Load("20260101-0000-aaaa")
	if reloaded.Approved == nil || !*reloaded.Approved {
		t.Error("approval should be persisted")
	}
}

func TestMergeSkipsUnapprovedWithYesFlag(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	store.EnsureDirs()

	var buf strings.Builder
	merged := []string{}
	runner := &mergeRunner{
		out:                 &buf,
		in:                  strings.NewReader(""),
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			merged = append(merged, pr)
			return nil
		},
		checkApproval: func(pr string) bool {
			return pr == "1" // Only PR 1 is approved
		},
		yesFlag: true,
	}

	err := runner.run([]string{"1", "2", "3"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// Only PR 1 should be merged; 2 and 3 should be skipped
	if len(merged) != 1 || merged[0] != "1" {
		t.Errorf("merged = %v, want [1]", merged)
	}

	output := buf.String()
	if !strings.Contains(output, "Skipping PR #2: not approved") {
		t.Error("should show skip message for PR #2")
	}
	if !strings.Contains(output, "Skipping PR #3: not approved") {
		t.Error("should show skip message for PR #3")
	}
}

func TestMergePromptsForUnapprovedInteractive(t *testing.T) {
	var buf strings.Builder
	merged := []string{}
	// Simulate user typing "y" then "s"
	input := strings.NewReader("y\ns\n")
	runner := &mergeRunner{
		out:                 &buf,
		in:                  input,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			merged = append(merged, pr)
			return nil
		},
		checkApproval: func(string) bool { return false },
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// PR 1: user said "y" → merged; PR 2: user said "s" → skipped
	if len(merged) != 1 || merged[0] != "1" {
		t.Errorf("merged = %v, want [1]", merged)
	}
}

func TestMergeForceBypassesApproval(t *testing.T) {
	var buf strings.Builder
	merged := []string{}
	runner := &mergeRunner{
		out:                 &buf,
		in:                  strings.NewReader(""),
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			merged = append(merged, pr)
			return nil
		},
		checkApproval: func(string) bool { return false },
		forceApproval: true,
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if len(merged) != 2 {
		t.Errorf("merged = %v, want [1 2]", merged)
	}
}

func TestMergeApprovedPRsPassThrough(t *testing.T) {
	var buf strings.Builder
	merged := []string{}
	runner := &mergeRunner{
		out:                 &buf,
		in:                  strings.NewReader(""),
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			merged = append(merged, pr)
			return nil
		},
		checkApproval: func(string) bool { return true },
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if len(merged) != 2 {
		t.Errorf("merged = %v, want [1 2]", merged)
	}
}
