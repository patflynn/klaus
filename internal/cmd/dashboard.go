package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/patflynn/klaus/internal/webhook"
	"github.com/spf13/cobra"
)

// dashboardError holds a pipeline error for TUI display.
type dashboardError struct {
	Time    time.Time
	Message string
}

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Live TUI dashboard for monitoring agents and PRs",
	Long: `Shows a persistent, auto-refreshing terminal UI that monitors all active
agent runs and their PR statuses: CI status, merge conflicts, and review
decisions.

Local state updates instantly via filesystem watching.
GitHub state (CI, conflicts, reviews) polls every 30 seconds by default.

When webhook config is present in .klaus/config.json, the dashboard starts
an HTTP server to receive push events from github-relay instead of polling.
Set "webhook": {"port": 9800} in config to enable.

In webhook-only mode (poll_fallback false), a slow reconcile heartbeat runs
a full status re-fetch every 5 minutes by default (configurable via
"reconcile_interval_seconds") so a dropped webhook can't strand a PR forever.

Runs are listed by what they work on: the runs on a PR share its row, and
the runs without a PR on one change (the same change ID in the same repo)
share the change's row; a run with neither has a row of its own. Rows are
grouped by whose move it is, most recent first within each group:
  yours     needs you: needs attention (ATTN), a valley change to approve
            (APPROVE) or to look at (STALE, REJECTED, FAILED, UNCERTAIN,
            DRAFT), a pushed branch to submit (SUBMIT), or a PR that is ready
            to merge and not yet approved
  agents    an agent is running on it: a fix agent on a PR, a follow-up
            (--resume-from, --pr or a pipeline fix) on a change
  pipeline  an open PR the pipeline is handling: CI running or failing,
            conflicts, behind, approved and waiting to merge; or a valley
            change that is approved and waiting on the integrator (APPROVED)
  done      merged, closed, integrated, gone, withdrawn, no access, or
            exited
The line above the list counts each group (e.g. "2 yours") and shows the
scroll position (e.g. "12–30 of 34").

A pushed branch (a run that finished without a PR) shows SUBMIT until its
work lands on the default branch. Every minute the dashboard checks the run's
clone against origin: INTEGRATED means the pushed commit, or the branch's
current tip, is on the default branch; INTEGRATED~ means a commit on the
default branch carries the same change (equal patch-id), which is strong
evidence rather than proof; GONE means the branch was deleted without
landing. A PR the agent can no longer read on GitHub shows NO ACCESS and is
re-checked every 30 minutes.

A run whose repo delivers to a valley shows as its change: the repo (always
shown), the change's handle and its title, then the version and head (e.g.
v2@1a2b3c4). Finalize marked the pushed head ready. Every minute, one
"valley changes --all --json" for all such runs says where each stands:
APPROVE (ready and waiting for you), APPROVED, INTEGRATED (landed),
UNCERTAIN (no landing evidence: look), STALE, REJECTED or FAILED (the
integrator's decision), WITHDRAWN, or DRAFT (returned to draft, as launch
does when a follow-up run continues the change, and no run of it is
running). If valley
can't be asked, the git check above decides, and a change waiting on it
reads APPROVE. A run whose change finalize couldn't mark ready shows SUBMIT
and the reason.

A change's row is built like a PR's. It shows the valley's handle, else the
first 8 characters of the change ID; the valley's title, else the subject
of the head its latest run pushed; and where the valley says the change's
active version stands, with that version's head to approve, wherever it
was marked ready (for a change no valley lists, where its latest push
stands). Earlier versions fold into the row. A newer push the valley
doesn't hold as a version, as when its ready failed, doesn't take the row
over: the footer notes it ("a newer push 1a2b3c4 isn't ready"). While an agent runs on a PR or a change, the row names it (e.g.
"agent:1a2b running"), sorts with the agents and offers no approve command,
and the footer says which agent is working on it. Once it exits, the row
goes back to where the PR or change stands.

Done rows older than an hour are hidden until you press h; the line above the
list says how many are hidden. A row the dashboard watched finish stays for
the full hour. Set "dashboard": {"hide_finished_after_minutes": N} in config
to change the threshold, or a negative N to never hide them.

Set "dashboard": {"approve_command": "cd ~/hack/{repo} && valley approve {handle} --head {head}"}
to show, in the footer, the command that approves the selected APPROVE row
(or submits a SUBMIT row). {repo} is the repo as the row names it, {branch}
the pushed branch, {change} the full change ID, {handle} the change's handle
and {head} the pushed head, so the approval binds the head you saw. A row
with no value for a placeholder the template uses, such as {handle} on a
branch no valley knows, shows no command, and neither does a DRAFT row or
one with an agent running on it. The command is for you to run on your own
machine; the dashboard never runs it. "submit_command" is the
deprecated name of "approve_command", read only when approve_command is
unset.

Every 30 seconds the dashboard resolves klaus on PATH, symlinks and all.
When it names a different file than the running binary, and that file runs
--version, as after klaus self-update, the footer says "klaus updated to
<version>; restarting". Once no key has been pressed for 10 seconds, the
dashboard shuts down as on q, releasing its webhook port, and execs the new
klaus with the same arguments and environment. If the exec fails, it keeps
running on the old klaus, shows the error, and leaves that file alone.

Keyboard shortcuts:
  j / k or ↑ / ↓  move the selection
  PgUp / PgDn     move a page
  g / G           jump to the top / bottom (also Home / End)
  h               show all runs / hide old done rows
  a               approve the selected PR
  d               discuss the selected PR with the coordinator
  o               open the selected PR in a browser
  r               force refresh
  q               quit`,
	RunE: func(cmd *cobra.Command, args []string) error {
		bin := newBinCheck()
		var notice string
		for {
			next, err := runDashboard(cmd, bin, notice)
			if next == nil {
				return err
			}
			// The dashboard has shut down and released its port: become the
			// new klaus, with the same arguments and environment.
			err = syscall.Exec(next.path, os.Args, os.Environ())
			// Still here: keep running on this klaus, and leave that file be.
			bin.failed[next.path] = true
			notice = fmt.Sprintf("klaus updated to %s, but restarting onto it failed: %v", next.version, err)
		}
	},
}

