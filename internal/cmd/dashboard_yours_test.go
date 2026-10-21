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
			"2": {PRNumber: "2", PRURL: *pr(2), Stage: pipeline.StageStalled},
		},
		integration: map[string]integration.Result{
			submit.ID:        {Verdict: integration.Awaiting},
			unknownCheck.ID:  {Verdict: integration.Unknown},
			landedRunning.ID: {Verdict: integration.Integrated},
			integrated.ID:    {Verdict: integration.Integrated},
			content.ID:       {Verdict: integration.IntegratedContent},
			gone.ID:          {Verdict: integration.Gone},
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

// TestChangeRowsGroupRuns covers how runs without a PR group by change, as
// runs on a PR share its row: one row per repo and change ID, judged by the
// valley's view of the change's active version, wherever it was marked
// ready, so earlier versions fold in and a running follow-up puts the row
// with the agents and withholds the approve command. Salvaged work counts
// unless a run that pushed took it on. A change no valley lists is judged by its
// latest push. Runs without a change ID keep a row each.
func TestChangeRowsGroupRuns(t *testing.T) {
	now := time.Now()
	n := 0
	var states []*run.State
	// add records a run made ago minutes ago, on change changeID.
	add := func(prompt, changeID string, ago int, s *run.State) *run.State {
		n++
		s.ID = fmt.Sprintf("20261013-1200-%04d", n)
		s.Prompt, s.Type, s.ChangeID = prompt, "launch", changeID
		if s.TargetRepo == nil {
			s.TargetRepo = strPtr("o/r")
		}
		s.CreatedAt = now.Add(-time.Duration(ago) * time.Minute).Format(time.RFC3339)
		if s.TmuxPane == nil {
			s.CostUSD = float64Ptr(1)
		}
		states = append(states, s)
		return s
	}
	pushed := func(prompt, changeID string, ago int) *run.State {
		sha := fmt.Sprintf("%040d", n+1)
		return add(prompt, changeID, ago, &run.State{PushedBranch: strPtr("agent/" + strings.ReplaceAll(prompt, " ", "-")), PushedSHA: &sha})
	}
	chg := func(c byte) string { return strings.Repeat(string(c), 32) }
	results := map[string]integration.Result{}
	changes := map[integration.ChangeKey]integration.Result{}
	// activeIs records the valley's view of a change's active version.
	activeIs := func(changeID string, v integration.Verdict, version int, head string) {
		changes[integration.ChangeKey{Repo: "r", ChangeID: changeID}] = integration.Result{Verdict: v, Change: &integration.Change{
			Repo: "r", ChangeID: changeID, Handle: changeID[:4], Version: version, Head: head, Active: true}}
	}
	// valley records where a run's version stands; a version that isn't
	// superseded is its change's active one.
	valley := func(s *run.State, v integration.Verdict, version int) {
		results[s.ID] = integration.Result{Verdict: v, Change: &integration.Change{Repo: "r", ChangeID: s.ChangeID,
			Handle: s.ChangeID[:4], Version: version, Head: *s.PushedSHA, Active: v != integration.Superseded}}
		if v != integration.Superseded {
			activeIs(s.ChangeID, v, version, *s.PushedSHA)
		}
	}

	// Two versions: the later one is active, the earlier folds in.
	folded := pushed("folded v1", chg('k'), 30)
	valley(folded, integration.Superseded, 1)
	active := pushed("active v2", chg('k'), 20)
	valley(active, integration.Ready, 2)
	// The valley made an earlier run's version active again.
	reactivated := pushed("reactivated v1", chg('q'), 30)
	valley(reactivated, integration.Ready, 1)
	newer := pushed("newer v2", chg('q'), 20)
	valley(newer, integration.Superseded, 2)
	// A ready version and a follow-up still running.
	readied := pushed("readied v1", chg('l'), 30)
	valley(readied, integration.Ready, 1)
	follow := add("follow-up", chg('l'), 5, &run.State{TmuxPane: strPtr("%1")})
	// The same change ID in another repo is another change.
	elsewhere := pushed("elsewhere", chg('l'), 25)
	elsewhere.TargetRepo = strPtr("o/other")
	elsewhere.PushedSubject = "Port it elsewhere" // titles a change no valley knows
	results[elsewhere.ID] = integration.Result{Verdict: integration.Awaiting}
	// Salvaged work a run that pushed took on is continued; salvage after a
	// push needs the operator.
	salvage := add("continued salvage", chg('m'), 40, &run.State{NeedsAttention: strPtr("agent/x")})
	continuing := pushed("continuing push", chg('m'), 30)
	continuing.TookOver = []string{salvage.ID}
	valley(continuing, integration.Ready, 1)
	before := pushed("pushed first", chg('n'), 40)
	valley(before, integration.Ready, 1)
	add("salvaged later", chg('n'), 30, &run.State{NeedsAttention: strPtr("agent/y")})
	landed := pushed("landed first", chg('r'), 40)
	valley(landed, integration.Integrated, 1)
	add("salvaged after landing", chg('r'), 30, &run.State{NeedsAttention: strPtr("agent/z")})
	// Nothing pushed yet.
	first := add("first run", chg('o'), 3, &run.State{TmuxPane: strPtr("%2")})
	// A version that landed, then gave way, with the rest, to one marked
	// ready elsewhere and awaiting approval.
	landedOld := pushed("landed long ago", chg('s'), 46)
	valley(landedOld, integration.Integrated, 1)
	results[landedOld.ID].Change.Active = false
	valley(pushed("gave way later", chg('s'), 45), integration.Superseded, 2)
	elsewhereHead := strings.Repeat("e", 40)
	activeIs(chg('s'), integration.Ready, 3, elsewhereHead)
	// Every version replaced by one from elsewhere, which landed.
	replaced := pushed("replaced", chg('p'), 50)
	valley(replaced, integration.Superseded, 1)
	activeIs(chg('p'), integration.Integrated, 2, strings.Repeat("f", 40))
	// No change ID: a row each.
	loneA := pushed("lone a", "", 10)
	loneB := pushed("lone b", "", 11)
	results[loneA.ID] = integration.Result{Verdict: integration.Awaiting}
	results[loneB.ID] = integration.Result{Verdict: integration.Awaiting}

	m := dashboardModel{
		states:          states,
		tmuxDeps:        testDashboardTmuxDeps(),
		ghStatus:        map[string]*prStatus{},
		integration:     results,
		changes:         changes,
		hideAfter:       defaultHideFinishedAfter,
		approveTemplate: "valley approve {handle} --head {head}",
	}
	rows := m.buildRows()
	type want struct {
		key, id, category, label string
		runs, running            int
		approve                  string
	}
	wants := map[string]want{
		"active v2":         {"change:o/r#" + chg('k'), "kkkk", "yours", "READY  v2@", 2, 0, "valley approve kkkk --head " + *active.PushedSHA},
		"readied v1":        {"change:o/r#" + chg('l'), "llll", "agents", "READY  agent:" + shortRunID(follow.ID) + " running  v1@", 2, 1, ""},
		"Port it elsewhere": {"change:o/other#" + chg('l'), "llllllll", "yours", "SUBMIT", 1, 0, ""},
		"continuing push":   {"change:o/r#" + chg('m'), "mmmm", "yours", "READY  v1@", 2, 0, "valley approve mmmm --head " + *continuing.PushedSHA},
		"pushed first":      {"change:o/r#" + chg('n'), "nnnn", "yours", "ATTN  v1@", 2, 0, ""},
		"first run":         {"change:o/r#" + chg('o'), "oooooooo", "agents", "agent:" + shortRunID(first.ID) + " running", 1, 1, ""},
		"landed first":      {"change:o/r#" + chg('r'), "rrrr", "yours", "ATTN  v1@", 2, 0, ""},
		"reactivated v1":    {"change:o/r#" + chg('q'), "qqqq", "yours", "READY  v1@", 2, 0, "valley approve qqqq --head " + *reactivated.PushedSHA},
		"gave way later":    {"change:o/r#" + chg('s'), "ssss", "yours", "READY  v3@eeeeeee", 2, 0, "valley approve ssss --head " + elsewhereHead},
		"replaced":          {"change:o/r#" + chg('p'), "pppp", "done", "INTEGRATED  v2@fffffff", 1, 0, ""},
		"lone a":            {"run:" + loneA.ID, "agent:" + shortRunID(loneA.ID), "yours", "SUBMIT", 1, 0, ""},
		"lone b":            {"run:" + loneB.ID, "agent:" + shortRunID(loneB.ID), "yours", "SUBMIT", 1, 0, ""},
	}
	if len(rows) != len(wants) {
		t.Errorf("%d rows, want %d", len(rows), len(wants))
	}
	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 40})
	plain := ansi.Strip(m.View())
	for _, r := range rows {
		desc := r.description()
		w, ok := wants[desc]
		if !ok {
			t.Errorf("unexpected row %q (%s)", desc, r.key)
			continue
		}
		_, cmd := m.approveCommand(r)
		if r.key != w.key || r.idLabel() != w.id || categoryNames[r.category] != w.category ||
			len(r.runs) != w.runs || len(r.running) != w.running || cmd != w.approve {
			t.Errorf("%q: %s %s %s, %d runs, %d running, command %q; want %+v",
				desc, r.key, r.idLabel(), categoryNames[r.category], len(r.runs), len(r.running), cmd, w)
		}
		if line := lineWith(plain, desc); !strings.Contains(line, w.id+" ") || !strings.Contains(line, w.label) {
			t.Errorf("%q row should show %s and %q: %q", desc, w.id, w.label, line)
		}
	}
	if strings.Contains(plain, "SUPERSEDED") || strings.Count(plain, "INTEGRATED") != 1 {
		t.Errorf("a change reads as its active version stands: none SUPERSEDED, and only the one landed elsewhere INTEGRATED:\n%s", plain)
	}
}

