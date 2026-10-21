package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// TestFormatStreamEmitsCheckEvents pipes a Claude stream through the real
// klaus _format-stream, as an agent's pane does, and checks it reports the
// stream's test run as a check of the pane's run (KLAUS_RUN_ID), with the
// run's keys, and nothing for _pre-review, which reports itself. Outside a
// run's pane it reports nothing.
func TestFormatStreamEmitsCheckEvents(t *testing.T) {
	bin := klausBinary(t)
	home := t.TempDir()
	const session, id = "session-20261020-1200-abcd", "20261020-1200-0badc0de"
	store := run.NewHomeDirStoreFromPath(filepath.Join(home, ".klaus", "sessions", session))
	pr, prURL := "12", "https://github.com/acme/widget/pull/12"
	if err := store.Save(&run.State{ID: id, ChangeID: "kkkkllllmmmmnnnnooooppppqqqqrrrr", PR: &pr, PRURL: &prURL,
		DispatchReason: event.DispatchCIFix, HeadSHA: "0123456789abcdef0123456789abcdef01234567", CreatedAt: "2026-10-20T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"klaus _pre-review"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":false,"content":"All checks passed."}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.1,"duration_ms":1000}`,
	}, "\n") + "\n"

	format := func(runID string) string {
		c := exec.Command(bin, "_format-stream")
		c.Env = append(os.Environ(), "HOME="+home, sessionIDEnv+"="+session, runIDEnv+"="+runID)
		c.Stdin = strings.NewReader(stream)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("_format-stream: %v\n%s", err, out)
		}
		return string(out)
	}

	if out := format(id); !strings.Contains(out, "▶ Bash: go test ./...") {
		t.Errorf("_format-stream should still format the stream:\n%s", out)
	}
	evts, err := event.NewLog(store.BaseDir()).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 2 || evts[0].Type != event.CheckStarted || evts[1].Type != event.CheckCompleted {
		t.Fatalf("want check:started and check:completed for go test only, got %+v", evts)
	}
	for _, e := range evts {
		if e.RunID != id {
			t.Errorf("%s on run %q, want %s", e.Type, e.RunID, id)
		}
		for k, want := range map[string]interface{}{
			"kind": "test", "command": "go test ./...", "source": "stream",
			"repo_key": "acme/widget", "pr_number": "12", "key": "acme/widget#12",
			"change_id": "kkkkllllmmmmnnnnooooppppqqqqrrrr", "dispatch_reason": "ci-fix",
			"head_sha": "0123456789abcdef0123456789abcdef01234567",
		} {
			if e.Data[k] != want {
				t.Errorf("%s %s = %v, want %v", e.Type, k, e.Data[k], want)
			}
		}
	}
	if done := evts[1].Data; done["success"] != true || done["duration_ms"] == nil || done["ended_at"] == nil {
		t.Errorf("check:completed should carry success, duration_ms and ended_at: %+v", done)
	}

	format("")
	if after, _ := event.NewLog(store.BaseDir()).Read(); len(after) != len(evts) {
		t.Errorf("_format-stream outside a run's pane emitted %+v", after[len(evts):])
	}
}

// TestPreReviewEmitsCheckEvents runs the real klaus _pre-review in a run's
// pane (KLAUS_RUN_ID) on a branch with nothing to review, once with a linter
// that passes and once with one that fails, and checks each run is a
// pre-review check of that run, which says how it ended, even when
// _pre-review exits 1.
func TestPreReviewEmitsCheckEvents(t *testing.T) {
	bin := klausBinary(t)
	home := t.TempDir()
	const session, id = "session-20261020-1300-abcd", "20261020-1300-feedface"
	store := run.NewHomeDirStoreFromPath(filepath.Join(home, ".klaus", "sessions", session))
	if err := store.Save(&run.State{ID: id, ChangeID: "kkkkllllmmmmnnnnooooppppqqqqrrrr", RepoKey: "valley:widget",
		DispatchReason: event.DispatchLaunch, CreatedAt: "2026-10-20T13:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitIn("init", "-q", "-b", "main")
	gitIn("commit", "-q", "--allow-empty", "-m", "base")

	preReview := func(linter string) error {
		if err := os.MkdirAll(filepath.Join(repo, ".klaus"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `{"pre_review": {"linters": [` + strconv.Quote(linter) + `]}}`
		if err := os.WriteFile(filepath.Join(repo, ".klaus", "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		c := exec.Command(bin, "_pre-review", "--dir", repo)
		c.Env = append(os.Environ(), "HOME="+home, sessionIDEnv+"="+session, runIDEnv+"="+id, "KLAUS_BACKEND=claude")
		out, err := c.CombinedOutput()
		t.Logf("_pre-review with %s:\n%s", linter, out)
		return err
	}

	if err := preReview("true"); err != nil {
		t.Fatalf("_pre-review with a passing linter: %v", err)
	}
	if err := preReview("false"); err == nil {
		t.Fatal("_pre-review with a failing linter should exit 1")
	}

	evts, err := event.NewLog(store.BaseDir()).Read()
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range evts {
		types = append(types, e.Type)
		if e.RunID != id || e.Data["kind"] != "pre-review" || e.Data["source"] != "pre-review" ||
			e.Data["repo_key"] != "valley:widget" || e.Data["key"] != "valley:widget@kkkkllllmmmmnnnnooooppppqqqqrrrr" || e.Data["dispatch_reason"] != "launch" {
			t.Errorf("%s: %+v", e.Type, e.Data)
		}
	}
	if got, want := strings.Join(types, " "), "check:started check:completed check:started check:completed"; got != want {
		t.Fatalf("events %s, want %s", got, want)
	}
	if d := evts[1].Data; d["success"] != true || d["lint_failed"] != false || d["findings"] != float64(0) || d["duration_ms"] == nil {
		t.Errorf("passing _pre-review completed with %+v", d)
	}
	if d := evts[3].Data; d["success"] != false || d["lint_failed"] != true {
		t.Errorf("failing _pre-review completed with %+v", d)
	}
}
