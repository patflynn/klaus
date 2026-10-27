//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/restack"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// gitStdin runs real git in dir with stdin, as GitOutput does.
func (h *Harness) gitStdin(dir, stdin string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+h.Home, "GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// valleyIn runs the valley on PATH in dir, failing the test unless it
// succeeds.
func (h *Harness) valleyIn(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("valley", args...)
	cmd.Dir, cmd.Env = dir, h.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("valley %v: %v: %s", args, err, out)
	}
	return string(out)
}

// commitToMain commits content to file on main and pushes it, returning
// main's new tip.
func (h *Harness) commitToMain(file, content string) string {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.RepoDir, file), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(h.RepoDir, "add", file)
	h.git(h.RepoDir, "commit", "-qm", "main: "+file)
	h.git(h.RepoDir, "push", "-q", "origin", "main")
	return h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
}

// plantReport writes a conflict report on change id to origin, as the
// host's restacker pushes one: a commit holding report.json alone, on the
// change's previous report, at refs/the-valley/restack/main/<id>. It
// observes what origin holds now (the record, the branch's tip, version
// and head as the valley lists them) and main at tip, and names path as
// the conflict. It returns the report commit.
func (h *Harness) plantReport(id, tip, path string) string {
	h.t.Helper()
	var ch valley.Change
	for _, c := range h.valleyChanges().Changes {
		if c.ChangeID == id {
			ch = c
		}
	}
	var head, branch string
	for _, v := range ch.Versions {
		if v.N == ch.Active {
			head, branch = v.Head, v.Branch
		}
	}
	if head == "" {
		h.t.Fatalf("the valley lists no active version of %s", id)
	}
	sha := func(oid string) string { return "git-sha1:" + oid }
	report := map[string]any{
		"schema": valley.RestackSchema, "change_id": id, "target": "main", "target_tip": sha(tip),
		"operation": fmt.Sprintf("e2e-%d", time.Now().UnixNano()), "result": valley.RestackConflict,
		"layers": []any{map[string]any{
			"change_id": id, "record": sha(ch.Record), "version": ch.Active, "head": sha(head), "branch": branch,
			"branch_tip": sha(h.GitOutput(h.OriginDir, "rev-parse", "refs/heads/"+branch)), "published": nil,
		}},
		"failing_layer": id, "conflict_paths": []string{path}, "blocking_requests": []any{}, "refusal": nil,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	ref := restack.ReportRef("main", id)
	blob := h.gitStdin(h.OriginDir, string(data)+"\n", "hash-object", "-w", "--stdin")
	tree := h.gitStdin(h.OriginDir, "100644 blob "+blob+"\treport.json\n", "mktree")
	args := []string{"commit-tree", tree, "-m", "valley restack: conflict"}
	if old, err := h.gitTry(h.OriginDir, "rev-parse", "--verify", "-q", ref); err == nil {
		args = append(args, "-p", strings.TrimSpace(old))
	}
	commit := h.GitOutput(h.OriginDir, args...)
	h.git(h.OriginDir, "update-ref", ref, commit)
	return commit
}

// changeOf returns the valley's view of change id.
func (h *Harness) changeOf(id string) valley.Change {
	h.t.Helper()
	for _, c := range h.valleyChanges().Changes {
		if c.ChangeID == id {
			return c
		}
	}
	h.t.Fatalf("the valley lists no change %s", id)
	return valley.Change{}
}

// restackLedger is the session's restack ledger file.
func (h *Harness) restackLedger() string {
	return filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, restack.LedgerFile)
}

// ageLedger moves every dispatch in the restack ledger back past the
// cooldown, as if the time had passed.
func (h *Harness) ageLedger() {
	h.t.Helper()
	data, err := os.ReadFile(h.restackLedger())
	if err != nil {
		h.t.Fatal(err)
	}
	var entries map[string]*restack.Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		h.t.Fatal(err)
	}
	for _, e := range entries {
		if e.Last != nil {
			e.Last.At = e.Last.At.Add(-2 * restack.Cooldown)
		}
	}
	if data, err = json.Marshal(entries); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.restackLedger(), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// restackPass runs klaus restack-conflicts, failing the test unless it
