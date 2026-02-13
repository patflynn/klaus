package stream

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatLineSystem(t *testing.T) {
	line := `{"type":"system","subtype":"init","model":"claude-sonnet-4-5-20250929"}`
	var buf bytes.Buffer
	FormatLine(line, &buf)

	want := "── session started (model: claude-sonnet-4-5-20250929) ──\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFormatLineSystemNoModel(t *testing.T) {
	line := `{"type":"system","subtype":"init"}`
	var buf bytes.Buffer
	FormatLine(line, &buf)

	want := "── session started (model: unknown) ──\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFormatLineAssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`
	var buf bytes.Buffer
	FormatLine(line, &buf)

	want := "Hello world\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFormatLineAssistantToolUse(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "Read",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/foo/bar.go"}}]}}`,
			want: "▶ Read /foo/bar.go\n",
		},
		{
			name: "Edit",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/foo/bar.go"}}]}}`,
			want: "▶ Edit /foo/bar.go\n",
		},
		{
			name: "Write",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/foo/bar.go"}}]}}`,
			want: "▶ Write /foo/bar.go\n",
		},
		{
			name: "Bash",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}`,
			want: "▶ Bash: git status\n",
		},
		{
			name: "Glob",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Glob","input":{"pattern":"**/*.go"}}]}}`,
			want: "▶ Glob **/*.go\n",
		},
		{
			name: "Grep",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}]}}`,
			want: "▶ Grep TODO\n",
		},
		{
			name: "unknown tool",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"CustomTool","input":{}}]}}`,
			want: "▶ CustomTool\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			FormatLine(tt.line, &buf)
			if buf.String() != tt.want {
				t.Errorf("got %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

func TestFormatLineResult(t *testing.T) {
	line := `{"type":"result","total_cost_usd":3.42,"duration_ms":45000}`
	var buf bytes.Buffer
	FormatLine(line, &buf)

	want := "\n── done (45.0s, $3.4200) ──\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFormatLineInvalidJSON(t *testing.T) {
	var buf bytes.Buffer
	FormatLine("not json", &buf)
	if buf.String() != "" {
		t.Errorf("invalid JSON should produce no output, got %q", buf.String())
	}
}

func TestFormatStream(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"test-model"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it..."}]}}`,
		`{"type":"result","total_cost_usd":1.5,"duration_ms":10000}`,
	}, "\n")

	var buf bytes.Buffer
	err := FormatStream(strings.NewReader(input), &buf)
	if err != nil {
		t.Fatalf("FormatStream() error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "session started") {
		t.Error("output should contain session started")
	}
	if !strings.Contains(output, "Working on it...") {
		t.Error("output should contain assistant text")
	}
	if !strings.Contains(output, "done") {
		t.Error("output should contain done")
	}
}

func TestFormatStreamEmptyLines(t *testing.T) {
	input := "\n\n" + `{"type":"system","subtype":"init","model":"m"}` + "\n\n"
	var buf bytes.Buffer
	err := FormatStream(strings.NewReader(input), &buf)
	if err != nil {
		t.Fatalf("FormatStream() error: %v", err)
	}
	if !strings.Contains(buf.String(), "session started") {
		t.Error("should handle empty lines gracefully")
	}
}
