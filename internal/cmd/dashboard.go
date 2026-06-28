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
agent runs and their PR statuses. Groups runs by repository and displays
CI status, merge conflicts, and review decisions.

Local state updates instantly via filesystem watching.
GitHub state (CI, conflicts, reviews) polls every 30 seconds by default.

When webhook config is present in .klaus/config.json, the dashboard starts
an HTTP server to receive push events from github-relay instead of polling.
Set "webhook": {"port": 9800} in config to enable.

In webhook-only mode (poll_fallback false), a slow reconcile heartbeat runs
a full status re-fetch every 5 minutes by default (configurable via
"reconcile_interval_seconds") so a dropped webhook can't strand a PR forever.

Keyboard shortcuts:
  j / k or ↑ / ↓  move the PR selection
  a               approve the selected PR
  d               discuss the selected PR with the coordinator
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
	cursor         int         // index into selectablePRs(states) for keyboard selection
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
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if n := len(selectablePRs(m.states)); m.cursor < n-1 {
				m.cursor++
			}
		case "a":
			// Approve the selected PR immediately (no confirmation).
			entries := selectablePRs(m.states)
			if len(entries) == 0 {
				break
			}
			e := entries[clampCursor(m.cursor, len(entries))]
			if e.state != nil {
				if err := markApproved(e.state, m.store); err != nil {
					m.noteError(fmt.Sprintf("approve PR #%s: %v", e.prNum, err))
				}
			}
			// Reload so the row reflects approval immediately; the fsnotify
			// watcher also catches the save, but this avoids the round-trip lag.
			return m, loadStatesCmd(m.store)
		case "d":
			// Discuss the selected PR with the coordinator: pre-fill a prompt
			// in the coordinator pane and switch focus there.
			entries := selectablePRs(m.states)
			if len(entries) == 0 {
				break
			}
			e := entries[clampCursor(m.cursor, len(entries))]
			pane := m.coordinatorPane()
			if pane == "" {
				m.noteError("coordinator pane unknown (older session; relaunch to enable discuss)")
				break
			}
			if err := m.discussPR(e.prNum, pane); err != nil {
				m.noteError(fmt.Sprintf("discuss PR #%s: %v", e.prNum, err))
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case statesLoadedMsg:
		prevEntries := selectablePRs(m.states)
		m.states = msg.states
		m.reconcileSelection(prevEntries)
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

	groups := groupByRepo(m.states)

	// Populate each group's PRMap from the fetched GitHub statuses.
	for i := range groups {
		for _, s := range groups[i].Runs {
			prNum := extractPRNumber(s)
			if prNum != "" {
				if ps, ok := m.ghStatus[prNum]; ok {
					groups[i].PRMap[prNum] = ps
				}
			}
		}
	}

	totalCost := computeTotalCost(m.states)
	runningCount, totalCount := m.countAgents(m.states)
	sessionDur := computeSessionDuration(m.states)

	var b strings.Builder

	// Header
	headerRight := fmt.Sprintf("Session: %s | Cost: $%.2f", formatDuration(sessionDur), totalCost)
	header := headerStyle.Render(fmt.Sprintf(
		" klaus dashboard%s",
		rightAlignPad(headerRight, m.width-18),
	))
	b.WriteString(header)
	b.WriteString("\n")

	// Data source status line
	if m.useWebhook {
		addr := m.webhookAddr
		if addr == "" {
			addr = "starting..."
		}
		tag := fmt.Sprintf("  webhook: listening on %s", addr)
		if m.pollEnabled {
			tag += " + polling: 30s"
		} else if m.reconcileEvery > 0 {
			tag += fmt.Sprintf(" + reconcile: %s", formatDuration(m.reconcileEvery))
		}
		b.WriteString(dimStyle.Render(tag))
		b.WriteString("\n")
	} else {
		b.WriteString(dimStyle.Render("  polling: 30s"))
		b.WriteString("\n")
	}

	// Sandbox status line (only if any runs have a Host set)
	if sandboxLine := renderSandboxStatus(m.sandboxHosts); sandboxLine != "" {
		b.WriteString(sandboxLine)
	}
	b.WriteString("\n")

	if len(groups) == 0 {
		b.WriteString(dimStyle.Render("  No runs found."))
		b.WriteString("\n")
	}

	for _, g := range groups {
		b.WriteString(m.renderGroup(g))
		b.WriteString("\n")
	}

	// Pipeline errors
	if len(m.recentErrors) > 0 {
		for _, e := range m.recentErrors {
			ts := e.Time.Format("15:04")
			line := fmt.Sprintf("  %s ✗ %s", ts, e.Message)
			b.WriteString(dimRedStyle.Render(truncate(line, clamp(m.width-2, 20, 120))))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Footer
	footer := dimStyle.Render(fmt.Sprintf(
		"  %d/%d agents running | j/k move · a approve · d discuss | r refresh · q quit",
		runningCount, totalCount,
	))
	b.WriteString(footer)
	b.WriteString("\n")

	return b.String()
}
