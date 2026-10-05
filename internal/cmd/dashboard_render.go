package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
)

// Styling.

var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	greenStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	redStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cyanStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	sandboxStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimRedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Faint(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("237"))
)

// defaultDashboardWidth is assumed until the terminal reports its size.
const defaultDashboardWidth = 80

// chromeKind is a line of the dashboard outside the scrolling run list.
type chromeKind int

const (
	chromeHeader  chromeKind = iota // title, session duration and cost
	chromeSource                    // polling / webhook status
	chromeSandbox                   // sandbox host reachability
	chromeSummary                   // run counts, scroll position, hidden count
	chromeBlank                     // spacing
	chromeError                     // a recent pipeline error
	chromeFooter                    // agent count and key help
)

// chromeItem is one chrome line. Top items render above the run list, the
// rest below it.
type chromeItem struct {
	kind chromeKind
	top  bool
	idx  int // index into recentErrors for chromeError
}

// dropRank orders chrome lines for removal on short panes: the highest rank
// goes first. The summary line, which carries the scroll position, goes last.
func dropRank(k chromeKind) int {
	switch k {
	case chromeBlank:
		return 6
	case chromeSource, chromeSandbox:
		return 5
	case chromeError:
		return 4
	case chromeFooter:
		return 3
	case chromeHeader:
		return 2
	default:
		return 1
	}
}

// fitLayout picks the chrome lines to draw and the number of run rows that
// fit in the pane. On a short pane it drops chrome, least useful first, until
// a few rows fit, so rows are dropped last. An unknown height (0) shows
// everything.
func (m dashboardModel) fitLayout(nRows int) ([]chromeItem, int) {
	items := []chromeItem{{kind: chromeHeader, top: true}, {kind: chromeSource, top: true}}
	if len(m.sandboxHosts) > 0 {
		items = append(items, chromeItem{kind: chromeSandbox, top: true})
	}
	items = append(items,
		chromeItem{kind: chromeSummary, top: true},
		chromeItem{kind: chromeBlank, top: true},
		chromeItem{kind: chromeBlank},
	)
	for i := range m.recentErrors {
		items = append(items, chromeItem{kind: chromeError, idx: i})
	}
	items = append(items, chromeItem{kind: chromeFooter})

	if m.height <= 0 {
		return items, nRows
	}
	// The body needs a line even with no rows, for the "no runs" message.
	minBody := clamp(nRows, 1, 3)
	for len(items) > 0 && m.height-len(items) < minBody {
		// Drop the highest-ranked line; among equals the bottom-most, except
		// errors, where the oldest goes first.
		drop := 0
		for i, it := range items {
			r, best := dropRank(it.kind), dropRank(items[drop].kind)
			if r > best || (r == best && it.kind != chromeError) {
				drop = i
			}
		}
		items = append(items[:drop], items[drop+1:]...)
	}
	return items, clamp(m.height-len(items), 0, nRows)
}

// rowLayout holds the column widths shared by every row, computed over all
// rows so columns don't shift while scrolling.
type rowLayout struct {
	repoW   int // 0 hides the repo column (single-repo lists)
	idW     int
	promptW int // 0 hides the prompt column
}

func newRowLayout(rows []dashRow, width int) rowLayout {
	lay := rowLayout{idW: 6} // "#" plus a five-digit PR number
	repos := make(map[string]bool)
	for _, r := range rows {
		repos[r.repo] = true
		if r.prNum == "" {
			lay.idW = 10 // "agent:" plus a four-character run ID
		}
	}
	if len(repos) > 1 {
		for repo := range repos {
			lay.repoW = max(lay.repoW, len([]rune(shortRepoName(repo))))
		}
		lay.repoW = min(lay.repoW, 14)
		if width < 60 {
			lay.repoW = min(lay.repoW, 6)
		}
	}
	fixed := 2 + lay.idW // gutter and ID
	if lay.repoW > 0 {
		fixed += lay.repoW + 2
	}
	lay.promptW = promptWidth(width, fixed+2)
	return lay
}

