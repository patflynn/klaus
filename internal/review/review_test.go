package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseReviewResponse_valid(t *testing.T) {
	input := `{
		"findings": [
			{"severity": "CRITICAL", "file": "main.go", "line": 10, "description": "nil pointer"},
			{"severity": "low", "file": "util.go", "line": 5, "description": "unused var"}
		],
		"summary": "Found 2 issues"
	}`
	result, err := parseReviewResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(result.Findings))
	}
	if result.Findings[0].Severity != "critical" {
		t.Errorf("severity = %q, want %q", result.Findings[0].Severity, "critical")
	}
	if result.Findings[0].File != "main.go" {
		t.Errorf("file = %q, want %q", result.Findings[0].File, "main.go")
	}
	if result.Findings[0].Line != 10 {
		t.Errorf("line = %d, want %d", result.Findings[0].Line, 10)
	}
	if result.Findings[1].Severity != "low" {
		t.Errorf("severity = %q, want %q", result.Findings[1].Severity, "low")
	}
	if result.Summary != "Found 2 issues" {
		t.Errorf("summary = %q, want %q", result.Summary, "Found 2 issues")
	}
}

func TestParseReviewResponse_withCodeFences(t *testing.T) {
	input := "```json\n{\"findings\": [], \"summary\": \"Clean\"}\n```"
	result, err := parseReviewResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(result.Findings))
	}
	if result.Summary != "Clean" {
		t.Errorf("summary = %q, want %q", result.Summary, "Clean")
	}
}

func TestParseReviewResponse_noFindings(t *testing.T) {
	input := `{"findings": [], "summary": "No issues found."}`
	result, err := parseReviewResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(result.Findings))
	}
}

func TestParseReviewResponse_malformed(t *testing.T) {
	input := "this is not json at all"
	result, err := parseReviewResponse(input)
	if err == nil {
		t.Fatal("expected error for malformed JSON input")
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

func TestBuildReviewPrompt(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n+fmt.Println(\"hello\")"
	prompt := buildReviewPrompt(diff)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !contains(prompt, "Correctness bugs") {
		t.Error("prompt should mention correctness bugs")
	}
	if !contains(prompt, diff) {
		t.Error("prompt should contain the diff")
	}
}

func TestCallReviewAPI_success(t *testing.T) {
	// Create a fake claude script that returns valid JSON
	tmp := t.TempDir()
	script := filepath.Join(tmp, "claude")
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"findings":[{"severity":"high","file":"foo.go","line":1,"description":"test issue"}],"summary":"one issue"}'
`), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	result, err := callReviewAPI("diff --git a/foo.go", ReviewConfig{Model: "haiku"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result.Findings))
	}
	if result.Findings[0].Severity != "high" {
		t.Errorf("severity = %q, want %q", result.Findings[0].Severity, "high")
	}
	if result.Summary != "one issue" {
		t.Errorf("summary = %q, want %q", result.Summary, "one issue")
	}
}

func TestCallReviewAPI_cliFailure(t *testing.T) {
	// Create a fake claude script that exits with an error
	tmp := t.TempDir()
	script := filepath.Join(tmp, "claude")
	err := os.WriteFile(script, []byte(`#!/bin/sh
echo "something went wrong" >&2
exit 1
`), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	_, err = callReviewAPI("diff --git a/foo.go", ReviewConfig{})
	if err == nil {
		t.Fatal("expected error when claude CLI fails")
	}
	if !searchString(err.Error(), "calling claude CLI") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "calling claude CLI")
	}
	if !searchString(err.Error(), "something went wrong") {
		t.Errorf("error = %q, want it to contain stderr output", err.Error())
	}
}

func TestCallReviewAPI_defaultModel(t *testing.T) {
	// Create a fake claude script that validates the --model flag
	tmp := t.TempDir()
	script := filepath.Join(tmp, "claude")
	err := os.WriteFile(script, []byte(`#!/bin/sh
# Check that --model haiku is passed (the default)
while [ $# -gt 0 ]; do
  case "$1" in
    --model) shift; echo "$1" >&2; break;;
    *) shift;;
  esac
done
echo '{"findings":[],"summary":"clean"}'
`), 0o755)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	result, err := callReviewAPI("some diff", ReviewConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "clean" {
		t.Errorf("summary = %q, want %q", result.Summary, "clean")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
