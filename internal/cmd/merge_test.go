package cmd

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/run"
)

// testMergeRunner creates a mergeRunner with sensible defaults for tests.
// All functions ignore the repo parameter since tests don't exercise real gh.
func testMergeRunner(out *bytes.Buffer) *mergeRunner {
	return &mergeRunner{
		out:                 out,
		in:                  strings.NewReader(""),
		getPRTitle:          func(string, string) string { return "Test PR" },
		getPRCI:             func(string, string) string { return "passing" },
		getPRConflicts:      func(string, string) string { return "none" },
		getPRReviewDecision: func(string, string) string { return "APPROVED" },
		rebaseAndPush:       func(string, string) error { return nil },
		pollCI:              func(string, string) error { return nil },
		mergePR:             func(string, string, bool, string) error { return nil },
		resolveRepo:         func(string) string { return "" },
		forceApproval:       true, // default to bypassing approval in tests
	}
}

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
		repo         string
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
		{
			name:         "with repo flag",
			prNumber:     "42",
			mergeMethod:  "squash",
			deleteBranch: true,
			repo:         "owner/repo",
			want:         []string{"pr", "merge", "--squash", "--delete-branch", "--repo", "owner/repo", "--", "42"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gh.MergeArgs(tt.prNumber, tt.mergeMethod, tt.deleteBranch, tt.repo)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeArgs() = %v, want %v", got, tt.want)
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
	client := gh.NewPRClient("")
	got := client.ViewTitleArgs("99")
	want := []string{"pr", "view", "--json", "title", "-q", ".title", "--", "99"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ViewTitleArgs(99) = %v, want %v", got, want)
	}

	// With repo
	clientWithRepo := gh.NewPRClient("owner/repo")
	got = clientWithRepo.ViewTitleArgs("99")
	want = []string{"pr", "view", "--json", "title", "-q", ".title", "--repo", "owner/repo", "--", "99"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ViewTitleArgs(99) with repo = %v, want %v", got, want)
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
	runner := testMergeRunner(&buf)
	runner.getPRTitle = func(pr, repo string) string {
		titles := map[string]string{"1": "Fix bug", "2": "Add feature"}
		return titles[pr]
	}

	err := runner.dryRun([]string{"1", "2"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Merge plan (dry run)") {
		t.Error("missing header")
	}
	if !strings.Contains(output, "PR #1") {
		t.Error("missing PR #1")
	}
	if !strings.Contains(output, "Fix bug") {
		t.Error("missing PR #1 title")
	}
	if !strings.Contains(output, "PR #2") {
		t.Error("missing PR #2")
	}
	if !strings.Contains(output, "Merge: ready") {
		t.Error("missing merge status 'ready'")
	}
}

func TestDryRunShowsBlockedStatus(t *testing.T) {
	var buf bytes.Buffer
	runner := testMergeRunner(&buf)
	runner.getPRTitle = func(string, string) string { return "Blocked PR" }
	runner.getPRCI = func(string, string) string { return "failing" }
	runner.getPRConflicts = func(string, string) string { return "yes" }
	runner.getPRReviewDecision = func(string, string) string { return "CHANGES_REQUESTED" }

	err := runner.dryRun([]string{"5"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Merge: blocked") {
		t.Error("should show blocked status")
	}
}

func TestDryRunShowsRepo(t *testing.T) {
	var buf bytes.Buffer
	runner := testMergeRunner(&buf)
	runner.resolveRepo = func(pr string) string {
		if pr == "1" {
			return "owner/repo-a"
		}
		return "owner/repo-b"
	}

	err := runner.dryRun([]string{"1", "2"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "[owner/repo-a]") {
		t.Errorf("should show repo-a, got: %s", output)
	}
	if !strings.Contains(output, "[owner/repo-b]") {
		t.Errorf("should show repo-b, got: %s", output)
	}
}

func TestDryRunShowsLocalWhenNoRepo(t *testing.T) {
	var buf bytes.Buffer
	runner := testMergeRunner(&buf)
	runner.resolveRepo = func(string) string { return "" }

	err := runner.dryRun([]string{"1"})
	if err != nil {
		t.Fatalf("dryRun() error = %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "(local)") {
		t.Errorf("should show (local) when no repo, got: %s", output)
	}
}

func TestRunAllSuccess(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
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
	runner := testMergeRunner(&bytes.Buffer{})
	runner.getPRReviewDecision = func(string, string) string { return "" }
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		gotMethod = method
		gotDelete = del
		return nil
	}

	runner.run([]string{"1"}, "rebase", false)

	if gotMethod != "rebase" {
		t.Errorf("merge method = %q, want %q", gotMethod, "rebase")
	}
	if gotDelete != false {
		t.Error("deleteBranch should be false")
	}
}

func TestRunPassesRepoToMergePR(t *testing.T) {
	var gotRepos []string
	runner := testMergeRunner(&bytes.Buffer{})
	runner.resolveRepo = func(pr string) string {
		return "owner/repo-" + pr
	}
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		gotRepos = append(gotRepos, repo)
		return nil
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	want := []string{"owner/repo-1", "owner/repo-2"}
	if !reflect.DeepEqual(gotRepos, want) {
		t.Errorf("repos passed to mergePR = %v, want %v", gotRepos, want)
	}
}

func TestRunStopsOnRebaseFailure(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.getPRConflicts = func(pr, repo string) string {
		if pr == "2" {
			return "yes"
		}
		return "none"
	}
	runner.rebaseAndPush = func(string, string) error {
		return fmt.Errorf("conflict in main.go")
	}
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
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
	runner := testMergeRunner(&buf)
	runner.getPRCI = func(string, string) string { return "pending" }
	runner.pollCI = func(string, string) error {
		return fmt.Errorf("CI timed out after 10m0s")
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
	runner := testMergeRunner(&buf)
	runner.getPRConflicts = func(string, string) string { return "yes" }
	runner.rebaseAndPush = func(string, string) error {
		rebaseCalled = true
		return nil
	}
	runner.pollCI = func(string, string) error {
		pollCICalled = true
		return nil
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
	runner := testMergeRunner(&buf)
	runner.getPRReviewDecision = func(string, string) string { return "CHANGES_REQUESTED" }

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
	runner := testMergeRunner(&buf)
	runner.getPRCI = func(string, string) string { return "failing" }

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
	runner := testMergeRunner(&buf)
	runner.getPRConflicts = func(string, string) string { return "yes" }
	runner.pollCI = func(string, string) error {
		return fmt.Errorf("CI checks failed")
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
	runner := testMergeRunner(&buf)
	runner.mergePR = func(string, string, bool, string) error {
		return fmt.Errorf("gh pr merge: exit status 1")
	}

	err := runner.run([]string{"1"}, "squash", true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "merge failed") {
		t.Errorf("error should mention merge failed: %v", err)
	}
}

func TestRunUpdatesStateAfterMerge(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/owner/repo/pull/42"
	prURL99 := "https://github.com/owner/repo/pull/99"
	prURL7 := "https://github.com/owner/repo/pull/7"

	states := []*run.State{
		{ID: "20260101-0000-aaaa", Prompt: "fix bug", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "20260101-0000-bbbb", Prompt: "add feature", Branch: "b2", PRURL: &prURL99, CreatedAt: "2026-01-01T00:01:00Z"},
		{ID: "20260101-0000-cccc", Prompt: "other work", Branch: "b3", PRURL: &prURL7, CreatedAt: "2026-01-01T00:02:00Z"},
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	var buf bytes.Buffer
	runner := testMergeRunner(&buf)
	runner.markMerged = markRunsMerged(store)

	err := runner.run([]string{"42", "99"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	s42, err := store.Load("20260101-0000-aaaa")
	if err != nil {
		t.Fatalf("Load aaaa: %v", err)
	}
	if s42.MergedAt == nil {
		t.Error("PR #42 state should have MergedAt set after merge")
	}

	s99, err := store.Load("20260101-0000-bbbb")
	if err != nil {
		t.Fatalf("Load bbbb: %v", err)
	}
	if s99.MergedAt == nil {
		t.Error("PR #99 state should have MergedAt set after merge")
	}

	s7, err := store.Load("20260101-0000-cccc")
	if err != nil {
		t.Fatalf("Load cccc: %v", err)
	}
	if s7.MergedAt != nil {
		t.Errorf("PR #7 state should NOT have MergedAt set, got %q", *s7.MergedAt)
	}
}

func TestRunDoesNotUpdateStateOnMergeFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/owner/repo/pull/42"
	s := &run.State{ID: "20260101-0000-dddd", Prompt: "fix", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var buf bytes.Buffer
	runner := testMergeRunner(&buf)
	runner.mergePR = func(string, string, bool, string) error { return fmt.Errorf("merge failed") }
	runner.markMerged = markRunsMerged(store)

	_ = runner.run([]string{"42"}, "squash", true)

	reloaded, err := store.Load("20260101-0000-dddd")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.MergedAt != nil {
		t.Error("MergedAt should not be set when merge fails")
	}
}

func TestMarkRunsMergedNilStore(t *testing.T) {
	fn := markRunsMerged(nil)
	fn("42") // Should be a no-op.
}

func TestBuildRepoResolverFromRunState(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/acme/widgets/pull/42"
	prURL99 := "https://github.com/acme/gadgets/pull/99"
	states := []*run.State{
		{ID: "20260101-0000-aaaa", Prompt: "fix", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "20260101-0000-bbbb", Prompt: "add", Branch: "b2", PRURL: &prURL99, CreatedAt: "2026-01-01T00:01:00Z"},
	}
	for _, s := range states {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	resolver := buildRepoResolver(store, "")

	if got := resolver("42"); got != "acme/widgets" {
		t.Errorf("resolver(42) = %q, want %q", got, "acme/widgets")
	}
	if got := resolver("99"); got != "acme/gadgets" {
		t.Errorf("resolver(99) = %q, want %q", got, "acme/gadgets")
	}
	if got := resolver("7"); got != "" {
		t.Errorf("resolver(7) = %q, want empty", got)
	}
}

func TestBuildRepoResolverFallsBackToFlag(t *testing.T) {
	// Use an empty store rather than nil to avoid loading state from the
	// environment, which makes this test non-deterministic.
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	resolver := buildRepoResolver(store, "flag/repo")

	if got := resolver("42"); got != "flag/repo" {
		t.Errorf("resolver(42) = %q, want %q", got, "flag/repo")
	}
}

func TestBuildRepoResolverRunStateTakesPriority(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/state/repo/pull/42"
	s := &run.State{ID: "20260101-0000-aaaa", Prompt: "fix", Branch: "b1", PRURL: &prURL42, CreatedAt: "2026-01-01T00:00:00Z"}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	resolver := buildRepoResolver(store, "flag/repo")

	if got := resolver("42"); got != "state/repo" {
		t.Errorf("resolver(42) = %q, want %q", got, "state/repo")
	}
	if got := resolver("99"); got != "flag/repo" {
		t.Errorf("resolver(99) = %q, want %q", got, "flag/repo")
	}
}

func TestRepoFromPRURLMerge(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo/pull/42", "owner/repo"},
		{"https://github.com/acme/widgets/pull/123", "acme/widgets"},
		{"http://github.com/acme/widgets/pull/1", "acme/widgets"},
		{"not a url", "(unknown)"},
		{"https://github.com/incomplete", "(unknown)"},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := repoFromPRURL(tt.url)
			if got != tt.want {
				t.Errorf("repoFromPRURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestMergeSkipsUnapprovedWithYesFlag(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.forceApproval = false
	runner.yesFlag = true
	runner.checkApproval = func(pr string) bool {
		return pr == "1" // Only PR 1 is approved
	}
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
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
	var buf bytes.Buffer
	merged := []string{}
	// Simulate user typing "y" then "s"
	runner := testMergeRunner(&buf)
	runner.in = strings.NewReader("y\ns\n")
	runner.forceApproval = false
	runner.checkApproval = func(string) bool { return false }
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// PR 1: user said "y" -> merged; PR 2: user said "s" -> skipped
	if len(merged) != 1 || merged[0] != "1" {
		t.Errorf("merged = %v, want [1]", merged)
	}
}

func TestMergeForceBypassesApproval(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.forceApproval = true
	runner.checkApproval = func(string) bool { return false }
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
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
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.forceApproval = false
	runner.checkApproval = func(string) bool { return true }
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if len(merged) != 2 {
		t.Errorf("merged = %v, want [1 2]", merged)
	}
}

func TestMergePassesWithGitHubApproval(t *testing.T) {
	var buf bytes.Buffer
	merged := []string{}
	runner := testMergeRunner(&buf)
	runner.forceApproval = false
	// Simulate GitHub APPROVED via getPRReviewDecision
	runner.getPRReviewDecision = func(pr, repo string) string { return "APPROVED" }
	// checkApproval returns false (no internal approval), but the runner also
	// checks GitHub review via getPRReviewDecision. To test the full flow
	// we wire checkApproval to also consult the review decision.
	runner.checkApproval = func(pr string) bool {
		decision := runner.getPRReviewDecision(pr, "")
		return strings.EqualFold(decision, "APPROVED")
	}
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		merged = append(merged, pr)
		return nil
	}

	err := runner.run([]string{"1"}, "squash", true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if len(merged) != 1 || merged[0] != "1" {
		t.Errorf("merged = %v, want [1]", merged)
	}
}

func TestMergeEOFStopsQueue(t *testing.T) {
	var buf bytes.Buffer
	// Empty reader simulates EOF/Ctrl+D
	runner := testMergeRunner(&buf)
	runner.in = strings.NewReader("")
	runner.forceApproval = false
	runner.checkApproval = func(string) bool { return false }
	runner.mergePR = func(pr, method string, del bool, repo string) error {
		t.Fatal("merge should not be called on EOF")
		return nil
	}

	err := runner.run([]string{"1", "2"}, "squash", true)
	if err == nil {
		t.Fatal("expected error on EOF")
	}
	if !strings.Contains(err.Error(), "merge not confirmed") {
		t.Errorf("error should mention 'merge not confirmed': %v", err)
	}
}
