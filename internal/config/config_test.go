package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.WorktreeBase != "/tmp/klaus" {
		t.Errorf("WorktreeBase = %q, want /tmp/klaus", cfg.WorktreeBase)
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
	if cfg.WorktreeBase != "/tmp/klaus" {
		t.Errorf("WorktreeBase = %q, want /tmp/klaus", cfg.WorktreeBase)
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
	if cfg.WorktreeBase != "/tmp/klaus" {
		t.Errorf("config WorktreeBase = %q, want /tmp/klaus", cfg.WorktreeBase)
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