// promptWidth shares a row between the prompt and the status labels after
// it. The prompt grows on wide panes and shrinks first on narrow ones, so the
// labels stay visible as long as possible; anything that still doesn't fit is
// cut at the right edge rather than wrapped onto another line.
func promptWidth(width, fixed int) int {
	const statusRoom = 34 // e.g. "OPEN  CI ✓  conflicts ✗  ready  2h"
	const minPrompt, maxPrompt = 12, 48
	if w := width - fixed - statusRoom; w >= minPrompt {
		return min(w, maxPrompt)
	}
	// Narrow pane: keep room for a state label and squeeze the prompt.
	w := clamp(width-fixed-10, 0, minPrompt)
	if w < 4 {
		return 0
	}
	return w
}

// shortRepoName drops the owner from "owner/repo".
func shortRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// renderRow renders one run-list row as a single line.
func (m dashboardModel) renderRow(r dashRow, selected bool, lay rowLayout, now time.Time) string {
	s := r.state()
	var b strings.Builder
	// Gutter marker for the cursor-selected row.
	if selected {
		b.WriteString("> ")
	} else {
		b.WriteString("  ")
	}
	if lay.repoW > 0 {
		fmt.Fprintf(&b, "%-*s  ", lay.repoW, truncateLine(shortRepoName(r.repo), lay.repoW))
	}
	if r.prNum != "" {
		// Pad the visible label first, then wrap only the visible text in a
		// hyperlink so the OSC 8 escape bytes are not counted in the padding
		// and column alignment is preserved.
		label := fmt.Sprintf("#%-*s", lay.idW-1, r.prNum)
		if s.PRURL != nil && *s.PRURL != "" {
			label = hyperlink(*s.PRURL, label)
		}
		b.WriteString(label)
	} else {
		fmt.Fprintf(&b, "%-*s", lay.idW, "agent:"+shortRunID(s.ID))
	}
	if lay.promptW > 0 {
		fmt.Fprintf(&b, "  %-*s", lay.promptW, truncateLine(s.Prompt, lay.promptW))
	}

	prefix := b.String()
	switch {
	case selected:
		prefix = selectedStyle.Render(prefix)
	case r.category == catFinished:
		prefix = dimStyle.Render(prefix)
	}

	var parts []string
	if r.prNum != "" {
		parts = m.prStatusParts(r)
	} else {
		parts = bareAgentParts(r)
	}
	if !r.lastSeen.IsZero() {
		parts = append(parts, dimStyle.Render(humanizeDuration(now.Sub(r.lastSeen))))
	}
	return prefix + "  " + strings.Join(parts, "  ")
}

// prStatusParts returns the status labels for a PR row.
func (m dashboardModel) prStatusParts(r dashRow) []string {
	ps := r.ps
	state := "OPEN"
	for _, s := range r.runs {
		if s.MergedAt != nil {
			state = "MERGED"
		}
	}
	if state != "MERGED" && ps != nil && ps.State != "" {
		state = ps.State
	}

	parts := []string{stateLabel(state)}
	if len(r.running) > 0 {
		parts = append(parts, runningLabel(r.running))
	}
	if r.category == catAttention {
		parts = append(parts, redStyle.Render("ATTN"))
	}

	if ps != nil && state == "OPEN" {
		parts = append(parts, ciLabel(ps.CI))
		if ps.Conflicts == "yes" {
			parts = append(parts, redStyle.Render("conflicts ✗"))
		}
		if ps.BehindBy > 0 {
			parts = append(parts, dimStyle.Render(fmt.Sprintf("behind %d", ps.BehindBy)))
		}
		rd := ps.ReviewDecision
		if strings.EqualFold(rd, "APPROVED") {
			parts = append(parts, greenStyle.Render("ready"))
		} else if strings.EqualFold(rd, "CHANGES_REQUESTED") {
			parts = append(parts, redStyle.Render("changes requested"))
		}
	}

	// Show klaus-internal approval if any run for this PR is approved.
	if state == "OPEN" && isAnyRunApproved(r.runs) {
		parts = append(parts, cyanStyle.Render("✓ approved"))
	}

	// Append pipeline stage if available.
	if pps, ok := m.pipelineStates[r.prNum]; ok {
		parts = append(parts, dimStyle.Render(pipeline.StageLabel(pps.Stage)))
	}
	return parts
}

