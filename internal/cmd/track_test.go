package cmd

import (
	"testing"

	"github.com/patflynn/klaus/internal/run"
)

func TestParsePRRef(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		wantRepo string
		wantPR   string
		wantErr  bool
	}{
		{"bare number", "405", "", "405", false},
		{"full URL", "https://github.com/org/repo/pull/405", "org/repo", "405", false},
		{"owner/repo#number", "owner/repo#123", "owner/repo", "123", false},
		{"invalid ref", "not-a-pr", "", "", true},
		{"empty ref", "", "", "", true},
		{"bad URL", "https://github.com/bad", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, pr, err := parsePRRef(tt.ref)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePRRef(%q) error = %v, wantErr %v", tt.ref, err, tt.wantErr)
				return
			}
			if repo != tt.wantRepo {
				t.Errorf("parsePRRef(%q) repo = %q, want %q", tt.ref, repo, tt.wantRepo)
			}
			if pr != tt.wantPR {
				t.Errorf("parsePRRef(%q) pr = %q, want %q", tt.ref, pr, tt.wantPR)
			}
		})
	}
}

func TestTrackCreateState(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	// Simulate what trackPR does (without calling gh CLI)
	prURL := "https://github.com/owner/repo/pull/42"
	title := "Fix the widget"
	repo := "owner/repo"

	id, err := run.GenID()
	if err != nil {
		t.Fatalf("GenID: %v", err)
	}

	st := &run.State{
		ID:         id,
		Prompt:     title,
		Branch:     "fix-widget",
		PRURL:      &prURL,
		Type:       "track",
		TargetRepo: &repo,
		CreatedAt:  "2026-04-01T00:00:00Z",
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify the state was saved correctly
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Type != "track" {
		t.Errorf("Type = %q, want %q", loaded.Type, "track")
	}
	if loaded.PRURL == nil || *loaded.PRURL != prURL {
		t.Errorf("PRURL = %v, want %q", loaded.PRURL, prURL)
	}
	if loaded.Prompt != title {
		t.Errorf("Prompt = %q, want %q", loaded.Prompt, title)
	}
	if loaded.Branch != "fix-widget" {
		t.Errorf("Branch = %q, want %q", loaded.Branch, "fix-widget")
	}
	if loaded.TargetRepo == nil || *loaded.TargetRepo != repo {
		t.Errorf("TargetRepo = %v, want %q", loaded.TargetRepo, repo)
	}
	if loaded.Worktree != "" {
		t.Errorf("Worktree = %q, want empty", loaded.Worktree)
	}
}

func TestTrackDuplicateDetection(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL := "https://github.com/owner/repo/pull/42"
	existing := &run.State{
		ID:        "20260401-0000-aaaa",
		Prompt:    "existing PR",
		Branch:    "fix-it",
		PRURL:     &prURL,
		Type:      "track",
		CreatedAt: "2026-04-01T00:00:00Z",
	}
	if err := store.Save(existing); err != nil {
		t.Fatalf("Save: %v", err)
	}

	states, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Check that the duplicate detection logic works
	isDuplicate := false
	for _, s := range states {
		if s.PRURL != nil && *s.PRURL == prURL {
			isDuplicate = true
			break
		}
	}
	if !isDuplicate {
		t.Error("should detect duplicate PRURL")
	}
}

func TestUntrackOnlyRemovesTrackedType(t *testing.T) {
	tmpDir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(tmpDir)
	if err := store.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	prURL42 := "https://github.com/owner/repo/pull/42"
	prURL99 := "https://github.com/owner/repo/pull/99"

	// Agent-created state (Type is empty, not "track")
	agentState := &run.State{
		ID:        "20260401-0000-aaaa",
		Prompt:    "agent work",
		Branch:    "agent/branch",
		PRURL:     &prURL42,
		Type:      "agent",
		CreatedAt: "2026-04-01T00:00:00Z",
	}
	// Tracked state
	trackedState := &run.State{
		ID:        "20260401-0000-bbbb",
		Prompt:    "tracked PR",
		Branch:    "fix-it",
		PRURL:     &prURL99,
		Type:      "track",
		CreatedAt: "2026-04-01T00:01:00Z",
	}
	for _, s := range []*run.State{agentState, trackedState} {
		if err := store.Save(s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	states, _ := store.List()

	// Try to untrack PR #42 (agent-created) — should not find it
	found := false
	for _, s := range states {
		if s.Type != "track" {
			continue
		}
		if extractPRNumber(s) == "42" {
			found = true
		}
	}
	if found {
		t.Error("should not find agent-created PR #42 as a tracked PR")
	}

	// Untrack PR #99 (tracked) — should find it
	found = false
	for _, s := range states {
		if s.Type != "track" {
			continue
		}
		if extractPRNumber(s) == "99" {
			found = true
			if err := store.Delete(s.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}
	}
	if !found {
		t.Error("should find tracked PR #99")
	}

	// Verify agent state still exists
	remaining, _ := store.List()
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining state, got %d", len(remaining))
	}
	if remaining[0].ID != "20260401-0000-aaaa" {
		t.Errorf("remaining state should be agent state, got %s", remaining[0].ID)
	}
}

func TestDetermineStatusTrack(t *testing.T) {
	tests := []struct {
		name string
		s    *run.State
		want string
	}{
		{
			name: "tracked PR returns tracking",
			s:    &run.State{Type: "track", PRURL: strPtr("https://github.com/owner/repo/pull/1")},
			want: "tracking",
		},
		{
			name: "tracked PR with merged returns merged",
			s: &run.State{
				Type:     "track",
				PRURL:    strPtr("https://github.com/owner/repo/pull/1"),
				MergedAt: strPtr("2026-04-01T00:00:00Z"),
			},
			want: "merged",
		},
		{
			name: "tracked PR no PRURL returns tracking",
			s:    &run.State{Type: "track"},
			want: "tracking",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineStatus(tt.s)
			if got != tt.want {
				t.Errorf("determineStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
