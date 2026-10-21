package cmd

import (
	"io/fs"
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
// run's keys, and nothing for _pre-review, which reports itself. A command
// line that runs go vet and then _pre-review is a lint check that says it
// includes the pre-review. Outside a run's pane it reports nothing.
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
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"go vet ./... && klaus _pre-review"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"1 high+ finding"}]}}`,
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
	var types []string
	for _, e := range evts {
		types = append(types, e.Type)
	}
	if got, want := strings.Join(types, " "), "check:started check:completed check:started check:completed"; got != want {
		t.Fatalf("want check:started and check:completed for go test and go vet, got %+v", evts)
	}
	for i, e := range evts {
		if e.RunID != id {
			t.Errorf("%s on run %q, want %s", e.Type, e.RunID, id)
		}
		check := map[string]interface{}{"kind": "test", "command": "go test ./...", "includes_pre_review": nil}
		if i >= 2 {
			check = map[string]interface{}{"kind": "lint", "command": "go vet ./... && klaus _pre-review", "includes_pre_review": true}
		}
		for k, want := range map[string]interface{}{
			"kind": check["kind"], "command": check["command"], "includes_pre_review": check["includes_pre_review"], "source": "stream",
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
	if done := evts[3].Data; done["success"] != false {
		t.Errorf("the go vet and _pre-review line failed: %+v", done)
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

// TestSandboxPreReviewReachesTheRun runs a sandbox run's real pane command:
// ssh is a stub that runs the remote command as another host would, with its
// own HOME and none of the pane's environment, and the agent is a script
// that streams a call to the real klaus _pre-review and its output. The
// sandbox has no event log for the run, so _pre-review reports its check in
// its output, and the pane's _format-stream records it on the run. When the
// agent cuts the output, as with tail, _format-stream times the call from
// the stream instead. Nothing is written on the sandbox's side.
func TestSandboxPreReviewReachesTheRun(t *testing.T) {
	bin := klausBinary(t)
	for _, tc := range []struct {
		name, filter string
		want         map[string]interface{}
	}{
		{"reported", "", map[string]interface{}{"source": "pre-review", "via": "stream", "command": "klaus _pre-review", "findings": float64(0), "lint_failed": false}},
		{"output cut", " | tail -1", map[string]interface{}{"source": "stream", "via": nil, "findings": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, home, remoteHome := t.TempDir(), t.TempDir(), t.TempDir()
			const session, id = "session-20261021-0100-abcd", "20261021-0100-5a4db0c5"
			store := run.NewHomeDirStoreFromPath(filepath.Join(home, ".klaus", "sessions", session))
			if err := store.Save(&run.State{ID: id, ChangeID: "kkkkllllmmmmnnnnooooppppqqqqrrrr", RepoKey: "acme/widget",
				DispatchReason: event.DispatchLaunch, CreatedAt: "2026-10-21T01:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			worktree := filepath.Join(dir, "worktree")
			for _, args := range [][]string{{"init", "-q", "-b", "main", worktree}, {"-C", worktree, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "base"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			if err := os.MkdirAll(filepath.Join(worktree, ".klaus"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, ".klaus", "config.json"), []byte(`{"pre_review": {"linters": ["true"]}}`), 0o644); err != nil {
				t.Fatal(err)
			}

			stubs := filepath.Join(dir, "bin")
			if err := os.Mkdir(stubs, 0o755); err != nil {
				t.Fatal(err)
			}
			preReview := "klaus _pre-review --dir " + worktree + tc.filter
			agent := `#!/bin/sh
cat >/dev/null
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + preReview + `"}}]}}'
out=$(` + preReview + ` 2>&1)
esc=$(printf '%s\n' "$out" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/	/\\t/g' | awk '{printf "%s\\n", $0}')
printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"%s"}]}}\n' "$esc"
echo '{"type":"result","subtype":"success","total_cost_usd":0.1,"duration_ms":1000}'
`
			for name, script := range map[string]string{
				"ssh":   "#!/bin/sh\nshift\nexec env -u " + runIDEnv + " -u " + sessionIDEnv + " HOME=" + shellQuote(remoteHome) + " sh -c \"$1\"\n",
				"rsync": "#!/bin/sh\nexit 0\n",
				"klaus": "#!/bin/sh\n[ \"$1\" = _finalize ] && exit 0\nexec " + shellQuote(bin) + " \"$@\"\n",
				"agent": agent,
			} {
				if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv(sessionIDEnv, session)
			command := buildSandboxPaneCommand("sandbox-0", worktree, "KLAUS_BACKEND=claude agent", "", filepath.Join(dir, "log.jsonl"), "klaus", "", id)
			c := exec.Command("sh", "-c", command)
			c.Env = append(os.Environ(), "HOME="+home, "PATH="+stubs+":"+os.Getenv("PATH"))
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("pane command: %v\n%s", err, out)
			}
			log, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
			t.Logf("stream:\n%s", log)

			evts, err := event.NewLog(store.BaseDir()).Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(evts) != 2 || evts[0].Type != event.CheckStarted || evts[1].Type != event.CheckCompleted {
				t.Fatalf("want check:started and check:completed for the pre-review, got %+v", evts)
			}
			done := evts[1].Data
			want := map[string]interface{}{"kind": "pre-review", "success": true, "repo_key": "acme/widget", "dispatch_reason": "launch",
				"key": "acme/widget@kkkkllllmmmmnnnnooooppppqqqqrrrr"}
			for k, v := range tc.want {
				want[k] = v
			}
			for k, v := range want {
				if done[k] != v {
					t.Errorf("check:completed %s = %v, want %v", k, done[k], v)
				}
			}
			if evts[1].RunID != id || done["duration_ms"] == nil || done["started_at"] == nil || done["ended_at"] == nil {
				t.Errorf("check:completed on %s with %+v", evts[1].RunID, done)
			}

			filepath.WalkDir(remoteHome, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					t.Errorf("the sandbox side wrote %s", path)
				}
				return nil
			})
		})
	}
}
