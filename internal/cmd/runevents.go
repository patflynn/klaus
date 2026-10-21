package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// runIDEnv names the run whose pane a process runs in. Launch exports it in
// the agent's pane, and on a sandbox host in the agent's command, so
// _format-stream and the klaus commands the agent runs (_pre-review) put
// their events on the run.
const runIDEnv = "KLAUS_RUN_ID"

// runEventsEnv, set to runEventsOutput, tells the klaus commands in a run
// that they can't reach its event log, as on a sandbox host, so they report
// their events in their output (stream.ReportPrefix). The agent's stream
// carries the output back to the pane's _format-stream, which records them.
const (
	runEventsEnv    = "KLAUS_RUN_EVENTS"
	runEventsOutput = "output"
)

// runEnvPrefix exports runIDEnv for a pane command.
func runEnvPrefix(id string) string {
	return fmt.Sprintf("export %s=%s; ", runIDEnv, shellQuote(id))
}

// remoteRunEnvPrefix exports runIDEnv for a command on a sandbox host, with
// runEventsEnv set so its klaus commands report in their output.
func remoteRunEnvPrefix(id string) string {
	return fmt.Sprintf("export %s=%s %s=%s; ", runIDEnv, shellQuote(id), runEventsEnv, runEventsOutput)
}

// runFields returns the fields that say what a run worked on and why, which
// every run event carries:
//
//   - repo_key: the repo, qualified: owner/repo on GitHub, valley:<name> for
//     a repo that delivers to a valley, else the repo's short name
//   - change_id and, once the run has one, pr_number
//   - key: the change, qualified by repo_key: <repo_key>#<pr_number> once
//     the run has a PR, else <repo_key>@<change_id>
//   - dispatch_reason: why the run was launched (event.Dispatch*)
//   - head_sha: the run's head where known: the branch tip finalize found on
//     origin, else the worktree's HEAD when the agent finished, else when it
//     started
//
// A field that isn't known is left out.
func runFields(s *run.State) map[string]interface{} {
	f := map[string]interface{}{}
	repo, pr := runRepoKey(s), runPRNumber(s)
	put := func(k, v string) {
		if v != "" {
			f[k] = v
		}
	}
	put("repo_key", repo)
	put("change_id", s.ChangeID)
	put("pr_number", pr)
	put("key", changeKey(repo, pr, s.ChangeID))
	put("dispatch_reason", s.DispatchReason)
	if s.PushedSHA != nil && *s.PushedSHA != "" {
		put("head_sha", *s.PushedSHA)
	} else {
		put("head_sha", s.HeadSHA)
	}
	return f
}

// withRunFields returns data with s's runFields added; data's own fields
// win.
func withRunFields(s *run.State, data map[string]interface{}) map[string]interface{} {
	out := runFields(s)
	for k, v := range data {
		out[k] = v
	}
	return out
}

// emitRunEvent emits an event of s's run carrying its runFields.
func emitRunEvent(baseDir string, s *run.State, eventType string, data map[string]interface{}) {
	emitEvent(baseDir, s.ID, eventType, withRunFields(s, data))
}

// changeKey names a change across repos: <repo>#<pr> for a PR, else
// <repo>@<change id>. It is "" without a repo, or without either.
func changeKey(repo, pr, changeID string) string {
	switch {
	case repo == "":
		return ""
	case pr != "":
		return repo + "#" + pr
	case changeID != "":
		return repo + "@" + changeID
	}
	return ""
}

// runPRNumber returns the number of s's PR, or "".
func runPRNumber(s *run.State) string {
	if s.PR != nil && *s.PR != "" {
		return *s.PR
	}
	if s.PRURL != nil {
		return extractPRNumberFromURL(*s.PRURL)
	}
	return ""
}

// runRepoKey names s's repo for its events: the repo of its PR, the valley
// finalize or launch talked to, else what launch recorded (RepoKey). State
// from before launch recorded it gets its target's owner/repo or short name.
func runRepoKey(s *run.State) string {
	if s.PRURL != nil {
		if slug := github.OwnerRepoFromPRURL(*s.PRURL); slug != "" {
			return slug
		}
	}
	switch {
	case s.Valley != nil && s.Valley.Repo != "":
		return "valley:" + s.Valley.Repo
	case s.Unready != nil && s.Unready.Repo != "":
		return "valley:" + s.Unready.Repo
	case s.RepoKey != "":
		return s.RepoKey
	}
	if s.TargetRepo != nil {
		if slug := repoSlug(*s.TargetRepo); slug != "" {
			return slug
		}
	}
	return integration.RepoName(s)
}

// launchRepoKey names a new run's repo for its events (State.RepoKey): the
// GitHub owner/repo of a run that delivers by PR, valley:<name> for a run
// whose starting head carries a valley's policy, else the repo's short name.
func launchRepoKey(ctx context.Context, s *run.State, repoRoot string) string {
	if key := runRepoKey(&run.State{PRURL: s.PRURL, Unready: s.Unready}); key != "" {
		return key
	}
	if s.Delivery != run.DeliveryPush {
		if slug := repoSlugForDir(repoRoot); slug != "" {
			return slug
		}
	}
	name := integration.RepoName(s)
	if name == "" {
		name = repoShortName(gitRemoteURL(repoRoot))
	}
	if name == "" {
		name = filepath.Base(repoRoot)
	}
	if s.Delivery == run.DeliveryPush && s.HeadSHA != "" && valley.HasPolicy(ctx, s.Worktree, s.HeadSHA) {
		return "valley:" + name
	}
	return name
}