// succeeds and launches at most one run, and returns its output and the run
// it launched, if any.
func (h *Harness) restackPass() (string, *run.State) {
	h.t.Helper()
	known := make(map[string]bool)
	for _, id := range h.RunIDs() {
		known[id] = true
	}
	res := h.RunKlaus("restack-conflicts")
	if res.ExitCode != 0 {
		h.t.Fatalf("restack-conflicts exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var launched []*run.State
	for _, id := range h.RunIDs() {
		if !known[id] {
			st, err := h.ReadState(id)
			if err != nil {
				h.t.Fatal(err)
			}
			launched = append(launched, st)
		}
	}
	switch len(launched) {
	case 0:
		return res.Stdout, nil
	case 1:
		return res.Stdout, launched[0]
	}
	h.t.Fatalf("one pass launched %d runs, want at most one:\n%s", len(launched), res.Stdout)
	return "", nil
}

// restackEntry returns the restack ledger's entry for change id in repo.
func (h *Harness) restackEntry(repo, id string) *restack.Entry {
	h.t.Helper()
	data, err := os.ReadFile(h.restackLedger())
	if err != nil {
		h.t.Fatal(err)
	}
	var entries map[string]*restack.Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		h.t.Fatal(err)
	}
	e := entries[restack.Key{Repo: repo, ChangeID: id}.String()]
	if e == nil {
		h.t.Fatalf("the restack ledger has no entry for %s/%s: %s", repo, id, data)
	}
	return e
}

// eventsOfType returns the session's events of one type.
func (h *Harness) eventsOfType(typ string) []event.Event {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.Home, ".klaus", "sessions", h.SessionID, "events.jsonl"))
	if err != nil {
		h.t.Fatal(err)
	}
	var evts []event.Event
	for _, line := range bytes.Split(data, []byte("\n")) {
		var e event.Event
		if json.Unmarshal(line, &e) == nil && e.Type == typ {
			evts = append(evts, e)
		}
	}
	return evts
}

