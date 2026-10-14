//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// useWorker makes the fake worker of a backend, claude, codex or agy, print
// that backend's stream for a run whose conversation is session, with no PR
// URL, so finalize records the pushed branch. Every fake worker is
// claudeStubScript, so resetWorker, WaitForClaudeStart, ClaudeArgv and
// ReleaseClaude serve all three. The claude stub is left as it is, which may
// be recordConversation's.
func (h *Harness) useWorker(kind, session string) {
	h.t.Helper()
	switch kind {
	case "claude":
		h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + session + `"}` + "\n")
	case "codex":
		h.WriteStub("codex", h.claudeStubScript())
		h.WriteDefaultClaudeOutput(`{"type":"thread.started","thread_id":"` + session + `"}
{"type":"item.completed","item":{"type":"agent_message","text":"Pushed the branch."}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}` + "\n")
	case "agy":
		h.WriteStub("agy", h.claudeStubScript())
		h.WriteDefaultClaudeOutput(`{"event":"init","conversation_id":"` + session + `","init":{"model":"test"}}
{"event":"result","result":{"conversation_id":"` + session + `","status":"SUCCESS","response":"Pushed the branch.","duration_seconds":1}}` + "\n")
	default:
		h.t.Fatalf("no fake worker for backend %q", kind)
	}
}

// startRun launches a run, waits for its fake worker to start, and has it
// commit and push file; it returns the run and the launch's output.
func (h *Harness) startRun(file string, args ...string) (*runAndOutput, string) {
	h.t.Helper()
	st, res := h.launchRun(args...)
	h.WaitForClaudeStart(30 * time.Second)
	head := h.pushWork(st, file, "Work on "+file)
	return &runAndOutput{st.ID, st.Branch, st.ChangeID, st.Worktree, res.Stdout}, head
}

type runAndOutput struct{ ID, Branch, ChangeID, Worktree, Stdout string }

// waitNextSecond sleeps into the next second. Runs record when they were
// created to the second, so a run meant to be a change's newest must start
// in a later second than the run before it.
func waitNextSecond() {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
}

// TestChangeContinuesOnEveryBackend continues a change a Claude run made
// with a Codex worker and with an agy worker. Each works on the change's
// branch with its change ID, stamps it on its commit, and finalize marks
// the pushed head ready as a version of the same change. Neither can resume
// a Claude conversation, and the replay: line says so.
func TestChangeContinuesOnEveryBackend(t *testing.T) {
	for kind, why := range map[string]string{
		"codex": "a codex worker can't resume its conversation",
		"agy":   "agy workers can't resume a conversation",
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.RegisterProject("acme/widget", h.RepoDir)
			h.installValleyStub()
			h.joinValley()

			h.useWorker("claude", changeSession)
			first, _ := h.startRun("health.go", "add a health check endpoint")
			h.finishRun(first.ID)

			h.resetWorker()
			h.useWorker(kind, kind+"-session")
			follow, head := h.startRun("health_test.go", "--repo", "widget", "--backend", kind, "--change", first.ID, "test the health check")
			want := "replay:   fresh agent (" + why + ")"
			if kind == "codex" {
				want = "replay:   fresh agent (the latest run, " + first.ID + " (claude), ran on another backend; " + why + ")"
			}
			if !strings.Contains(follow.Stdout, want) {
				t.Errorf("launch output missing %q:\n%s", want, follow.Stdout)
			}
			if follow.Branch != first.Branch || follow.ChangeID != first.ChangeID {
				t.Errorf("%s run on branch %q, change %q; want %q, %q", kind, follow.Branch, follow.ChangeID, first.Branch, first.ChangeID)
			}
			// The stubs record their arguments, not their name.
			if argv := h.ClaudeArgv(); argv == "" || strings.HasPrefix(argv, "exec\nfork\n") {
				t.Errorf("%s worker should start fresh:\n%s", kind, argv)
			}
			if got := h.GitOutput(follow.Worktree, "log", "-1", "--format=%(trailers:key=Change-Id,valueonly)"); got != first.ChangeID {
				t.Errorf("%s run's commit Change-Id = %q, want %q", kind, got, first.ChangeID)
			}
			st := h.finishRun(follow.ID)
			if st.Backend != kind {
				t.Errorf("run's backend = %q, want %q", st.Backend, kind)
			}
			ready := follow.Worktree + ": ready --change " + first.ChangeID + " --head " + head + " " + first.Branch
			if calls := h.valleyCalls("ready"); len(calls) != 2 || calls[1] != ready {
				t.Errorf("valley ready calls = %q, want the second to be %q", calls, ready)
			}
		})
	}
}