// TestPipelineStateIsPerRepo: the pipeline keys PRs by number alone, so its
// state for o/a#5 must not make o/b#5 the operator's or show its stage.
func TestPipelineStateIsPerRepo(t *testing.T) {
	now := time.Now()
	pr := func(repo string) *run.State {
		return &run.State{
			ID: "20261012-1200-" + repo, Type: "launch", Prompt: "repo " + repo, CostUSD: float64Ptr(1),
			TargetRepo: strPtr("o/" + repo), PRURL: strPtr("https://github.com/o/" + repo + "/pull/5"),
			CreatedAt: now.Format(time.RFC3339),
		}
	}
	a, b := pr("a"), pr("b")
	m := dashboardModel{
		states:   []*run.State{a, b},
		tmuxDeps: testDashboardTmuxDeps(),
		ghStatus: map[string]*prStatus{
			*a.PRURL: {State: "OPEN", CI: "failing", Conflicts: "none"},
			*b.PRURL: {State: "OPEN", CI: "pending", Conflicts: "none"},
		},
		pipelineStates: map[string]*pipeline.PRPipelineState{
			"5": {PRNumber: "5", PRURL: *a.PRURL, Stage: pipeline.StageStalled},
		},
		hideAfter: defaultHideFinishedAfter,
	}
	for _, r := range m.buildRows() {
		want := map[string]rowCategory{"repo a": catYours, "repo b": catPipeline}[r.state().Prompt]
		if r.category != want {
			t.Errorf("%q: category %s, want %s", r.state().Prompt, categoryNames[r.category], categoryNames[want])
		}
	}

	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 20})
	plain := ansi.Strip(m.View())
	if line := lineWith(plain, "repo a"); !strings.Contains(line, "stalled") {
		t.Errorf("o/a#5 should show its pipeline stage: %q", line)
	}
	if line := lineWith(plain, "repo b"); strings.Contains(line, "stalled") {
		t.Errorf("o/b#5 should not show o/a#5's pipeline stage: %q", line)
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
		return integrationMsg{results: map[string]integration.Result{s.ID: {Verdict: v}}}
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
		msg := checkIntegrationCmd(m.checker, m.states, nil)()
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
		hideAfter: defaultHideFinishedAfter, approveTemplate: "cd ~/hack/{repo} && valley review {branch}"}
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
	m = send(m, integrationMsg{results: map[string]integration.Result{states[0].ID: {Verdict: integration.Integrated}}})
	if view := ansi.Strip(m.View()); strings.Contains(view, "submit:") {
		t.Errorf("footer should not show a submit command once integrated:\n%s", view)
	}
}

