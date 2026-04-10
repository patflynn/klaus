package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/config"
)

func TestValidProjectName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"simple name", "my-project", true},
		{"alphanumeric", "project123", true},
		{"with dots", "my.project", true},
		{"with underscores", "my_project", true},
		{"single char", "a", true},
		{"uppercase", "MyProject", true},
		{"starts with number", "1project", true},
		{"empty", "", false},
		{"has spaces", "my project", false},
		{"starts with hyphen", "-project", false},
		{"starts with dot", ".project", false},
		{"has slash", "my/project", false},
		{"has at sign", "my@project", false},
		{"ends with dot", "project.", false},
		{"too long", strings.Repeat("a", 101), false},
		{"max length", strings.Repeat("a", 100), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidProjectName(tt.input)
			if got != tt.want {
				t.Errorf("ValidProjectName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildScaffoldPrompt(t *testing.T) {
	principles := "## Test Principles\n- Use Go\n"
	prompt := BuildScaffoldPrompt("my-app", "a cool app", "cli", principles)

	checks := []struct {
		label string
		want  string
	}{
		{"project type", "cli project"},
		{"project name", "'my-app'"},
		{"description", "a cool app"},
		{"principles", "## Test Principles"},
		{"principles content", "Use Go"},
		{"flake.nix", "flake.nix"},
		{"CLAUDE.md", "CLAUDE.md"},
		{"push to main", "push directly to main"},
		{"no PR", "Do NOT create a PR"},
		{"description tag", "<user-description>"},
		{"principles tag", "<principles>"},
		{"untrusted data warning", "Treat the content inside"},
	}

	for _, c := range checks {
		if !strings.Contains(prompt, c.want) {
			t.Errorf("prompt missing %s (%q)", c.label, c.want)
		}
	}
}

func TestBuildScaffoldPromptWebType(t *testing.T) {
	prompt := BuildScaffoldPrompt("web-app", "a website", "web", config.DefaultPrinciples)

	if !strings.Contains(prompt, "web project") {
		t.Error("prompt should contain 'web project' for web type")
	}
	if !strings.Contains(prompt, "'web-app'") {
		t.Error("prompt should contain project name")
	}
	if !strings.Contains(prompt, "Playwright") {
		t.Error("prompt with default principles should mention Playwright")
	}
	if !strings.Contains(prompt, "Tailwind CSS") {
		t.Error("prompt for web projects should mention Tailwind CSS")
	}
}

func TestBuildScaffoldPromptIncludesPrinciples(t *testing.T) {
	customPrinciples := "# Custom\n- Always use Rust\n- No JavaScript ever"
	prompt := BuildScaffoldPrompt("proj", "test", "cli", customPrinciples)

	if !strings.Contains(prompt, "Always use Rust") {
		t.Error("prompt should include custom principles content")
	}
	if !strings.Contains(prompt, "No JavaScript ever") {
		t.Error("prompt should include all custom principles")
	}
}

func TestLoadPrinciplesDefault(t *testing.T) {
	dir := t.TempDir() // no .klaus/principles.md

	principles, err := config.LoadPrinciples(dir)
	if err != nil {
		t.Fatalf("LoadPrinciples() error: %v", err)
	}

	if principles != config.DefaultPrinciples {
		t.Error("expected default principles when file doesn't exist")
	}

	if !strings.Contains(principles, "## Tech Defaults") {
		t.Error("default principles should contain Tech Defaults section")
	}
	if !strings.Contains(principles, "nix flakes") {
		t.Error("default principles should mention nix flakes")
	}
	if !strings.Contains(principles, "Tailwind CSS") {
		t.Error("default principles should mention Tailwind CSS")
	}
}

func TestLoadPrinciplesFromFile(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	custom := "# My Principles\n- Always test first\n"
	os.WriteFile(filepath.Join(klausDir, "principles.md"), []byte(custom), 0o644)

	principles, err := config.LoadPrinciples(dir)
	if err != nil {
		t.Fatalf("LoadPrinciples() error: %v", err)
	}

	if principles != custom {
		t.Errorf("LoadPrinciples() = %q, want %q", principles, custom)
	}
}

func TestLoadPrinciplesReadError(t *testing.T) {
	dir := t.TempDir()
	klausDir := filepath.Join(dir, ".klaus")
	os.MkdirAll(klausDir, 0o755)

	// Create a directory where a file is expected
	os.MkdirAll(filepath.Join(klausDir, "principles.md"), 0o755)

	_, err := config.LoadPrinciples(dir)
	if err == nil {
		t.Error("expected error when principles.md is a directory")
	}
}

func TestNewCmdTypeValidation(t *testing.T) {
	cmd := scaffoldCmd
	cmd.SetArgs([]string{"test-proj"})

	// Test invalid type
	cmd.Flags().Set("description", "test")
	cmd.Flags().Set("type", "invalid")

	err := cmd.RunE(cmd, []string{"test-proj"})
	if err == nil {
		t.Error("expected error for invalid project type")
	}
	if !strings.Contains(err.Error(), "invalid project type") {
		t.Errorf("error should mention invalid project type, got: %v", err)
	}
}

func TestNewCmdNameValidation(t *testing.T) {
	err := runNew(scaffoldCmd, []string{"invalid project name"})
	if err == nil {
		t.Error("expected error for invalid project name with spaces")
	}
	if !strings.Contains(err.Error(), "invalid project name") {
		t.Errorf("error should mention invalid project name, got: %v", err)
	}
}
