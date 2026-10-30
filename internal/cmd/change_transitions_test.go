package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// changeSession is a session directory whose dashboards run the real
// integration check, a ValleyChecker over real git, against a fake valley
// serving the listing the test last wrote for repo cosmo.
type changeSession struct {
	*gitFixture
	store   *run.HomeDirStore
	listing string
	seen    int // events next has returned
}

func newChangeSession(t *testing.T) *changeSession {
	t.Helper()
	f := newGitFixture(t)
	bin := t.TempDir()
	listing := filepath.Join(bin, "changes.json")
	script := "#!/bin/sh\nif [ \"$1\" = changes ] && [ -f '" + listing + "' ]; then exec cat '" + listing + "'; fi\n" +
		"echo 'valley: cannot reach classic-laddie' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := run.NewHomeDirStoreFromPath(t.TempDir())
	if err := store.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return &changeSession{gitFixture: f, store: store, listing: listing}
}

// pushed records a run of the session that pushed files on a branch of its
// own as a version of change handle, as finalize does: version 0 is a run
// finalize has recorded the push of but not yet the version it became.
func (c *changeSession) pushed(id, handle string, version int, files ...string) *run.State {
	c.t.Helper()
	s := c.push(id, "work on "+handle, time.Now().Add(-time.Hour), files...)
	s.ChangeID = handleChangeID(handle)
	if version > 0 {
		s.Valley = &run.Valley{Repo: "cosmo", Handle: handle, Version: version, Title: "Tune " + handle}
	}
	if err := c.store.Save(s); err != nil {
		c.t.Fatal(err)
	}
	return s
}

// handleChangeID is a change ID that handle is the start of.
func handleChangeID(handle string) string { return handle + strings.Repeat("k", 32-len(handle)) }