// dirRepoKey names the repo checked out at dir as runFields' repo_key does,
// from its origin: owner/repo on GitHub, valley:<name> when its HEAD carries
// a valley's policy, else the origin's short name. It is "" when dir has no
// origin.
func dirRepoKey(ctx context.Context, dir string) string {
	origin := gitRemoteURL(dir)
	if origin == "" {
		return ""
	}
	if git.IsGitHubRemote(origin) {
		if slug := repoSlug(origin); slug != "" {
			return slug
		}
	}
	name := repoShortName(origin)
	if valley.HasPolicy(ctx, dir, "HEAD") {
		return "valley:" + name
	}
	return name
}

// repoShortName is the last path component of a remote URL, without .git.
func repoShortName(remote string) string {
	name := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// gitHead returns the commit HEAD names in dir, or "".
func gitHead(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dispatchReasonRe is what launch --reason accepts.
var dispatchReasonRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// checkDispatchReason rejects a --reason launch doesn't accept; "" is none.
func checkDispatchReason(flag string) error {
	if flag != "" && !dispatchReasonRe.MatchString(flag) {
		return fmt.Errorf("--reason %q: use lowercase letters, digits and hyphens, such as docs-timer", flag)
	}
	return nil
}

// dispatchReason decides why a launch is happening, for its run and its
// events: --reason when given, else what its flags say.
func dispatchReason(flag string, isPRFix, budgetPaused, onChange bool, resumeFrom string) string {
	switch {
	case flag != "":
		return flag
	case isPRFix && budgetPaused:
		return event.DispatchBudgetResume
	case isPRFix:
		return event.DispatchPRFollowUp
	case onChange:
		return event.DispatchChangeFollowUp
	case resumeFrom != "":
		return event.DispatchResume
	}
	return event.DispatchLaunch
}

// eventTime formats a time for an event field.
func eventTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// phaseData is the data of a phase:completed event for phase, which ran
// from start to end.
func phaseData(phase string, start, end time.Time) map[string]interface{} {
	return map[string]interface{}{
		"phase":       phase,
		"started_at":  eventTime(start),
		"ended_at":    eventTime(end),
		"duration_ms": end.Sub(start).Milliseconds(),
	}
}

// emitAgentPhase ends a finalized run's agent phase, which ran from launch
// starting its pane (CreatedAt) to finalize starting at end, with the
// agent's cost when known, and starts the finalize phase.
func emitAgentPhase(baseDir string, s *run.State, end time.Time) {
	if start, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
		data := phaseData(event.PhaseAgent, start, end)
		if s.Backend != "" {
			data["backend"] = s.Backend
		}
		if s.CostUSD != nil {
			data["cost_usd"] = *s.CostUSD
		}
		emitRunEvent(baseDir, s, event.PhaseCompleted, data)
	}
	emitRunEvent(baseDir, s, event.PhaseStarted, map[string]interface{}{
		"phase":      event.PhaseFinalize,
		"started_at": eventTime(end),
	})
}

// paneRunSaveWait bounds how long a pane's process, finishing with events
// for its run, waits for launch to save the run's state. Launch saves it just
// after the pane starts, so a fast agent can finish first.
const paneRunSaveWait = 2 * time.Second

// paneRun is the run whose pane this process runs in (runIDEnv).
type paneRun struct {
	baseDir string     // the session's directory
	id      string     // the run's ID
	state   *run.State // nil until launch has saved it
}

// currentPaneRun returns the run whose pane this process runs in, with its
// state if launch has saved it, or nil outside a run's pane.
func currentPaneRun() *paneRun {
	id := os.Getenv(runIDEnv)
	if id == "" {
		return nil
	}
	store, err := sessionStore()
	if err != nil {
		return nil
	}
	hds, ok := store.(*run.HomeDirStore)
	if !ok {
		return nil
	}
	p := &paneRun{baseDir: hds.BaseDir(), id: id}
	p.load()
	return p
}

// load loads p's state if launch has saved it, and reports whether p has it.
func (p *paneRun) load() bool {
	if p.state == nil {
		if s, err := run.NewHomeDirStoreFromPath(p.baseDir).Load(p.id); err == nil {
			p.state = s
		}
	}
	return p.state != nil
}

// awaitState waits up to paneRunSaveWait for launch to save p's state.
func (p *paneRun) awaitState() {
	deadline := time.Now().Add(paneRunSaveWait)
	for !p.load() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// emit emits an event of p's run: with its runFields once p has its state,
// else under its ID alone, so it isn't lost when launch never saves the
// state in time; the report takes the run's fields from its other events.
func (p *paneRun) emit(eventType string, data map[string]interface{}) {
	if p.state != nil {
		emitRunEvent(p.baseDir, p.state, eventType, data)
		return
	}
	emitEvent(p.baseDir, p.id, eventType, data)
}