// runningLabel names the agent running on a PR, e.g. a dispatched CI fix.
func runningLabel(running []*run.State) string {
	if len(running) > 1 {
		return yellowStyle.Render(fmt.Sprintf("%d agents running", len(running)))
	}
	s := running[0]
	return yellowStyle.Render("agent:"+shortRunID(s.ID)+" running") + sandboxTag(s)
}

// bareAgentParts returns the status labels for an agent row with no PR.
func bareAgentParts(r dashRow) []string {
	s := r.state()
	var status string
	switch {
	case len(r.running) > 0:
		status = yellowStyle.Render("RUNNING")
	case r.category == catAttention:
		status = redStyle.Render(agentStatusLabel(s))
	default:
		status = dimStyle.Render(agentStatusLabel(s))
	}
	parts := []string{status, dimStyle.Render(formatCost(s))}
	if tag := sandboxTag(s); tag != "" {
		parts = append(parts, strings.TrimPrefix(tag, " "))
	}
	return parts
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

// renderSummary renders the line above the run list: the repo, counts per
// category, and on the right the scroll position and how many runs are hidden.
func (m dashboardModel) renderSummary(v dashView, width int) string {
	var counts [catFinished + 1]int
	repos := make(map[string]bool)
	for _, r := range v.all {
		counts[r.category]++
		repos[r.repo] = true
	}
	var left []string
	switch len(repos) {
	case 0:
	case 1:
		for repo := range repos {
			left = append(left, repo)
		}
	default:
		left = append(left, fmt.Sprintf("%d repos", len(repos)))
	}
	names := [...]string{"active", "attention", "open", "finished"}
	for c, n := range counts {
		if n > 0 {
			left = append(left, fmt.Sprintf("%d %s", n, names[c]))
		}
	}

	var right []string
	if n := len(v.rows); n > 0 {
		last := min(v.offset+v.bodyH, n)
		first := min(v.offset+1, last)
		right = append(right, fmt.Sprintf("%d–%d of %d", first, last, n))
	}
	if v.hidden > 0 {
		right = append(right, yellowStyle.Render(fmt.Sprintf("%d hidden", v.hidden)))
	} else if m.showAll {
		right = append(right, "all shown")
	}

	// The right side wins on a narrow pane: cut the counts, not the position.
	rightText := strings.Join(right, " · ")
	rw := lipgloss.Width(rightText)
	leftText := ansi.Truncate("  "+strings.Join(left, " · "), max(width-rw-2, 0), "…")
	pad := max(width-lipgloss.Width(leftText)-rw-1, 1)
	return dimStyle.Render(leftText) + strings.Repeat(" ", pad) + rightText
}

// renderFooter renders the agent count and key help, falling back to shorter
// help on narrower panes.
func (m dashboardModel) renderFooter(v dashView, width int) string {
	running, total := 0, 0
	for _, r := range v.all {
		running += len(r.running)
		total += len(r.runs)
	}
	showKey := "h show all"
	if m.showAll {
		showKey = "h hide old"
	}
	variants := []string{
		fmt.Sprintf("  %d/%d agents running | j/k move · pgup/pgdn page · g/G top/bottom · %s · a approve · d discuss · o open | r refresh · q quit",
			running, total, showKey),
		fmt.Sprintf("  j/k pgup/pgdn g/G move · %s · a approve · d discuss · o open · r refresh · q quit", showKey),
		fmt.Sprintf("  j/k pgup/pgdn g/G · %s · a approve · d discuss · o open · q quit", showKey),
	}
	footer := variants[len(variants)-1]
	for _, f := range variants {
		if lipgloss.Width(f) <= width {
			footer = f
			break
		}
	}
	return dimStyle.Render(footer)
}

// renderSourceLine renders the data source status line.
func (m dashboardModel) renderSourceLine() string {
	if !m.useWebhook {
		return dimStyle.Render("  polling: 30s")
	}
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
	// Freshness indicator: humanized age of the last webhook delivery,
	// color-coded so a long silence is visible. It re-renders on the
	// existing 30s tick, so the displayed age advances without a new timer.
	freshText, sev := webhookFreshnessText(m.lastWebhookAt, time.Now())
	return dimStyle.Render(tag) + webhookSeverityStyle(sev).Render(" · "+freshText)
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