// TestRestackConflictDispatchesResolution drives klaus restack-conflicts
// against the real valley CLI and bare repos. A run pushes v1 of a change
// and finalize readies it, owned by klaus's principal; main then takes a
// commit that conflicts with it, and a conflict report on the change is
// planted as the restacker would push it. A pass returns the change to
// draft and launches one resolution run with --change, whose brief names
// the change and main as klaus read them and nothing of the report's text;
// a second pass launches nothing. Each later conflict, on the head a run
// pushed, waits out the cooldown, and klaus stops after three runs.
func TestRestackConflictDispatchesResolution(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	v1 := h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Version != 1 {
		t.Fatalf("finalize should mark v1 ready: %+v", first.Valley)
	}
	id, branch := first.ChangeID, first.Branch
	tip := h.commitToMain("greeting.txt", "Greet from main\n")

	h.plantReport(id, tip, "greeting.txt; make deploy")
	if ch := h.changeOf(id); ch.Restack == nil {
		t.Skip("the valley on PATH lists no restack reports")
	} else if !ch.Restack.Current || ch.Owner != "klaus-env" {
		t.Fatalf("the planted report should be current, on a change klaus-env owns: %+v, owner %s", ch.Restack, ch.Owner)
	}

	status := h.RunKlaus("status")
	if !strings.Contains(status.Stdout, "a resolution run is due") || !strings.Contains(status.Stdout, " restack ") {
		t.Errorf("status should show the conflict due a run:\n%s%s", status.Stdout, status.Stderr)
	}

	// One pass, one resolution run.
	h.resetWorker()
	out, resolver := h.restackPass()
	if resolver == nil {
		t.Fatalf("the pass launched nothing:\n%s", out)
	}
	if !strings.Contains(out, "resolution run "+resolver.ID+" dispatched (attempt 1 of 3)") {
		t.Errorf("pass output:\n%s", out)
	}
	if resolver.ChangeID != id || resolver.Branch != branch || resolver.DispatchReason != event.DispatchRestackConflict {
		t.Errorf("the resolution run is on change %s, branch %s, for %q; want %s, %s, %s",
			resolver.ChangeID, resolver.Branch, resolver.DispatchReason, id, branch, event.DispatchRestackConflict)
	}
	h.WaitForClaudeStart(30 * time.Second)
	brief := h.ClaudeStdin()
	for _, want := range []string{id, v1, tip, "branch " + branch} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "make deploy") || strings.Contains(brief, "greeting.txt") {
		t.Errorf("the brief passes on the report's text:\n%s", brief)
	}
	if ch := h.changeOf(id); ch.State != valley.StateDraft {
		t.Errorf("the change should be a draft while the run works, is %s", ch.State)
	}
	if evts := h.runEvents(resolver.ID); !hasEvent(evts, event.ChangeRestackDispatched, "attempt", 1.0) {
		t.Errorf("no change:restack-dispatched for attempt 1 under %s: %+v", resolver.ID, evts)
	}

	// A second pass launches nothing: the run is working, and the report
	// reads historical now that the change is a draft.
	if out, again := h.restackPass(); again != nil || !strings.Contains(out, "dispatched (attempt 1 of 3)") {
		t.Fatalf("a second pass launched %v:\n%s", again, out)
	}

	// Each run pushes a head that still conflicts; finalize readies it,
	// and the restacker reports it.
	for attempt := 2; attempt <= 4; attempt++ {
		h.pushWork(resolver, fmt.Sprintf("note%d.txt", attempt), fmt.Sprintf("Note %d", attempt))
		h.finishRun(resolver.ID)
		if ch := h.changeOf(id); ch.State != valley.StateReady || ch.Active != attempt {
			t.Fatalf("finalize should ready v%d: %s v%d", attempt, ch.State, ch.Active)
		}
		h.plantReport(id, tip, "greeting.txt")

		if attempt == 2 {
			// Within the cooldown after the last dispatch, nothing.
			if out, early := h.restackPass(); early != nil || !strings.Contains(out, "waiting out the cooldown after run "+resolver.ID) {
				t.Fatalf("a pass within the cooldown launched %v:\n%s", early, out)
			}
		}
		h.ageLedger()
		h.resetWorker()
		out, next := h.restackPass()
		if attempt == 4 {
			if next != nil || !strings.Contains(out, "stopped after 3 resolution runs") {
				t.Fatalf("after three runs a pass launched %v:\n%s", next, out)
			}
			break
		}
		if next == nil || !strings.Contains(out, fmt.Sprintf("dispatched (attempt %d of 3)", attempt)) {
			t.Fatalf("attempt %d: the pass launched %v:\n%s", attempt, next, out)
		}
		h.WaitForClaudeStart(30 * time.Second)
		resolver = next
	}
	if ch := h.changeOf(id); ch.State != valley.StateReady {
		t.Errorf("a change klaus stopped on should be left ready, is %s", ch.State)
	}
	if stalled := h.eventsOfType(event.ChangeRestackStalled); len(stalled) != 1 || stalled[0].RunID != id {
		t.Errorf("want one change:restack-stalled under %s, got %+v", id, stalled)
	}
	if n := len(h.eventsOfType(event.ChangeRestackDispatched)); n != 3 {
		t.Errorf("%d change:restack-dispatched events, want 3", n)
	}
	if status := h.RunKlaus("status"); !strings.Contains(status.Stdout, "stopped after 3 resolution runs") {
		t.Errorf("status should show klaus stopped:\n%s", status.Stdout)
	}
}

