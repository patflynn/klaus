package stream

import "encoding/json"

// NormalizeLine adapts Codex and agy JSONL events to the event shapes Klaus already
// consumes. Original logs are retained unchanged for diagnosis.
func NormalizeLine(line []byte) []byte {
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Output  string `json:"aggregated_output"`
		} `json:"item"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return normalizeAgy(line)
	}
	var out any
	switch ev.Type {
	case "thread.started":
		out = map[string]any{"type": "system", "subtype": "init", "session_id": ev.ThreadID, "model": "codex"}
	case "item.completed":
		switch ev.Item.Type {
		case "agent_message":
			out = map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": ev.Item.Text}}}}
		case "command_execution":
			out = map[string]any{"type": "tool_result", "content": ev.Item.Output}
		}
	case "item.started":
		if ev.Item.Type == "command_execution" {
			out = map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": ev.Item.Command}}}}}
		}
	case "turn.completed":
		out = map[string]any{"type": "result", "subtype": "success"}
	case "turn.failed":
		out = map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []string{ev.Error.Message}}
	}
	if out == nil {
		return normalizeAgy(line)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return normalizeAgy(line)
	}
	return data
}