// TestApproveCommandQuotesValues runs the expanded approve command in a real
// shell: values a shell would read as code (a branch an agent named, a
// handle from the valley) arrive as the literal arguments, and plain values
// stay unquoted.
func TestApproveCommandQuotesValues(t *testing.T) {
	pwn := filepath.Join(t.TempDir(), "pwn")
	sha := "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct{ branch, handle string }{
		{"agent/x;touch${IFS}" + pwn, "nxvp"},
		{"agent/it's-$(touch${IFS}" + pwn + ")", "nx'; touch " + pwn + "; '"},
		{"agent/plain-1.2_x", "nxvp"},
	} {
		branch := tc.branch
		s := &run.State{ID: "20261012-0900-0001", Type: "launch", Prompt: "pushed", TargetRepo: strPtr("patflynn/cosmo"),
			CostUSD: float64Ptr(1), CreatedAt: time.Now().Format(time.RFC3339), ChangeID: "nxvp-id",
			PushedBranch: &branch, PushedSHA: &sha}
		m := dashboardModel{states: []*run.State{s}, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
			hideAfter: defaultHideFinishedAfter, approveTemplate: `printf '%s\n' {repo} {branch} {change} {handle} {head}`}
		m = send(m, integrationMsg{results: map[string]integration.Result{s.ID: {Verdict: integration.Ready,
			Change: &integration.Change{Repo: "cosmo", ChangeID: "nxvp-id", Handle: tc.handle, Version: 1, Head: sha}}}})
		v := m.resolve(time.Now())
		verb, cmd := m.approveCommand(v.rows[v.cursor])
		if verb != "approve" {
			t.Fatalf("%s: verb %q, want approve", branch, verb)
		}
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("%s: sh -c %q: %v", branch, cmd, err)
		}
		if want := strings.Join([]string{"cosmo", branch, "nxvp-id", tc.handle, sha}, "\n") + "\n"; string(out) != want {
			t.Errorf("sh -c %q printed %q, want %q", cmd, out, want)
		}
		if _, err := os.Stat(pwn); err == nil {
			t.Fatalf("sh -c %q ran code from a value", cmd)
		}
		if branch == "agent/plain-1.2_x" && !strings.Contains(cmd, " cosmo agent/plain-1.2_x nxvp-id nxvp "+sha) {
			t.Errorf("plain values should stay unquoted: %q", cmd)
		}
	}
}

