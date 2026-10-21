package stream

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCheckKind(t *testing.T) {
	for _, tc := range []struct {
		line, want string
		preReview  bool
	}{
		{"go test ./...", "test", false},
		{"go build ./... && go vet ./... && go test -race ./...", "test", false},
		{"cd internal/cmd && go vet ./...", "lint", false},
		{"GOFLAGS=-mod=mod CGO_ENABLED=0 go build -o bin/klaus ./cmd/klaus", "build", false},
		{"timeout 600 go test ./e2e/... 2>&1 | tail -50", "test", false},
		{"nix develop -c cargo clippy --all-targets", "lint", false},
		{"(cd web && npm run build)", "build", false},
		{"make", "build", false},
		{"make -j8 test-e2e", "test", false},
		{"make -C docs lint", "lint", false},
		{"python3 -m pytest -x tests/", "test", false},
		{"nix flake check", "test", false},
		{"git status && git diff", "", false},
		{"grep -rn 'go test' Makefile", "", false},
		{"echo go test", "", false},
		{"klaus _pre-review", "", true},
		{"go build ./... && klaus _pre-review", "build", true},
		{"go test ./... && KLAUS_BACKEND=codex ~/bin/klaus _pre-review --dir .", "test", true},
		{"grep -rn 'klaus _pre-review' README.md", "", false},
		{"echo klaus _pre-review", "", false},
		{"", "", false},
	} {
		if got := CheckKind(tc.line); got != tc.want {
			t.Errorf("CheckKind(%q) = %q, want %q", tc.line, got, tc.want)
		}
		if got := RunsPreReview(tc.line); got != tc.preReview {
			t.Errorf("RunsPreReview(%q) = %v, want %v", tc.line, got, tc.preReview)
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

// TestCheckWatcherPreReviewCalls feeds a Claude stream that runs go test and
// _pre-review in one command line, whose result reports the pre-review's
// check, then _pre-review alone with a result that reports nothing, and a
// grep whose output quotes a report, then a failing go build and _pre-review
// line that reports nothing. The command lines' checks say they include the
// pre-review, and the report comes out of the first one's result; a sandbox
// run times the bare _pre-review from the stream, since nothing else reports
// it, but not the last line's, which go build may have stopped first. The
// grep runs no _pre-review, so what it prints reports nothing.
func TestCheckWatcherPreReviewCalls(t *testing.T) {
	report := ReportPrefix + `{"kind":"pre-review","source":"pre-review","started_at":"2026-10-20T12:00:01Z","ended_at":"2026-10-20T12:00:03Z","duration_ms":2000,"success":true,"findings":0}`
	result := func(id, output string) string {
		b, _ := json.Marshal(output)
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":false,"content":[{"type":"text","text":` + string(b) + `}]}]}}`
	}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"go test ./... && klaus _pre-review"}}]}}`,
		result("a", "ok  \tgithub.com/acme/widget\t0.1s\n=== Pre-PR Review ===\n"+report+"\nAll checks passed."),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"klaus _pre-review 2>&1 | tail -1"}}]}}`,
		result("b", "All checks passed."),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c","name":"Bash","input":{"command":"grep -rn klaus-check-completed ."}}]}}`,
		result("c", "./log:"+report),
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"d","name":"Bash","input":{"command":"go build ./... && klaus _pre-review"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"d","is_error":true,"content":"undefined: x"}]}}`,
	}
	run := func(sandbox bool) (checks []string, reports []map[string]interface{}) {
		t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		w := &CheckWatcher{
			Sandbox: sandbox,
			OnEnd: func(c Check, end time.Time, success *bool) {
				checks = append(checks, fmt.Sprintf("%s %q %dms pre-review:%v ok:%v", c.Kind, c.Command, end.Sub(c.Start).Milliseconds(), c.IncludesPreReview, success != nil && *success))
			},
			OnReport: func(data map[string]interface{}) { reports = append(reports, data) },
		}
		for i, l := range lines {
			w.Observe(NormalizeLine([]byte(l)), t0.Add(time.Duration(i)*time.Second))
		}
		return checks, reports
	}

	checks, reports := run(false)
	if want := []string{
		`test "go test ./... && klaus _pre-review" 1000ms pre-review:true ok:true`,
		`build "go build ./... && klaus _pre-review" 1000ms pre-review:true ok:false`,
	}; strings.Join(checks, "\n") != strings.Join(want, "\n") {
		t.Errorf("local run checks:\n%s\nwant:\n%s", strings.Join(checks, "\n"), strings.Join(want, "\n"))
	}
	if len(reports) != 1 || reports[0]["kind"] != "pre-review" || reports[0]["duration_ms"] != float64(2000) || reports[0]["findings"] != float64(0) {
		t.Errorf("reports %+v, want the first call's pre-review only", reports)
	}

	checks, reports = run(true)
	want := []string{
		`test "go test ./... && klaus _pre-review" 1000ms pre-review:true ok:true`,
		`pre-review "klaus _pre-review 2>&1 | tail -1" 1000ms pre-review:false ok:true`,
		`build "go build ./... && klaus _pre-review" 1000ms pre-review:true ok:false`,
	}
	if strings.Join(checks, "\n") != strings.Join(want, "\n") {
		t.Errorf("sandbox run checks:\n%s\nwant:\n%s", strings.Join(checks, "\n"), strings.Join(want, "\n"))
	}
	if len(reports) != 1 {
		t.Errorf("sandbox run reports %+v, want the first call's pre-review only", reports)
	}
}
