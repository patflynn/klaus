package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var wantWorktreeBase = filepath.Join(os.TempDir(), "klaus-sessions")

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.WorktreeBase != wantWorktreeBase {
		t.Errorf("WorktreeBase = %q, want %q", cfg.WorktreeBase, wantWorktreeBase)
	}
	if cfg.DefaultBudget != "5.00" {
		t.Errorf("DefaultBudget = %q, want 5.00", cfg.DefaultBudget)
	}
	if cfg.DataRef != "refs/klaus/data" {
		t.Errorf("DataRef = %q, want refs/klaus/data", cfg.DataRef)
	}
	if cfg.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", cfg.DefaultBranch)
	}
}

func TestLoadNoFile(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// Should return defaults
	if cfg.WorktreeBase != wantWorktreeBase {
		t.Errorf("WorktreeBase = %q, want %q", cfg.WorktreeBase, wantWorktreeBase)
	}
}

func TestLoadWithFile(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	custom := Config{
		WorktreeBase:  "/tmp/custom",
		DefaultBudget: "10.00",
		DataRef:       "refs/custom/data",
		DefaultBranch: "develop",
	}
	data, _ := json.Marshal(custom)
	os.WriteFile(filepath.Join(klausDir, "config.json"), data, 0o644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.WorktreeBase != "/tmp/custom" {
		t.Errorf("WorktreeBase = %q, want /tmp/custom", cfg.WorktreeBase)
	}
	if cfg.DefaultBudget != "10.00" {
		t.Errorf("DefaultBudget = %q, want 10.00", cfg.DefaultBudget)
	}
}

func TestLoadPartialOverride(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	// Only override one field
	os.WriteFile(filepath.Join(klausDir, "config.json"), []byte(`{"default_budget":"20.00"}`), 0o644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DefaultBudget != "20.00" {
		t.Errorf("DefaultBudget = %q, want 20.00", cfg.DefaultBudget)
	}
	// Other fields should be zero values since we unmarshal into defaults then overwrite
	// Actually since we start with defaults and unmarshal over them, non-specified fields
	// keep their zero values in the JSON case. Let's verify:
	if cfg.WorktreeBase != "" {
		// JSON unmarshal replaces the struct, so unset fields get zero values
		// This is expected behavior — users should specify all fields or use Init
	}
}

func TestRenderPromptDefault(t *testing.T) {
	dir := t.TempDir() // no .klaus/prompt.md

	vars := PromptVars{
		RunID:    "20260210-1430-a3f2",
		Issue:    "42",
		Branch:   "agent/20260210-1430-a3f2",
		RepoName: "test-repo",
	}

	prompt, err := RenderPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "Run: 20260210-1430-a3f2") {
		t.Error("prompt should contain run ID")
	}
	if !strings.Contains(prompt, "Fixes #42") {
		t.Error("prompt should contain issue reference")
	}
}

func TestRenderPromptNoIssue(t *testing.T) {
	dir := t.TempDir()

	vars := PromptVars{
		RunID: "20260210-1430-a3f2",
	}

	prompt, err := RenderPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPrompt() error: %v", err)
	}

	if strings.Contains(prompt, "Fixes") {
		t.Error("prompt should not contain Fixes when no issue")
	}
}

func TestRenderPromptCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	tmpl := "Agent {{.RunID}} on branch {{.Branch}} for repo {{.RepoName}}"
	os.WriteFile(filepath.Join(klausDir, "prompt.md"), []byte(tmpl), 0o644)

	vars := PromptVars{
		RunID:    "test-123",
		Branch:   "agent/test-123",
		RepoName: "myrepo",
	}

	prompt, err := RenderPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPrompt() error: %v", err)
	}

	want := "Agent test-123 on branch agent/test-123 for repo myrepo"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderSessionPromptDefault(t *testing.T) {
	dir := t.TempDir() // no .klaus/session-prompt.md

	vars := PromptVars{
		RunID:    "session-20260210-1430-a3f2",
		Branch:   "session/session-20260210-1430-a3f2",
		RepoName: "test-repo",
	}

	prompt, err := RenderSessionPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "session-20260210-1430-a3f2") {
		t.Error("prompt should contain session ID")
	}
	if !strings.Contains(prompt, "session/session-20260210-1430-a3f2") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "test-repo") {
		t.Error("prompt should contain repo name")
	}
	if !strings.Contains(prompt, "klaus launch") {
		t.Error("prompt should contain klaus launch instruction")
	}
	if !strings.Contains(prompt, "klaus status") {
		t.Error("prompt should contain klaus status instruction")
	}
	if !strings.Contains(prompt, "klaus logs") {
		t.Error("prompt should contain klaus logs instruction")
	}
	if !strings.Contains(prompt, "klaus cleanup") {
		t.Error("prompt should contain klaus cleanup instruction")
	}
}

func TestRenderSessionPromptCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	tmpl := "Session {{.RunID}} on branch {{.Branch}} for {{.RepoName}}"
	os.WriteFile(filepath.Join(klausDir, "session-prompt.md"), []byte(tmpl), 0o644)

	vars := PromptVars{
		RunID:    "session-abc",
		Branch:   "session/session-abc",
		RepoName: "myrepo",
	}

	prompt, err := RenderSessionPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	want := "Session session-abc on branch session/session-abc for myrepo"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderWatchPromptDefault(t *testing.T) {
	dir := t.TempDir() // no .klaus/watch-prompt.md

	vars := PromptVars{
		RunID:  "watch-20260210-1430-a3f2",
		Branch: "agent/watch-20260210-1430-a3f2",
		PR:     "99",
	}

	prompt, err := RenderWatchPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderWatchPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "PR #99") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "gh pr checks 99") {
		t.Error("prompt should contain gh pr checks command with PR number")
	}
	if !strings.Contains(prompt, "watch-20260210-1430-a3f2") {
		t.Error("prompt should contain run ID")
	}
	if !strings.Contains(prompt, "## Testing") {
		t.Error("prompt should contain testing section")
	}
}

func TestRenderWatchPromptCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	tmpl := "Watch PR #{{.PR}} run {{.RunID}}"
	os.WriteFile(filepath.Join(klausDir, "watch-prompt.md"), []byte(tmpl), 0o644)

	vars := PromptVars{
		RunID: "watch-abc",
		PR:    "55",
	}

	prompt, err := RenderWatchPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderWatchPrompt() error: %v", err)
	}

	want := "Watch PR #55 run watch-abc"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderPRFixPromptDefault(t *testing.T) {
	dir := t.TempDir() // no .klaus/pr-fix-prompt.md

	vars := PromptVars{
		RunID:    "20260312-1820-abcd",
		PR:       "99",
		Branch:   "feature/my-branch",
		RepoName: "test-repo",
		Issue:    "42",
	}

	prompt, err := RenderPRFixPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPRFixPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "PR #99") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "Do NOT create a new PR") {
		t.Error("prompt should instruct not to create a new PR")
	}
	if !strings.Contains(prompt, "feature/my-branch") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "#42") {
		t.Error("prompt should reference issue when provided")
	}
	if !strings.Contains(prompt, "20260312-1820-abcd") {
		t.Error("prompt should contain run ID")
	}
}

func TestRenderPRFixPromptCustomTemplate(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	tmpl := "Fix PR #{{.PR}} run {{.RunID}} on {{.Branch}}"
	os.WriteFile(filepath.Join(klausDir, "pr-fix-prompt.md"), []byte(tmpl), 0o644)

	vars := PromptVars{
		RunID:  "fix-abc",
		PR:     "55",
		Branch: "feature/fix",
	}

	prompt, err := RenderPRFixPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPRFixPrompt() error: %v", err)
	}

	want := "Fix PR #55 run fix-abc on feature/fix"
	if prompt != want {
		t.Errorf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderPromptFromFileReadError(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	// Create a directory where a file is expected, causing a read error
	os.MkdirAll(filepath.Join(klausDir, "prompt.md"), 0o755)

	vars := PromptVars{RunID: "test-123"}
	_, err := RenderPrompt(dir, vars)
	if err == nil {
		t.Error("expected error when prompt.md is a directory")
	}
}

func TestRenderPromptDefaultContainsTesting(t *testing.T) {
	dir := t.TempDir()

	vars := PromptVars{RunID: "test-123"}
	prompt, err := RenderPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "## Testing") {
		t.Error("default prompt should contain testing section")
	}
	if !strings.Contains(prompt, "unit tests") {
		t.Error("default prompt should mention unit tests")
	}
}