// TestValleyChangeOutlivesOutageAndDeletedBranch drives the dashboard with a
// real ValleyChecker, a fake valley and real git: a change the valley holds
// as uncertain stays yours through a valley outage and the deletion of its
// branch, showing its last known verdict. A dashboard that never saw the
// valley's view still doesn't count the deleted branch as done.
func TestValleyChangeOutlivesOutageAndDeletedBranch(t *testing.T) {
	f := newGitFixture(t)
	bin := t.TempDir()
	listing := filepath.Join(bin, "changes.json")
	script := "#!/bin/sh\nif [ \"$1\" = changes ] && [ -f '" + listing + "' ]; then exec cat '" + listing + "'; fi\n" +
		"echo 'valley: cannot reach classic-laddie' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := f.push("20261012-0900-0001", "tune the cache", time.Now().Add(-3*time.Hour), "cache.txt")
	s.ChangeID = "uncrkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
	s.Valley = &run.Valley{Repo: "cosmo", Handle: "uncr", Version: 1, Title: "Tune the cache"}
	f.advanceMain("other.txt")
	data := fmt.Sprintf(`{"schema":"valley-changes/v1","host":"classic-laddie","repos":[{"repo":"cosmo","error":null,
		"changes":[{"change_id":%q,"handle":"uncr","state":"ready","active":1,"versions":[{"n":1,"head":%q,
		"title":"Tune the cache","landing":"uncertain","request":{"ref":"refs/valley/requests/uncr","commit":"r"}}]}]}]}`,
		s.ChangeID, *s.PushedSHA)
	if err := os.WriteFile(listing, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	newModel := func() dashboardModel {
		m := dashboardModel{states: []*run.State{s}, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
			hideAfter: defaultHideFinishedAfter, checker: integration.NewValleyChecker(nil)}
		return send(m, tea.WindowSizeMsg{Width: 160, Height: 20})
	}
	check := func(m dashboardModel) dashboardModel {
		return send(m, checkIntegrationCmd(m.checker, m.states, nil)())
	}
	expect := func(m dashboardModel, step string, want ...string) {
		t.Helper()
		v := m.resolve(time.Now())
		if len(v.rows) != 1 || v.rows[0].category != catYours {
			t.Fatalf("%s: the change should be yours: %+v", step, v.rows)
		}
		line := lineWith(ansi.Strip(m.View()), "uncr")
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: row should show %q: %q", step, w, line)
			}
		}
	}

	m := check(newModel())
	expect(m, "uncertain", "UNCERTAIN", "Tune the cache")
	if strings.Contains(ansi.Strip(m.View()), "(last known)") {
		t.Error("a verdict the valley just gave isn't outdated")
	}

	if err := os.Remove(listing); err != nil { // the valley goes down
		t.Fatal(err)
	}
	m = check(m)
	expect(m, "outage", "UNCERTAIN", "(last known)")

	f.git(f.ops, "push", "-q", "origin", "--delete", *s.PushedBranch)
	m = check(m)
	expect(m, "outage, branch deleted", "UNCERTAIN", "(last known)")

	// Without the valley's earlier view, git alone can't settle it either.
	fresh := check(newModel())
	expect(fresh, "first check in the outage, branch deleted", "READY")
	if line := lineWith(ansi.Strip(fresh.View()), "uncr"); strings.Contains(line, "GONE") {
		t.Errorf("a deleted branch shouldn't read GONE for a valley change: %q", line)
	}
}

