package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/webhook"
)

// Messages for the bubbletea event loop.

type statesLoadedMsg struct {
	states []*run.State
}

type ghStatusMsg struct {
	statuses map[string]*prStatus // keyed by PR URL
}

// integrationTickMsg starts an integration check of pushed runs.
type integrationTickMsg struct{}

// integrationMsg carries the results of an integration check, by run ID.
type integrationMsg struct {
	results map[string]integration.Result
	changes map[integration.ChangeKey]integration.Result
}

type fsEventMsg struct{}

type tickMsg struct{}

// reconcileTickMsg fires the slow reconcile heartbeat in webhook-only mode.
type reconcileTickMsg struct{}

type sandboxStatusMsg struct {
	hosts map[string]bool // host -> reachable
}

type pipelineActionMsg struct {
	actions []pipeline.Action
}

type errMsg struct {
	err error
}

// errHintMsg carries a transient, non-fatal hint to surface in the dashboard
// (via the recentErrors strip) without triggering the fatal error screen that
// errMsg does. Used by best-effort background actions like opening a browser.
type errHintMsg struct {
	msg string
}

type webhookMsg struct {
	event webhook.Event
}

// webhookClosedMsg signals that the webhook event channel was closed, so live
// webhook consumption has stopped. Surfaced as a non-fatal error in the TUI.
type webhookClosedMsg struct{}

// internalEventMsg delivers a klaus-emitted event (e.g. PRApprovalChanged
// from `klaus approve`) into the dashboard's Update loop. Internal events
// are treated as invalidation signals — the dashboard reacts by re-fetching
// PR status, mirroring how it reacts to a real GitHub webhook.
type internalEventMsg struct {
	event event.Event
}

type trustedCommentsMsg struct {
	prURL                 string
	hasNewTrustedComments bool
}

// prStatus holds the GitHub-fetched status for a single PR.
type prStatus struct {
	PRNumber              string
	State                 string   // OPEN, MERGED, CLOSED, NOACCESS (gh.StateNoAccess) or UNKNOWN
	Draft                 bool     // checked only when the PR is otherwise ready to merge
	CI                    string   // passing, failing, pending, unknown
	Conflicts             string   // yes, none, unknown
	BehindBy              int      // commits base has that head lacks; >0 = stale-but-mergeable
	ReviewDecision        string   // APPROVED, CHANGES_REQUESTED, etc.
	HasNewTrustedComments bool     // unaddressed comments from trusted reviewers
	Labels                []string // applied PR labels (used to surface klaus:budget-paused)
}

// Commands for the bubbletea event loop.

func loadStatesCmd(store run.StateStore) tea.Cmd {
	return func() tea.Msg {
		states, err := store.List()
		if err != nil {
			return errMsg{err: err}
		}
		return statesLoadedMsg{states: states}
	}
}

// noAccessRecheck is how long a PR the agent can't read goes without being
// queried again, so a fixed credential recovers on its own without the
// dashboard asking GitHub about it on every poll.
const noAccessRecheck = 30 * time.Minute

// fetchGHStatusCmd fetches the GitHub status of every PR the states refer
// to, keyed by PR URL so equal PR numbers in different repos don't collide.
// PRs last found unreadable less than noAccessRecheck ago are skipped; their
// NOACCESS status stays in place.
func (m dashboardModel) fetchGHStatusCmd(states []*run.State) tea.Cmd {
	skip := make(map[string]bool)
	now := time.Now()
	for url, at := range m.noAccessAt {
		if now.Sub(at) < noAccessRecheck {
			skip[url] = true
		}
	}
	return fetchGHStatusCmd(m.ghClient, states, skip)
}

func fetchGHStatusCmd(client gh.Client, states []*run.State, skip map[string]bool) tea.Cmd {
	return func() tea.Msg {
		statuses := make(map[string]*prStatus)
		for _, s := range states {
			prRef := extractPRRef(s)
			if prRef == "" || skip[prRef] || statuses[prRef] != nil {
				continue
			}
			statuses[prRef] = fetchPRStatus(client, extractPRNumber(s), prRef)
		}
		return ghStatusMsg{statuses: statuses}
	}
}

// integrationInterval is how often the dashboard checks whether pushed runs
// have landed. It is independent of GitHub polling.
const integrationInterval = time.Minute

// integrationTimeout bounds one integration check.
const integrationTimeout = 5 * time.Minute