// runDashboard runs the dashboard until it quits. It returns the klaus to
// restart onto when it quit for that, once everything is shut down. notice,
// if set, starts in the footer as an error.
func runDashboard(cmd *cobra.Command, bin *binCheck, notice string) (*binUpdateMsg, error) {
	{
		store, err := sessionStore()
		if err != nil {
			return nil, fmt.Errorf("KLAUS_SESSION_ID not set; run inside a klaus session")
		}

		// Load config once — used for both the dashboard model and webhook setup.
		repoRoot, _ := git.RepoRoot()
		cfg, err := config.Load(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: loading config: %v\n", err)
		}

		// Shared context for coordinating graceful shutdown of all subsystems.
		ctx, cancel := context.WithCancel(cmd.Context())

		ghClient := gh.NewGHCLIClient("")
		model := newDashboardModel(store, cfg, ghClient)
		model.shutdownCancel = cancel
		model.bin = bin
		model.binNotice, model.binNoticeErr = notice, notice != ""

		// Tail the session event log so klaus-internal commands (e.g.
		// `klaus approve`) can wake the dashboard FSM without waiting for
		// a GitHub webhook. The tail runs even when no webhook server is
		// configured — it is the invalidation channel for our own CLI.
		if model.eventsPath != "" {
			internalCh := make(chan event.Event, 64)
			model.internalEventCh = internalCh
			go func() {
				defer close(internalCh)
				if err := event.Tail(ctx, model.eventsPath, internalCh); err != nil {
					fmt.Fprintf(os.Stderr, "warning: event tail stopped: %v\n", err)
				}
			}()
		}

		var webhookSrv *webhook.Server
		if cfg.Webhook != nil {
			ch := make(chan webhook.Event, 64)
			port := cfg.Webhook.Port
			if port == 0 {
				port = 9800
			}
			webhookSrv = webhook.NewServer(port, cfg.Webhook.Path, ch)

			if err := webhookSrv.Listen(); err != nil {
				cancel()
				return nil, fmt.Errorf("webhook server: %w", err)
			}

			model.webhookCh = ch
			model.useWebhook = true
			model.pollEnabled = cfg.Webhook.PollFallback
			model.webhookAddr = webhookSrv.Addr()
			model.reconcileEvery = reconcileInterval(cfg.Webhook)

			go func() {
				if err := webhookSrv.Serve(); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "webhook server error: %v\n", err)
				}
			}()
		} else {
			model.pollEnabled = true
		}

		p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx))
		finalModel, err := p.Run()
		cancel()

		// BubbleTea has exited — centralize cleanup of watcher and log file.
		var restart *binUpdateMsg
		if m, ok := finalModel.(dashboardModel); ok {
			if m.watcher != nil {
				m.watcher.Close()
			}
			if m.logFile != nil {
				m.logFile.Close()
			}
			if m.restarting && err == nil {
				restart = m.restart
			}
		}

		// Gracefully shut down the webhook server so the port is released promptly.
		if webhookSrv != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := webhookSrv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "webhook server shutdown: %v\n", err)
			}
		}

		return restart, err
	}
}

