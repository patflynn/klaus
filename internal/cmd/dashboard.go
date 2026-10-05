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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
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

Runs are listed one per row: active runs first, then runs needing attention,
then open PRs, then finished runs, most recent first within each group. The
list scrolls when it doesn't fit the pane; the line above it shows the
position (e.g. "12–30 of 34"). Merged, closed and cleaned-up runs older than
an hour are hidden until you press h; the same line says how many are hidden.
Set "dashboard": {"hide_finished_after_minutes": N} in config to change the
threshold, or a negative N to never hide them.

Keyboard shortcuts:
  j / k or ↑ / ↓  move the selection
  PgUp / PgDn     move a page
  g / G           jump to the top / bottom (also Home / End)
  h               show all runs / hide old finished runs
  a               approve the selected PR
  d               discuss the selected PR with the coordinator
  o               open the selected PR in a browser
  r               force refresh
  q               quit`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return fmt.Errorf("KLAUS_SESSION_ID not set; run inside a klaus session")
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
				return fmt.Errorf("webhook server: %w", err)
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
		if m, ok := finalModel.(dashboardModel); ok {
			if m.watcher != nil {
				m.watcher.Close()
			}
			if m.logFile != nil {
				m.logFile.Close()
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

		return err
	},
}

func init() {
	rootCmd.AddCommand(dashboardCmd)
}

// dashboardModel is the bubbletea model for the dashboard.
type dashboardModel struct {
	store          run.StateStore
	ghClient       gh.Client
	tmuxDeps       run.TmuxDeps
	tmux           tmux.Client // for keyboard-driven actions (discuss)
	cursor         int         // index into the displayed rows of the selected row
	selKey         string      // dashRow.key of the selected row; survives reordering
	offset         int         // index of the first row in the viewport
	showAll        bool        // show old finished runs that are hidden by default
	hideAfter      time.Duration
	runningSnap    map[string]bool      // run ID -> agent running, see agentRunning
	seenUnfinished map[string]bool      // row keys seen before they finished
	finishedAt     map[string]time.Time // when a watched row was first seen finished
	states         []*run.State
	ghStatus       map[string]*prStatus // keyed by PR number
	sandboxHosts   map[string]bool      // host -> reachable
	pipelineCtrl   *pipeline.Controller
	pipelineStates map[string]*pipeline.PRPipelineState
	recentErrors   []dashboardError // last N pipeline errors shown in TUI
	width          int
	height         int
	err            error
	watcher        *fsnotify.Watcher
	logFile        *os.File
	shutdownCancel context.CancelFunc   // cancels the shared shutdown context
	webhookCh      <-chan webhook.Event // non-nil when webhook mode is active
	webhookAddr    string               // e.g. "127.0.0.1:9800"
	useWebhook     bool                 // true when webhook server is running
	pollEnabled    bool                 // true when polling is active (default or poll_fallback)
	reconcileEvery time.Duration        // slow reconcile heartbeat interval (webhook-only mode)
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
		store:          store,
		ghClient:       ghClient,
		tmuxDeps:       run.DefaultTmuxDeps(),
		tmux:           tmux.NewExecClient(),
		ghStatus:       make(map[string]*prStatus),
		sandboxHosts:   make(map[string]bool),
		pipelineCtrl:   ctrl,
		pipelineStates: make(map[string]*pipeline.PRPipelineState),
		hideAfter:      hideFinishedAfter(cfg.Dashboard),
		logFile:        logFile,
		eventsPath:     eventsPath,
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
	case statesLoadedMsg, ghStatusMsg, tickMsg:
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
				fetchGHStatusCmd(m.ghClient, m.states),
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
		return m, fetchGHStatusCmd(m.ghClient, m.states)

	case ghStatusMsg:
		for k, v := range msg.statuses {
			m.ghStatus[k] = v
		}
		// Feed statuses to pipeline controller.
		pStatuses := make(map[string]*pipeline.PRStatus, len(msg.statuses))
		for k, v := range msg.statuses {
			ps := &pipeline.PRStatus{
				PRNumber:              v.PRNumber,
				State:                 v.State,
				CI:                    v.CI,
				Conflicts:             v.Conflicts,
				BehindBy:              v.BehindBy,
				ReviewDecision:        v.ReviewDecision,
				HasNewTrustedComments: v.HasNewTrustedComments,
				Labels:                v.Labels,
			}
			// Find the PR URL and target repo from run states.
			for _, s := range m.states {
				prNum := extractPRNumber(s)
				if prNum == k {
					if s.PRURL != nil {
						ps.PRURL = *s.PRURL
					}
					if s.TargetRepo != nil {
						ps.TargetRepo = *s.TargetRepo
					}
					break
				}
			}
			pStatuses[k] = ps
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
					fetchGHStatusCmd(m.ghClient, matchedStates),
					waitForWebhookCmd(m.webhookCh),
				)
			}
		} else if ev.EventType == "push" && ev.Repo != "" {
			// Push to default branch — re-fetch all open PRs since
			// conflicts may have changed.
			return m, tea.Batch(
				fetchGHStatusCmd(m.ghClient, m.states),
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
					fetchGHStatusCmd(m.ghClient, matchedStates),
					waitForInternalEventCmd(m.internalEventCh),
				)
			}
		}
		return m, waitForInternalEventCmd(m.internalEventCh)

	case trustedCommentsMsg:
		existing, ok := m.ghStatus[msg.prNumber]
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
			fetchGHStatusCmd(m.ghClient, m.states),
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
		if m.pollEnabled {
			cmds = append(cmds, fetchGHStatusCmd(m.ghClient, m.states))
		}
		return m, tea.Batch(cmds...)

	case *fsnotify.Watcher:
		m.watcher = msg
		return m, watchFSCmd(msg)

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
			msg = fmt.Sprintf("  No runs to show: %d finished hidden (h to show all).", v.hidden)
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
