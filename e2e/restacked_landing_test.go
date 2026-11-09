//go:build e2e

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/valley"
)

// restackAsHost rebases change id onto origin/main and records the rebased
// head as its next version, as the host's restacker does after a landing:
// `valley rebase` under the restacker's principal rather than klaus's.
func (h *Harness) restackAsHost(id string) {
	h.t.Helper()
	cmd := exec.Command("valley", "rebase", id)
	cmd.Dir = h.RepoDir
	cmd.Env = append(h.env(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=valley.principal", "GIT_CONFIG_VALUE_0=restacker")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("valley rebase %s: %v\n%s", id, err, out.String())
	}
}

// TestRestackedChangeLandsIntegrated drives a change the host restacks
// twice and then lands at the restacked version, with a real launch against
// the real valley CLI and bare repos. The run that readied v1 shows
// integrated in klaus status, its change's dashboard row reads INTEGRATED in
// the done group, and cleanup --done removes the run as finished.
func TestRestackedChangeLandsIntegrated(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Version != 1 || first.Valley.Handle == "" {
		t.Fatalf("finalize should mark v1 ready: %+v", first.Valley)
	}
	id, handle := first.ChangeID, first.Valley.Handle

	// Two other changes land, and the host restacks this one after each.
	h.commitToMain("one.txt", "one\n")
	h.restackAsHost(id)
	h.commitToMain("two.txt", "two\n")
	h.restackAsHost(id)
	ch := h.changeOf(id)
	if ch.Active != 3 || len(ch.Versions) != 3 || ch.Versions[2].Uploader != "restacker" {
		t.Fatalf("the restacker should have recorded v2 and v3: %+v", ch)
	}

	// v3 lands.
	h.git(h.OriginDir, "update-ref", "refs/heads/main", ch.Versions[2].Head)
	if ch := h.changeOf(id); ch.Versions[0].Landing != valley.NotLanded || ch.Versions[2].Landing != valley.Landed {
		t.Fatalf("the valley should say v3 landed and v1 didn't: %+v", ch.Versions)
	}

	res := h.RunKlaus("status")
	if got := statusOf(t, res.Stdout, first.ID); got != "integrated" {
		t.Errorf("the run that readied v1 shows %q, want integrated:\n%s%s", got, res.Stdout, res.Stderr)
	}

	_, screen := h.startDashboard(170)
	s := screen("1 done", "INTEGRATED")
	if row := rowOf(t, s, handle); !strings.Contains(row, "INTEGRATED") || !strings.Contains(row, "v3@") {
		t.Errorf("the change's row should read INTEGRATED at v3: %q", row)
	}

	res = h.RunKlaus("cleanup", "--done")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "removed "+first.ID+"  ") {
		t.Errorf("cleanup --done should remove the run of the landed change (exit %d):\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// landOnMain lands head on origin's main and keeps main's content: main
// becomes a commit on head alone holding head's tree merged with main's, so
// head is on main's first-parent history with no merge above it, and the
// valley reads head's version landed. The integrator's own landing, a commit
// with a signed transfer statement, needs the integrator's key.
func (h *Harness) landOnMain(head string) {
	h.t.Helper()
	tree := h.GitOutput(h.OriginDir, "merge-tree", "--write-tree", head, "main")
	tip := h.GitOutput(h.OriginDir, "commit-tree", tree, "-p", head, "-m", "land "+head)
	h.git(h.OriginDir, "update-ref", "refs/heads/main", tip)
}

// TestChangeLandedBehindItsActiveVersionIntegrated drives a change whose
// approved restack lands after the host recorded another, with real
// launches against the real valley CLI and bare repos. Runs ready v1 and
// v2, the host restacks the change as v3, v3 is approved, another change
// lands and the host restacks v3 as v4, and then v3's request lands. The
// valley counts the change landed and `valley pending` leaves it out; both
// runs show integrated in klaus status, the change's row reads INTEGRATED
// in the done group at v4, and cleanup --done removes both runs.
func TestChangeLandedBehindItsActiveVersionIntegrated(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Version != 1 || first.Valley.Handle == "" {
		t.Fatalf("finalize should mark v1 ready: %+v", first.Valley)
	}
	id, handle := first.ChangeID, first.Valley.Handle

	time.Sleep(time.Second) // the later run is created in a later second
	h.resetWorker()
	later, _ := h.launchRun("--change", handle, "tidy the greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(later, "greeting.txt", "Tidy the greeting")
	later = h.finishRun(later.ID)
	if later.Valley == nil || later.Valley.Version != 2 || later.ChangeID != id {
		t.Fatalf("finalize should mark the later head ready as v2 of %s: %+v", id, later.Valley)
	}

	// The host restacks the change as v3 after another lands, and v3 is
	// approved: its integration request names v3's head.
	h.commitToMain("one.txt", "one\n")
	h.restackAsHost(id)
	v3 := h.changeOf(id).Versions[2]
	h.git(h.OriginDir, "update-ref", "refs/the-valley/integration-requests/main/c-"+id, v3.Head)

	// Another change lands before v3 does, and the host restacks v3 as v4.
	h.commitToMain("two.txt", "two\n")
	h.restackAsHost(id)
	if ch := h.changeOf(id); ch.Active != 4 || ch.Versions[2].Request == nil || ch.Versions[3].Uploader != "restacker" {
		t.Fatalf("the restacker should have recorded v4 after v3 was approved: %+v", ch)
	}

	h.landOnMain(v3.Head)
	ch := h.changeOf(id)
	if ch.State != valley.StateReady || ch.Active != 4 || ch.Versions[2].Landing != valley.Landed || ch.Versions[3].Landing != valley.NotLanded {
		t.Fatalf("the valley should list the change ready at v4, with v3 landed and v4 not: %+v", ch)
	}
	if out := h.valleyIn(h.RepoDir, "pending", "--json"); strings.Contains(out, id) {
		t.Fatalf("valley pending should count the change landed and leave it out:\n%s", out)
	}

	res := h.RunKlaus("status")
	for _, r := range []string{first.ID, later.ID} {
		if got := statusOf(t, res.Stdout, r); got != "integrated" {
			t.Errorf("%s shows %q, want integrated:\n%s%s", r, got, res.Stdout, res.Stderr)
		}
	}

	_, screen := h.startDashboard(170)
	s := screen("1 done", "INTEGRATED")
	if row := rowOf(t, s, handle); !strings.Contains(row, "INTEGRATED") || !strings.Contains(row, "v4@") {
		t.Errorf("the change's row should read INTEGRATED at v4: %q", row)
	}

	res = h.RunKlaus("cleanup", "--done")
	for _, r := range []string{first.ID, later.ID} {
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, "removed "+r+"  ") {
			t.Errorf("cleanup --done should remove %s, a run of the landed change (exit %d):\n%s%s", r, res.ExitCode, res.Stdout, res.Stderr)
		}
	}
}

// TestOlderVersionUntilTheChangeLands drives a change a later run gives a
// newer version, with real launches against the real valley CLI and bare
// repos. Until the change lands, the run that readied v1 shows
// older-version and the later run awaiting-approval; once the change is
// withdrawn, both show withdrawn.
func TestOlderVersionUntilTheChangeLands(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.useRealValley()
	h.joinValley()
	h.WriteDefaultClaudeOutput(`{"type":"result","subtype":"success","total_cost_usd":0.5,"duration_ms":1000,"session_id":"` + changeSession + `"}` + "\n")

	first, _ := h.launchRun("add a greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(first, "greeting.txt", "Add a greeting")
	first = h.finishRun(first.ID)
	if first.Valley == nil || first.Valley.Version != 1 {
		t.Fatalf("finalize should mark v1 ready: %+v", first.Valley)
	}

	time.Sleep(time.Second) // the later run is created in a later second
	h.resetWorker()
	later, _ := h.launchRun("--change", first.Valley.Handle, "tidy the greeting")
	h.WaitForClaudeStart(30 * time.Second)
	h.pushWork(later, "greeting.txt", "Tidy the greeting")
	later = h.finishRun(later.ID)
	if later.Valley == nil || later.Valley.Version != 2 || later.ChangeID != first.ChangeID {
		t.Fatalf("finalize should mark the later head ready as v2 of %s: %+v", first.ChangeID, later.Valley)
	}

	res := h.RunKlaus("status")
	for id, want := range map[string]string{first.ID: "older-version", later.ID: "awaiting-approval"} {
		if got := statusOf(t, res.Stdout, id); got != want {
			t.Errorf("%s shows %q, want %q:\n%s%s", id, got, want, res.Stdout, res.Stderr)
		}
	}

	cmd := exec.Command("valley", "withdraw", first.ChangeID)
	cmd.Dir, cmd.Env = h.RepoDir, h.env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("valley withdraw: %v\n%s", err, out)
	}
	res = h.RunKlaus("status")
	for _, id := range []string{first.ID, later.ID} {
		if got := statusOf(t, res.Stdout, id); got != "withdrawn" {
			t.Errorf("%s shows %q, want withdrawn:\n%s%s", id, got, res.Stdout, res.Stderr)
		}
	}
}
