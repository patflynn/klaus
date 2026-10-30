package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/jobs"
	"github.com/patflynn/klaus/internal/run"
)

// sampleJobs returns a running threaded consult, a failed one, a succeeded
// review whose answer is in its record, a succeeded consult with an answer
// file, and one that finished too long ago to list.
func sampleJobs(t *testing.T, now time.Time) []*jobs.Job {
	t.Helper()
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	answer := filepath.Join(t.TempDir(), "answer")
	if err := os.WriteFile(answer, []byte("first line of the answer\nsecond line of the answer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []*jobs.Job{
		{ID: "consult-20261017-1200-aaaa0001", Kind: "consult", Thread: "design", Backend: "codex", Model: "gpt-9",
			Prompt: "Design the jobs table", Status: jobs.Running, StartedAt: now.Add(-12 * time.Minute)},
		{ID: "consult-20261017-1150-bbbb0002", Kind: "consult", Backend: "claude", Prompt: "Why did it break",
			Status: jobs.Failed, Error: "consult claude: exit status 7", RawOutput: "/tmp/consult-0002.raw",
			StartedAt: now.Add(-6 * time.Minute), FinishedAt: at(5 * time.Minute)},
		{ID: "review-20261017-1140-cccc0003", Kind: "review", PR: "acme/widget#7", Backend: "agy",
			Status: jobs.Succeeded, StartedAt: now.Add(-23 * time.Minute), FinishedAt: at(20 * time.Minute),
			Review: &jobs.Review{Verdict: "Does what it says.", Findings: nil}},
		{ID: "consult-20261017-1130-dddd0004", Kind: "consult", Backend: "codex", Prompt: "Answer me",
			Status: jobs.Succeeded, AnswerFile: answer, StartedAt: now.Add(-41 * time.Minute), FinishedAt: at(40 * time.Minute)},
		{ID: "consult-20261017-0100-eeee0005", Kind: "consult", Backend: "codex", Prompt: "Long ago",
			Status: jobs.Succeeded, StartedAt: now.Add(-11 * time.Hour), FinishedAt: at(10 * time.Hour)},
	}
}

func newJobsModel(t *testing.T, width, height int) dashboardModel {
	t.Helper()
	now := time.Now()
	m := dashboardModel{
		states: []*run.State{{ID: "20261017-1200-0001", Type: "launch", Prompt: "active 01", TmuxPane: strPtr("%1"),
			CreatedAt: now.Format(time.RFC3339), Worktree: "/wt", TargetRepo: strPtr("testowner/testrepo")}},
		tmuxDeps:  testDashboardTmuxDeps(),
		ghStatus:  map[string]*prStatus{},
		hideAfter: defaultHideFinishedAfter,
	}
	m = send(m, tea.WindowSizeMsg{Width: width, Height: height})
	return send(m, jobsLoadedMsg{jobs: sampleJobs(t, now)})
}

func TestDashboardJobsSection(t *testing.T) {
	m := newJobsModel(t, 140, 30)
	view := ansi.Strip(m.View())
	lines := strings.Split(view, "\n")
	find := func(s string) int {
		for i, l := range lines {
			if strings.Contains(l, s) {
				return i
			}
		}
		return -1
	}
	row, header := find("active 01"), find("jobs · 1 running · 3 finished")
	if row < 0 || header <= row {
		t.Fatalf("jobs section should follow the run rows:\n%s", view)
	}
	for i, want := range [][]string{
		{"consult:0001", "design", "codex gpt-9", "RUNNING", "12m", "Design the jobs table"},
		{"consult:0002", "-", "claude", "FAILED", "1m", "Why did it break"},
		{"review:0003", "acme/widget#7", "agy", "SUCCEEDED", "3m"},
		{"consult:0004", "-", "codex", "SUCCEEDED", "1m", "Answer me"},
	} {
		line := lines[header+1+i]
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("job line %d %q missing %q", i, line, w)
			}
		}
	}
	if strings.Contains(view, "Long ago") {
		t.Errorf("a job that finished 10h ago is listed:\n%s", view)
	}
	if !strings.Contains(view, "o open · tab jobs") {
		t.Errorf("footer should offer tab:\n%s", view)
	}
	// The jobs don't join the run rows.
	if !strings.Contains(view, "1 agents") || strings.Contains(view, "1 done") {
		t.Errorf("summary counts jobs as rows:\n%s", view)
	}
}

