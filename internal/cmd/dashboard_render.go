package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
)

// Styling.

var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	repoStyle     = lipgloss.NewStyle().Bold(true)
	greenStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	redStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cyanStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	sandboxStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimRedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Faint(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("237"))
)

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

	// Determine which PR row (if any) is currently selected so it can be
	// rendered with the highlight gutter.
	selRepo, selPR, hasSel := m.selectedPR()

	// Render PRs and their agents
	for _, prNum := range prOrder {
		agents := prToAgents[prNum]
		if len(agents) == 0 {
			continue
		}
		selected := hasSel && selRepo == g.Repo && selPR == prNum
		b.WriteString(m.renderPRLine(prNum, agents, g.PRMap[prNum], selected))
		b.WriteString("\n")
		for _, s := range agents {
			if m.isAgentRunning(s) {
				b.WriteString(renderAgentSubline(s))
				b.WriteString("\n")
			}
		}
	}

	// Render bare agents
	for _, s := range bareAgents {
		b.WriteString(m.renderBareAgentLine(s))
		b.WriteString("\n")
	}

	return b.String()
}

func (m dashboardModel) renderPRLine(prNum string, agents []*run.State, ps *prStatus, selected bool) string {
	s := agents[0]
	// Gutter marker for the cursor-selected row.
	gutter := "  "
	if selected {
		gutter = "> "
	}
	// Build the visible PR label first (visible width 6), then wrap only the
	// visible text in a hyperlink so the OSC 8 escape bytes are not counted in
	// the %-5s padding and column alignment is preserved.
	prText := fmt.Sprintf("#%-5s", prNum)
	if s.PRURL != nil && *s.PRURL != "" {
		prText = hyperlink(*s.PRURL, prText)
	}
	prLabel := gutter + prText
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

	prefix := fmt.Sprintf("%s  %-20s", prLabel, prompt)
	if selected {
		prefix = selectedStyle.Render(prefix)
	}
	return fmt.Sprintf("%s  %s", prefix, strings.Join(parts, "  "))
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

func (m *dashboardModel) renderBareAgentLine(s *run.State) string {
	shortID := shortRunID(s.ID)
	status := agentStatusLabel(s)
	cost := formatCost(s)
	prompt := truncate(s.Prompt, 20)
	hostTag := sandboxTag(s)

	if m.isAgentRunning(s) {
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

// hyperlink wraps text in an OSC 8 terminal hyperlink escape sequence.
// The escape bytes are invisible and stripped by lipgloss.Width(), so the
// visible width of the returned string equals that of text.
//
// If url is empty or contains any byte outside printable ASCII (32–126), the
// plain text is returned unwrapped. This guards against terminal escape
// sequence injection when the URL originates from untrusted input.
func hyperlink(url, text string) string {
	if url == "" {
		return text
	}
	for i := 0; i < len(url); i++ {
		if url[i] < 32 || url[i] > 126 {
			return text
		}
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

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

// shortRunID returns the last 4 chars of a run ID.
func shortRunID(id string) string {
	if len(id) < 4 {
		return id
	}
	return id[len(id)-4:]
}

// webhookSeverity classifies how stale the last webhook delivery is, for the
// dashboard freshness indicator.
type webhookSeverity int

const (
	webhookFresh webhookSeverity = iota // normal/idle — quiet, not necessarily broken
	webhookStale                        // moderately old — worth a glance
	webhookDead                         // very old — likely a broken delivery path
)

// Webhook freshness thresholds. Deliberately generous: a quiet repo is not a
// broken one, so only escalate once an absence of deliveries is unusually long.
const (
	webhookStaleAfter = 30 * time.Minute
	webhookDeadAfter  = 2 * time.Hour
)

// webhookFreshnessText returns the humanized indicator text and severity for
// the last webhook delivery time, evaluated relative to now.
//
// Limitation: an absence of webhook events can mean either a genuinely quiet
// repo or a broken delivery path (relay down, port firewalled, etc.). This
// indicator only surfaces the AGE of the last delivery so a human can judge —
// it is NOT a definitive health check.
func webhookFreshnessText(lastWebhookAt, now time.Time) (string, webhookSeverity) {
	if lastWebhookAt.IsZero() {
		return "no events yet", webhookFresh
	}
	age := now.Sub(lastWebhookAt)
	text := "last event " + humanizeDuration(age)
	switch {
	case age >= webhookDeadAfter:
		return text, webhookDead
	case age >= webhookStaleAfter:
		return text, webhookStale
	default:
		return text, webhookFresh
	}
}

// webhookSeverityStyle maps a freshness severity to its lipgloss style.
func webhookSeverityStyle(sev webhookSeverity) lipgloss.Style {
	switch sev {
	case webhookDead:
		return redStyle
	case webhookStale:
		return yellowStyle
	default:
		return dimStyle
	}
}

// humanizeDuration renders a duration in a single compact unit, e.g. "12s",
// "5m", "3h", "1d". Negative durations are clamped to "0s".
func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s < 0 {
			s = 0
		}
		return fmt.Sprintf("%ds", s)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
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
