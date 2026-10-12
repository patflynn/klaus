package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/patflynn/klaus/internal/config"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
)

// TestClassifyByWhoseMoveItIs covers every category and the precedence
// between them: a running agent wins, then done, then yours, then pipeline.
func TestClassifyByWhoseMoveItIs(t *testing.T) {
	now := time.Now()
	n := 0
	var states []*run.State
	pr := func(num int) *string { return strPtr(fmt.Sprintf("https://github.com/o/r/pull/%d", num)) }
	add := func(prompt string, s *run.State) *run.State {
		n++
		s.ID = fmt.Sprintf("20261012-1200-%04d", n)
		s.Prompt = prompt
		s.Type = "launch"
		s.TargetRepo = strPtr("o/r")
		s.CreatedAt = now.Add(-time.Duration(n) * time.Minute).Format(time.RFC3339)
		if s.CostUSD == nil && s.TmuxPane == nil {
			s.CostUSD = float64Ptr(1)
		}
		states = append(states, s)
		return s
	}
	pushed := func(prompt string) *run.State {
		branch := "agent/" + strings.ReplaceAll(prompt, " ", "-")
		return add(prompt, &run.State{PushedBranch: &branch, PushedSHA: strPtr("0123456789abcdef0123456789abcdef01234567")})
	}
	approved := true

	// yours
	add("yours attn", &run.State{NeedsAttention: strPtr("agent/x")})
	submit := pushed("yours submit")
	add("yours ready", &run.State{PRURL: pr(1)})
	add("yours stalled", &run.State{PRURL: pr(2)})
	unknownCheck := pushed("yours unchecked")
	// agents
	add("agents bare", &run.State{TmuxPane: strPtr("%1")})
	add("agents merged pr", &run.State{PRURL: pr(3), TmuxPane: strPtr("%2"), MergedAt: strPtr(now.Format(time.RFC3339))})
	landedRunning := pushed("agents landed")
	landedRunning.TmuxPane, landedRunning.CostUSD = strPtr("%3"), nil
	// pipeline
	add("pipeline pending", &run.State{PRURL: pr(4)})
	add("pipeline failing", &run.State{PRURL: pr(5)})
	add("pipeline conflicts", &run.State{PRURL: pr(6)})
	add("pipeline behind", &run.State{PRURL: pr(7)})
	add("pipeline approved", &run.State{PRURL: pr(8), Approved: &approved})
	add("pipeline draft", &run.State{PRURL: pr(9)})
	add("pipeline unfetched", &run.State{PRURL: pr(10)})
	// done
	add("done merged", &run.State{PRURL: pr(11), MergedAt: strPtr(now.Format(time.RFC3339))})
	add("done closed", &run.State{PRURL: pr(12)})
	add("done no access", &run.State{PRURL: pr(13), NeedsAttention: strPtr("agent/y")})
	integrated := pushed("done integrated")
	content := pushed("done content")
	gone := pushed("done gone")
	add("done exited", &run.State{Worktree: "/wt"})

	ready := func() *prStatus {
		return &prStatus{State: "OPEN", CI: "passing", Conflicts: "none", ReviewDecision: "APPROVED"}
	}
	status := map[int]*prStatus{
		1:  ready(),
		2:  {State: "OPEN", CI: "pending", Conflicts: "none"},
		4:  {State: "OPEN", CI: "pending", Conflicts: "none"},
		5:  {State: "OPEN", CI: "failing", Conflicts: "none"},
		6:  {State: "OPEN", CI: "passing", Conflicts: "yes"},
		7:  {State: "OPEN", CI: "passing", Conflicts: "none", BehindBy: 2},
		8:  ready(),
		9:  {State: "OPEN", CI: "passing", Conflicts: "none", Draft: true},
		12: {State: "CLOSED"},
		13: {State: gh.StateNoAccess},
	}
	m := dashboardModel{
		states:   states,
		tmuxDeps: testDashboardTmuxDeps(),
		ghStatus: map[string]*prStatus{},
		pipelineStates: map[string]*pipeline.PRPipelineState{
			"2": {PRNumber: "2", Stage: pipeline.StageStalled},
		},
		integration: map[string]integration.Verdict{
			submit.ID:        integration.Awaiting,
			unknownCheck.ID:  integration.Unknown,
			landedRunning.ID: integration.Integrated,
			integrated.ID:    integration.Integrated,
			content.ID:       integration.IntegratedContent,
			gone.ID:          integration.Gone,
		},
		hideAfter: defaultHideFinishedAfter,
	}
	for num, ps := range status {
		m.ghStatus[*pr(num)] = ps
	}

	rows := m.buildRows()
	var gotOrder []string
	for _, r := range rows {
		prompt := r.state().Prompt
		gotOrder = append(gotOrder, prompt)
		want := strings.Fields(prompt)[0]
		if got := categoryNames[r.category]; got != want {
			t.Errorf("%q: category %s, want %s", prompt, got, want)
		}
	}
	// Within a category, most recent first: states were created a minute apart
	// in the order added, and the merged PR's merge (now) is its latest
	// activity.
	wantOrder := []string{
		"yours attn", "yours submit", "yours ready", "yours stalled", "yours unchecked",
		"agents merged pr", "agents bare", "agents landed",
		"pipeline pending", "pipeline failing", "pipeline conflicts", "pipeline behind", "pipeline approved", "pipeline draft", "pipeline unfetched",
		"done merged", "done closed", "done no access", "done integrated", "done content", "done gone", "done exited",
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("order:\n got %v\nwant %v", gotOrder, wantOrder)
	}

	for _, r := range rows {
		if r.category != catDone {
			continue
		}
		// An exited agent whose worktree is still there is kept; every other
		// done row may be hidden once old.
		if want := r.state().Prompt != "done exited"; r.hideable != want {
			t.Errorf("%q: hideable = %v, want %v", r.state().Prompt, r.hideable, want)
		}
	}

	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 60})
	plain := ansi.Strip(m.View())
	if !strings.Contains(plain, "5 yours · 3 agents · 7 pipeline · 7 done") {
		t.Errorf("summary should count each category:\n%s", plain)
	}
	for prompt, label := range map[string]string{
		"yours submit":    "SUBMIT",
		"yours unchecked": "SUBMIT",
		"done integrated": "INTEGRATED ",
		"done content":    "INTEGRATED~",
		"done gone":       "GONE",
		"done no access":  "NO ACCESS",
		"yours stalled":   "ATTN",
	} {
		if line := lineWith(plain, prompt); !strings.Contains(line, label) {
			t.Errorf("%q row should show %s: %q", prompt, label, line)
		}
	}
	if line := lineWith(plain, "done no access"); strings.Contains(line, "CI") {
		t.Errorf("a PR we can't read shows no CI: %q", line)
	}
}