// On a short pane the jobs lines go after the spacing and source lines and
// before the errors, the footer and any run row; their header goes last.
func TestDashboardFitLayoutDropsJobsBeforeRows(t *testing.T) {
	m := dashboardModel{recentErrors: []dashboardError{{Time: time.Now(), Message: "boom"}}}
	const rows, nJobs = 10, 3
	prevJobs := nJobs
	for h := 30; h >= 1; h-- {
		m.height = h
		items, bodyH := m.fitLayout(rows, nJobs)
		count := map[chromeKind]int{}
		for _, it := range items {
			count[it.kind]++
		}
		shown := count[chromeJob]
		if shown > prevJobs {
			t.Errorf("height %d: %d job lines, more than at a taller pane", h, shown)
		}
		prevJobs = shown
		if bodyH < min(3, h) && len(items) > 0 {
			t.Errorf("height %d: %d rows while %d chrome lines are kept", h, bodyH, len(items))
		}
		if shown < nJobs && (count[chromeBlank] > 0 || count[chromeSource] > 0) {
			t.Errorf("height %d: a job line went before the spacing and source lines: %+v", h, count)
		}
		if shown > 0 && (count[chromeError] == 0 || count[chromeFooter] == 0 || count[chromeJobs] == 0) {
			t.Errorf("height %d: the errors, footer or jobs header went before a job line: %+v", h, count)
		}
	}

	// The rendered view fits, and names the jobs it had no room for.
	view := newJobsModel(t, 80, 7).View()
	assertFits(t, view, 80, 7)
	if plain := ansi.Strip(view); !strings.Contains(plain, "active 01") || !strings.Contains(plain, "not shown") {
		t.Errorf("short pane should keep the row and count the hidden jobs:\n%s", plain)
	}
}

func TestDashboardSelectJobAndReadIt(t *testing.T) {
	m := newJobsModel(t, 140, 30)
	selected := func(view string) string {
		for _, l := range strings.Split(ansi.Strip(view), "\n") {
			if strings.HasPrefix(l, "> ") {
				return l
			}
		}
		return ""
	}
	if sel := selected(m.View()); !strings.Contains(sel, "active 01") {
		t.Fatalf("the run row starts selected, got %q", sel)
	}

	m = press(m, "tab")
	view := ansi.Strip(m.View())
	if sel := selected(view); !strings.Contains(sel, "consult:0001") || strings.Count(view, "\n> ") != 1 {
		t.Errorf("tab should select the top job, and only it: %q\n%s", sel, view)
	}
	if !strings.Contains(view, "consult:0001 running for 12m | enter read") {
		t.Errorf("footer should say how the running job stands:\n%s", view)
	}

	// The footer of a failed job says why, and where its raw output is.
	m = press(m, "j")
	view = ansi.Strip(m.View())
	if sel := selected(view); !strings.Contains(sel, "consult:0002") ||
		!strings.Contains(view, "failed: consult claude: exit status 7 · raw output: /tmp/consult-0002.raw") {
		t.Errorf("failed job selection %q:\n%s", sel, view)
	}

	// a and d act on PRs, not jobs; q still quits from the jobs.
	m = press(m, "a")
	if view := ansi.Strip(m.View()); !strings.Contains(view, "a job has no PR") {
		t.Errorf("approve on a job should say why it can't:\n%s", view)
	}

	// enter reads a review's answer, its findings, in place of the list; q
	// closes it rather than quitting.
	m = press(m, "j", "enter")
	view = ansi.Strip(m.View())
	if !strings.Contains(view, "Verdict: Does what it says.") || !strings.Contains(view, "pr:         acme/widget#7") || strings.Contains(view, "active 01") {
		t.Errorf("review detail:\n%s", view)
	}
	m = press(m, "q")
	if sel := selected(m.View()); !strings.Contains(sel, "review:0003") {
		t.Fatalf("q should close the detail back onto the job, got %q", sel)
	}

	// A consult's answer, scrolled with j/k; esc goes back, and again to
	// the runs.
	m = press(m, "G", "enter")
	view = ansi.Strip(m.View())
	for _, want := range []string{"job consult-20261017-1130-dddd0004", "status:     succeeded", "second line of the answer", "esc back"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail missing %q:\n%s", want, view)
		}
	}
	m = press(m, "esc", "esc")
	if sel := selected(m.View()); !strings.Contains(sel, "active 01") {
		t.Errorf("esc should go back to the runs, got %q", sel)
	}
}

