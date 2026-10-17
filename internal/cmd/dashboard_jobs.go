package cmd

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/jobs"
)

// The jobs section lists the session's consults and reviews below the run
// rows, as klaus status does: every running job, then those that finished
// within recentJobs. tab moves the selection between the runs and the jobs;
// enter on a job opens its record and answer (showJob) in place of the list.

// jobsLoadedMsg carries the session's job records, newest first.
type jobsLoadedMsg struct {
	jobs []*jobs.Job
}

// jobsFSMsg says a job record changed on disk.
type jobsFSMsg struct{}

// jobDetail is the job whose record and answer the dashboard shows in
// place of the list, scrolled to offset.
type jobDetail struct {
	id     string
	text   string // showJob's output, made printable
	offset int
}

// loadJobsCmd reads the job records. Reading judges which running jobs
// died, so a job killed without a trace shows as died on the next load.
func loadJobsCmd(dir string) tea.Cmd {
	if dir == "" {
		return nil
	}
	return func() tea.Msg {
		list, err := jobs.Store{Dir: dir}.List()
		if err != nil {
			return errHintMsg{msg: "reading jobs: " + err.Error()}
		}
		return jobsLoadedMsg{jobs: list}
	}
}

// recentJobList is what the jobs section lists.
func (m dashboardModel) recentJobList(now time.Time) []*jobs.Job {
	return jobs.Recent(m.jobs, now, recentJobs)
}

// renderJobsHeader renders the line above the job lines, counting the
// running and finished jobs and any that don't fit.
func renderJobsHeader(v dashView) string {
	running := 0
	for _, j := range v.jobs {
		if !j.Finished() {
			running++
		}
	}
	parts := []string{"  jobs"}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	if n := len(v.jobs) - running; n > 0 {
		parts = append(parts, fmt.Sprintf("%d finished", n))
	}
	if n := len(v.jobs) - v.jobsShown; n > 0 {
		parts = append(parts, fmt.Sprintf("%d not shown", n))
	}
	return dimStyle.Render(strings.Join(parts, " · "))
}

// renderJobLine renders one job: its kind and the tail of its ID, its
// thread or PR, backend and model, status, time and prompt.
func renderJobLine(j *jobs.Job, selected bool, now time.Time) string {
	gutter := "  "
	if selected {
		gutter = "> "
	}
	prefix := fmt.Sprintf("%s%-14s  %-14s  %-20s", gutter, j.Kind+":"+shortRunID(j.ID),
		truncateLine(printable(jobTarget(j)), 14), truncateLine(jobBackendModel(j), 20))
	if selected {
		prefix = selectedStyle.Render(prefix)
	}
	status := fmt.Sprintf("%-11s", strings.ToUpper(string(j.Status)))
	return prefix + "  " + jobStatusStyle(j.Status).Render(status) + "  " +
		dimStyle.Render(fmt.Sprintf("%-6s", jobTime(j, now))) + "  " + printable(j.Prompt)
}

func jobStatusStyle(s jobs.Status) lipgloss.Style {
	switch s {
	case jobs.Running:
		return yellowStyle
	case jobs.Succeeded:
		return greenStyle
	case jobs.Interrupted:
		return dimStyle
	}
	return redStyle
}

// renderJobsFooter is the footer while a job is selected: how the job
// stands (its error and raw output, if it didn't succeed), then key help.
func renderJobsFooter(v dashView, width int) string {
	j := v.jobs[v.jobCursor]
	note := j.Kind + ":" + shortRunID(j.ID) + " " + jobStatusText(j)
	if j.Status == jobs.Running {
		note += " for " + jobTime(j, time.Now())
	}
	if j.RawOutput != "" {
		note += " · raw output: " + j.RawOutput
	}
	note = "  " + printable(note)
	help := []string{
		" | enter read · j/k move · tab runs · q quit",
		" | enter read · tab runs",
		"",
	}
	footer := note + help[len(help)-1]
	for _, h := range help {
		if lipgloss.Width(note+h) <= width {
			footer = note + h
			break
		}
	}
	return jobStatusStyle(j.Status).Render(note) + dimStyle.Render(strings.TrimPrefix(footer, note))
}

