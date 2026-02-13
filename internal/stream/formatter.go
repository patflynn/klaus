package stream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Event represents a parsed JSONL event from Claude's stream-json output.
type Event struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Model   string          `json:"model,omitempty"`
	Message *AssistantMsg   `json:"message,omitempty"`

	// Result fields
	TotalCostUSD *float64 `json:"total_cost_usd,omitempty"`
	DurationMS   *int64   `json:"duration_ms,omitempty"`
}

// AssistantMsg represents the message field in an assistant event.
type AssistantMsg struct {
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents a single content block in an assistant message.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ToolInput is used to extract specific fields from tool_use input.
type ToolInput struct {
	FilePath string `json:"file_path,omitempty"`
	Command  string `json:"command,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

// FormatStream reads JSONL from r and writes human-readable progress to w.
func FormatStream(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	// Allow large lines (Claude output can be big)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		FormatLine(line, w)
	}
	return scanner.Err()
}

// FormatLine formats a single JSONL line and writes it to w.
func FormatLine(line string, w io.Writer) {
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}

	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			model := ev.Model
			if model == "" {
				model = "unknown"
			}
			fmt.Fprintf(w, "── session started (model: %s) ──\n", model)
		}

	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				fmt.Fprintln(w, block.Text)
			case "tool_use":
				fmt.Fprintln(w, formatToolUse(block))
			}
		}

	case "result":
		cost := float64(0)
		if ev.TotalCostUSD != nil {
			cost = *ev.TotalCostUSD
		}
		durationMS := int64(0)
		if ev.DurationMS != nil {
			durationMS = *ev.DurationMS
		}
		durationS := float64(durationMS) / 1000.0
		fmt.Fprintln(w)
		fmt.Fprintf(w, "── done (%.1fs, $%.4f) ──\n", durationS, cost)
	}
}

func formatToolUse(block ContentBlock) string {
	var input ToolInput
	if block.Input != nil {
		json.Unmarshal(block.Input, &input)
	}

	switch block.Name {
	case "Read":
		return fmt.Sprintf("▶ Read %s", input.FilePath)
	case "Edit":
		return fmt.Sprintf("▶ Edit %s", input.FilePath)
	case "Write":
		return fmt.Sprintf("▶ Write %s", input.FilePath)
	case "Bash":
		cmd := input.Command
		// Show only first line of command
		for i, c := range cmd {
			if c == '\n' {
				cmd = cmd[:i]
				break
			}
		}
		return fmt.Sprintf("▶ Bash: %s", cmd)
	case "Glob":
		return fmt.Sprintf("▶ Glob %s", input.Pattern)
	case "Grep":
		return fmt.Sprintf("▶ Grep %s", input.Pattern)
	default:
		return fmt.Sprintf("▶ %s", block.Name)
	}
}