// checkIntegrationCmd checks, in the background, the runs that are waiting
// for their pushed branch to be integrated. With a ValleyChecker that is at
// most one `valley changes` for all of them. Then, with a store, it settles
// the knots of runs whose change the check found landed (settleKnots). The
// next check starts only once this one is done, so no two settle a knot at
// once.
func checkIntegrationCmd(checker integration.Checker, states []*run.State, store run.StateStore) tea.Cmd {
	if checker == nil {
		return nil
	}
	var pushed []*run.State
	for _, s := range states {
		if awaitsIntegration(s) {
			cp := *s // the check runs off the Update goroutine
			pushed = append(pushed, &cp)
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
		defer cancel()
		rep := checker.Check(ctx, pushed)
		if store != nil {
			settleKnots(ctx, store, pushed, rep.Runs, rep.Changes)
		}
		return integrationMsg{results: rep.Runs, changes: rep.Changes}
	}
}

// keepValleyViews returns a check's results, except where the valley placed
// a run before and git judged it now (the valley couldn't be asked, or
// couldn't read the repo): unless git found it landed, the run keeps the
// valley's last view, marked Outdated. A valley outage then leaves an
// uncertain or rejected change where it was instead of showing it as ready
// to approve, or as done because its branch went away.
func keepValleyViews(prev, next map[string]integration.Result) map[string]integration.Result {
	for id, r := range next {
		if old, ok := prev[id]; ok && old.Change != nil && r.Change == nil && !r.Verdict.Landed() {
			old.Outdated = true
			next[id] = old
		}
	}
	return next
}

// keepChangeViews does for the valley's views of changes what keepValleyViews
// does for runs': a change the latest check has no view of (the valley
// couldn't be asked, or couldn't read its repo) keeps its last view, marked
// Outdated.
func keepChangeViews(prev, next map[integration.ChangeKey]integration.Result) map[integration.ChangeKey]integration.Result {
	if next == nil {
		next = make(map[integration.ChangeKey]integration.Result)
	}
	for k, old := range prev {
		if _, ok := next[k]; !ok {
			old.Outdated = true
			next[k] = old
		}
	}
	return next
}

func integrationTickAfterCmd() tea.Cmd {
	return tea.Tick(integrationInterval, func(time.Time) tea.Msg {
		return integrationTickMsg{}
	})
}

func waitForWebhookCmd(ch <-chan webhook.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			// The webhook channel was closed. Re-reading it would return
			// immediately forever (a busy-loop), so we do NOT re-arm.
			// Surface a visible (non-fatal) error instead of going silently
			// dark — in webhook-only mode this would otherwise permanently
			// kill all reconcile triggers except the slow heartbeat (see
			// issue #271).
			return webhookClosedMsg{}
		}
		return webhookMsg{event: ev}
	}
}

func waitForInternalEventCmd(ch <-chan event.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return internalEventMsg{event: ev}
	}
}

// shouldInvalidate reports whether an event type should trigger a refetch
// of GitHub PR status. Most klaus events (agent:started, pr:merged, …) are
// pure notifications and either reflect state the dashboard already learned
// from the pipeline FSM, or are observational. Only events that signal an
// external precondition change need an active invalidation.
func shouldInvalidate(eventType string) bool {
	switch eventType {
	case event.PRApprovalChanged:
		return true
	}
	return false
}

// internalEventPRNumber returns the PR number associated with an event,
// or empty if none. The event Data is map[string]interface{}; JSON decodes
// numbers as float64, but events constructed in-process may carry int /
// int64, so all common numeric forms are tolerated.
func internalEventPRNumber(ev event.Event) string {
	if ev.Data == nil {
		return ""
	}
	switch v := ev.Data["pr_number"].(type) {
	case string:
		return v
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
	}
	return ""
}

func tickCmd() tea.Cmd {
	return func() tea.Msg {
		return tickMsg{}
	}
}

func tickAfterCmd() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

// defaultReconcileInterval is the slow reconcile heartbeat interval used in
// webhook-only mode when no override is configured.
const defaultReconcileInterval = 5 * time.Minute

// shouldScheduleReconcile reports whether the slow reconcile heartbeat should
// run. It runs only in webhook mode when polling is NOT active: poll mode
// already re-fetches every 30s (tickMsg), so the heartbeat would be redundant.
// A non-positive interval disables the heartbeat entirely.
func shouldScheduleReconcile(useWebhook, pollEnabled bool, interval time.Duration) bool {
	return useWebhook && !pollEnabled && interval > 0
}

