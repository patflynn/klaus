package stream

import (
	"strings"
	"testing"
	"time"
)

func TestCheckKind(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"go test ./...", "test"},
		{"go build ./... && go vet ./... && go test -race ./...", "test"},
		{"cd internal/cmd && go vet ./...", "lint"},
		{"GOFLAGS=-mod=mod CGO_ENABLED=0 go build -o bin/klaus ./cmd/klaus", "build"},
		{"timeout 600 go test ./e2e/... 2>&1 | tail -50", "test"},
		{"nix develop -c cargo clippy --all-targets", "lint"},
		{"(cd web && npm run build)", "build"},
		{"make", "build"},
		{"make -j8 test-e2e", "test"},
		{"make -C docs lint", "lint"},
		{"python3 -m pytest -x tests/", "test"},
		{"nix flake check", "test"},
		{"git status && git diff", ""},
		{"grep -rn 'go test' Makefile", ""},
		{"echo go test", ""},
		{"klaus _pre-review", ""},
		{"go build ./... && klaus _pre-review", ""},
		{"", ""},
	} {
		if got := CheckKind(tc.line); got != tc.want {
			t.Errorf("CheckKind(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestCheckWatcherPairsCallsAndResults feeds a Claude stream with parallel
// calls, a background command and a non-check call, and a Codex stream whose
// calls carry no IDs, and checks each check ends at its own result.
func TestCheckWatcherPairsCallsAndResults(t *testing.T) {
	type ended struct {
		kind, cmd string
		ms        int64
		success   string
	}
	run := func(lines []string) (started []string, done []ended) {
		t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		w := &CheckWatcher{
			OnStart: func(c Check) { started = append(started, c.Kind+": "+c.Command) },
			OnEnd: func(c Check, end time.Time, success *bool) {
				s := "unknown"
				if success != nil && *success {
					s = "ok"
				} else if success != nil {
					s = "failed"
				}
				done = append(done, ended{c.Kind, c.Command, end.Sub(c.Start).Milliseconds(), s})
			},
		}
		for i, l := range lines {
			w.Observe(NormalizeLine([]byte(l)), t0.Add(time.Duration(i)*time.Second))
		}
		return started, done
	}

	t.Run("claude", func(t *testing.T) {
		started, done := run([]string{
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"go build ./..."}},{"type":"tool_use","id":"b","name":"Bash","input":{"command":"go test ./...\necho done"}}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"go.mod"}}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"d","name":"Bash","input":{"command":"go test ./e2e/...","run_in_background":true}}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"module x"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"FAIL"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"d","content":"started"}]}}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","is_error":false,"content":""}]}}`,
		})
		if got := strings.Join(started, "; "); got != "build: go build ./...; test: go test ./..." {
			t.Errorf("started %q", got)
		}
		want := []ended{{"test", "go test ./...", 4000, "failed"}, {"build", "go build ./...", 6000, "ok"}}
		if len(done) != len(want) || done[0] != want[0] || done[1] != want[1] {
			t.Errorf("ended %+v, want %+v", done, want)
		}
	})

	t.Run("codex", func(t *testing.T) {
		_, done := run([]string{
			`{"type":"item.started","item":{"type":"command_execution","command":"ls"}}`,
			`{"type":"item.completed","item":{"type":"command_execution","command":"ls","aggregated_output":"go.mod"}}`,
			`{"type":"item.started","item":{"type":"command_execution","command":"cargo test"}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"waiting"}}`,
			`{"type":"item.completed","item":{"type":"command_execution","command":"cargo test","aggregated_output":"ok"}}`,
		})
		if want := []ended{{"test", "cargo test", 2000, "unknown"}}; len(done) != 1 || done[0] != want[0] {
			t.Errorf("ended %+v, want %+v", done, want)
		}
	})
}
