package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
)

// TestRecordPushedChange runs recordPushedChange against real git: a clone
// of a bare origin whose branch launch-b holds a change pushed before the
// run started.
func TestRecordPushedChange(t *testing.T) {
	launchChange := strings.Repeat("qz", 16)
	newChange := strings.Repeat("vp", 16)

	setup := func(t *testing.T) (dir, launchHead string, gitc func(...string) string) {
		root := t.TempDir()
		origin, dir := filepath.Join(root, "origin.git"), filepath.Join(root, "clone")
		gitc = func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if out, err := exec.Command("git", "clone", "-q", origin, dir).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		gitc("checkout", "-q", "-b", "main")
		gitc("commit", "-q", "--allow-empty", "-m", "base")
		gitc("push", "-q", "origin", "main")
		gitc("checkout", "-q", "-b", "launch-b")
		commit(gitc, "keys", launchChange)
		gitc("push", "-q", "-u", "origin", "launch-b")
		return dir, gitc("rev-parse", "HEAD"), gitc
	}
	launched := func(dir, launchHead string) *run.State {
		return &run.State{ID: "20261022-1036-d8deeb25", Branch: "launch-b", ChangeID: launchChange, Worktree: dir,
			HeadSHA: launchHead, TookOver: []string{"20261021-2343-2a82bcc5"}, Unready: &run.Unready{Repo: "widget", Handle: "qvzz"}}
	}
	recorded := func(t *testing.T, baseDir string) []map[string]interface{} {
		evts, err := event.NewLog(baseDir).Read()
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		var out []map[string]interface{}
		for _, e := range evts {
			if e.Type == event.AgentChangeRecorded {
				out = append(out, e.Data)
			}
		}
		return out
	}

	t.Run("pushed nothing new", func(t *testing.T) {
		dir, launchHead, _ := setup(t)
		baseDir := t.TempDir()
		st := launched(dir, launchHead)
		recordPushedChange(context.Background(), baseDir, st, launchHead, "main")
		if st.ChangeID != launchChange || st.Branch != "launch-b" || len(st.TookOver) != 1 || st.Unready.ChangeID != "" {
			t.Errorf("launch's values should stay: %+v", st)
		}
		if st.PushedBranch != nil {
			t.Errorf("the run pushed nothing, but recorded %s", *st.PushedBranch)
		}
		if evts := recorded(t, baseDir); len(evts) != 0 {
			t.Errorf("events = %v, want none", evts)
		}
	})

	t.Run("pushed two changes, ending on the new one", func(t *testing.T) {
		dir, launchHead, gitc := setup(t)
		baseDir := t.TempDir()
		// The agent pushes more of the launch change, then a new change of
		// its own from main, where it ends.
		commit(gitc, "more keys", launchChange)
		gitc("push", "-q", "origin", "launch-b")
		launchTip := gitc("rev-parse", "HEAD")
		gitc("switch", "-q", "-c", "registry", "origin/main")
		commit(gitc, "registry", newChange)
		gitc("push", "-q", "-u", "origin", "HEAD")
		final := gitc("rev-parse", "HEAD")

		st := launched(dir, launchHead)
		st.HeadSHA = final
		recordPushedChange(context.Background(), baseDir, st, launchHead, "main")
		if st.ChangeID != newChange || st.Branch != "registry" || deref(st.PushedBranch) != "registry" || deref(st.PushedSHA) != final {
			t.Errorf("recorded change %s on %s, pushed %s@%s; want %s on registry at %s",
				st.ChangeID, st.Branch, deref(st.PushedBranch), deref(st.PushedSHA), newChange, final)
		}
		if st.PushedSubject != "registry" {
			t.Errorf("pushed subject = %q", st.PushedSubject)
		}
		// Launch's takeovers were of its change; its draft stays that change's.
		if len(st.TookOver) != 0 || st.Unready.ChangeID != launchChange || st.UnreadyHandle() != "" {
			t.Errorf("took over %v, unready %+v", st.TookOver, st.Unready)
		}
		evts := recorded(t, baseDir)
		if len(evts) != 1 {
			t.Fatalf("agent:change-recorded events = %v, want one", evts)
		}
		want := fmt.Sprint([]interface{}{
			map[string]interface{}{"change_id": newChange, "branch": "registry", "head": final},
			map[string]interface{}{"change_id": launchChange, "branch": "launch-b", "head": launchTip},
		})
		if got := fmt.Sprint(evts[0]["pushed"]); got != want {
			t.Errorf("pushed = %s, want %s", got, want)
		}
		if evts[0]["launch_change_id"] != launchChange || evts[0]["launch_branch"] != "launch-b" || evts[0]["change_id"] != newChange {
			t.Errorf("event data = %v", evts[0])
		}
	})
}

// commit makes an empty commit with subject and a Change-Id trailer.
func commit(gitc func(...string) string, subject, changeID string) {
	gitc("commit", "-q", "--allow-empty", "-m", subject+"\n\nChange-Id: "+changeID)
}