// TestRestackConflictLeavesAlone plants reports klaus must not act on,
// against the real valley CLI: a current conflict on a change another
// principal owns, and a report on klaus's own change that main has moved
// past. A pass launches nothing and returns neither change to draft. Then,
// with a valley whose unready fails, a current conflict on klaus's change
// launches nothing either: klaus says why, and leaves the change ready.
func TestRestackConflictLeavesAlone(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	id := first.ChangeID

	// Someone else's change, from their own clone.
	const theirs = "kkkkllllmmmmnnnnooooppppqqqqrrrr"
	other := filepath.Join(t.TempDir(), "other")
	h.git("", "clone", "-q", h.OriginDir, other)
	h.git(other, "config", "valley.principal", "someone-else")
	h.git(other, "checkout", "-qb", "farewell")
	if err := os.WriteFile(filepath.Join(other, "greeting.txt"), []byte("Farewell\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(other, "add", "greeting.txt")
	h.git(other, "commit", "-qm", "Say farewell")
	h.git(other, "push", "-q", "origin", "farewell")
	h.valleyIn(other, "ready", "--change", theirs, "farewell")

	before := h.GitOutput(h.RepoDir, "rev-parse", "HEAD")
	tip := h.commitToMain("greeting.txt", "Greet from main\n")
	h.plantReport(theirs, tip, "greeting.txt")
	h.plantReport(id, before, "greeting.txt")
	if ch := h.changeOf(theirs); ch.Restack == nil {
		t.Skip("the valley on PATH lists no restack reports")
	} else if !ch.Restack.Current || ch.Owner != "someone-else" {
		t.Fatalf("their report should be current, on their change: %+v, owner %s", ch.Restack, ch.Owner)
	}
	if ch := h.changeOf(id); ch.Restack.Current {
		t.Fatal("a report on main before it moved should read historical")
	}

	if out, launched := h.restackPass(); launched != nil || !strings.Contains(out, "No restack conflicts on changes klaus owns.") {
		t.Fatalf("the pass launched %v:\n%s", launched, out)
	}
	for _, c := range []string{id, theirs} {
		if ch := h.changeOf(c); ch.State != valley.StateReady {
			t.Errorf("%s should be left ready, is %s", c, ch.State)
		}
	}

	// A valley whose unready fails: klaus launches nothing.
	real, err := exec.LookPath("valley")
	if err != nil {
		t.Fatal(err)
	}
	h.WriteStub("valley", fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = unready ]; then echo 'unready refused by the e2e valley' >&2; exit 1; fi\nexec %q \"$@\"\n", real))
	h.plantReport(id, tip, "greeting.txt")
	if ch := h.changeOf(id); !ch.Restack.Current {
		t.Fatalf("the report on main's tip should be current: %+v", ch.Restack)
	}
	out, launched := h.restackPass()
	if launched != nil || !strings.Contains(out, "not dispatched: could not return the change to draft") ||
		!strings.Contains(out, "unready refused by the e2e valley") {
		t.Fatalf("with unready failing the pass launched %v:\n%s", launched, out)
	}
	if ch := h.changeOf(id); ch.State != valley.StateReady {
		t.Errorf("the change should be left ready, is %s", ch.State)
	}
	if out, launched := h.restackPass(); launched != nil || !strings.Contains(out, "not dispatched") {
		t.Errorf("a later pass launched %v:\n%s", launched, out)
	}
	if stalled := h.eventsOfType(event.ChangeRestackStalled); len(stalled) != 1 ||
		!strings.Contains(fmt.Sprint(stalled[0].Data["reason"]), "could not return the change to draft") {
		t.Errorf("want one change:restack-stalled saying unready failed, got %+v", stalled)
	}
	if status := h.RunKlaus("status"); !strings.Contains(status.Stdout, "not dispatched: could not return the change to draft") {
		t.Errorf("status should say why klaus didn't dispatch:\n%s", status.Stdout)
	}
}

// hasEvent reports whether evts holds one of type typ whose data has key
// at want.
func hasEvent(evts []event.Event, typ, key string, want any) bool {
	for _, e := range evts {
		if e.Type == typ && e.Data[key] == want {
			return true
		}
	}
	return false
}