func init() {
	rootCmd.AddCommand(dashboardCmd)
}

// dashboardModel is the bubbletea model for the dashboard.
type dashboardModel struct {
	store           run.StateStore
	ghClient        gh.Client
	tmuxDeps        run.TmuxDeps
	tmux            tmux.Client // for keyboard-driven actions (discuss)
	cursor          int         // index into the displayed rows of the selected row
	selKey          string      // dashRow.key of the selected row; survives reordering
	offset          int         // index of the first row in the viewport
	showAll         bool        // show old done rows that are hidden by default
	hideAfter       time.Duration
	runningSnap     map[string]bool      // run ID -> agent running, see agentRunning
	seenUnfinished  map[string]bool      // row keys seen before they finished
	finishedAt      map[string]time.Time // when a watched row was first seen finished
	states          []*run.State
	ghStatus        map[string]*prStatus // keyed by PR URL
	sandboxHosts    map[string]bool      // host -> reachable
	pipelineCtrl    *pipeline.Controller
	pipelineStates  map[string]*pipeline.PRPipelineState
	noAccessAt      map[string]time.Time // PR URL -> when last found unreadable
	checker         integration.Checker
	integration     map[string]integration.Result                // run ID -> latest check; nil before the first check
	changes         map[integration.ChangeKey]integration.Result // the valley's view of each change's active version
	integrationOn   bool                                         // the integration check loop has started
	approveTemplate string                                       // dashboard.approve_command, or the deprecated submit_command
	recentErrors    []dashboardError                             // last N pipeline errors shown in TUI
	width           int
	height          int
	err             error
	watcher         *fsnotify.Watcher
	logFile         *os.File
	shutdownCancel  context.CancelFunc   // cancels the shared shutdown context
	webhookCh       <-chan webhook.Event // non-nil when webhook mode is active
	webhookAddr     string               // e.g. "127.0.0.1:9800"
	useWebhook      bool                 // true when webhook server is running
	pollEnabled     bool                 // true when polling is active (default or poll_fallback)
	reconcileEvery  time.Duration        // slow reconcile heartbeat interval (webhook-only mode)
	// internalEventCh carries klaus-emitted invalidation events (e.g.
	// PRApprovalChanged from `klaus approve`). It is symmetric to webhookCh
	// but sourced from the local event log rather than an HTTP listener,
	// so internal CLI commands can wake the FSM without waiting for a real
	// GitHub webhook.
	internalEventCh <-chan event.Event
	eventsPath      string // path to events.jsonl for the tail goroutine
	// lastWebhookAt records the wall-clock time of the most recent webhook
	// delivery (any event). The zero value means none received yet. It drives
	// the footer freshness indicator; the existing 30s tick re-renders the view
	// so the displayed age advances without a dedicated timer.
	lastWebhookAt time.Time
	// bin watches klaus on PATH; restart is the new klaus it found, which
	// the dashboard restarts onto once the keyboard is quiet (lastKeyAt).
	// restarting means it quit to do so. binNotice is the footer's line
	// about it, an error if binNoticeErr.
	bin          *binCheck
	restart      *binUpdateMsg
	restarting   bool
	lastKeyAt    time.Time
	binNotice    string
	binNoticeErr bool
}

