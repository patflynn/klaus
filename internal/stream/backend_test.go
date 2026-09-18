package stream

import (
	"bytes"
	"strings"
	"testing"
)

func TestBackendStreams(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"codex", `{"type":"thread.started","thread_id":"thread"}
{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}
{"type":"item.completed","item":{"type":"agent_message","text":"Finished"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}
`, []string{"session started", "go test ./...", "Finished", "cost unavailable"}},
		{"agy", `{"event":"init","conversation_id":"thread","init":{"model":"test-model"}}
{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"Fin"}}
{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","text_delta":"ished\n"}}
{"event":"result","result":{"status":"SUCCESS","response":"Finished\n"}}
`, []string{"test-model", "Finished\n", "cost unavailable"}},
		{"agy failure", `{"event":"result","result":{"status":"ERROR","error":"authentication required"}}
`, []string{"agent failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := FormatStream(strings.NewReader(tc.input), &out); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %q", want, out.String())
				}
			}
		})
	}
}