// The dashboard follows job records through its fsnotify watcher: a job
// appears when its record is written, and changes when it finishes.
func TestDashboardReloadsJobsOnRecordChange(t *testing.T) {
	dir := t.TempDir()
	store := run.NewHomeDirStoreFromPath(dir)
	jobStore := jobs.NewStore(dir)

	// A run's record is told apart from a job's.
	w, ok := startWatcherCmd(store, jobStore.Dir)().(*fsnotify.Watcher)
	if !ok {
		t.Fatal("watcher didn't start")
	}
	if err := store.Save(&run.State{ID: "20261017-1200-0001", Type: "launch", CreatedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if msg := awaitCmd(t, watchFSCmd(w, jobStore.Dir)); msg != (fsEventMsg{}) {
		t.Errorf("a run's record gave %T, want fsEventMsg", msg)
	}
	w.Close()

	w, ok = startWatcherCmd(store, jobStore.Dir)().(*fsnotify.Watcher)
	if !ok {
		t.Fatal("watcher didn't start")
	}
	defer w.Close()
	l := &teaLoop{t: t, msgs: make(chan tea.Msg, 64), m: dashboardModel{
		store: store, jobsDir: jobStore.Dir, states: []*run.State{},
		tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{}, width: 120, height: 30,
	}}
	l.deliver(w)

	job := &jobs.Job{ID: "consult-20261017-1200-abcd0001", Kind: "consult", Backend: "codex", Prompt: "watch me"}
	if err := jobStore.Start(job); err != nil { // by this test process, so it reads as running
		t.Fatal(err)
	}
	l.until("watch me", "RUNNING")
	if err := jobStore.Finish(job, jobs.TimedOut, "timeout after 1s"); err != nil {
		t.Fatal(err)
	}
	l.until("watch me", "TIMED OUT")
}

// awaitCmd runs cmd, failing the test if it doesn't return promptly.
func awaitCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("command didn't return")
	}
	return nil
}

// teaLoop runs a dashboard model's commands and feeds their messages back
// through Update, as bubbletea does.
type teaLoop struct {
	t    *testing.T
	m    dashboardModel
	msgs chan tea.Msg
}

func (l *teaLoop) start(c tea.Cmd) {
	if c != nil {
		go func() { l.msgs <- c() }()
	}
}

func (l *teaLoop) deliver(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			l.start(c)
		}
	default:
		next, c := l.m.Update(msg)
		l.m = next.(dashboardModel)
		l.start(c)
	}
}

// until delivers messages until the view shows every one of want.
func (l *teaLoop) until(want ...string) {
	l.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		view := ansi.Strip(l.m.View())
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(view, w)
		}
		if ok {
			return
		}
		select {
		case msg := <-l.msgs:
			l.deliver(msg)
		case <-deadline:
			l.t.Fatalf("view never showed %q:\n%s", want, view)
		}
	}
}