// lineWith returns the first line of s that contains sub.
func lineWith(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// TestSubmitThenIntegratedStaysVisible: a run the dashboard saw as SUBMIT
// that then integrates stays visible for the full hide threshold, like a PR
// it watched merge. One it only ever saw integrated is hidden when old.
func TestSubmitThenIntegratedStaysVisible(t *testing.T) {
	branch := "agent/20261012-0900-0001"
	s := &run.State{
		ID: "20261012-0900-0001", Type: "launch", Prompt: "pushed 01", CostUSD: float64Ptr(1),
		CreatedAt:    time.Now().Add(-6 * time.Hour).Format(time.RFC3339),
		PushedBranch: &branch, PushedSHA: strPtr("0123456789abcdef0123456789abcdef01234567"),
	}
	newModel := func() dashboardModel {
		m := dashboardModel{states: []*run.State{s}, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
			hideAfter: defaultHideFinishedAfter}
		return send(m, tea.WindowSizeMsg{Width: 160, Height: 20})
	}
	verdict := func(v integration.Verdict) integrationMsg {
		return integrationMsg{verdicts: map[string]integration.Verdict{s.ID: v}}
	}
	shown := func(m dashboardModel) string {
		return lineWith(ansi.Strip(m.View()), "pushed 01")
	}

	watching := send(newModel(), verdict(integration.Awaiting))
	if line := shown(watching); !strings.Contains(line, "SUBMIT") {
		t.Fatalf("awaiting run should show SUBMIT, got %q", line)
	}
	watching = send(watching, verdict(integration.Integrated))
	if line := shown(watching); !strings.Contains(line, "INTEGRATED") {
		t.Errorf("run seen as SUBMIT should stay visible as INTEGRATED, got %q", line)
	}
	if v := watching.resolve(time.Now().Add(59 * time.Minute)); len(v.rows) != 1 {
		t.Errorf("still within the threshold of the transition: %d rows shown, want 1", len(v.rows))
	}

	// Never seen awaiting (the first check couldn't complete, then found it
	// integrated): it's old, so it's hidden.
	fresh := send(send(newModel(), verdict(integration.Unknown)), verdict(integration.Integrated))
	if line := shown(fresh); line != "" {
		t.Errorf("old run first seen integrated should be hidden, got %q", line)
	}
}

// gitFixture is a bare origin with the run's clone (CloneDir) and an
// operator clone that lands and deletes branches.
type gitFixture struct {
	t                  *testing.T
	origin, clone, ops string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	dir := t.TempDir()
	f := &gitFixture{t: t, origin: filepath.Join(dir, "origin.git"), clone: filepath.Join(dir, "clone"), ops: filepath.Join(dir, "ops")}
	f.git(dir, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(dir, "clone", "-q", f.origin, f.ops)
	f.commitFile(f.ops, "README", "init")
	f.git(f.ops, "push", "-q", "origin", "HEAD:main")
	f.git(dir, "clone", "-q", f.origin, f.clone)
	return f
}

func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) commitFile(dir, name, msg string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.git(dir, "add", name)
	f.git(dir, "commit", "-q", "-m", msg)
}

// push commits files on a new branch off origin's main and pushes it, the
// way a direct-push agent finishes, and returns the run as finalize records it.
func (f *gitFixture) push(id, prompt string, created time.Time, files ...string) *run.State {
	f.t.Helper()
	branch := "agent/" + id
	f.git(f.clone, "fetch", "-q", "origin")
	f.git(f.clone, "checkout", "-q", "-b", branch, "origin/main")
	for _, name := range files {
		f.commitFile(f.clone, name, name+" on "+branch)
	}
	f.git(f.clone, "push", "-q", "origin", branch)
	sha := f.git(f.clone, "rev-parse", "HEAD")
	f.git(f.clone, "checkout", "-q", "--detach")
	f.git(f.clone, "branch", "-q", "-D", branch)
	clone := f.clone
	return &run.State{
		ID: id, Type: "launch", Prompt: prompt, TargetRepo: strPtr("patflynn/cosmo"), CloneDir: &clone,
		Branch: branch, PushedBranch: &branch, PushedSHA: &sha, CostUSD: float64Ptr(1),
		CreatedAt: created.Format(time.RFC3339),
	}
}

func (f *gitFixture) advanceMain(name string) {
	f.t.Helper()
	f.git(f.ops, "fetch", "-q", "origin")
	f.git(f.ops, "checkout", "-q", "-B", "main", "origin/main")
	f.commitFile(f.ops, name, "unrelated "+name)
	f.git(f.ops, "push", "-q", "origin", "main")
}

func (f *gitFixture) fastForward(sha string) {
	f.t.Helper()
	f.git(f.ops, "fetch", "-q", "origin")
	f.git(f.ops, "push", "-q", "origin", sha+":refs/heads/main")
}

// integrate lands a branch as the valley's integrator does when it can't
// fast-forward: one commit, parent main, tree from merge-tree.
func (f *gitFixture) integrate(branch string) {
	f.t.Helper()
	f.git(f.ops, "fetch", "-q", "origin")
	main := f.git(f.ops, "rev-parse", "origin/main")
	tree := f.git(f.ops, "merge-tree", "--write-tree", main, f.git(f.ops, "rev-parse", "origin/"+branch))
	commit := f.git(f.ops, "commit-tree", tree, "-p", main, "-m", "land "+branch)
	f.git(f.ops, "push", "-q", "origin", commit+":refs/heads/main")
}

func (f *gitFixture) rebaseOntoMain(branch string) string {
	f.t.Helper()
	f.git(f.ops, "fetch", "-q", "origin")
	f.git(f.ops, "checkout", "-q", "-B", branch, "origin/"+branch)
	f.git(f.ops, "rebase", "-q", "origin/main")
	f.git(f.ops, "push", "-q", "--force", "origin", branch)
	tip := f.git(f.ops, "rev-parse", "HEAD")
	f.git(f.ops, "checkout", "-q", "--detach")
	return tip
}

func (f *gitFixture) deleteBranch(branch string) {
	f.t.Helper()
	f.git(f.ops, "push", "-q", "origin", "--delete", branch)
}

var octRowRe = regexp.MustCompile(`\b(waiting|landed|active) \d\d\b`)

// TestDashboardOctober12 replays the situation of 2026-10-12: seven pushed
// runs, six of which had already landed on main (by fast-forward, after a
// rebase, or as an integrator commit; some with their branch deleted since),
// one still waiting, and an agent running. The waiting branch is the
// operator's one move and comes first; the six are done.
func TestDashboardOctober12(t *testing.T) {
	f := newGitFixture(t)
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	waiting := f.push("20261012-0700-0007", "waiting 01", ago(3*time.Hour), "w.txt")
	ff := f.push("20261012-0900-0001", "landed 01", ago(50*time.Minute), "a.txt")
	f.fastForward(*ff.PushedSHA)
	ffDeleted := f.push("20261012-0900-0002", "landed 02", ago(45*time.Minute), "b.txt")
	f.fastForward(*ffDeleted.PushedSHA)
	f.deleteBranch(*ffDeleted.PushedBranch)
	rebased := f.push("20261012-0900-0003", "landed 03", ago(40*time.Minute), "c.txt")
	f.advanceMain("u1.txt")
	f.fastForward(f.rebaseOntoMain(*rebased.PushedBranch))
	content := f.push("20261012-0900-0004", "landed 04", ago(35*time.Minute), "d1.txt", "d2.txt")
	f.advanceMain("u2.txt")
	f.integrate(*content.PushedBranch)
	contentDeleted := f.push("20261012-0900-0005", "landed 05", ago(30*time.Minute), "e1.txt", "e2.txt", "e3.txt")
	f.advanceMain("u3.txt")
	f.integrate(*contentDeleted.PushedBranch)
	f.deleteBranch(*contentDeleted.PushedBranch)
	ff2 := f.push("20261012-0900-0006", "landed 06", ago(25*time.Minute), "g.txt")
	f.fastForward(*ff2.PushedSHA)
	running := &run.State{ID: "20261012-1000-0008", Type: "launch", Prompt: "active 01", TargetRepo: strPtr("patflynn/cosmo"),
		TmuxPane: strPtr("%1"), Worktree: "/wt", CreatedAt: ago(5 * time.Minute).Format(time.RFC3339)}
	states := []*run.State{waiting, ff, ffDeleted, rebased, content, contentDeleted, ff2, running}

	newModel := func() dashboardModel {
		m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
			hideAfter: defaultHideFinishedAfter, checker: integration.NewGitChecker(nil)}
		return send(m, tea.WindowSizeMsg{Width: 160, Height: 30})
	}
	check := func(m dashboardModel) dashboardModel {
		t.Helper()
		msg := checkIntegrationCmd(m.checker, m.states)()
		if _, ok := msg.(integrationMsg); !ok {
			t.Fatalf("integration check returned %T", msg)
		}
		return send(m, msg)
	}
	rowsOf := func(view string) []string {
		var rows []string
		for _, line := range strings.Split(ansi.Strip(view), "\n") {
			if p := octRowRe.FindString(line); p != "" {
				rows = append(rows, p)
			}
		}
		return rows
	}

	m := check(newModel())
	view := ansi.Strip(m.View())
	want := []string{"waiting 01", "active 01", "landed 06", "landed 05", "landed 04", "landed 03", "landed 02", "landed 01"}
	if got := rowsOf(view); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows:\n got %v\nwant %v\n%s", got, want, view)
	}
	if !strings.Contains(view, "1 yours · 1 agents · 6 done") {
		t.Errorf("summary should show the one row that's yours:\n%s", view)
	}
	if line := lineWith(view, "waiting 01"); !strings.Contains(line, "SUBMIT") {
		t.Errorf("the waiting branch should be SUBMIT: %q", line)
	}
	for _, p := range []string{"landed 01", "landed 02", "landed 03", "landed 06"} {
		if line := lineWith(view, p); !strings.Contains(line, "INTEGRATED ") {
			t.Errorf("%s landed by fast-forward: %q", p, line)
		}
	}
	for _, p := range []string{"landed 04", "landed 05"} {
		if line := lineWith(view, p); !strings.Contains(line, "INTEGRATED~") {
			t.Errorf("%s landed as an integrator commit: %q", p, line)
		}
	}

	// A dashboard opened two hours later never saw the six waiting: once the
	// check has run, only the waiting branch and the running agent are shown.
	later := check(newModel())
	v := later.resolve(now.Add(2 * time.Hour))
	var shown []string
	for _, r := range v.rows {
		shown = append(shown, r.state().Prompt)
	}
	if strings.Join(shown, ",") != "waiting 01,active 01" || v.hidden != 6 {
		t.Errorf("two hours later: shown %v, %d hidden; want [waiting 01 active 01], 6 hidden", shown, v.hidden)
	}
}

