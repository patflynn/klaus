package review

import (
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

func TestModelID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"haiku", "claude-haiku-4-5-20251001"},
		{"sonnet", "claude-sonnet-4-5-20250514"},
		{"opus", "claude-opus-4-0-20250514"},
		{"HAIKU", "claude-haiku-4-5-20251001"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
	}
	for _, tt := range tests {
		got := string(modelID(tt.input))
		if got != tt.want {
			t.Errorf("modelID(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