// TestChangeRowShowsValleysActiveVersion drives the dashboard with a real
// ValleyChecker and a fake valley: a change's row shows where the valley's
// active version stands and approves that version's head, whichever local
// run is newest. A newer push whose ready failed doesn't take the row over;
// the footer notes it. An active version marked ready elsewhere holds the
// row though no local run pushed it.
func TestChangeRowShowsValleysActiveVersion(t *testing.T) {
	bin := t.TempDir()
	listing := filepath.Join(bin, "changes.json")
	script := "#!/bin/sh\n[ \"$1\" = changes ] && exec cat '" + listing + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "valley"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const changeID = "nxvpkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	// pushedRun is a run on the change that pushed head ago minutes ago, as
	// finalize records it.
	pushedRun := func(id string, ago int, head string, valley *run.Valley) *run.State {
		branch := "agent/" + id
		return &run.State{ID: id, Type: "launch", Prompt: "prompt " + id, TargetRepo: strPtr("acme/widget"),
			ChangeID: changeID, ChangeRepo: "widget", PushedBranch: &branch, PushedSHA: &head, Valley: valley,
			CostUSD: float64Ptr(1), CreatedAt: time.Now().Add(-time.Duration(ago) * time.Minute).Format(time.RFC3339)}
	}
	readied := func(version int) *run.Valley {
		return &run.Valley{Repo: "widget", Handle: "nxvp", Version: version, Title: "Add a health check"}
	}
	version := func(n int, head, branch string) string {
		return fmt.Sprintf(`{"n":%d,"head":%q,"branch":%q,"title":"Add a health check v%d","landing":"not-landed"}`, n, head, branch, n)
	}
	writeListing := func(active int, versions ...string) {
		data := fmt.Sprintf(`{"schema":"valley-changes/v1","host":"classic-laddie","repos":[{"repo":"widget","error":null,
			"changes":[{"change_id":%q,"handle":"nxvp","state":"ready","active":%d,"versions":[%s]}]}]}`,
			changeID, active, strings.Join(versions, ","))
		if err := os.WriteFile(listing, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// show checks the states as the dashboard would and returns its one row
	// and its screen.
	show := func(states ...*run.State) (dashRow, string, dashboardModel) {
		t.Helper()
		m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(), ghStatus: map[string]*prStatus{},
			hideAfter: defaultHideFinishedAfter, checker: integration.NewValleyChecker(nil),
			approveTemplate: "valley approve {handle} --head {head} # {branch}"}
		m = send(m, tea.WindowSizeMsg{Width: 200, Height: 20})
		m = send(m, checkIntegrationCmd(m.checker, m.states, nil)())
		rows := m.buildRows()
		if len(rows) != 1 {
			t.Fatalf("%d rows, want the change's one", len(rows))
		}
		return rows[0], ansi.Strip(m.View()), m
	}

	t.Run("newer push not ready", func(t *testing.T) {
		v1 := pushedRun("20261013-0900-0001", 30, sha('1'), readied(1))
		newer := pushedRun("20261013-0900-0002", 5, sha('2'), &run.Valley{Error: "valley ready: refused"})
		writeListing(1, version(1, sha('1'), *v1.PushedBranch))
		r, screen, m := show(v1, newer)
		if r.category != catYours || r.current != v1 {
			t.Errorf("row %s, current %v; want yours, held by v1", categoryNames[r.category], r.current)
		}
		if line := lineWith(screen, "nxvp"); !strings.Contains(line, "READY  v1@1111111") || strings.Contains(line, "SUBMIT") {
			t.Errorf("the row should read v1 as the valley holds it: %q", line)
		}
		if _, cmd := m.approveCommand(r); cmd != "valley approve nxvp --head "+sha('1')+" # "+*v1.PushedBranch {
			t.Errorf("approve command %q should bind v1's head and branch", cmd)
		}
		if !strings.Contains(screen, "approve: valley approve nxvp --head "+sha('1')) ||
			!strings.Contains(screen, "a newer push 2222222 isn't ready") {
			t.Errorf("the footer should offer v1's approval and note the newer push:\n%s", screen)
		}
	})

	t.Run("active version readied elsewhere", func(t *testing.T) {
		v1 := pushedRun("20261013-0900-0001", 30, sha('1'), readied(1))
		v2 := pushedRun("20261013-0900-0002", 20, sha('2'), readied(2))
		writeListing(3, version(1, sha('1'), *v1.PushedBranch), version(2, sha('2'), *v2.PushedBranch),
			version(3, sha('3'), "agent/elsewhere"))
		r, screen, m := show(v1, v2)
		if r.category != catYours || r.current != nil {
			t.Errorf("row %s, current %v; want yours, with no local run holding it", categoryNames[r.category], r.current)
		}
		if line := lineWith(screen, "nxvp"); !strings.Contains(line, "READY  v3@3333333") || !strings.Contains(line, "Add a health check v3") {
			t.Errorf("the row should read v3 as the valley holds it: %q", line)
		}
		if _, cmd := m.approveCommand(r); cmd != "valley approve nxvp --head "+sha('3')+" # agent/elsewhere" {
			t.Errorf("approve command %q should bind v3's head and branch", cmd)
		}
		if strings.Contains(screen, "SUPERSEDED") || strings.Contains(screen, "isn't ready") {
			t.Errorf("local versions fold into the row, and both are versions the valley holds:\n%s", screen)
		}
	})
}

// TestPRRowTakesItsChangesRunsWithoutAPR: a run that stopped at the usage
// limit before opening its PR joins the row of its change's PR, behind the
// PR's agent. Its attention stops counting only once a run records taking
// its work on (TookOver) and is running or finished, whatever the creation
// times say: then nothing reads ATTN, whether the PR is open or merged. A
// successor that ended needing attention or failing carries the attention
// itself, whatever became of its PR: merged, closed or out of reach. A
// follow-up that only resumed it, on a fresh branch, or one that never
// finished, hands nothing off. Salvage newer than a merged PR is
// still the operator's, and a running follow-up without a PR runs on the
// PR's row. A push without a PR after the PR keeps its change's own row
// while it waits for integration, and folds into the PR's once it landed.
func TestPRRowTakesItsChangesRunsWithoutAPR(t *testing.T) {
	now := time.Now()
	n := 0
	var states []*run.State
	chg := func(c byte) string { return strings.Repeat(string(c), 32) }
	add := func(prompt, change string, minutesAgo int, s *run.State) *run.State {
		n++
		s.ID = fmt.Sprintf("20261020-1200-%04d", n)
		s.Prompt, s.Type, s.ChangeID = prompt, "launch", change
		s.TargetRepo = strPtr("o/r")
		s.CreatedAt = now.Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339)
		if s.TmuxPane == nil {
			s.CostUSD = float64Ptr(1)
		}
		states = append(states, s)
		return s
	}
	salvaged := func(prompt, change string, minutesAgo int) *run.State {
		branch := "agent/" + strings.ReplaceAll(prompt, " ", "-")
		return add(prompt, change, minutesAgo, &run.State{
			NeedsAttention: strPtr("session_limit; branch " + branch + " preserved (pushed)"),
			PushedBranch:   &branch, PushedSHA: strPtr(strings.Repeat("a", 40)),
		})
	}
	pr := func(num int) *string { return strPtr(fmt.Sprintf("https://github.com/o/r/pull/%d", num)) }
	open := &prStatus{State: "OPEN", CI: "pending", Conflicts: "none"}
	status := map[string]*prStatus{}

	a := salvaged("stopped before 335", chg('x'), 60)
	status[*add("opened 335", chg('x'), 30, &run.State{PRURL: pr(335), TookOver: []string{a.ID}}).PRURL] = &prStatus{State: "MERGED"}
	// Its successor's clock ran behind: created "before" it.
	b := salvaged("stopped before 334", chg('y'), 60)
	status[*add("opened 334", chg('y'), 90, &run.State{PRURL: pr(334), TookOver: []string{b.ID}}).PRURL] = open
	add("opened 336", chg('z'), 60, &run.State{PRURL: pr(336), MergedAt: strPtr(now.Add(-time.Hour).Format(time.RFC3339))})
	salvaged("stopped after 336 merged", chg('z'), 10)
	running := add("opened 337", chg('v'), 50, &run.State{PRURL: pr(337)})
	status[*running.PRURL] = open
	follow := add("following up 337", chg('v'), 5, &run.State{TmuxPane: strPtr("%1")})
	salvaged("stopped with no follow-up", chg('w'), 20)
	// Resumed on a fresh branch: the successor recorded no handoff.
	fresh := salvaged("stopped before 338", chg('u'), 40)
	status[*add("resumed it on a fresh branch", chg('u'), 15, &run.State{PRURL: pr(338), OriginalRunID: &fresh.ID}).PRURL] = open
	// Its successor took it on and was salvaged in turn, or failed.
	c := salvaged("stopped, taken on, stopped again", chg('t'), 40)
	add("took it on and stopped", chg('t'), 20, &run.State{TookOver: []string{c.ID},
		NeedsAttention: strPtr("session_limit; branch agent/t preserved (pushed)")})
	d := salvaged("stopped, taken on, crashed", chg('s'), 40)
	add("took it on and crashed", chg('s'), 20, &run.State{TookOver: []string{d.ID}, FailureReason: strPtr("error_during_execution")})
	// Its successor continued it under a PR and failed, and the PR merged,
	// closed or went out of reach: the attention it took on stays.
	var failedUnderPR []*run.State
	for i, st := range []string{"MERGED", "CLOSED", gh.StateNoAccess} {
		f := salvaged(fmt.Sprintf("stopped before %d", 341+i), chg(byte('l'+i)), 40)
		failedUnderPR = append(failedUnderPR, f)
		status[*add(fmt.Sprintf("took it on under %d and crashed", 341+i), chg(byte('l'+i)), 20, &run.State{PRURL: pr(341 + i),
			TookOver: []string{f.ID}, FailureReason: strPtr("error_during_execution")}).PRURL] = &prStatus{State: st}
	}
	// Its successor's pane is gone and it never finished.
	e := salvaged("stopped, taken on, vanished", chg('q'), 40)
	add("took it on and vanished", chg('q'), 20, &run.State{TookOver: []string{e.ID}, TmuxPane: strPtr("gone")})
	// A push after the PR waits on its own row until it lands; then it
	// folds into the PR's.
	status[*add("opened 339", chg('r'), 60, &run.State{PRURL: pr(339)}).PRURL] = &prStatus{State: "MERGED"}
	waiting := add("pushed after 339", chg('r'), 30, &run.State{PushedBranch: strPtr("agent/r"), PushedSHA: strPtr(strings.Repeat("b", 40))})
	status[*add("opened 340", chg('p'), 60, &run.State{PRURL: pr(340)}).PRURL] = &prStatus{State: "MERGED"}
	landed := add("pushed and landed after 340", chg('p'), 30, &run.State{PushedBranch: strPtr("agent/p"), PushedSHA: strPtr(strings.Repeat("c", 40))})

	m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(), ghStatus: status, hideAfter: defaultHideFinishedAfter,
		integration: map[string]integration.Result{waiting.ID: {Verdict: integration.Awaiting}, landed.ID: {Verdict: integration.Integrated}}}
	rows := m.buildRows()
	type want struct {
		key, category string
		attention     bool
		runs, running int
	}
	wants := map[string]want{
		"opened 339":                       {"pr:o/r#339", "done", false, 1, 0},
		"pushed after 339":                 {"change:o/r#" + chg('r'), "yours", false, 1, 0},
		"opened 340":                       {"pr:o/r#340", "done", false, 2, 0},
		"opened 335":                       {"pr:o/r#335", "done", false, 2, 0},
		"opened 334":                       {"pr:o/r#334", "pipeline", false, 2, 0},
		"opened 336":                       {"pr:o/r#336", "yours", true, 2, 0},
		"opened 337":                       {"pr:o/r#337", "agents", false, 2, 1},
		"stopped with no follow-up":        {"change:o/r#" + chg('w'), "yours", true, 1, 0},
		"resumed it on a fresh branch":     {"pr:o/r#338", "yours", true, 2, 0},
		"took it on and stopped":           {"change:o/r#" + chg('t'), "yours", true, 2, 0},
		"took it on and crashed":           {"change:o/r#" + chg('s'), "yours", true, 2, 0},
		"took it on and vanished":          {"change:o/r#" + chg('q'), "yours", true, 2, 0},
		"took it on under 341 and crashed": {"pr:o/r#341", "yours", true, 2, 0},
		"took it on under 342 and crashed": {"pr:o/r#342", "yours", true, 2, 0},
		"took it on under 343 and crashed": {"pr:o/r#343", "yours", true, 2, 0},
	}
	if len(rows) != len(wants) {
		t.Errorf("%d rows, want %d", len(rows), len(wants))
	}
	for _, r := range rows {
		prompt := r.state().Prompt
		w, ok := wants[prompt]
		if !ok {
			t.Errorf("unexpected row %q (%s)", prompt, r.key)
			continue
		}
		if r.key != w.key || categoryNames[r.category] != w.category || r.attention != w.attention ||
			len(r.runs) != w.runs || len(r.running) != w.running {
			t.Errorf("%q: %s %s, attention %v, %d runs, %d running; want %+v",
				prompt, r.key, categoryNames[r.category], r.attention, len(r.runs), len(r.running), w)
		}
	}
	superseded := supersededRuns(states, m.agentRunning)
	for _, s := range append([]*run.State{a, b, c, d}, failedUnderPR...) {
		if !superseded[s.ID] {
			t.Errorf("%q was taken on: it should be superseded", s.Prompt)
		}
	}
	for _, s := range []*run.State{fresh, e} {
		if superseded[s.ID] {
			t.Errorf("%q handed nothing off: it should still need attention", s.Prompt)
		}
	}

	m = send(m, tea.WindowSizeMsg{Width: 200, Height: 30})
	plain := ansi.Strip(m.View())
	for _, num := range []string{"#335", "#334"} {
		if line := lineWith(plain, num+" "); line == "" || strings.Contains(line, "ATTN") {
			t.Errorf("%s should show, without ATTN: %q\n%s", num, line, plain)
		}
	}
	if line := lineWith(plain, "#336 "); !strings.Contains(line, "MERGED") || !strings.Contains(line, "ATTN") {
		t.Errorf("salvage after #336 merged should show ATTN on its row: %q", line)
	}
	for _, num := range []string{"#341", "#342", "#343"} {
		if line := lineWith(plain, num+" "); !strings.Contains(line, "ATTN") {
			t.Errorf("the attention %s's failed run took on should show ATTN on its row: %q\n%s", num, line, plain)
		}
	}
	if line := lineWith(plain, "#337 "); !strings.Contains(line, "agent:"+shortRunID(follow.ID)+" running") {
		t.Errorf("the follow-up should run on #337's row: %q", line)
	}
}

