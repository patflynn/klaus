package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/event"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
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
GitHub state (CI, conflicts, reviews) polls every 30 seconds.

Keyboard shortcuts:
  q  quit
  r  force refresh`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStoreOrAll()
		if err != nil {
			return err
		}
		if store == nil {
			return fmt.Errorf("KLAUS_SESSION_ID not set; run inside a klaus session")
		}
		p := tea.NewProgram(newDashboardModel(store), tea.WithAltScreen())
		_, err = p.Run()
		return err
	},
}

func init() {
	rootCmd.AddCommand(dashboardCmd)
}

// Messages for the bubbletea event loop.

type statesLoadedMsg struct {
	states []*run.State
}

type ghStatusMsg struct {
	statuses map[string]*prStatus
}

type fsEventMsg struct{}

type tickMsg struct{}

type sandboxStatusMsg struct {
	hosts map[string]bool // host -> reachable
}

type pipelineActionMsg struct {
	actions []pipeline.Action
}

type errMsg struct {
	err error
}

// prStatus holds the GitHub-fetched status for a single PR.
type prStatus struct {
	PRNumber              string
	State                 string // OPEN, MERGED, CLOSED
	CI                    string // passing, failing, pending, unknown
	Conflicts             string // yes, none, unknown
	ReviewDecision        string // APPROVED, CHANGES_REQUESTED, etc.
	HasNewTrustedComments bool   // unaddressed comments from trusted reviewers
}

// repoGroup is a set of runs and PRs belonging to the same repository.
type repoGroup struct {
	Repo  string
	Runs  []*run.State
	PRMap map[string]*prStatus
}

// dashboardModel is the bubbletea model for the dashboard.
type dashboardModel struct {
	store          run.StateStore
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
}

func newDashboardModel(store run.StateStore) dashboardModel {
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

	return dashboardModel{
		store:          store,
		ghStatus:       make(map[string]*prStatus),
		sandboxHosts:   make(map[string]bool),
		pipelineCtrl:   ctrl,
		pipelineStates: make(map[string]*pipeline.PRPipelineState),
		logFile:        logFile,
	}
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(
		loadStatesCmd(m.store),
		startWatcherCmd(m.store),
		tickCmd(),
	)
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.watcher != nil {
				m.watcher.Close()
			}
			if m.logFile != nil {
				m.logFile.Close()
			}
			return m, tea.Quit
		case "r":
			return m, tea.Batch(
				loadStatesCmd(m.store),
				fetchGHStatusCmd(m.states),
			)
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case statesLoadedMsg:
		m.states = msg.states
		return m, fetchGHStatusCmd(m.states)

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

		return m, tea.Batch(
			fetchGHStatusCmd(m.states),
			checkSandboxCmd(m.states),
			tickAfterCmd(),
		)

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
	runningCount, totalCount := countAgents(m.states)
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

func (m dashboardModel) renderGroup(g repoGroup) string {
	var b strings.Builder

	// Repo header with counts
	prCount := 0
	agentCount := 0
	for _, s := range g.Runs {
		if s.Type == "session" {
			continue
		}
		agentCount++
		if s.PRURL != nil {
			prCount++
		}
	}

	repoLabel := repoStyle.Render(fmt.Sprintf(" %s", g.Repo))
	counts := dimStyle.Render(fmt.Sprintf(
		"%d agent%s, %d PR%s",
		agentCount, pluralS(agentCount),
		prCount, pluralS(prCount),
	))
	b.WriteString(fmt.Sprintf("%s%s\n", repoLabel, rightAlignPad(counts, m.width-lipgloss.Width(repoLabel))))
	b.WriteString(dimStyle.Render(" " + strings.Repeat("─", clamp(m.width-2, 0, 120))))
	b.WriteString("\n")

	// Group agents by PR number in a single pass (O(N)).
	prToAgents := make(map[string][]*run.State)
	var bareAgents []*run.State
	var prOrder []string
	seenPRs := make(map[string]bool)

	for _, s := range g.Runs {
		if s.Type == "session" {
			continue
		}
		prNum := extractPRNumber(s)
		if prNum != "" {
			prToAgents[prNum] = append(prToAgents[prNum], s)
			if !seenPRs[prNum] {
				prOrder = append(prOrder, prNum)
				seenPRs[prNum] = true
			}
		} else {
			bareAgents = append(bareAgents, s)
		}
	}

	// Render PRs and their agents
	for _, prNum := range prOrder {
		agents := prToAgents[prNum]
		if len(agents) == 0 {
			continue
		}
		b.WriteString(m.renderPRLine(prNum, agents, g.PRMap[prNum]))
		b.WriteString("\n")
		for _, s := range agents {
			if isAgentRunning(s) {
				b.WriteString(renderAgentSubline(s))
				b.WriteString("\n")
			}
		}
	}

	// Render bare agents
	for _, s := range bareAgents {
		b.WriteString(renderBareAgentLine(s))
		b.WriteString("\n")
	}

	return b.String()
}

func (m dashboardModel) renderPRLine(prNum string, agents []*run.State, ps *prStatus) string {
	s := agents[0]
	prLabel := fmt.Sprintf("  #%-5s", prNum)
	prompt := truncate(s.Prompt, 20)

	state := "OPEN"
	if s.MergedAt != nil {
		state = "MERGED"
	} else if ps != nil && ps.State != "" {
		state = ps.State
	}

	var parts []string
	parts = append(parts, stateLabel(state))

	if ps != nil && state == "OPEN" {
		parts = append(parts, ciLabel(ps.CI))
		if ps.Conflicts == "yes" {
			parts = append(parts, redStyle.Render("conflicts ✗"))
		}
		rd := ps.ReviewDecision
		if strings.EqualFold(rd, "APPROVED") {
			parts = append(parts, greenStyle.Render("ready"))
		} else if strings.EqualFold(rd, "CHANGES_REQUESTED") {
			parts = append(parts, redStyle.Render("changes requested"))
		}
	}

	// Show klaus-internal approval if any run for this PR is approved.
	if state == "OPEN" && isAnyRunApproved(agents) {
		parts = append(parts, cyanStyle.Render("✓ approved"))
	}

	// Append pipeline stage if available.
	if pps, ok := m.pipelineStates[prNum]; ok {
		parts = append(parts, dimStyle.Render(pipeline.StageLabel(pps.Stage)))
	}

	return fmt.Sprintf("%s  %-20s  %s", prLabel, prompt, strings.Join(parts, "  "))
}

// isAnyRunApproved returns true if any of the given run states has been
// approved via `klaus approve`.
func isAnyRunApproved(states []*run.State) bool {
	for _, s := range states {
		if s.Approved != nil && *s.Approved {
			return true
		}
	}
	return false
}

func renderAgentSubline(s *run.State) string {
	shortID := shortRunID(s.ID)
	prompt := truncate(s.Prompt, 20)
	hostTag := sandboxTag(s)
	return yellowStyle.Render(fmt.Sprintf("   └─ agent:%s %s...%s", shortID, prompt, hostTag))
}

func renderBareAgentLine(s *run.State) string {
	shortID := shortRunID(s.ID)
	status := agentStatusLabel(s)
	cost := formatCost(s)
	prompt := truncate(s.Prompt, 20)
	hostTag := sandboxTag(s)

	if isAgentRunning(s) {
		return yellowStyle.Render(fmt.Sprintf("  agent:%s  %-20s  RUNNING   %s", shortID, prompt, cost)) + hostTag
	}
	return dimStyle.Render(fmt.Sprintf("  agent:%s  %-20s  %s   %s", shortID, prompt, status, cost)) + hostTag
}

// sandboxTag returns a styled "[sandbox]" tag if the agent ran on a sandbox host.
func sandboxTag(s *run.State) string {
	if s.Host != nil {
		return " " + sandboxStyle.Render("[sandbox]")
	}
	return ""
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

func fetchGHStatusCmd(states []*run.State) tea.Cmd {
	return func() tea.Msg {
		statuses := make(map[string]*prStatus)
		seen := make(map[string]bool)
		for _, s := range states {
			prNum := extractPRNumber(s)
			prRef := extractPRRef(s)
			if prNum == "" || seen[prNum] {
				continue
			}
			seen[prNum] = true
			statuses[prNum] = fetchPRStatus(prNum, prRef)
		}
		return ghStatusMsg{statuses: statuses}
	}
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

func startWatcherCmd(store run.StateStore) tea.Cmd {
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
		return w
	}
}

func watchFSCmd(w *fsnotify.Watcher) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return nil
				}
				if filepath.Ext(event.Name) == ".json" {
					return fsEventMsg{}
				}
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

func renderSandboxStatus(hosts map[string]bool) string {
	if len(hosts) == 0 {
		return ""
	}
	var parts []string
	for host, reachable := range hosts {
		if reachable {
			parts = append(parts, greenStyle.Render(fmt.Sprintf("  sandbox %s: ✓", host)))
		} else {
			parts = append(parts, redStyle.Render(fmt.Sprintf("  sandbox %s: ✗", host)))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "  ") + "\n"
}

// fetchPRStatus queries GitHub for a single PR's status.
// prRef should be a full PR URL so gh can resolve it from any directory.
func fetchPRStatus(prNumber, prRef string) *prStatus {
	client := gh.NewPRClient("") // full URL in prRef handles repo resolution
	ps := &prStatus{PRNumber: prNumber}
	ps.State = client.GetState(prRef)
	if ps.State == "MERGED" || ps.State == "CLOSED" {
		return ps
	}
	ps.CI = client.GetCI(prRef)
	ps.Conflicts = client.GetConflicts(prRef)
	ps.ReviewDecision = client.GetReviewDecision(prRef)

	// When reviewDecision is not CHANGES_REQUESTED, check for unaddressed
	// trusted reviewer comments that GitHub doesn't reflect in reviewDecision.
	if !strings.EqualFold(ps.ReviewDecision, "CHANGES_REQUESTED") &&
		!strings.EqualFold(ps.ReviewDecision, "APPROVED") {
		ownerRepo := ownerRepoFromPRURL(prRef)
		if ownerRepo != "" {
			ps.HasNewTrustedComments = hasUnaddressedTrustedComments(ownerRepo, prNumber)
		}
	}
	return ps
}

// Data layer functions (testable).

// groupByRepo organizes states into repo groups, sorted by repo name.
func groupByRepo(states []*run.State) []repoGroup {
	reg, _ := project.Load()
	groups := make(map[string]*repoGroup)
	for _, s := range states {
		if s.Type == "session" {
			continue
		}
		repo := repoFromState(s, reg)
		g, ok := groups[repo]
		if !ok {
			g = &repoGroup{
				Repo:  repo,
				PRMap: make(map[string]*prStatus),
			}
			groups[repo] = g
		}
		g.Runs = append(g.Runs, s)
	}

	var result []repoGroup
	for _, g := range groups {
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Repo < result[j].Repo
	})
	return result
}

// repoFromState extracts the repo identifier from a run state, normalizing
// against the project registry so that different forms of the same repo
// (e.g. "cosmo" vs "patflynn/cosmo") group together.
func repoFromState(s *run.State, reg *project.Registry) string {
	if s.TargetRepo != nil && *s.TargetRepo != "" {
		return project.NormalizeRepoName(*s.TargetRepo, reg)
	}
	if s.PRURL != nil {
		ownerRepo := repoFromPRURL(*s.PRURL)
		if ownerRepo != "(unknown)" {
			return project.NormalizeRepoName(ownerRepo, reg)
		}
		return ownerRepo
	}
	return "(local)"
}

// repoFromPRURL extracts "owner/repo" from a GitHub PR URL.
func repoFromPRURL(prURL string) string {
	// https://github.com/owner/repo/pull/123
	prURL = strings.TrimPrefix(prURL, "https://github.com/")
	prURL = strings.TrimPrefix(prURL, "http://github.com/")
	parts := strings.Split(prURL, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return "(unknown)"
}

// computeTotalCost sums the cost across all runs. For runs without final cost,
// it includes the budget as an upper bound.
func computeTotalCost(states []*run.State) float64 {
	var total float64
	for _, s := range states {
		if s.CostUSD != nil {
			total += *s.CostUSD
		}
	}
	return total
}

// countAgents returns (running, total) agent counts (excludes sessions).
func countAgents(states []*run.State) (int, int) {
	var running, total int
	for _, s := range states {
		if s.Type == "session" {
			continue
		}
		total++
		if isAgentRunning(s) {
			running++
		}
	}
	return running, total
}

// computeSessionDuration returns the duration from the oldest active run's creation time to now.
func computeSessionDuration(states []*run.State) time.Duration {
	if len(states) == 0 {
		return 0
	}
	var oldest time.Time
	for _, s := range states {
		t, err := time.Parse(time.RFC3339, s.CreatedAt)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}

// isAgentRunning checks if a run's agent is currently active in tmux.
func isAgentRunning(s *run.State) bool {
	return s.IsAgentRunning()
}

// agentStatusLabel returns a display label for a non-running agent.
func agentStatusLabel(s *run.State) string {
	if s.PRURL != nil {
		return "PR"
	}
	return "EXITED"
}

// shortRunID returns the last 4 chars of a run ID.
func shortRunID(id string) string {
	if len(id) < 4 {
		return id
	}
	return id[len(id)-4:]
}

// formatDuration renders a duration as "Xh Ym" or "Xm Ys".
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// Styling.

var (
	headerStyle  = lipgloss.NewStyle().Bold(true)
	repoStyle    = lipgloss.NewStyle().Bold(true)
	greenStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	redStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cyanStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	sandboxStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimRedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Faint(true)
)

func stateLabel(state string) string {
	switch state {
	case "MERGED":
		return greenStyle.Render("MERGED")
	case "CLOSED":
		return dimStyle.Render("CLOSED")
	default:
		return yellowStyle.Render("OPEN")
	}
}

func ciLabel(ci string) string {
	switch ci {
	case "passing":
		return greenStyle.Render("CI ✓")
	case "failing":
		return redStyle.Render("CI ✗")
	case "pending":
		return yellowStyle.Render("CI …")
	default:
		return dimStyle.Render("CI ?")
	}
}

func rightAlignPad(s string, totalWidth int) string {
	w := lipgloss.Width(s)
	pad := totalWidth - w
	if pad <= 0 {
		return s
	}
	return strings.Repeat(" ", pad) + s
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
