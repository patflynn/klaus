package cmd

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidateMergeMethod(t *testing.T) {
	tests := []struct {
		method  string
		wantErr bool
	}{
		{"squash", false},
		{"merge", false},
		{"rebase", false},
		{"invalid", true},
		{"", true},
	}

	for _, tt := range tests {
		name := tt.method
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			err := validateMergeMethod(tt.method)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMergeMethod(%q) error = %v, wantErr %v", tt.method, err, tt.wantErr)
			}
		})
	}
}

func TestGHPRMergeArgs(t *testing.T) {
	tests := []struct {
		name         string
		prNumber     string
		mergeMethod  string
		deleteBranch bool
		want         []string
	}{
		{
			name:         "squash with delete",
			prNumber:     "42",
			mergeMethod:  "squash",
			deleteBranch: true,
			want:         []string{"pr", "merge", "--squash", "--delete-branch", "--", "42"},
		},
		{
			name:         "merge without delete",
			prNumber:     "10",
			mergeMethod:  "merge",
			deleteBranch: false,
			want:         []string{"pr", "merge", "--merge", "--", "10"},
		},
		{
			name:         "rebase with delete",
			prNumber:     "7",
			mergeMethod:  "rebase",
			deleteBranch: true,
			want:         []string{"pr", "merge", "--rebase", "--delete-branch", "--", "7"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ghPRMergeArgs(tt.prNumber, tt.mergeMethod, tt.deleteBranch)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ghPRMergeArgs() = %v, want %v", got, tt.want)
			}

			// Verify flags before "--" separator
			separatorIdx := -1
			for i, arg := range got {
				if arg == "--" {
					separatorIdx = i
					break
				}
			}
			if separatorIdx == -1 {
				t.Fatal("missing '--' separator")
			}
			for i := separatorIdx + 1; i < len(got); i++ {
				if strings.HasPrefix(got[i], "-") {
					t.Errorf("flag %q found after '--' separator at index %d", got[i], i)
				}
			}
		})
	}
}

func TestGHPRTitleArgs(t *testing.T) {
	got := ghPRTitleArgs("99")
	want := []string{"pr", "view", "--json", "title", "-q", ".title", "--", "99"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ghPRTitleArgs(99) = %v, want %v", got, want)
	}

	// Verify flags before separator
	separatorIdx := -1
	for i, arg := range got {
		if arg == "--" {
			separatorIdx = i
			break
		}
	}
	if separatorIdx == -1 {
		t.Fatal("missing '--' separator")
	}
	for i := separatorIdx + 1; i < len(got); i++ {
		if strings.HasPrefix(got[i], "-") {
			t.Errorf("flag %q found after '--' separator at index %d", got[i], i)
		}
	}
}