func newDashboardModel(store run.StateStore, cfg config.Config, ghClient gh.Client) dashboardModel {
	var eventLog *event.Log
	var logWriter io.Writer = io.Discard
	var logFile *os.File
	var eventsPath string
	if hds, ok := store.(*run.HomeDirStore); ok {
		eventLog = event.NewLog(hds.BaseDir())
		eventsPath = eventLog.Path()
		logPath := filepath.Join(hds.BaseDir(), "dashboard.log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			logWriter = f
			logFile = f
		}
	}
	logger := slog.New(slog.NewTextHandler(logWriter, nil))
	ctrl := pipeline.New(store, eventLog, logger)
	ctrl.SetAutoMergeOnApproval(cfg.AutoMergesOnApproval())

	return dashboardModel{
		store:           store,
		ghClient:        ghClient,
		tmuxDeps:        run.DefaultTmuxDeps(),
		tmux:            tmux.NewExecClient(),
		ghStatus:        make(map[string]*prStatus),
		sandboxHosts:    make(map[string]bool),
		pipelineCtrl:    ctrl,
		pipelineStates:  make(map[string]*pipeline.PRPipelineState),
		noAccessAt:      make(map[string]time.Time),
		checker:         integration.NewValleyChecker(logger),
		approveTemplate: cfg.Dashboard.ApproveTemplate(),
		hideAfter:       hideFinishedAfter(cfg.Dashboard),
		logFile:         logFile,
		eventsPath:      eventsPath,
	}
}

func (m dashboardModel) Init() tea.Cmd {
	cmds := []tea.Cmd{
		loadStatesCmd(m.store),
		startWatcherCmd(m.store),
		tickCmd(), // always tick once on startup for initial fetch
	}
	if m.webhookCh != nil {
		cmds = append(cmds, waitForWebhookCmd(m.webhookCh))
	}
	if shouldScheduleReconcile(m.useWebhook, m.pollEnabled, m.reconcileEvery) {
		cmds = append(cmds, reconcileTickAfterCmd(m.reconcileEvery))
	}
	if m.internalEventCh != nil {
		cmds = append(cmds, waitForInternalEventCmd(m.internalEventCh))
	}
	return tea.Batch(cmds...)
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	dm, ok := next.(dashboardModel)
	if !ok {
		return next, cmd
	}
	// Re-resolve the run list whenever it or the pane can have changed, so
	// the selection stays on its row and in view.
	switch msg.(type) {
	case statesLoadedMsg, ghStatusMsg, tickMsg, integrationMsg:
		dm.snapshotRunning()
		dm.syncView(time.Now())
	case tea.KeyMsg, tea.WindowSizeMsg:
		dm.syncView(time.Now())
	}
	return dm, cmd
}