// fakeGHClient answers the status queries the dashboard makes from a table
// keyed by PR URL, and records which PRs were asked about.
type fakeGHClient struct {
	gh.Client
	states  map[string]string
	asked   map[string][]string // PR URL -> queries
	pending map[string]bool     // PR URLs whose CI is pending
}

func (f *fakeGHClient) record(ref, q string) { f.asked[ref] = append(f.asked[ref], q) }
func (f *fakeGHClient) GetState(_ context.Context, ref string) string {
	f.record(ref, "state")
	return f.states[ref]
}
func (f *fakeGHClient) GetCI(_ context.Context, ref string) string {
	f.record(ref, "ci")
	if f.pending[ref] {
		return "pending"
	}
	return "passing"
}
func (f *fakeGHClient) GetConflicts(_ context.Context, ref string) string {
	f.record(ref, "conflicts")
	return "none"
}
func (f *fakeGHClient) GetCommitsBehind(_ context.Context, ref string) int {
	f.record(ref, "behind")
	return 0
}
func (f *fakeGHClient) GetReviewDecision(_ context.Context, ref string) string {
	f.record(ref, "review")
	return "APPROVED"
}
func (f *fakeGHClient) GetLabels(_ context.Context, ref string) []string {
	f.record(ref, "labels")
	return nil
}
func (f *fakeGHClient) IsDraft(_ context.Context, ref string) bool {
	f.record(ref, "draft")
	return false
}

