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

Keyboard shortcuts:
  q  quit
  r  force refresh`,
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
	shutdownCancel context.CancelFunc  // cancels the shared shutdown context
	webhookCh      <-chan webhook.Event // non-nil when webhook mode is active
	webhookAddr    string              // e.g. "127.0.0.1:9800"
	useWebhook     bool                // true when webhook server is running
	pollEnabled    bool                // true when polling is active (default or poll_fallback)
}

func newDashboardModel(store run.StateStore, cfg config.Config, ghClient gh.Client) dashboardModel {
	var eventLog *event.Log
	var logWriter io.Writer = io.Discard
	var logFile *os.File
	if hds, ok := store.(*run.HomeDirStore); ok {
		eventLog = event.NewLog(hds.BaseDir())
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
		ghStatus:       make(map[string]*prStatus),
		sandboxHosts:   make(map[string]bool),
		pipelineCtrl:   ctrl,
		pipelineStates: make(map[string]*pipeline.PRPipelineState),
		logFile:        logFile,
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
				ReviewDecision:        v.ReviewDecision,
				HasNewTrustedComments: v.HasNewTrustedComments,
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

	case trustedCommentsMsg:
		existing, ok := m.ghStatus[msg.prNumber]
		if ok {
			existing.HasNewTrustedComments = msg.hasNewTrustedComments
		}

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
		"  %d/%d agents running | q quit | r refresh",
		runningCount, totalCount,
	))
	b.WriteString(footer)
	b.WriteString("\n")

	return b.String()
}
