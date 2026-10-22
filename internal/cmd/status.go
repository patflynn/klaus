package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/jobs"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all runs and their current state",
	Long: `Shows all runs in the session and their current state.

STATUS is one of: running (the agent's pane is still running it, judged as
the dashboard judges it: a dead pane, or an idle one once the run finished,
isn't running), needs-attention (finalize salvaged its work, or it failed
after taking on the work of runs that needed attention), superseded (it
needed attention, but a run of its change, the same change ID in the same
repo, took its pushed work on and is running or finished: that run
recorded it, as launch does for a --pr, --change or --resume-from run
continuing a branch that holds the work, and finalize does when the head a
run left on origin contains it; that run carries the attention from then
on, and shows needs-attention itself if it ended needing attention or
failing; neither status nor the dashboard counts the superseded run as
needing attention), pr-created, merged, closed,
no-access (GitHub won't show the PR with the current credentials), exited,
and, for runs that finished by pushing a branch without a PR:

  awaiting-integration  the branch is on origin and hasn't landed
  integrated            its work is on origin's default branch, by ancestry
                        or by an equal patch-id, or the valley says it landed
  branch-gone           the branch was deleted from origin without landing

and, for a branch finalize recorded as a version of its change on a valley
(ready, or a draft when ready_at_finalize is off), where the valley's record
says the change stands:

  awaiting-review       a draft finalize left for review, as it does when
                        ready_at_finalize is off; approve refuses it until
                        'klaus ready' marks it ready once its review passes
  awaiting-approval     ready and waiting for you to approve it
  approved              approved; the integrator hasn't decided
  uncertain             a request was consumed or failed with no landing
                        evidence: look at it
  stale, rejected,      the integrator's decision on the request
  failed
  withdrawn             the change was withdrawn
  superseded            another version of the change is the active one
  draft                 returned to draft ('valley unready'), as launch
                        does when a follow-up run continues the change;
                        approve refuses it until a head is ready again.
                        A draft reads draft even when its landing is
                        uncertain; only landing evidence comes first

A run that isn't running and isn't done shows working while another run of
its change (the same change ID in the same repo) is running: a follow-up
(--pr, --resume-from or a pipeline fix) is still pushing to it, so there is
nothing to approve yet.

A valley change added with klaus track shows where the valley's listing
says its active version stands, by the statuses above (awaiting-approval,
approved, integrated, draft, withdrawn and so on), with its title as the
prompt; it reads tracking while the valley can't be asked. A tracked PR
reads tracking until GitHub says it is merged or closed.

CHANGE names the change a run belongs to: its PR's number (#319), else the
number of its change's PR (the PR of the newest run of the change that has
one), else its valley change's handle, else the first 8 characters of its
change ID, so runs of one change share it, running or not. The handle is the valley
listing's, else (when the valley can't be asked) the one a run of the
change recorded when finalize marked it ready or launch returned it to
draft. A valley run shows its change's title as the
prompt.

Integration is checked with one "valley changes --all --json" when a valley
is involved, and otherwise against each run's clone (one ls-remote and fetch
per repo). If the valley can't be asked, its runs are checked against their
clones. If the check can't complete, the run stays awaiting-integration (or
awaiting-approval, or awaiting-review). A deleted branch alone never settles a valley change: it
stays awaiting-approval rather than branch-gone.

After the runs, a jobs table lists the session's consults and reviews (see
klaus jobs): every one running, and those that finished in the last 3 hours,
with how long each has run or took. It is left out when there are none.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := sessionStore()
		if err != nil {
			return err
		}
		if err := printRuns(cmd.Context(), store); err != nil {
			return err
		}
		printRecentJobs(os.Stdout, store, time.Now())
		return nil
	},
}

// printRecentJobs prints, after a blank line, the jobs table of the
// session's running jobs and those that finished within recentJobs; nothing
// when there are none.
func printRecentJobs(w io.Writer, store run.StateStore, now time.Time) {
	hds, ok := store.(*run.HomeDirStore)
	if !ok {
		return
	}
	list, err := jobs.NewStore(hds.BaseDir()).List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: reading jobs: %v\n", err)
		return
	}
	if list = jobs.Recent(list, now, recentJobs); len(list) > 0 {
		fmt.Fprintln(w)
		printJobsTable(w, list, now)
	}
}

// printRuns prints the runs table, or says there are no runs.
func printRuns(ctx context.Context, store run.StateStore) error {
	tmuxClient := tmux.NewExecClient()
	states, err := store.List()
	if err != nil {
		return err
	}

	if len(states) == 0 {
		fmt.Println("No runs found.")
		return nil
	}

	ghClient := gh.NewGHCLIClient("")

	fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-8s  %-10s  %-10s  %-8s  %-10s  %s\n",
		"RUN ID", "STATUS", "BACKEND", "COST", "ISSUE", "REPO", "HOST", "CHANGE", "CI", "CONFLICTS", "BEHIND", "MERGE", "PROMPT")
	fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-8s  %-10s  %-10s  %-8s  %-10s  %s\n",
		"------", "------", "-------", "----", "-----", "----", "----", "------", "--", "---------", "------", "-----", "------")

	statuses := make([]string, len(states))
	running := make(map[string]bool)
	for i, s := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		statuses[i] = determineStatus(ctx, s, tmuxClient)
		running[s.ID] = statuses[i] == "running"
	}
	// A run whose work a later run took on hands its attention to that run.
	superseded := supersededRuns(states, func(s *run.State) bool { return running[s.ID] })
	var pushed []*run.State
	for i, s := range states {
		if statuses[i] == "needs-attention" && superseded[s.ID] {
			statuses[i] = "superseded"
		}
		if statuses[i] == "awaiting-integration" || integration.TracksChange(s) {
			pushed = append(pushed, s)
		}
	}
	// Where pushed branches stand: one valley listing for the runs a
	// valley knows and the tracked valley changes, one check per repo
	// against each run's own clone for the rest.
	report := integration.NewValleyChecker(nil).Check(ctx, pushed)
	handles := changeHandles(states, report.Changes)
	prs := changePRs(states)

	// The changes some run is still running on: any other run of one
	// that isn't done is working.
	changeOf := changeKeys(states)
	busy := make(map[integration.ChangeKey]bool)
	for i, s := range states {
		if k, ok := changeOf(s); ok && statuses[i] == "running" {
			busy[k] = true
		}
	}

	for i, s := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		status := statuses[i]
		prompt := truncate(s.Prompt, 40)
		var change *integration.Change
		if status == "awaiting-integration" {
			res := report.Runs[s.ID]
			change = res.Change
			if change == nil {
				change = integration.RecordedChange(s)
			}
			status = pushedStatus(res.Verdict, change != nil)
			if change != nil && awaitsReview(s, res.Verdict) {
				status = "awaiting-review"
			}
			if change != nil && change.Title != "" {
				prompt = truncate(change.Title, 40)
			}
		}
		// A tracked valley change stands where the valley's listing says
		// its active version does; tracking while the valley can't be
		// asked.
		if integration.TracksChange(s) {
			if view := report.Changes[valleyKey(s)]; view.Change != nil {
				status = pushedStatus(view.Verdict, true)
				if view.Change.Title != "" {
					prompt = truncate(view.Change.Title, 40)
				}
			}
		}
		cost := formatCost(s)
		issue := "-"
		if s.Issue != nil {
			issue = *s.Issue
		}
		repo := "-"
		if s.TargetRepo != nil {
			repo = truncate(*s.TargetRepo, 20)
		}
		host := "-"
		if s.Host != nil {
			host = *s.Host
		}
		ci, conflicts, behind, merge := "-", "-", "-", "-"
		if prRef := extractPRRef(s); prRef != "" {
			prState := ghClient.GetState(ctx, prRef)
			switch prState {
			case "MERGED":
				status = prSettled(status, s, "merged")
				merge = "merged"
			case "CLOSED":
				status = prSettled(status, s, "closed")
			case gh.StateNoAccess:
				// GitHub won't show this PR to us: nothing else to ask.
				status = prSettled(status, s, "no-access")
			default:
				ci = ghClient.GetCI(ctx, prRef)
				conflicts = ghClient.GetConflicts(ctx, prRef)
				behindBy := ghClient.GetCommitsBehind(ctx, prRef)
				if behindBy > 0 {
					behind = fmt.Sprintf("%d", behindBy)
				}
				merge = computeMergeStatus(ci, conflicts, ghClient.GetReviewDecision(ctx, prRef), behindBy)
			}
		}

		if k, ok := changeOf(s); ok && busy[k] && live(status) {
			status = "working"
		}

		fmt.Fprintf(os.Stdout, "%-22s  %-10s  %-8s  %-8s  %-6s  %-20s  %-15s  %-8s  %-10s  %-10s  %-8s  %-10s  %s\n",
			s.ID, status, runBackendName(s), cost, issue, repo, host, changeLabel(s, changeOf, prs, handles), ci, conflicts, behind, merge, prompt)
	}

	return nil
}

// prSettled is the status of run s, status so far, once GitHub says its PR
// is settled (merged, closed or out of reach): settled, unless the run
// still needs attention it took on with other runs' work (TookOver), which
// its PR's fate settles nothing about, as on the dashboard.
func prSettled(status string, s *run.State, settled string) string {
	if status == "needs-attention" && len(s.TookOver) > 0 {
		return status
	}
	return settled
}

func determineStatus(ctx context.Context, s *run.State, tc tmux.Client) string {
	if s.Type == "session" {
		if _, err := os.Stat(s.Worktree); err == nil {
			return "active"
		}
		return "ended"
	}

	if s.Type == "track" {
		if s.MergedAt != nil {
			return "merged"
		}
		return "tracking"
	}

	// Running as the dashboard decides it: a pane that exists isn't enough
	// when its command is dead, or, once the run finalized, idle.
	if s.IsAgentRunningWith(run.TmuxDepsFor(ctx, tc)) {
		return "running"
	}

	// The attention a run took on with other runs' work (TookOver) outlasts
	// its PR's merge, as on the dashboard.
	if needsAttention(s) && len(s.TookOver) > 0 {
		return "needs-attention"
	}
	if s.MergedAt != nil {
		return "merged"
	}
	if needsAttention(s) {
		return "needs-attention"
	}
	if s.PRURL != nil {
		return "pr-created"
	}
	if s.PushedSHA != nil {
		// Direct-push finish: the branch on origin waits for the operator.
		// The status command refines this with an integration check.
		return "awaiting-integration"
	}
	return "exited"
}

// live reports whether a status leaves something to do for a run that isn't
// running: salvaged work, an open PR, or a pushed branch that hasn't landed,
// gone, or been withdrawn or superseded.
func live(status string) bool {
	switch status {
	case "needs-attention", "pr-created", "awaiting-integration", "awaiting-review", "awaiting-approval",
		"approved", "uncertain", "stale", "rejected", "failed", "draft":
		return true
	}
	return false
}

// pushedStatus is the status of a run awaiting integration once an
// integration check has looked at it; valley says a valley knows the run's
// change. An unknown verdict (no clone, origin unreachable) keeps it
// awaiting.
func pushedStatus(v integration.Verdict, valley bool) string {
	switch v {
	case integration.Integrated, integration.IntegratedContent:
		return "integrated"
	case integration.Gone:
		return "branch-gone"
	case integration.Ready:
		return "awaiting-approval"
	case integration.Unknown, integration.Awaiting:
		if valley {
			return "awaiting-approval"
		}
		return "awaiting-integration"
	}
	// approved, uncertain, stale, rejected, failed, withdrawn, superseded,
	// draft
	return v.String()
}

// awaitsReview reports whether a valley change's version, which run s
// pushed and a check says v of, awaits review rather than approval:
// finalize left it a draft (ready_at_finalize off), klaus ready hasn't
// marked it ready since, and the valley says it is still a draft, or can't
// be asked. A draft whose review passed is marked ready with klaus ready.
func awaitsReview(s *run.State, v integration.Verdict) bool {
	if s == nil || s.Valley == nil || !s.Valley.Draft {
		return false
	}
	return v == integration.Draft || v == integration.Unknown || v == integration.Awaiting
}

func formatCost(s *run.State) string {
	if s.CostUSD != nil {
		return fmt.Sprintf("$%.2f", *s.CostUSD)
	}
	if s.Budget != nil {
		return fmt.Sprintf("<$%s", *s.Budget)
	}
	return "-"
}

// changeLabel names the change a run belongs to, for the CHANGE column: its
// PR's number (#319), else its change's PR's (changePRs), else its valley
// change's handle (changeHandles), else the first 8 characters of its
// change ID, as the dashboard names a PR's or a change's row; "-" for a run
// with none of them.
func changeLabel(s *run.State, changeOf func(*run.State) (integration.ChangeKey, bool), prs, handles map[integration.ChangeKey]string) string {
	k, _ := changeOf(s)
	switch {
	case s.PRURL != nil:
		return formatPR(s)
	case prs[k] != "":
		return prs[k]
	case handles[k] != "":
		return handles[k]
	case s.ChangeID != "":
		return s.ChangeID[:min(8, len(s.ChangeID))]
	}
	return "-"
}

// changePRs names each change some run with a PR belongs to by that PR's
// number, the newest such run's, as the dashboard puts the change's runs
// without a PR on that PR's row.
func changePRs(states []*run.State) map[integration.ChangeKey]string {
	changeOf := changeKeys(states)
	out := make(map[integration.ChangeKey]string)
	by := make(map[integration.ChangeKey]*run.State) // the run whose PR names each change
	for _, s := range states {
		k, ok := changeOf(s)
		if !ok || s.PRURL == nil || by[k] != nil && createdBefore(s, by[k]) {
			continue
		}
		out[k], by[k] = formatPR(s), s
	}
	return out
}

// changeHandles names each valley change some run belongs to, so every run
// on it, running or not, shows the same handle: the valley's listing's, which
// is current (a handle lengthens when another change comes to share its
// prefix); else, when the valley can't be asked or doesn't list the change,
// the handle the latest run to record one got from finalize's ready or
// launch's unready.
func changeHandles(states []*run.State, listed map[integration.ChangeKey]integration.Result) map[integration.ChangeKey]string {
	changeOf := changeKeys(states)
	out := make(map[integration.ChangeKey]string)
	by := make(map[integration.ChangeKey]*run.State) // the run whose record names each unlisted change
	for _, s := range states {
		k, ok := changeOf(s)
		if !ok {
			continue
		}
		if c := listed[valleyKey(s)].Change; c != nil {
			out[k] = c.Handle
			continue
		}
		h := recordedHandle(s)
		if h == "" || by[k] != nil && createdBefore(s, by[k]) {
			continue
		}
		out[k], by[k] = h, s
	}
	return out
}

// recordedHandle is the handle a run recorded for its change: from
// finalize's ready, else from launch's unready; "" when neither got one.
func recordedHandle(s *run.State) string {
	if s.Valley != nil && s.Valley.Handle != "" {
		return s.Valley.Handle
	}
	return s.UnreadyHandle()
}

func formatPR(s *run.State) string {
	if s.PRURL == nil {
		return "-"
	}
	url := *s.PRURL
	// Extract PR number from URL
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return "#" + url[i+1:]
		}
	}
	return url
}

// extractPRNumber returns the PR number (e.g. "18") from a run state, or "".
func extractPRNumber(s *run.State) string {
	if s.PRURL == nil {
		return ""
	}
	url := *s.PRURL
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			return url[i+1:]
		}
	}
	return ""
}

// extractPRRef returns the full PR URL from a run state (for use with gh CLI),
// or "" if no PRURL is set. Using the full URL ensures gh can find the PR
// regardless of the current working directory.
func extractPRRef(s *run.State) string {
	if s.PRURL == nil {
		return ""
	}
	return *s.PRURL
}

// computeMergeStatus determines overall merge readiness. behind is the base-ahead
// commit count; a would-be-ready-but-behind PR reads "behind N" (rebased at merge)
// so it is never shown as a clean "ready".
func computeMergeStatus(ci, conflicts, reviewDecision string, behind int) string {
	if conflicts == "yes" {
		return "blocked"
	}
	if ci == "failing" {
		return "blocked"
	}
	if strings.EqualFold(reviewDecision, "CHANGES_REQUESTED") {
		return "blocked"
	}
	if ci == "pending" || reviewDecision == "unknown" {
		return "pending"
	}
	if ci == "passing" && conflicts == "none" && (strings.EqualFold(reviewDecision, "APPROVED") || reviewDecision == "") {
		if behind > 0 {
			return fmt.Sprintf("behind %d", behind)
		}
		return "ready"
	}
	return "pending"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runBackendName(s *run.State) string {
	if s.Type == "track" {
		return "-"
	}
	if s.Backend == "" {
		return "claude"
	}
	return s.Backend
}