// TestSameNamedReposAreDifferentChanges: the same change ID in alice/widget
// and bob/widget names two changes. bob's salvaged run doesn't join alice's
// PR, and alice's run recording it as taken on (it can't, in practice)
// doesn't supersede it. A run that knows its repo only by its short name
// joins the one owner its change's other runs are known by.
func TestSameNamedReposAreDifferentChanges(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	change := strings.Repeat("k", 32)
	branch, sha := "agent/x", strings.Repeat("a", 40)
	bob := &run.State{ID: "20261021-1200-0001", Type: "launch", Prompt: "bob's salvage", ChangeID: change,
		TargetRepo: strPtr("bob/widget"), RepoKey: "bob/widget", CreatedAt: now, CostUSD: float64Ptr(1),
		NeedsAttention: strPtr("session_limit"), PushedBranch: &branch, PushedSHA: &sha}
	alice := &run.State{ID: "20261021-1200-0002", Type: "launch", Prompt: "alice's PR", ChangeID: change,
		TargetRepo: strPtr("widget"), PRURL: strPtr("https://github.com/alice/widget/pull/7"),
		TookOver: []string{bob.ID}, CreatedAt: now, CostUSD: float64Ptr(1)}
	short := &run.State{ID: "20261021-1200-0003", Type: "launch", Prompt: "alice's salvage, by short name", ChangeID: change,
		TargetRepo: strPtr("widget"), CreatedAt: now, CostUSD: float64Ptr(1), NeedsAttention: strPtr("session_limit")}
	states := []*run.State{bob, alice}

	changeOf := changeKeys(states)
	kb, _ := changeOf(bob)
	ka, _ := changeOf(alice)
	if kb == ka || kb.Repo != "bob/widget" || ka.Repo != "alice/widget" {
		t.Errorf("bob's change %+v and alice's %+v should differ, by owner", kb, ka)
	}
	if supersededRuns(states, func(*run.State) bool { return false })[bob.ID] {
		t.Error("a run in alice/widget can't take bob/widget's work on")
	}
	m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(),
		ghStatus: map[string]*prStatus{*alice.PRURL: {State: "MERGED"}}, hideAfter: defaultHideFinishedAfter}
	for _, r := range m.buildRows() {
		if len(r.runs) != 1 {
			t.Errorf("row %s holds %d runs; each repo's run should have its own row", r.key, len(r.runs))
		}
		if r.state() == bob && (!r.attention || r.category != catYours) {
			t.Errorf("bob's salvage should read ATTN, yours: %s attention %v", categoryNames[r.category], r.attention)
		}
	}

	// Known only by its short name, alice's salvage joins alice's change:
	// the one owner known for it among these runs.
	if k, _ := changeKeys([]*run.State{alice, short})(short); k != ka {
		t.Errorf("a run known by its short name should take the one known owner: %+v, want %+v", k, ka)
	}
	// With two owners known, it can't tell which, and joins neither.
	if k, _ := changeKeys([]*run.State{alice, bob, short})(short); k == ka || k == kb {
		t.Errorf("with two owners known, a run known by its short name should join neither: %+v", k)
	}
}