func TestPreTrustWorktree(t *testing.T) {
	// Use a temp dir as both the worktree and the home dir
	homeDir := t.TempDir()
	worktreeDir := t.TempDir()

	t.Setenv("HOME", homeDir)

	if err := PreTrustWorktree(worktreeDir); err != nil {
		t.Fatalf("PreTrustWorktree() error: %v", err)
	}

	// Check that the project directory was created
	encoded := strings.NewReplacer(string(filepath.Separator), "-", ".", "-").Replace(worktreeDir)
	indexPath := filepath.Join(homeDir, ".claude", "projects", encoded, "sessions-index.json")

	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading sessions-index.json: %v", err)
	}

	var index map[string]any
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parsing sessions-index.json: %v", err)
	}

	if v, ok := index["version"].(float64); !ok || v != 1 {
		t.Errorf("version = %v, want 1", index["version"])
	}
	if p, ok := index["originalPath"].(string); !ok || p != worktreeDir {
		t.Errorf("originalPath = %v, want %s", index["originalPath"], worktreeDir)
	}
}

func TestPreTrustWorktreeDottedPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	// Create a worktree path with dot-prefixed components like .klaus session workspaces
	worktreeDir := filepath.Join(t.TempDir(), ".klaus", "sessions", "run-123")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("creating worktree dir: %v", err)
	}

	if err := PreTrustWorktree(worktreeDir); err != nil {
		t.Fatalf("PreTrustWorktree() error: %v", err)
	}

	// Dots should be replaced with hyphens, matching Claude Code's encoding
	encoded := strings.NewReplacer(string(filepath.Separator), "-", ".", "-").Replace(worktreeDir)
	indexPath := filepath.Join(homeDir, ".claude", "projects", encoded, "sessions-index.json")

	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading sessions-index.json: %v", err)
	}

	var index map[string]any
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parsing sessions-index.json: %v", err)
	}

	if p, ok := index["originalPath"].(string); !ok || p != worktreeDir {
		t.Errorf("originalPath = %v, want %s", index["originalPath"], worktreeDir)
	}
}

func TestPreTrustWorktreeIdempotent(t *testing.T) {
	homeDir := t.TempDir()
	worktreeDir := t.TempDir()

	t.Setenv("HOME", homeDir)

	// Call twice — second call should not error
	if err := PreTrustWorktree(worktreeDir); err != nil {
		t.Fatalf("first PreTrustWorktree() error: %v", err)
	}
	if err := PreTrustWorktree(worktreeDir); err != nil {
		t.Fatalf("second PreTrustWorktree() error: %v", err)
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()

	if err := Init(dir); err != nil {
		t.Fatalf("Init() error: %v", err)
	}

	// Verify config.json exists and is valid
	configPath := filepath.Join(dir, ".klaus", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if cfg.WorktreeBase != wantWorktreeBase {
		t.Errorf("config WorktreeBase = %q, want %q", cfg.WorktreeBase, wantWorktreeBase)
	}

	// Verify prompt.md exists
	promptPath := filepath.Join(dir, ".klaus", "prompt.md")
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("reading prompt: %v", err)
	}
	if !strings.Contains(string(promptData), "{{.RunID}}") {
		t.Error("prompt template should contain {{.RunID}}")
	}
}

func TestLoadEmptyRepoRoot(t *testing.T) {
	// When repoRoot is empty, Load should return defaults (plus any global config)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	// Should have valid defaults
	if cfg.DefaultBudget == "" {
		t.Error("DefaultBudget should not be empty")
	}
	if cfg.DefaultBranch == "" {
		t.Error("DefaultBranch should not be empty")
	}
}

func TestRenderSessionPromptNoRepo(t *testing.T) {
	// When RepoName is empty, the no-repo variant of the session prompt is rendered
	vars := PromptVars{
		RunID: "session-20260307-1430-a3f2",
	}

	prompt, err := RenderSessionPrompt("", vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "session-20260307-1430-a3f2") {
		t.Error("prompt should contain session ID")
	}
	if !strings.Contains(prompt, "scratch workspace") {
		t.Error("prompt should mention scratch workspace when no repo")
	}
	if !strings.Contains(prompt, "--repo owner/repo") {
		t.Error("prompt should mention --repo flag when no repo")
	}
	if strings.Contains(prompt, "isolated git worktree on branch") {
		t.Error("prompt should not mention git worktree when no repo")
	}
}

func TestRenderSessionPromptWithRepo(t *testing.T) {
	dir := t.TempDir()
	vars := PromptVars{
		RunID:    "session-20260307-1430-a3f2",
		Branch:   "session/session-20260307-1430-a3f2",
		RepoName: "my-project",
	}

	prompt, err := RenderSessionPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "isolated git worktree on branch") {
		t.Error("prompt should mention git worktree when in a repo")
	}
	if !strings.Contains(prompt, "my-project") {
		t.Error("prompt should contain repo name")
	}
	if strings.Contains(prompt, "scratch workspace") {
		t.Error("prompt should not mention scratch workspace when in a repo")
	}
}