func TestFormatPRList(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"single", []string{"1"}, "#1"},
		{"multiple", []string{"1", "2", "3"}, "#1, #2, #3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPRList(tt.input)
			if got != tt.want {
				t.Errorf("formatPRList(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out: &buf,
		getPRTitle: func(pr string) string {
			titles := map[string]string{"1": "Fix bug", "2": "Add feature"}
			return titles[pr]
		},
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
	}

	err := runner.dryRun([]string{"1", "2"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Merge plan (dry run)") {
		t.Error("missing header")
	}
	if !strings.Contains(output, "PR #1: Fix bug") {
		t.Error("missing PR #1")
	}
	if !strings.Contains(output, "PR #2: Add feature") {
		t.Error("missing PR #2")
	}
	if !strings.Contains(output, "Merge: ready") {
		t.Error("missing merge status 'ready'")
	}
}

func TestDryRunShowsBlockedStatus(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Blocked PR" },
		getPRCI:             func(string) string { return "failing" },
		getPRConflicts:      func(string) string { return "yes" },
		getPRReviewDecision: func(string) string { return "CHANGES_REQUESTED" },
	}

	err := runner.dryRun([]string{"5"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Merge: blocked") {
		t.Error("should show blocked status")
	}
}

func TestRunAllSuccess(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := &mergeRunner{
		out:                 &buf,
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
	}

	err := runner.run([]string{"1", "2", "3"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if !reflect.DeepEqual(merged, []string{"1", "2", "3"}) {
		t.Errorf("merged = %v, want [1 2 3]", merged)
	}
	if !strings.Contains(buf.String(), "All 3 PRs merged successfully") {
		t.Error("missing success message")
	}
}

func TestRunPassesMergeMethodAndDeleteBranch(t *testing.T) {
	var gotMethod string
	var gotDelete bool
	runner := &mergeRunner{
		out:                 &bytes.Buffer{},
		getPRTitle:          func(string) string { return "Test" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			gotMethod = method
			gotDelete = del
			return nil
		},
	}

	runner.run([]string{"1"}, "rebase", false)

	if gotMethod != "rebase" {
		t.Errorf("merge method = %q, want %q", gotMethod, "rebase")
	}
	if gotDelete != false {
		t.Error("deleteBranch should be false")
	}
}

func TestRunStopsOnRebaseFailure(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := &mergeRunner{
		out:        &buf,
		getPRTitle: func(string) string { return "Test PR" },
		getPRCI:    func(string) string { return "passing" },
		getPRConflicts: func(pr string) string {
			if pr == "2" {
				return "yes"
			}
			return "none"
		},
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush: func(string) error {
			return fmt.Errorf("conflict in main.go")
		},
		pollCI: func(string) error { return nil },
		mergePR: func(pr, method string, del bool) error {
			merged = append(merged, pr)
			return nil
		},
	}

	err := runner.run([]string{"1", "2", "3"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "PR #2") {
		t.Errorf("error should mention PR #2: %v", err)
	}
	if !strings.Contains(err.Error(), "rebase failed") {
		t.Errorf("error should mention rebase: %v", err)
	}

	// PR 1 should have been merged, but not 2 or 3
	if !reflect.DeepEqual(merged, []string{"1"}) {
		t.Errorf("merged = %v, want [1]", merged)
	}

	output := buf.String()
	if !strings.Contains(output, "Remaining PRs: #3") {
		t.Errorf("should list remaining PRs, got: %s", output)
	}
}

func TestRunStopsOnCITimeout(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "pending" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI: func(string) error {
			return fmt.Errorf("CI timed out after 10m0s")
		},
		mergePR: func(string, string, bool) error { return nil },
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "PR #1") {
		t.Errorf("error should mention PR #1: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout: %v", err)
	}
	if !strings.Contains(buf.String(), "Remaining PRs: #2") {
		t.Error("should list remaining PRs")
	}
}

func TestRunHandlesConflictsWithRebase(t *testing.T) {
	var buf bytes.Buffer
	rebaseCalled := false
	pollCICalled := false
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "yes" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush: func(string) error {
			rebaseCalled = true
			return nil
		},
		pollCI: func(string) error {
			pollCICalled = true
			return nil
		},
		mergePR: func(string, string, bool) error { return nil },
	}

	err := runner.run([]string{"1"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !rebaseCalled {
		t.Error("rebaseAndPush should have been called")
	}
	if !pollCICalled {
		t.Error("pollCI should have been called after rebase")
	}
	if !strings.Contains(buf.String(), "Rebasing onto main") {
		t.Error("should show rebase message")
	}
}

func TestRunStopsOnChangesRequested(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "CHANGES_REQUESTED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR:             func(string, string, bool) error { return nil },
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "changes requested") {
		t.Errorf("error should mention changes requested: %v", err)
	}
	if !strings.Contains(buf.String(), "Remaining PRs: #2") {
		t.Error("should list remaining PRs")
	}
}

func TestRunStopsOnCIFailing(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "failing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR:             func(string, string, bool) error { return nil },
	}

	err := runner.run([]string{"1"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "CI is failing") {
		t.Errorf("error should mention CI failing: %v", err)
	}
}

func TestRunCIFailsAfterRebase(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "yes" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI: func(string) error {
			return fmt.Errorf("CI checks failed")
		},
		mergePR: func(string, string, bool) error { return nil },
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "CI after rebase") {
		t.Errorf("error should mention CI after rebase: %v", err)
	}
	if !strings.Contains(buf.String(), "Remaining PRs: #2") {
		t.Error("should list remaining PRs")
	}
}

func TestRunMergeFailure(t *testing.T) {
	var buf bytes.Buffer
	runner := &mergeRunner{
		out:                 &buf,
		getPRTitle:          func(string) string { return "Test PR" },
		getPRCI:             func(string) string { return "passing" },
		getPRConflicts:      func(string) string { return "none" },
		getPRReviewDecision: func(string) string { return "APPROVED" },
		rebaseAndPush:       func(string) error { return nil },
		pollCI:              func(string) error { return nil },
		mergePR: func(string, string, bool) error {
			return fmt.Errorf("gh pr merge: exit status 1")
		},
	}

	err := runner.run([]string{"1"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "merge failed") {
		t.Errorf("error should mention merge failed: %v", err)
	}
}