func (m dashboardModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.lastKeyAt = time.Now()
		switch msg.String() {
		case "q", "ctrl+c":
			// Cancel the shared context to signal all subsystems to stop.
			if m.shutdownCancel != nil {
				m.shutdownCancel()
			}
			return m, tea.Quit
		case "r":
			return m, tea.Batch(
				loadStatesCmd(m.store),
				m.fetchGHStatusCmd(m.states),
			)
		case "up", "k":
			m.moveSelection(-1, 0)
		case "down", "j":
			m.moveSelection(1, 0)
		case "pgup":
			m.moveSelection(0, -1)
		case "pgdown":
			m.moveSelection(0, 1)
		case "g", "home":
			m.selectEdge(false)
		case "G", "end":
			m.selectEdge(true)
		case "h":
			m.showAll = !m.showAll
		case "a":
			// Approve the selected PR immediately (no confirmation).
			r, ok := m.selectedPR("approve")
			if !ok {
				break
			}
			if err := markApproved(r.state(), m.store); err != nil {
				m.noteError(fmt.Sprintf("approve PR #%s: %v", r.prNum, err))
			}
			// Reload so the row reflects approval immediately; the fsnotify
			// watcher also catches the save, but this avoids the round-trip lag.
			return m, loadStatesCmd(m.store)
		case "d":
			// Discuss the selected PR with the coordinator: pre-fill a prompt
			// in the coordinator pane and switch focus there.
			r, ok := m.selectedPR("discuss")
			if !ok {
				break
			}
			pane := m.coordinatorPane()
			if pane == "" {
				m.noteError("coordinator pane unknown (older session; relaunch to enable discuss)")
				break
			}
			if err := m.discussPR(r.prNum, pane); err != nil {
				m.noteError(fmt.Sprintf("discuss PR #%s: %v", r.prNum, err))
			}
		case "o":
			// Open the selected PR in the system browser.
			r, ok := m.selectedPR("open")
			if !ok {
				break
			}
			s := r.state()
			if s.PRURL == nil || *s.PRURL == "" {
				break
			}
			url := *s.PRURL
			prNum := r.prNum
			// Fire-and-forget so Update stays clean and the UI never blocks on
			// the browser launch. Surface a transient hint on failure.
			return m, func() tea.Msg {
				if err := openInBrowser(url); err != nil {
					return errHintMsg{msg: fmt.Sprintf("open PR #%s: %v", prNum, err)}
				}
				return nil
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case statesLoadedMsg:
		m.states = msg.states
		// Detect and finalize stale (orphaned) runs so they stop appearing as active.
		for _, s := range m.states {
			if s.IsStaleWith(m.tmuxDeps) {
				slog.Info("finalizing stale run", "id", s.ID)
				markRunFailed(m.store, s)
			}
		}
		cmd := m.fetchGHStatusCmd(m.states)
		if !m.integrationOn {
			// The first load starts the integration check loop; it then
			// re-arms itself each time a check completes.
			m.integrationOn = true
			cmd = tea.Batch(cmd, checkIntegrationCmd(m.checker, m.states))
		}
		return m, cmd

	case integrationTickMsg:
		return m, checkIntegrationCmd(m.checker, m.states)

	case integrationMsg:
		m.integration = keepValleyViews(m.integration, msg.results)
		m.changes = keepChangeViews(m.changes, msg.changes)
		return m, integrationTickAfterCmd()

	case ghStatusMsg:
		now := time.Now()
		for url, v := range msg.statuses {
			m.ghStatus[url] = v
			if v.State == gh.StateNoAccess {
				m.noAccessAt[url] = now
			} else {
				delete(m.noAccessAt, url)
			}
		}
		// Feed statuses to the pipeline controller, which keys PRs by number:
		// the first run state for a number decides which PR it tracks. PRs the
		// agent can't read are left out; the pipeline can't act on them.
		pStatuses := make(map[string]*pipeline.PRStatus, len(msg.statuses))
		claimed := make(map[string]bool)
		for _, s := range m.states {
			prNum, url := extractPRNumber(s), extractPRRef(s)
			if prNum == "" || claimed[prNum] {
				continue
			}
			claimed[prNum] = true
			v, ok := msg.statuses[url]
			if !ok || v.State == gh.StateNoAccess {
				continue
			}
			ps := &pipeline.PRStatus{
				PRNumber:              v.PRNumber,
				PRURL:                 url,
				State:                 v.State,
				CI:                    v.CI,
				Conflicts:             v.Conflicts,
				BehindBy:              v.BehindBy,
				ReviewDecision:        v.ReviewDecision,
				HasNewTrustedComments: v.HasNewTrustedComments,
				Labels:                v.Labels,
			}
			if s.TargetRepo != nil {
				ps.TargetRepo = *s.TargetRepo
			}
			pStatuses[prNum] = ps
		}
		actions := m.pipelineCtrl.HandleGHStatus(context.Background(), pStatuses, m.states)
		m.pipelineStates = m.pipelineCtrl.PipelineStates()
		if len(actions) > 0 {
			return m, func() tea.Msg {
				return pipelineActionMsg{actions: actions}
			}
		}

	case pipelineActionMsg:
		// Capture any error actions for TUI display.
		for _, a := range msg.actions {
			if a.Error != "" {
				errMsg := a.Detail + " — " + a.Error
				m.recentErrors = append(m.recentErrors, dashboardError{
					Time:    time.Now(),
					Message: errMsg,
				})
				if len(m.recentErrors) > 3 {
					m.recentErrors = m.recentErrors[len(m.recentErrors)-3:]
				}
			}
		}
		// Pipeline dispatched agents or merged PRs — refresh state.
		return m, loadStatesCmd(m.store)

	case fsEventMsg:
		return m, tea.Batch(loadStatesCmd(m.store), watchFSCmd(m.watcher))

	case sandboxStatusMsg:
		for k, v := range msg.hosts {
			m.sandboxHosts[k] = v
		}

	case webhookMsg:
		// Webhooks are invalidation signals, not data sources. When a
		// webhook arrives, trigger an immediate re-fetch of PR status
		// via the same code path that polling uses. This eliminates
		// the class of bugs where the webhook path diverges from polling.
		ev := msg.event
		// Record the delivery time for the footer freshness indicator. Every
		// delivery counts, regardless of PRNumber/type.
		m.lastWebhookAt = time.Now()
		if ev.PRNumber != "" {
			// Find run states that match this PR for a targeted fetch.
			var matchedStates []*run.State
			for _, s := range m.states {
				if extractPRNumber(s) == ev.PRNumber {
					matchedStates = append(matchedStates, s)
				}
			}
			if len(matchedStates) > 0 {
				return m, tea.Batch(
					m.fetchGHStatusCmd(matchedStates),
					waitForWebhookCmd(m.webhookCh),
				)
			}
		} else if ev.EventType == "push" && ev.Repo != "" {
			// Push to default branch — re-fetch all open PRs since
			// conflicts may have changed.
			return m, tea.Batch(
				m.fetchGHStatusCmd(m.states),
				waitForWebhookCmd(m.webhookCh),
			)
		}
		return m, waitForWebhookCmd(m.webhookCh)

	case webhookClosedMsg:
		// Live webhook consumption has stopped. Do not re-arm (re-reading a
		// closed channel busy-loops). Surface it non-fatally; the reconcile
		// heartbeat, if active, still bounds staleness.
		m.recentErrors = append(m.recentErrors, dashboardError{
			Time:    time.Now(),
			Message: "webhook event channel closed; live webhook updates stopped (reconcile heartbeat still active)",
		})
		if len(m.recentErrors) > 3 {
			m.recentErrors = m.recentErrors[len(m.recentErrors)-3:]
		}
		return m, nil

	case internalEventMsg:
		// Klaus-internal invalidation events are handled symmetrically to
		// webhooks: a CLI command (e.g. `klaus approve`) signalled that the
		// pipeline preconditions for a PR may have changed, so we trigger
		// a targeted re-fetch of GitHub status. That re-fetch funnels into
		// the same HandleGHStatus path that polling and webhooks use, so
		// the FSM gets a fresh evaluation without any divergent code path.
		ev := msg.event
		prNum := internalEventPRNumber(ev)
		if shouldInvalidate(ev.Type) && prNum != "" {
			var matchedStates []*run.State
			for _, s := range m.states {
				if extractPRNumber(s) == prNum {
					matchedStates = append(matchedStates, s)
				}
			}
			if len(matchedStates) > 0 {
				return m, tea.Batch(
					m.fetchGHStatusCmd(matchedStates),
					waitForInternalEventCmd(m.internalEventCh),
				)
			}
		}
		return m, waitForInternalEventCmd(m.internalEventCh)

	case trustedCommentsMsg:
		existing, ok := m.ghStatus[msg.prURL]
		if ok {
			existing.HasNewTrustedComments = msg.hasNewTrustedComments
		}

	case reconcileTickMsg:
		// Slow reconcile heartbeat (webhook-only mode). Webhooks are the only
		// reconcile trigger when poll_fallback is false, so a single dropped
		// or missed event would strand a PR forever. This periodic full
		// re-fetch bounds worst-case staleness (see issue #271). It re-arms
		// itself; it is never scheduled when polling is active (which already
		// re-fetches every 30s), so there is no double-fetch.
		return m, tea.Batch(
			m.fetchGHStatusCmd(m.states),
			reconcileTickAfterCmd(m.reconcileEvery),
		)

	case tickMsg:
		// Expire errors older than 3 minutes.
		cutoff := time.Now().Add(-3 * time.Minute)
		filtered := m.recentErrors[:0]
		for _, e := range m.recentErrors {
			if e.Time.After(cutoff) {
				filtered = append(filtered, e)
			}
		}
		m.recentErrors = filtered

		cmds := []tea.Cmd{
			checkSandboxCmd(m.states),
			tickAfterCmd(),
		}
		if m.bin != nil && m.restart == nil {
			cmds = append(cmds, checkBinCmd(m.bin))
		}
		if m.pollEnabled {
			cmds = append(cmds, m.fetchGHStatusCmd(m.states))
		}
		return m, tea.Batch(cmds...)

	case *fsnotify.Watcher:
		m.watcher = msg
		return m, watchFSCmd(msg)

	case binUpdateMsg:
		if m.restart != nil {
			break
		}
		m.restart = &msg
		m.binNotice, m.binNoticeErr = "klaus updated to "+msg.version+"; restarting", false
		// Show the notice for a moment before restarting.
		return m, restartWaitCmd()

	case restartWaitMsg:
		return m.restartWhenIdle(time.Now())

	case errHintMsg:
		m.noteError(msg.msg)

	case errMsg:
		m.err = msg.err
	}

	return m, nil
}

func (m dashboardModel) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err)
	}
	if m.states == nil {
		return "Loading..."
	}

	now := time.Now()
	v := m.resolve(now)
	width := m.width
	if width <= 0 {
		width = defaultDashboardWidth
	}

	chrome := func(c chromeItem) string {
		switch c.kind {
		case chromeHeader:
			title := " klaus dashboard"
			headerRight := fmt.Sprintf("Session: %s | Cost: $%.2f",
				formatDuration(computeSessionDuration(m.states)), computeTotalCost(m.states))
			pad := max(width-2-lipgloss.Width(title)-lipgloss.Width(headerRight), 1)
			return headerStyle.Render(title + strings.Repeat(" ", pad) + headerRight)
		case chromeSource:
			return m.renderSourceLine()
		case chromeSandbox:
			return strings.TrimSuffix(renderSandboxStatus(m.sandboxHosts), "\n")
		case chromeSummary:
			return m.renderSummary(v, width)
		case chromeError:
			e := m.recentErrors[c.idx]
			line := fmt.Sprintf("  %s ✗ %s", e.Time.Format("15:04"), e.Message)
			return dimRedStyle.Render(truncateLine(line, max(width-1, 1)))
		case chromeNotice:
			style := yellowStyle
			if m.binNoticeErr {
				style = dimRedStyle
			}
			return style.Render(truncateLine("  "+m.binNotice, max(width-1, 1)))
		case chromeFooter:
			return m.renderFooter(v, width)
		}
		return ""
	}

	var lines []string
	for _, c := range v.chrome {
		if c.top {
			lines = append(lines, chrome(c))
		}
	}
	if len(v.rows) == 0 && (m.height <= 0 || m.height > len(v.chrome)) {
		msg := "  No runs found."
		if v.hidden > 0 {
			msg = fmt.Sprintf("  No runs to show: %d done hidden (h to show all).", v.hidden)
		}
		lines = append(lines, dimStyle.Render(msg))
	}
	lay := newRowLayout(v.all, width)
	for i := v.offset; i < v.offset+v.bodyH && i < len(v.rows); i++ {
		lines = append(lines, m.renderRow(v.rows[i], i == v.cursor, lay, now))
	}
	for _, c := range v.chrome {
		if !c.top {
			lines = append(lines, chrome(c))
		}
	}

	// Never wrap: a wrapped line would push rows off the bottom of the pane.
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}