// reconcileInterval resolves the heartbeat interval from config. A zero value
// means "use the default"; a negative value disables the heartbeat.
func reconcileInterval(cfg *config.WebhookConfig) time.Duration {
	if cfg == nil || cfg.ReconcileIntervalSeconds == 0 {
		return defaultReconcileInterval
	}
	if cfg.ReconcileIntervalSeconds < 0 {
		return 0 // disabled
	}
	return time.Duration(cfg.ReconcileIntervalSeconds) * time.Second
}

func reconcileTickAfterCmd(interval time.Duration) tea.Cmd {
	// Defensive guard: tea.Tick fires immediately for a non-positive duration,
	// so re-arming with interval <= 0 (the disabled-heartbeat sentinel) would
	// spin an infinite loop of reconcileTickMsg messages and peg the CPU.
	// Returning nil cleanly disables the heartbeat (bubbletea ignores nil cmds).
	if interval <= 0 {
		return nil
	}
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return reconcileTickMsg{}
	})
}

// startWatcherCmd watches the run state directory and, when jobsDir is set,
// the job records.
func startWatcherCmd(store run.StateStore, jobsDir string) tea.Cmd {
	return func() tea.Msg {
		stateDir := store.StateDir()
		// Ensure the directory exists before watching
		os.MkdirAll(stateDir, 0o755)

		w, err := fsnotify.NewWatcher()
		if err != nil {
			return errMsg{err: fmt.Errorf("creating file watcher: %w", err)}
		}
		if err := w.Add(stateDir); err != nil {
			w.Close()
			return errMsg{err: fmt.Errorf("watching state dir: %w", err)}
		}
		if jobsDir != "" {
			os.MkdirAll(jobsDir, 0o700)
			if err := w.Add(jobsDir); err != nil {
				w.Close()
				return errMsg{err: fmt.Errorf("watching jobs dir: %w", err)}
			}
		}
		return w
	}
}

// watchFSCmd waits for a record to change: a job's in jobsDir (jobsFSMsg),
// else a run's (fsEventMsg).
func watchFSCmd(w *fsnotify.Watcher, jobsDir string) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return nil
				}
				if filepath.Ext(event.Name) != ".json" {
					continue
				}
				if jobsDir != "" && filepath.Dir(event.Name) == filepath.Clean(jobsDir) {
					return jobsFSMsg{}
				}
				return fsEventMsg{}
			case _, ok := <-w.Errors:
				if !ok {
					return nil
				}
				// Ignore watcher errors and keep watching
			}
		}
	}
}

func checkSandboxCmd(states []*run.State) tea.Cmd {
	return func() tea.Msg {
		hosts := make(map[string]bool)
		for _, s := range states {
			if s.Host != nil && *s.Host != "" {
				if _, ok := hosts[*s.Host]; !ok {
					hosts[*s.Host] = CheckSandboxReachable(*s.Host)
				}
			}
		}
		if len(hosts) == 0 {
			return nil
		}
		return sandboxStatusMsg{hosts: hosts}
	}
}

// fetchPRStatus queries GitHub for a single PR's status.
// prRef should be a full PR URL so gh can resolve it from any directory.
func fetchPRStatus(client gh.Client, prNumber, prRef string) *prStatus {
	ctx := context.TODO()
	ps := &prStatus{PRNumber: prNumber}
	ps.State = client.GetState(ctx, prRef)
	if ps.State == "MERGED" || ps.State == "CLOSED" || ps.State == gh.StateNoAccess {
		return ps
	}
	ps.CI = client.GetCI(ctx, prRef)
	ps.Conflicts = client.GetConflicts(ctx, prRef)
	ps.BehindBy = client.GetCommitsBehind(ctx, prRef)
	ps.ReviewDecision = client.GetReviewDecision(ctx, prRef)
	ps.Labels = client.GetLabels(ctx, prRef)
	// A ready PR waits on the operator unless it's a draft; only then does
	// draft-ness matter, so only then is it asked for.
	if computeMergeStatus(ps.CI, ps.Conflicts, ps.ReviewDecision, ps.BehindBy) == "ready" {
		ps.Draft = client.IsDraft(ctx, prRef)
	}

	// When reviewDecision is not CHANGES_REQUESTED, check for unaddressed
	// trusted reviewer comments that GitHub doesn't reflect in reviewDecision.
	if !strings.EqualFold(ps.ReviewDecision, "CHANGES_REQUESTED") &&
		!strings.EqualFold(ps.ReviewDecision, "APPROVED") {
		ownerRepo := gh.OwnerRepoFromPRURL(prRef)
		if ownerRepo != "" {
			ps.HasNewTrustedComments = hasUnaddressedTrustedComments(ownerRepo, prNumber)
		}
	}
	return ps
}