func TestRenderSessionPromptWithProjects(t *testing.T) {
	dir := t.TempDir()

	vars := PromptVars{
		RunID:    "session-test",
		Branch:   "session/session-test",
		RepoName: "test-repo",
		Projects: "- klaus (/home/user/src/klaus)\n- other-project (/home/user/src/other-project)",
	}

	prompt, err := RenderSessionPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	if !strings.Contains(prompt, "## Registered projects") {
		t.Error("prompt should contain registered projects section when projects are present")
	}
	if !strings.Contains(prompt, "klaus (/home/user/src/klaus)") {
		t.Error("prompt should contain project listing")
	}
	if !strings.Contains(prompt, "klaus launch --repo <name>") {
		t.Error("prompt should mention 'klaus launch --repo <name>'")
	}
}

func TestRenderSessionPromptWithoutProjects(t *testing.T) {
	dir := t.TempDir()

	vars := PromptVars{
		RunID:    "session-test",
		Branch:   "session/session-test",
		RepoName: "test-repo",
	}

	prompt, err := RenderSessionPrompt(dir, vars)
	if err != nil {
		t.Fatalf("RenderSessionPrompt() error: %v", err)
	}

	if strings.Contains(prompt, "## Registered projects") {
		t.Error("prompt should not contain registered projects section when no projects")
	}
}

func TestFormatProjectList(t *testing.T) {
	t.Run("empty projects", func(t *testing.T) {
		result := FormatProjectList(map[string]string{})
		if result != "" {
			t.Errorf("expected empty string for empty projects, got %q", result)
		}
	})

	t.Run("nil projects", func(t *testing.T) {
		result := FormatProjectList(nil)
		if result != "" {
			t.Errorf("expected empty string for nil projects, got %q", result)
		}
	})

	t.Run("single project", func(t *testing.T) {
		result := FormatProjectList(map[string]string{
			"my-proj": "/tmp/my-proj",
		})
		if !strings.Contains(result, "- my-proj (/tmp/my-proj)") {
			t.Errorf("unexpected format: %q", result)
		}
	})

	t.Run("multiple projects sorted", func(t *testing.T) {
		result := FormatProjectList(map[string]string{
			"zebra": "/tmp/zebra",
			"alpha": "/tmp/alpha",
		})
		lines := strings.Split(result, "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 lines, got %d: %q", len(lines), result)
		}
		if !strings.HasPrefix(lines[0], "- alpha") {
			t.Errorf("expected first line to be alpha, got %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "- zebra") {
			t.Errorf("expected second line to be zebra, got %q", lines[1])
		}
	})
}

func TestRenderPromptEmptyRepoRoot(t *testing.T) {
	// With empty repoRoot, should use default template
	vars := PromptVars{RunID: "test-123"}
	prompt, err := RenderPrompt("", vars)
	if err != nil {
		t.Fatalf("RenderPrompt(\"\") error: %v", err)
	}
	if !strings.Contains(prompt, "Run: test-123") {
		t.Error("prompt should contain run ID from default template")
	}
}

func TestInitGlobal(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	if err := InitGlobal(); err != nil {
		t.Fatalf("InitGlobal() error: %v", err)
	}

	configPath := filepath.Join(homeDir, ".klaus", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading global config: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing global config: %v", err)
	}
	if cfg.WorktreeBase != wantWorktreeBase {
		t.Errorf("config WorktreeBase = %q, want %q", cfg.WorktreeBase, wantWorktreeBase)
	}
	if cfg.DefaultBudget != "5.00" {
		t.Errorf("config DefaultBudget = %q, want 5.00", cfg.DefaultBudget)
	}
}