// list sets what the valley lists in repo cosmo.
func (c *changeSession) list(changes ...map[string]any) {
	c.t.Helper()
	data, err := json.Marshal(map[string]any{"schema": valley.SchemaV1, "host": "classic-laddie",
		"repos": []any{map[string]any{"repo": "cosmo", "url": "git@classic-laddie:cosmo.git", "error": nil, "changes": changes}}})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(c.listing, data, 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// dashboard starts a dashboard on the session, as klaus dashboard does, and
// returns what runs one of its integration checks on the session's runs.
func (c *changeSession) dashboard() (check func() integrationMsg) {
	m := newDashboardModel(c.store, config.Config{}, gh.NewGHCLIClient(""))
	m.restackOn = false // no restack reports here; a pass would read every session
	return func() integrationMsg {
		c.t.Helper()
		states, err := c.store.List()
		if err != nil {
			c.t.Fatal(err)
		}
		m.states = states
		_, cmd := m.Update(integrationTickMsg{})
		msg, ok := cmd().(integrationMsg)
		if !ok {
			c.t.Fatalf("the integration check returned %T", msg)
		}
		return msg
	}
}

// next returns the change transition events emitted since it last ran.
func (c *changeSession) next() []event.Event {
	c.t.Helper()
	evts, err := event.NewLog(c.store.BaseDir()).Read()
	if err != nil {
		c.t.Fatal(err)
	}
	var out []event.Event
	for _, e := range evts[c.seen:] {
		if strings.HasPrefix(e.Type, "change:") {
			out = append(out, e)
		}
	}
	c.seen = len(evts)
	return out
}

// expect checks the events since the last expect, each as "<run ID>
// <type> v<version>", and returns them.
func (c *changeSession) expect(step string, want ...string) []event.Event {
	c.t.Helper()
	evts := c.next()
	var got []string
	for _, e := range evts {
		got = append(got, fmt.Sprintf("%s %s v%v", e.RunID, e.Type, e.Data["version"]))
	}
	if !slices.Equal(got, want) {
		c.t.Errorf("%s: events %q, want %q", step, got, want)
	}
	return evts
}

// listed is a valley-changes/v1 version that uploader marked ready.
func listed(n int, head, uploader string) map[string]any {
	return map[string]any{"n": n, "head": head, "title": fmt.Sprintf("Tune the cache (v%d)", n), "branch": "agent/x",
		"uploader": uploader, "landing": "not-landed", "landed_commit": nil, "request": nil, "outcome": nil}
}

// with returns a copy of version v with its fields set from kv pairs.
func with(v map[string]any, kv ...any) map[string]any {
	out := maps.Clone(v)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// approved is version v with an integration request filed.
func approved(v map[string]any) map[string]any {
	return with(v, "request", map[string]any{"ref": "refs/valley/requests/x", "commit": strings.Repeat("e", 40)})
}

// decided is approved version v with the integrator's decision on it.
func decided(v map[string]any, decision string) map[string]any {
	return with(approved(v), "outcome", map[string]any{"decision": decision, "head": v["head"], "tip": strings.Repeat("f", 40)})
}

// landedAs is approved version v landed as commit.
func landedAs(v map[string]any, commit string) map[string]any {
	return with(approved(v), "landing", "landed", "landed_commit", commit)
}

// listedChange is a change record whose active version is its last.
func listedChange(handle, state string, versions ...map[string]any) map[string]any {
	return map[string]any{"change_id": handleChangeID(handle), "handle": handle, "target": "main", "owner": "klaus-env",
		"state": state, "active": versions[len(versions)-1]["n"], "versions": versions, "includes": []string{}}
}

// TestChangeApprovedThenLanded: the operator approving a change, and the
// change landing, each reach the event stream once, with the change's
// handle as the run ID, however many checks and dashboard restarts see it.
func TestChangeApprovedThenLanded(t *testing.T) {
	c := newChangeSession(t)
	s := c.pushed("20261030-0900-0001", "nxvp", 1, "cache.txt")
	v1 := listed(1, *s.PushedSHA, "klaus-env")
	c.list(listedChange("nxvp", "ready", v1))

	check := c.dashboard()
	check()
	c.expect("ready")

	c.list(listedChange("nxvp", "ready", approved(v1)))
	check()
	check()
	c.expect("approved", "nxvp change:approved v1")
	check = c.dashboard()
	check()
	c.expect("approved, the dashboard restarted")

	c.fastForward(*s.PushedSHA) // the integrator lands it
	c.list(listedChange("nxvp", "ready", landedAs(v1, *s.PushedSHA)))
	check()
	evts := c.expect("landed", "nxvp change:integrated v1")
	check()
	c.expect("landed, checked again")
	check = c.dashboard()
	check()
	c.expect("landed, the dashboard restarted")

	if len(evts) == 1 {
		want := map[string]any{"repo": "cosmo", "handle": "nxvp", "change_id": s.ChangeID, "version": 1.0,
			"head": *s.PushedSHA, "title": "Tune the cache (v1)", "landed_commit": *s.PushedSHA,
			"repo_key": "valley:cosmo", "key": "valley:cosmo@" + s.ChangeID}
		if !maps.Equal(evts[0].Data, want) {
			t.Errorf("change:integrated carries %v, want %v", evts[0].Data, want)
		}
	}
}

// TestChangeRestackedTwiceThenLanded: each version the valley's restacker
// records is a change:new-version, and the change landing at the last of
// them is the change's change:integrated, once, though the run's own
// version reads superseded.
func TestChangeRestackedTwiceThenLanded(t *testing.T) {
	c := newChangeSession(t)
	s := c.pushed("20261030-0900-0001", "rstk", 1, "restack.txt")
	v1 := listed(1, *s.PushedSHA, "klaus-env")
	c.list(listedChange("rstk", "ready", v1))
	check := c.dashboard()
	check()
	c.expect("ready")

	c.advanceMain("u1.txt")
	v2 := listed(2, c.rebaseOntoMain(*s.PushedBranch), "restacker")
	c.list(listedChange("rstk", "ready", v1, v2))
	check()
	if evts := c.expect("restacked", "rstk change:new-version v2"); len(evts) == 1 &&
		(evts[0].Data["uploader"] != "restacker" || evts[0].Data["head"] != v2["head"]) {
		t.Errorf("change:new-version should name the version's head and uploader: %v", evts[0].Data)
	}

	c.advanceMain("u2.txt")
	v3 := listed(3, c.rebaseOntoMain(*s.PushedBranch), "restacker")
	c.list(listedChange("rstk", "ready", v1, v2, v3))
	check()
	check()
	c.expect("restacked again", "rstk change:new-version v3")

	c.list(listedChange("rstk", "ready", v1, v2, approved(v3)))
	check()
	c.expect("approved", "rstk change:approved v3")

	c.fastForward(v3["head"].(string))
	c.list(listedChange("rstk", "ready", v1, v2, landedAs(v3, v3["head"].(string))))
	msg := check()
	c.expect("landed", "rstk change:integrated v3")
	if r := msg.results[s.ID]; r.Verdict != integration.Superseded {
		t.Errorf("the run's own version reads %v, want superseded", r.Verdict)
	}
	check()
	c.dashboard()()
	c.expect("landed, checked again and restarted")
}

// TestChangeStaleThenApprovedAgain: an approved version going stale is a
// change:stale; the restacker's next version, and its approval, report
// again; and a version a run of the session pushed is no news, even before
// finalize records the version it became.
func TestChangeStaleThenApprovedAgain(t *testing.T) {
	c := newChangeSession(t)
	s := c.pushed("20261030-0900-0001", "stal", 1, "stale.txt")
	v1 := listed(1, *s.PushedSHA, "klaus-env")
	c.list(listedChange("stal", "ready", v1))
	check := c.dashboard()
	check()
	c.list(listedChange("stal", "ready", approved(v1)))
	check()
	c.expect("approved", "stal change:approved v1")

	c.list(listedChange("stal", "ready", decided(v1, "stale")))
	check()
	check()
	c.expect("stale", "stal change:stale v1")

	c.advanceMain("u1.txt")
	v2 := listed(2, c.rebaseOntoMain(*s.PushedBranch), "restacker")
	c.list(listedChange("stal", "ready", decided(v1, "stale"), v2))
	check()
	c.expect("restacked", "stal change:new-version v2")

	c.list(listedChange("stal", "ready", decided(v1, "stale"), approved(v2)))
	check()
	c.expect("approved again", "stal change:approved v2")

	c.list(listedChange("stal", "ready", decided(v1, "stale"), decided(v2, "rejected")))
	check()
	c.expect("rejected", "stal change:rejected v2")

	followUp := c.pushed("20261030-0900-0002", "stal", 0, "fix.txt")
	v3 := listed(3, *followUp.PushedSHA, "klaus-env")
	c.list(listedChange("stal", "ready", decided(v1, "stale"), decided(v2, "rejected"), v3))
	check()
	c.expect("the session's follow-up pushed a version")
}

// TestChangeWithdrawn: a tracked change being withdrawn is a
// change:withdrawn, once.
func TestChangeWithdrawn(t *testing.T) {
	c := newChangeSession(t)
	head := c.push("20261030-0900-0001", "unused", time.Now(), "w.txt").PushedSHA
	clone := c.clone
	tracked := &run.State{ID: "20261030-0900-0002", Type: "track", ChangeID: handleChangeID("wdrn"), CloneDir: &clone,
		HeadSHA: *head, CreatedAt: time.Now().Format(time.RFC3339),
		Valley: &run.Valley{Repo: "cosmo", Handle: "wdrn", Version: 1, Title: "Drop the shim"}}
	if err := c.store.Save(tracked); err != nil {
		t.Fatal(err)
	}
	v1 := listed(1, *head, "someone")
	c.list(listedChange("wdrn", "ready", v1))
	check := c.dashboard()
	check()
	c.expect("ready")

	c.list(listedChange("wdrn", "withdrawn", v1))
	check()
	check()
	c.expect("withdrawn", "wdrn change:withdrawn v1")
}

// TestChangeFirstStartReportsNoHistory: a session's first look at its
// changes, with no ledger, records where each stands and reports none of
// it, landed, approved, decided, withdrawn or restacked; nor do later
// checks and restarts while nothing moves.
func TestChangeFirstStartReportsNoHistory(t *testing.T) {
	c := newChangeSession(t)
	runOf := func(n int, handle string) map[string]any {
		s := c.pushed(fmt.Sprintf("20261030-0900-000%d", n), handle, 1, handle+".txt")
		return listed(1, *s.PushedSHA, "klaus-env")
	}
	landed := runOf(1, "lndd")
	c.fastForward(landed["head"].(string))
	approvedV := runOf(2, "appr")
	staleV := runOf(3, "stal")
	withdrawn := runOf(4, "wdrn")
	restacked := runOf(5, "rstk")
	c.list(
		listedChange("lndd", "ready", landedAs(landed, landed["head"].(string))),
		listedChange("appr", "ready", approved(approvedV)),
		listedChange("stal", "ready", decided(staleV, "stale")),
		listedChange("wdrn", "withdrawn", withdrawn),
		listedChange("rstk", "ready", restacked, listed(2, strings.Repeat("a", 40), "restacker")),
	)

	check := c.dashboard()
	check()
	c.expect("first start")
	if _, err := os.Stat(filepath.Join(c.store.BaseDir(), pipeline.ChangeLedgerFile)); err != nil {
		t.Errorf("the first start should record where the changes stand: %v", err)
	}
	check()
	c.dashboard()()
	c.expect("checked again and restarted")
}