// TestDashboardNoAccessPRs: a PR the agent can't read shows NO ACCESS, is
// done, costs only the state query, and is re-queried every 30 minutes, not
// every poll. Statuses are keyed by URL, so PR #5 in two repos doesn't
// collide.
func TestDashboardNoAccessPRs(t *testing.T) {
	lost := "https://github.com/patflynn/cosmo/pull/5"
	readable := "https://github.com/patflynn/klaus/pull/5"
	states := []*run.State{
		{ID: "20261005-1000-0001", Type: "launch", Prompt: "cosmo fix", TargetRepo: strPtr("patflynn/cosmo"), PRURL: &lost,
			CostUSD: float64Ptr(1), CreatedAt: time.Now().Add(-20 * time.Minute).Format(time.RFC3339)},
		{ID: "20261012-1000-0002", Type: "launch", Prompt: "klaus fix", TargetRepo: strPtr("patflynn/klaus"), PRURL: &readable,
			CostUSD: float64Ptr(1), CreatedAt: time.Now().Add(-10 * time.Minute).Format(time.RFC3339)},
	}
	client := &fakeGHClient{
		states:  map[string]string{lost: gh.StateNoAccess, readable: "OPEN"},
		asked:   map[string][]string{},
		pending: map[string]bool{readable: true},
	}
	m := newDashboardModel(run.NewGitDirStore(t.TempDir()), config.Config{}, client)
	m.states, m.tmuxDeps = states, testDashboardTmuxDeps()
	m = send(m, tea.WindowSizeMsg{Width: 160, Height: 20})

	poll := func() {
		t.Helper()
		m = send(m, m.fetchGHStatusCmd(m.states)())
	}
	poll()
	if got := strings.Join(client.asked[lost], ","); got != "state" {
		t.Errorf("queries for the unreadable PR = %s, want only state", got)
	}
	if len(client.asked[readable]) < 2 {
		t.Errorf("the readable PR #5 in another repo should be fully queried, got %v", client.asked[readable])
	}
	view := ansi.Strip(m.View())
	if line := lineWith(view, "cosmo fix"); !strings.Contains(line, "NO ACCESS") {
		t.Errorf("cosmo #5 should show NO ACCESS: %q", line)
	}
	if line := lineWith(view, "klaus fix"); !strings.Contains(line, "OPEN") || !strings.Contains(line, "CI …") {
		t.Errorf("klaus #5 keeps its own status: %q", line)
	}
	if !strings.Contains(view, "0 yours · 1 pipeline · 1 done") {
		t.Errorf("the unreadable PR is done:\n%s", view)
	}

	// The next polls skip it...
	client.asked = map[string][]string{}
	poll()
	if q := client.asked[lost]; len(q) != 0 {
		t.Errorf("unreadable PR re-queried on the next poll: %v", q)
	}
	if line := lineWith(ansi.Strip(m.View()), "cosmo fix"); !strings.Contains(line, "NO ACCESS") {
		t.Errorf("skipped PR should keep NO ACCESS: %q", line)
	}

	// ...until 30 minutes have passed; then a fixed credential recovers it.
	m.noAccessAt[lost] = time.Now().Add(-31 * time.Minute)
	client.states[lost] = "OPEN"
	poll()
	if q := client.asked[lost]; len(q) < 2 {
		t.Errorf("after 30 minutes the PR should be queried again, got %v", q)
	}
	if _, ok := m.noAccessAt[lost]; ok {
		t.Error("a readable PR should be forgotten as unreadable")
	}
	if line := lineWith(ansi.Strip(m.View()), "cosmo fix"); strings.Contains(line, "NO ACCESS") {
		t.Errorf("recovered PR still shows NO ACCESS: %q", line)
	}
}