// TestChangeReplaysOnlyTheLatestRun: a change worked on by a Claude run, then
// a Codex run, continued on Claude, starts fresh. The Claude conversation on
// the data ref never saw the Codex run's work.
func TestChangeReplaysOnlyTheLatestRun(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.recordConversation()

	h.useWorker("claude", changeSession)
	a, _ := h.startRun("health.go", "add a health check endpoint")
	h.finishRun(a.ID)

	h.resetWorker()
	h.useWorker("codex", "codex-thread")
	waitNextSecond()
	b, _ := h.startRun("health_test.go", "--backend", "codex", "--change", a.ID, "test the health check")
	h.finishRun(b.ID)

	h.resetWorker()
	h.useWorker("claude", changeSession)
	c, res := h.launchRun("--change", a.ID, "rename the endpoint")
	h.WaitForClaudeStart(30 * time.Second)
	want := "replay:   fresh agent (the latest run, " + b.ID + " (codex), ran on another backend; a claude worker can't resume its conversation)"
	if !strings.Contains(res.Stdout, want) {
		t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
	}
	if argv := h.ClaudeArgv(); strings.Contains(argv, "--resume") {
		t.Errorf("claude should not resume %s's conversation, which predates %s:\n%s", a.ID, b.ID, argv)
	}
	h.finishRun(c.ID)
}

// TestChangeForksCodexSession: a Codex run continuing a change a Codex run
// made forks that run's session, as Claude replays; --no-replay starts fresh.
func TestChangeForksCodexSession(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useWorker("codex", "codex-thread")
	a, _ := h.startRun("health.go", "--backend", "codex", "add a health check endpoint")
	h.finishRun(a.ID)

	h.resetWorker()
	b, res := h.launchRun("--backend", "codex", "--change", a.ID, "test the health check")
	h.WaitForClaudeStart(30 * time.Second)
	if want := "replay:   forking Codex session codex-thread from run " + a.ID; !strings.Contains(res.Stdout, want) {
		t.Errorf("launch output missing %q:\n%s", want, res.Stdout)
	}
	if argv := h.ClaudeArgv(); !strings.HasPrefix(argv, "exec\nfork\ncodex-thread\n") {
		t.Errorf("codex should fork the change's session:\n%s", argv)
	}
	if b.OriginalRunID == nil || *b.OriginalRunID != a.ID {
		t.Errorf("run should record the run it continues, %s: %v", a.ID, b.OriginalRunID)
	}
	h.finishRun(b.ID)

	h.resetWorker()
	c, res := h.launchRun("--backend", "codex", "--change", a.ID, "--no-replay", "start the tests over")
	h.WaitForClaudeStart(30 * time.Second)
	if !strings.Contains(res.Stdout, "replay:   fresh agent (--no-replay)") {
		t.Errorf("launch output should say --no-replay starts fresh:\n%s", res.Stdout)
	}
	if argv := h.ClaudeArgv(); !strings.HasPrefix(argv, "exec\n--json\n") {
		t.Errorf("--no-replay should start a fresh codex worker:\n%s", argv)
	}
	h.finishRun(c.ID)
}