// jobKey handles a key while a job is selected; handled is false for keys
// that act as they do on the runs (q, r, h).
func (m dashboardModel) jobKey(key string, now time.Time) (dashboardModel, bool) {
	v := m.resolve(now)
	if !v.jobFocus {
		m.jobFocus = false
		return m, false
	}
	move := func(i int) {
		m.jobCursor = clampCursor(i, v.jobsShown)
		m.jobSel = v.jobs[m.jobCursor].ID
	}
	switch key {
	case "up", "k":
		move(v.jobCursor - 1)
	case "down", "j":
		move(v.jobCursor + 1)
	case "g", "home", "pgup":
		move(0)
	case "G", "end", "pgdown":
		move(v.jobsShown - 1)
	case "tab", "esc":
		m.jobFocus = false
	case "enter", "o":
		m.openJobDetail(v.jobs[v.jobCursor], now)
	case "a", "d":
		m.noteError("a job has no PR: tab back to the runs")
	default:
		return m, false
	}
	return m, true
}

// openJobDetail shows j's record and answer in place of the list.
func (m *dashboardModel) openJobDetail(j *jobs.Job, now time.Time) {
	var b strings.Builder
	if err := showJob(&b, jobs.Store{Dir: m.jobsDir}, j, now); err != nil {
		fmt.Fprintf(&b, "\n%v\n", err)
	}
	offset := 0
	if m.detail != nil && m.detail.id == j.ID {
		offset = m.detail.offset
	}
	m.detail = &jobDetail{id: j.ID, text: printable(b.String()), offset: offset}
}

// refreshJobDetail re-reads the open job, so its view follows it finishing.
func (m *dashboardModel) refreshJobDetail(now time.Time) {
	if m.detail == nil {
		return
	}
	for _, j := range m.jobs {
		if j.ID == m.detail.id {
			m.openJobDetail(j, now)
			return
		}
	}
}

// detailLayout wraps the open job's text to the pane and returns its lines
// and how many fit between the header and footer, which a pane under three
// lines goes without.
func (m dashboardModel) detailLayout(width int) (lines []string, bodyH int, chrome bool) {
	lines = strings.Split(strings.TrimRight(ansi.Wrap(m.detail.text, max(width-2, 1), ""), "\n"), "\n")
	switch {
	case m.height <= 0:
		return lines, len(lines), true
	case m.height < 3:
		return lines, m.height, false
	}
	return lines, m.height - 2, true
}

// detailKey scrolls or closes the open job's view.
func (m dashboardModel) detailKey(key string) dashboardModel {
	width := m.width
	if width <= 0 {
		width = defaultDashboardWidth
	}
	lines, bodyH, _ := m.detailLayout(width)
	off := m.detail.offset
	switch key {
	case "up", "k":
		off--
	case "down", "j":
		off++
	case "pgup":
		off -= bodyH
	case "pgdown":
		off += bodyH
	case "g", "home":
		off = 0
	case "G", "end":
		off = len(lines)
	case "esc", "enter", "q", "tab", "backspace":
		m.detail = nil
		return m
	}
	d := *m.detail
	d.offset = clamp(off, 0, max(len(lines)-bodyH, 0))
	m.detail = &d
	return m
}

// renderJobDetail renders the open job's record and answer.
func (m dashboardModel) renderJobDetail(width int) string {
	lines, bodyH, chrome := m.detailLayout(width)
	off := clamp(m.detail.offset, 0, max(len(lines)-bodyH, 0))
	end := min(off+bodyH, len(lines))
	var out []string
	if chrome {
		title := " job " + m.detail.id
		pos := fmt.Sprintf("%d–%d of %d", min(off+1, end), end, len(lines))
		pad := max(width-2-lipgloss.Width(title)-lipgloss.Width(pos), 1)
		out = append(out, headerStyle.Render(title+strings.Repeat(" ", pad)+pos))
	}
	for _, l := range lines[off:end] {
		out = append(out, "  "+l)
	}
	if chrome {
		out = append(out, dimStyle.Render("  j/k scroll · pgup/pgdn page · g/G top/bottom · esc back"))
	}
	for i, l := range out {
		out[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(out, "\n")
}

// printable drops escape sequences and control characters other than
// newlines from text a model or a user wrote, so it can't drive the
// terminal; tabs become spaces.
func printable(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
}
