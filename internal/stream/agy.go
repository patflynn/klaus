package stream

import "encoding/json"

// normalizeAgy follows https://antigravity.google/docs/cli/headless.
func normalizeAgy(line []byte) []byte {
	var ev struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		Init           struct {
			Model string `json:"model"`
		} `json:"init"`
		Step struct {
			Type  string `json:"step_type"`
			State string `json:"state"`
			Text  string `json:"text_delta"`
			Tool  struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
				Output     string          `json:"output"`
			} `json:"tool_info"`
		} `json:"step_update"`
		Result struct {
			ConversationID  string  `json:"conversation_id"`
			Status          string  `json:"status"`
			Response        string  `json:"response"`
			Error           string  `json:"error"`
			DurationSeconds float64 `json:"duration_seconds"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Event == "" {
		return line
	}
	var out any
	switch ev.Event {
	case "init":
		out = map[string]any{"type": "system", "subtype": "init", "session_id": ev.ConversationID, "model": ev.Init.Model}
	case "step_update":
		if ev.Step.Type == "agent_response" && ev.Step.Text != "" {
			out = map[string]any{"type": "text_delta", "content": ev.Step.Text}
		} else if ev.Step.Type == "tool" && ev.Step.State == "DONE" {
			out = map[string]any{"type": "tool_result", "content": ev.Step.Tool.Output}
		} else if ev.Step.Type == "tool" && ev.Step.State == "ACTIVE" {
			out = map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": ev.Step.Tool.Name, "input": ev.Step.Tool.Parameters}}}}
		}
	case "result":
		subtype := "success"
		failed := ev.Result.Status != "SUCCESS"
		if failed {
			subtype = "error_during_execution"
		}
		out = map[string]any{"type": "result", "subtype": subtype, "session_id": ev.Result.ConversationID, "is_error": failed, "errors": []string{ev.Result.Error}, "content": ev.Result.Response, "duration_ms": int64(ev.Result.DurationSeconds * 1000)}
	}
	if out == nil {
		return line
	}
	data, err := json.Marshal(out)
	if err != nil {
		return line
	}
	return data
}