// TestSubmitCommandInFooter: with dashboard.submit_command set, selecting a
// SUBMIT row shows the expanded command; other rows don't.
func TestSubmitCommandInFooter(t *testing.T) {
	branch := "agent/20261012-0900-0001"
	states := []*run.State{
		{ID: "20261012-0900-0001", Type: "launch", Prompt: "pushed", TargetRepo: strPtr("patflynn/cosmo"), CostUSD: float64Ptr(1),
			CreatedAt: time.Now().Format(time.RFC3339), PushedBranch: &branch, PushedSHA: strPtr("0123456789abcdef0123456789abcdef01234567")},
		{ID: "20261012-0800-0002", Type: "launch", Prompt: "exited", TargetRepo: strPtr("patflynn/cosmo"), CostUSD: float64Ptr(1),
			CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339)},
	}
	m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
		hideAfter: defaultHideFinishedAfter, submitTemplate: "cd ~/hack/{repo} && valley review {branch}"}
	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 20})

	want := "submit: cd ~/hack/cosmo && valley review " + branch
	if view := ansi.Strip(m.View()); !strings.Contains(view, want) {
		t.Errorf("footer should show %q with the SUBMIT row selected:\n%s", want, view)
	}
	m = press(m, "j")
	if view := ansi.Strip(m.View()); strings.Contains(view, "submit:") {
		t.Errorf("footer should not show a submit command for an exited row:\n%s", view)
	}

	// Integrated: nothing left to submit.
	m = press(m, "k")
	m = send(m, integrationMsg{verdicts: map[string]integration.Verdict{states[0].ID: integration.Integrated}})
	if view := ansi.Strip(m.View()); strings.Contains(view, "submit:") {
		t.Errorf("footer should not show a submit command once integrated:\n%s", view)
	}
}