// TestSameNumberedPRsInSameNamedRepos: alice/widget#7 and bob/widget#7 are
// two PRs, though their runs know the repo by its short name, as the
// project registry names a registered widget: each has its own row, with
// its own status. A run that names alice's repo in full shares alice's
// row, whatever the registry calls it.
func TestSameNumberedPRsInSameNamedRepos(t *testing.T) {
	now := time.Now()
	at := func(minutesAgo int) string {
		return now.Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339)
	}
	alicePR, bobPR := "https://github.com/alice/widget/pull/7", "https://github.com/bob/widget/pull/7"
	states := []*run.State{
		{ID: "20261021-1200-0001", Type: "launch", Prompt: "alice's PR", TargetRepo: strPtr("widget"), PRURL: &alicePR, CreatedAt: at(30), CostUSD: float64Ptr(1)},
		{ID: "20261021-1200-0002", Type: "launch", Prompt: "bob's PR", TargetRepo: strPtr("widget"), PRURL: &bobPR, CreatedAt: at(20), CostUSD: float64Ptr(1)},
		{ID: "20261021-1200-0003", Type: "pr-fix", Prompt: "alice's fix", TargetRepo: strPtr("alice/widget"), PRURL: &alicePR, CreatedAt: at(10), CostUSD: float64Ptr(1)},
	}
	m := dashboardModel{states: states, tmuxDeps: testDashboardTmuxDeps(), hideAfter: defaultHideFinishedAfter,
		ghStatus: map[string]*prStatus{alicePR: {State: "MERGED"}, bobPR: {State: "OPEN", CI: "pending", Conflicts: "none"}}}
	rows := m.buildRows()
	got := make(map[string]string)
	for _, r := range rows {
		got[r.key] = fmt.Sprintf("%s, %d runs", categoryNames[r.category], len(r.runs))
	}
	want := map[string]string{"pr:alice/widget#7": "done, 2 runs", "pr:bob/widget#7": "pipeline, 1 runs"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rows %v, want %v", got, want)
	}
}
