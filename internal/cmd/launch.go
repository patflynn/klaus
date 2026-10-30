package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/patflynn/klaus/internal/valley"
	"github.com/spf13/cobra"
)

var launchCmd = &cobra.Command{
	Use:   "launch \"<prompt>\" | --prompt-file <path> [flags]",
	Short: "Launch an autonomous Claude Code agent",
	Long: `Creates a git worktree, launches Claude Code in autonomous mode in a new
tmux pane, and tracks the run state. Must be run inside a tmux session.

The agent's pane goes into a detached tmux session (klaus-agents-<session-id>),
so your window is never split — watch agents with the dashboard, 'klaus status',
and 'klaus logs'. Because the tmux server owns that session, agents keep running
if the coordinator exits. Set agent_display to "pane" in .klaus/config.json to
split the current window instead.

A local worker runs without $TMUX or $TMUX_PANE, with TMUX_TMPDIR set to its
run's own empty directory (tmux/<run-id> in the session directory), so a tmux
command it runs, 'tmux kill-server' included, can't reach the server the
agents' panes and your session live on. klaus commands it runs still reach
that server through KLAUS_TMUX_SOCKET. It also gets KLAUS_SCRATCH, its run's
own directory outside the worktree (scratch/<run-id> in the session
directory) for build output and other scratch files; finalize removes it with
the worktree. A sandbox worker gets neither.

A local worker gets launch's own PATH: tmux gives a pane the PATH of the
client that creates it. Launch fails, before creating a worktree, when a
command the worker runs by name isn't on that PATH: the backend's CLI and,
for agy, bash, which runs every command agy's run_command tool runs. The
error names the command and the PATH it searched.

Just before the agent starts, launch snapshots the files the clone shares
across its worktrees and that decide what git does there: its config (less
the branch tracking git push -u writes), info/attributes and hooks. Finalize
compares them; on a difference it emits agent:shared-git-changed naming the
files, and klaus status shows the run needs-attention with a line naming
them. A worker can't be kept from writing them: it runs as you.

The prompt comes either from the positional argument or from --prompt-file
<path>; exactly one of the two is required. Prefer --prompt-file for long or
technical prompts: in zsh a backtick inside a double-quoted argument is command
substitution, so code spans in a shell-quoted prompt are silently mangled before
klaus ever sees them. File contents are used verbatim.

Either way klaus saves the prompt to prompts/<run-id>.md in the session
directory, kept as the record of the brief, and every worker reads it on stdin,
so a prompt of any length fits. agy, which has no system prompt flag, reads the
system prompt ahead of it, from prompts/<run-id>.input.md. If what still goes
on the tmux command line (the worker system prompt for codex and sandbox claude
runs) exceeds tmux's limit, launch fails before creating a worktree.

Use --repo to launch an agent against a different repository. If the name
matches a registered project (no owner/ prefix), the project's local path is
used directly. Otherwise, the repo is cloned from GitHub.

The agent is told to deliver by opening a PR when the target repo's origin is
on github.com. For any other origin (a plain ssh or local remote) it is told to
push its branch and not open a PR, and to end with a report of the branch, head
SHA, diff summary and checks run, for the operator to integrate from.

On a valley project, --issue takes a knot from the project's plan graph
instead of a GitHub issue: its ID (oc-, bd-, ida- or qn- and 7 hex digits),
or <project>:<id> for a knot in another project's graph on the same valley.
The repo must deliver to a valley, as finalize decides it, whose 'valley help'
lists knot; klaus reads and writes knots only through 'valley knot'. Launch
reads the knot, which must be an active outcome or bug, names the branch from
its title, and appends the knot, its parent chain and its comments to the
brief in a delimited "Project notes (untrusted)" block, as data the agent is
told never to take as instructions. Just before the agent starts it claims the
knot for the run ('valley knot claim <id> --run <run-id>'). A knot claimed by
a run that is still running, or by a run klaus doesn't know, is refused; the
claim of a run klaus knows has ended is released and taken over. At finalize,
a run whose pushed head the valley took links its change to the knot ('valley
knot link <id> --change valley:<repo>/<change-id>'); a run that failed,
paused or pushed nothing the valley took comments on the knot instead. Either
way the run's claim is cleared. Once the dashboard sees the change landed, it
comments on the knot with the landed commit, and sets it done (an outcome) or
closed (a bug) only if every change it links has landed, none of its children
is active and no other run claims it. Other knots that link the change get
the comment alone. A knot can't be combined with --pr.

An agent that opens a PR is told to request review from pr_reviewer. When
pr_reviewer is unset, launch picks a collaborator with push access to the
target repo, leaving out bots and the gh user the agent runs as (the PR's
author): the repo owner if they are one, else the only one. When none is
found, or a gh call fails, launch warns and the agent requests no review.

Use --pr to push fixes to an existing PR's branch instead of creating a new
PR. The agent will commit and push to the PR branch directly. This is also
how you resume a budget-paused PR: relaunch against the paused PR and the
follow-up picks up from the WIP commit klaus left on the branch. When the
follow-up agent's _finalize runs, the 'klaus:budget-paused' label is cleared
automatically.

Use --change <ref> to continue a change in a repo that delivers without PRs
(origin not on GitHub), as --pr continues a PR. <ref> is a run ID, a full
change ID or a valley handle. A run ID names that run's change. A change ID or
handle names the change of the runs recorded in any session for the target
repo whose change ID it is or starts, or whose recorded valley handle it is;
when no run matches and the repo delivers to a valley that lists changes,
'valley changes --json' is asked. A ref that names several changes is an error
listing them. The worktree tracks the change's branch on origin: its active
version's when the valley lists one, else the branch its newest run pushed.
Launch fails, naming the change and its last known head, when that branch
isn't on origin. The run keeps the change ID, returns the change to draft as
below, continues the conversation of the change's most recent run when its
backend can (below), and is told to push fast-forward to the same branch;
finalize marks the new head ready. In a repo that delivers to a valley, the
agent is also told never to merge another branch into the change, and that
when the brief asks it to rebase, or the change conflicts with origin/main,
it may restack it: rebase it onto origin/main and push it to the same branch
with 'git push --force-with-lease=refs/heads/<branch>:<head> origin
HEAD:refs/heads/<branch>', <head> being the branch's tip on origin when
launch checked it, which the worktree starts from. Finalize marks the rebased
head ready as the next version of the same change. Every backend keeps the
branch, the change ID and the provenance trailers; only the conversation
differs: Claude replays the stored conversation, on any machine that has the
data ref, Codex forks its own session on the same machine, and agy starts
fresh.
With --resume-from, that run's conversation is used, and --change still picks
the branch and the change ID. When that conversation can't be resumed, the
agent starts fresh rather than replaying another, for --pr as for --change, and
the replay: line says so.

--pr and --change, and --resume-from when it continues its run's branch
(below), refuse while a run in any session is still running on the branch
they would work on, or on the change they continue (for --pr, the change of
the run that opened the PR), in the target repo, naming that run. Wait for
it to finish, or stop it and clean it up ('klaus cleanup --force <run>',
which removes its worktree with any uncommitted work), then launch again.
Launch never removes a worktree to make way: while one has the branch
checked out, whether a run's or your own checkout, it refuses, naming it
and saying whether it holds uncommitted changes or commits origin lacks;
clean that run up, or remove the worktree yourself, first. Nor does it
discard commits on the local branch that aren't on origin/<branch>: it
refuses, naming the branch. --force-takeover launches past a run still
working on the branch or change, and resets the local branch to origin's
tip, discarding such commits; it never removes a worktree either. A new
run's worktree path must not exist. These launches, in one clone, take a
lock from the check for a running holder through the new run's
registration, so two of them can't both pass it: the second waits up to 5
minutes, saying so, then finds the first run and refuses.

A run without --pr or --change gets a new branch named <slug>-<tail>. The tail
is the random end of the run ID (bd92f081 for 20261011-1843-bd92f081). The slug
comes from the --issue title (a GitHub issue's or a knot's), else the prompt's first '# ' heading, else its
first non-empty line: a leading '<repo-name>:' is dropped, the text is
lowercased, each run of characters outside [a-z0-9] becomes one '-', and it is
cut at a word boundary to at most 40 characters. A brief that opens with a
descriptive '# title' names its branch. When no slug comes out, or git rejects
the name, the branch is agent/<run-id>. Use --branch <name> to name it exactly:
the name must pass 'git check-ref-format --branch' and must not exist locally or
on origin.
--branch, --pr and --change are mutually exclusive.

Every commit the agent makes carries two provenance trailers, added by a
commit-msg hook scoped to its worktree: 'Change-Id: <change id>' and
'Klaus-Run: <run id>'. A --pr run keeps the change ID of the run that opened the
PR, a --change run that of the change it names, a --resume-from run that of the
run it resumes; any other run gets a new one. The trailers label commits for
finding and auditing; they grant nothing.

A run that keeps an existing change in a repo that delivers to a valley
returns the change to draft before its agent starts ('valley unready
<change-id>', within 30 seconds), so it can't be approved while the agent
replaces it; finalize marks the pushed head ready again. This happens only
when 'valley help' lists unready. A change that wasn't ready is left as is,
and a failure is recorded on the run and printed as a warning; neither stops
the launch.

Use --resume-from <run-id> to continue a previous run's Claude conversation in
a fresh worktree, paused or not — the follow-up keeps what the earlier agent
learned instead of re-exploring the repo. Prior errors produce a warning but do
not prevent resume. It starts fresh if the transcript cannot be located or
staged.

Without --pr, --change or --branch, a --resume-from run continues the branch
its run pushed without a PR, as when finalize salvaged that run's work at a
usage limit, when the branch is still that change's and nothing took the
change further: the run is in the target repo (by name and, where both are
known, by GitHub owner); no run of the change has a PR; origin has the
branch; no run of another change worked on the branch, and origin's tip
carries no other change's Change-Id; the change's branch (its valley's
active version's, else its newest push's) is still that branch; that branch
has that run's push in its history, or a later head of the change that
rewrote it, as a restack does: a later run's push there, or the head the
change's branch was found at; and, for a repo on GitHub, no PR, open or not,
has the branch as its head (asked with 'gh api'). The run keeps the change ID, its
worktree tracks origin's branch, and it is told to push fast-forward to it
(and, where the repo takes PRs, to open the PR from it), as --change does; it
refuses as --change does (above). Otherwise, as when GitHub can't be asked,
the run gets a new branch instead and the resume: line of the launch output
says why. A run that pushed no branch, or opened a PR, gets a new branch too.

A --pr, --change or --resume-from run that continues a branch records, as
took_over, the runs of its change that needed attention and whose pushed
heads its starting head contains (the takes on: line names them), and
finalize adds those whose pushed heads the head a run left on origin
contains. Such a run carries their attention from then on: klaus status
shows them superseded once it is running or finished, and if it in turn
ended needing attention or failing, it shows needs-attention itself, even
once its PR is merged, closed or out of reach. A --resume-from run on a new
branch takes nothing on.

For a budget-paused PR, and for any --change run, klaus continues the
conversation of the most recent run on it by default (trajectory replay),
avoiding a cold re-exploration of the repo. Only that run counts: an older
conversation never saw the work after it. A Claude run restores a Claude
run's stored conversation and runs 'claude --resume'; the conversation is read
from the local clone's data ref, which is pushed to origin only when
push_data_ref is true, so by default replay works on the machine that ran the
earlier agent. A Codex run forks a Codex run's local session ('codex exec
fork'), on the same machine. agy workers can't resume a conversation. The run
starts fresh, and the replay: line names the run and says why, when that run
used another backend, its conversation was kept off the data ref as
potentially sensitive or is on another machine, it is over the size
threshold, or its session is unknown. Use --no-replay to force a fresh agent,
--replay (Claude only) to force replay (bypassing the size threshold), and
--replay-threshold-kb to tune the per-launch size cap.

Use --backend to select claude, codex, or agy independently of the coordinator.
The saved session worker selection overrides default_agent_backend in config.
Use --model and --effort to override backends.<backend>.model / effort. Legacy
default_agent_model / default_agent_effort apply only to Claude. Models are
passed through verbatim; each CLI validates its own identifiers. agy accepts
low/medium/high effort; Codex also accepts minimal/xhigh, Claude xhigh/max.
Dollar budgets and stored trajectory replay are Claude-only. Codex forks a
recorded local thread with --resume-from, or on --pr and --change follow-ups
of a Codex run; agy worker follow-ups start fresh.

Use --reason to say why the run is dispatched, such as docs-timer for a
timer's launch: lowercase letters, digits and hyphens. It is recorded on the
run and its events (dispatch_reason). Without it the reason is launch,
pr-follow-up (--pr), budget-resume (--pr on a budget-paused PR),
change-follow-up (--change) or resume (--resume-from); the pipeline's fix
agents pass ci-fix, review-fix, review-comment-fix or conflict-fix, and klaus
restack-conflicts passes restack-conflict. 'klaus report time' sums runs and
follow-ups by reason.

A dispatcher that checked a change before launching a run on it passes what
it checked, and launch starts no worker unless the change still stands so.
With --change naming a full change ID, --expect-owner, --expect-branch,
--expect-head and --expect-draft each require of the change, as 'valley
changes --json' lists it: its record's owner; its active version's branch,
which must also be the branch launch works on; its active version's head,
which must also be origin's tip of that branch and the worktree's start;
and that it is a draft. Under the lock it takes for continuing a branch,
launch fetches origin's branch and asks the valley, and checks them just
before it creates the worktree; it asks both again, after it has returned
the change to draft and set everything else up, just before the worker
starts. It fails closed: a valley or origin it can't ask, runs it can't read
to find a live holder, a change that isn't as expected, and a 'valley
unready' that fails or that the valley on PATH doesn't offer each fail the
launch. klaus restack-conflicts launches each resolution run this way.

--run-id names the run, in the form run IDs take (YYYYMMDD-HHMM- and 8 hex
digits), for a dispatcher that records the run before launching it; launch
refuses an ID a recorded run has. A launch with --run-id that fails before
starting its worker exits with status 3, so the dispatcher knows no run
started under the ID; any other failure exits with status 1. A worker whose
run launch can't record is stopped, so every worker that runs is recorded.

When sandbox_host is configured in ~/.klaus/config.json, agents run remotely
via SSH on the sandbox host. The worktree is synced before launch and results
are synced back after completion. Use --local to force local execution, or
--host to override the configured sandbox host.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		// A dispatcher that named the run learns from the exit status
		// whether a worker may have started.
		var workerStarting bool
		defer func() {
			if err != nil && !workerStarting && cmd.Flags().Changed("run-id") {
				err = &exitError{code: exitNoWorker, err: err}
			}
		}()
		promptFile, _ := cmd.Flags().GetString("prompt-file")
		prompt, err := resolvePrompt(args, promptFile)
		if err != nil {
			return err
		}
		issue, _ := cmd.Flags().GetString("issue")
		budget, _ := cmd.Flags().GetString("budget")
		repoRef, _ := cmd.Flags().GetString("repo")
		prNumber, _ := cmd.Flags().GetString("pr")
		forceLocal, _ := cmd.Flags().GetBool("local")
		hostOverride, _ := cmd.Flags().GetString("host")
		resumeFrom, _ := cmd.Flags().GetString("resume-from")
		model, _ := cmd.Flags().GetString("model")
		effort, _ := cmd.Flags().GetString("effort")
		replayFlag, _ := cmd.Flags().GetBool("replay")
		noReplay, _ := cmd.Flags().GetBool("no-replay")
		replayThresholdKB, _ := cmd.Flags().GetInt("replay-threshold-kb")
		branchFlag, _ := cmd.Flags().GetString("branch")
		changeRef, _ := cmd.Flags().GetString("change")
		forceTakeover, _ := cmd.Flags().GetBool("force-takeover")
		reasonFlag, _ := cmd.Flags().GetString("reason")
		runIDFlag, _ := cmd.Flags().GetString("run-id")
		var expect changeExpectation
		expect.Owner, _ = cmd.Flags().GetString("expect-owner")
		expect.Branch, _ = cmd.Flags().GetString("expect-branch")
		expect.Head, _ = cmd.Flags().GetString("expect-head")
		expect.Draft, _ = cmd.Flags().GetBool("expect-draft")
		setupStart := time.Now()
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()

		if !tmux.InSession() {
			return fmt.Errorf("klaus launch must be run inside a tmux session")
		}

		if replayFlag && noReplay {
			return fmt.Errorf("--replay and --no-replay are mutually exclusive")
		}
		if cmd.Flags().Changed("change") && changeRef == "" {
			return fmt.Errorf("--change needs a run ID, change ID or valley handle")
		}
		if err := checkContinuation(prNumber, changeRef, cmd.Flags().Changed("branch")); err != nil {
			return err
		}
		knotRef, isKnot := valley.ParseKnotRef(issue)
		if isKnot && prNumber != "" {
			return fmt.Errorf("--issue %s names a valley knot, and --pr continues a GitHub PR; a knot is worked in a valley project", issue)
		}
		if err := checkDispatchReason(reasonFlag); err != nil {
			return err
		}
		if err := expect.validate(changeRef); err != nil {
			return err
		}
		if cmd.Flags().Changed("run-id") && !validRunID(runIDFlag) {
			return fmt.Errorf("--run-id %q isn't a run ID: YYYYMMDD-HHMM- and 8 hex digits", runIDFlag)
		}
		if forceTakeover && prNumber == "" && changeRef == "" && resumeFrom == "" {
			return fmt.Errorf("--force-takeover takes over a PR's or a change's branch; use it with --pr, --change or --resume-from")
		}
		if cmd.Flags().Changed("branch") {
			if err := git.CheckBranchName(ctx, branchFlag); err != nil {
				return fmt.Errorf("--branch: %w", err)
			}
		}

		// Host repo — optional when --repo is specified or session target is set
		hostRoot, _ := git.RepoRoot()
		gitClient := git.NewExecClient()

		// Load session target (if any) to feed into resolution
		var sessionTarget string
		if s, storeErr := sessionStore(); storeErr == nil {
			if hds, ok := s.(*run.HomeDirStore); ok {
				if target, loadErr := run.LoadTarget(hds.BaseDir()); loadErr == nil {
					sessionTarget = target
				}
			}
		}

		// Resolve which repo to use. Priority: --repo > session target > hostRoot
		reg, _ := project.Load()
		repoRef, projectLocalPath := resolveRepoTarget(repoRef, sessionTarget, reg)

		if hostRoot == "" && repoRef == "" && projectLocalPath == "" {
			return fmt.Errorf("no target repo — use --repo owner/repo, 'klaus target owner/repo', or 'klaus project add' to register a project")
		}

		hostCfg, err := config.Load(hostRoot)
		if err != nil {
			return err
		}

		kind, err := resolveAgentBackend(cmd, hostCfg)
		if err != nil {
			return err
		}
		if kind != backend.Claude {
			if cmd.Flags().Changed("budget") {
				return fmt.Errorf("--budget is only supported by the claude backend")
			}
			if replayFlag {
				return fmt.Errorf("--replay is only supported by the claude backend")
			}
			if hostCfg.DefaultBudget != "" {
				fmt.Fprintf(os.Stderr, "%s does not enforce dollar budgets; default_budget does not apply\n", kind)
			}
		}
		if budget == "" {
			budget = hostCfg.DefaultBudget
		}
		defaults := hostCfg.AgentDefaults(string(kind))
		if model == "" {
			model = defaults.Model
		}
		if effort == "" {
			effort = defaults.Effort
		}
		if err := kind.ValidateEffort(effort); err != nil {
			return err
		}
		if err := config.ValidateAgentDisplay(hostCfg.AgentDisplay); err != nil {
			return err
		}

		store, err := sessionStore()
		if err != nil {
			return err
		}
		if err := store.EnsureDirs(); err != nil {
			return err
		}

		id := runIDFlag
		if id == "" {
			if id, err = run.GenID(); err != nil {
				return err
			}
		} else if all, err := run.ListAllSessions(); err != nil {
			return fmt.Errorf("--run-id: reading the recorded runs: %w", err)
		} else if findRun(all, id) != nil {
			return fmt.Errorf("--run-id: a run %s is already recorded", id)
		}

		// Determine the target repo for git operations.
		// When --repo is set, we clone the target and use it for worktree/branch ops.
		// When --repo matches a registered project, use the local path directly.
		// State is always tracked in the host repo.
		var (
			repoRoot      string // repo dir for git ops (clone or host)
			repoName      string
			defaultBranch string
			targetRepo    *string
			cloneDirPtr   *string
		)

		if projectLocalPath != "" {
			// Registered project — use local path directly, no cloning
			repoRoot = projectLocalPath
			repoName = filepath.Base(projectLocalPath)

			defaultBranch = "main"
			targetCfg, loadErr := config.Load(projectLocalPath)
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not load config from project %s: %v\n", repoRef, loadErr)
			} else if targetCfg.DefaultBranch != "" {
				defaultBranch = targetCfg.DefaultBranch
			}

			// Store as target for state tracking
			targetRepo = &repoRef
			cloneDirPtr = &projectLocalPath
		} else if repoRef != "" {
			owner, repo, cloneURL, err := git.ParseRepoRef(repoRef)
			if err != nil {
				return fmt.Errorf("parsing repo reference: %w", err)
			}

			cloneDir := filepath.Join(hostCfg.WorktreeBase, ".repos", owner, repo)

			fmt.Printf("Cloning/fetching %s/%s...\n", owner, repo)
			if err := gitClient.EnsureClone(ctx, cloneURL, cloneDir); err != nil {
				return fmt.Errorf("cloning %s: %w", repoRef, err)
			}

			repoRoot = cloneDir
			repoName = repo
			cloneDirPtr = &cloneDir
			targetRepo = &repoRef

			// Use target repo config for default_branch if available
			defaultBranch = "main"
			targetCfg, loadErr := config.Load(cloneDir)
			if loadErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not load config from target repo %s: %v\n", repoRef, loadErr)
			} else if targetCfg.DefaultBranch != "" {
				defaultBranch = targetCfg.DefaultBranch
			}
		} else {
			repoRoot = hostRoot
			repoName = filepath.Base(hostRoot)
			defaultBranch = hostCfg.DefaultBranch
		}

		worktree := filepath.Join(hostCfg.WorktreeBase, repoName, id)

		// Background-sync registered project clones so the agent's worktree
		// branches from fresh main. Non-blocking; results go to ~/.klaus/sync.log.
		// Exclude repoRoot to avoid racing with the foreground fetch below.
		kickoffBackgroundSync("launch", repoRoot)

		// Fetch all refs so the worktree starts from the latest state.
		// Skip when EnsureClone was already called — it fetches with the
		// same flags (--prune --tags), so a second fetch is redundant.
		if repoRef == "" || projectLocalPath != "" {
			if err := gitClient.FetchAll(ctx, repoRoot); err != nil {
				return fmt.Errorf("fetching origin: %w", err)
			}
		}

		// Normalize target repo name against the project registry so that
		// "cosmo", "patflynn/cosmo", and full URLs all resolve to the same
		// canonical name in the dashboard.
		normalizedTarget := normalizeTargetRepo(targetRepo, hostRoot)

		// A knot is read, and its claim checked, before anything is created.
		var knot *knotWork
		if isKnot {
			knot, err = prepareKnot(ctx, tmuxClient, repoRoot, repoName, "origin/"+defaultBranch, knotRef)
			if err != nil {
				return fmt.Errorf("--issue: %w", err)
			}
		}

		// When --pr or --change is set, track the PR's or the change's
		// branch instead of creating a new one; so does --resume-from, on
		// the branch its run pushed, when nothing took the change further.
		var branch string
		var isPRFix bool
		var prURL string
		var onChange *changeTarget
		var resumeNote string // why a --resume-from run gets a new branch
		// The valley's changes of the target repo, for a launch that
		// continues a change.
		listValley := func() (*valley.Changes, error) {
			if !valley.Detect(ctx, repoRoot, "", "origin/"+defaultBranch) {
				return nil, errNoValley
			}
			return valley.ListRepo(ctx, repoRoot)
		}

		if prNumber != "" {
			ghRepo := resolveGHRepo(repoRef, repoRoot)
			ghClient := gh.NewGHCLIClient(ghRepo)
			prBranch, err := ghClient.GetBranch(ctx, prNumber)
			if err != nil {
				return fmt.Errorf("getting PR branch: %w", err)
			}
			branch = prBranch
			isPRFix = true

			// Look up the PR URL for state tracking
			prURL, err = ghClient.GetURL(ctx, prNumber)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not get PR URL for #%s: %v\n", prNumber, err)
			}
		} else if changeRef != "" {
			if !isDirectPushRepo(repoRoot) {
				return fmt.Errorf("--change continues a change in a repo that delivers without PRs, and %s's origin is on GitHub; use --pr to continue a PR", repoName)
			}
			states, _ := run.ListAllSessions()
			onChange, err = resolveChange(changeRef, changeSources{Runs: states, Repo: targetName(normalizedTarget), Valley: listValley})
			if err != nil {
				return fmt.Errorf("--change: %w", err)
			}
			branch = onChange.Branch
		} else if branchFlag != "" {
			// A new run's branch must be new: reusing one would build on, or
			// reset, whatever work it already holds.
			const pickAnother = "pick a new name, or use --pr or --change to continue an existing PR's or change's branch"
			if git.RefExists(ctx, repoRoot, "refs/heads/"+branchFlag) {
				return fmt.Errorf("branch %q already exists in %s; %s", branchFlag, repoName, pickAnother)
			}
			sha, err := gitClient.RemoteBranchSHA(ctx, repoRoot, branchFlag)
			if err != nil {
				return fmt.Errorf("checking origin for branch %q: %w", branchFlag, err)
			}
			if sha != "" {
				return fmt.Errorf("branch %q already exists on origin; %s", branchFlag, pickAnother)
			}
			branch = branchFlag
		} else {
			if resumeFrom != "" {
				states, _ := run.ListAllSessions()
				src := changeSources{Runs: states, Repo: targetName(normalizedTarget), Valley: listValley, Slug: repoSlugForDir(repoRoot)}
				if src.Slug != "" {
					src.PRs = func(branch string) ([]string, error) {
						return gh.NewGHCLIClient(src.Slug).PRsForBranch(ctx, src.Slug, branch)
					}
				}
				onChange, resumeNote = resumeTarget(ctx, repoRoot, resumeFrom, src)
			}
			if onChange != nil {
				branch = onChange.Branch
			} else {
				var issueTitle string
				if knot != nil {
					issueTitle = knot.Knot.Title
				} else if issue != "" {
					title, err := gh.NewGHCLIClient(resolveGHRepo(repoRef, repoRoot)).GetIssueTitle(ctx, issue)
					if err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not fetch the title of issue %s for the branch name: %v\n", issue, err)
					}
					issueTitle = title
				}
				branch = defaultBranchName(ctx, id, repoName, issueTitle, prompt)
			}
		}
		// The flag that has the launch continue a branch, for its errors.
		continuing := "--change"
		switch {
		case isPRFix:
			continuing = "--pr"
		case changeRef == "" && onChange != nil:
			continuing = "--resume-from"
		}

		// Creating the worktree resets the branch, so a run still working on
		// the branch, or on the change, keeps it unless the operator takes
		// over, and a worktree that has it checked out is never removed,
		// even then. The check and the new run's registration happen under
		// one lock, so two launches continuing a branch can't both pass it.
		if isPRFix || onChange != nil {
			release, err := lockContinuation(ctx, repoRoot)
			if err != nil {
				return fmt.Errorf("%s: %w", continuing, err)
			}
			defer release()
			if !forceTakeover {
				if err := checkNoLiveHolder(ctx, tmuxClient, branch, prURL, normalizedTarget, onChange); err != nil {
					return fmt.Errorf("%s: %w", continuing, err)
				}
			}
		}
		if onChange != nil {
			// A dispatcher's expectations are of origin as it is, not as
			// the fetch before the lock found it.
			if expect.set() && onChange.Branch != "" {
				if err := git.FetchRemoteBranch(ctx, repoRoot, onChange.Branch); err != nil {
					return fmt.Errorf("--change: fetching origin's %s: %w", onChange.Branch, err)
				}
			}
			if err := onChange.checkBranch(ctx, repoRoot); err != nil {
				return fmt.Errorf("%s: %w", continuing, err)
			}
		}

		// The brief carries the knot as project notes, data rather than
		// instructions.
		if knot != nil {
			prompt += knot.notes()
		}

		// Build system prompt (from target repo's .klaus/prompt.md if it exists)
		var sysPrompt string
		delivery := runDelivery(isPRFix, repoRoot)
		directPush := delivery == run.DeliveryPush
		if isPRFix {
			sysPrompt, err = config.RenderPRFixPrompt(repoRoot, config.PromptVars{
				RunID:    id,
				Issue:    issue,
				PR:       prNumber,
				Branch:   branch,
				RepoName: repoName,
			})
		} else {
			// Only a run that opens a GitHub PR requests a review.
			var reviewer string
			if !directPush {
				var warning string
				reviewer, warning = gh.NewReviewerResolver().Resolve(ctx, repoSlugForDir(repoRoot), hostCfg.PRReviewer)
				if warning != "" {
					fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
				}
			}
			vars := config.PromptVars{
				RunID:      id,
				Issue:      issue,
				Branch:     branch,
				RepoName:   repoName,
				Reviewer:   reviewer,
				DirectPush: directPush,
			}
			if knot != nil {
				vars.Issue, vars.Knot = "", knotRef.String()
			}
			if onChange != nil {
				vars.Change = onChange.Name
				// A valley change may be restacked onto main, its rebase
				// pushed with a lease on the tip the worktree starts from.
				// A fetch elsewhere before the worktree exists can only
				// make that lease stale, so the push fails rather than
				// overwrite.
				if directPush && valley.Detect(ctx, repoRoot, "", "origin/"+defaultBranch) {
					vars.Valley, vars.Head = true, onChange.Tip
				}
			}
			sysPrompt, err = config.RenderPrompt(repoRoot, vars)
		}
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}
		sysPrompt = config.WithWorkerRules(sysPrompt)

		logFile := filepath.Join(store.LogDir(), id+".jsonl")
		promptPath := filepath.Join(run.PromptDir(store), id+".md")
		sysPromptPath := filepath.Join(run.PromptDir(store), id+".system.md")
		// The worker reads its input on stdin. agy's, with the system prompt
		// ahead of the brief, gets a file of its own.
		inputPath := promptPath
		if kind.Stdin(sysPrompt, prompt) != prompt {
			inputPath = filepath.Join(run.PromptDir(store), id+".input.md")
		}
		dirs := workerDirs{tmux: workerTmuxDir(store, id), scratch: workerScratchDir(store, id)}
		workerOpts := backend.Options{
			SystemPrompt: sysPrompt, SystemPromptFile: sysPromptPath, Budget: budget, Prompt: prompt,
			RunID: id, Model: model, Effort: effort,
		}

		// For cross-repo launches with a host repo, finalize must run from the
		// host repo context so that data-ref sync works correctly.
		var finalizePrefix string
		if targetRepo != nil && hostRoot != "" {
			finalizePrefix = fmt.Sprintf("cd %s && ", shellQuote(hostRoot))
		}
		// Determine sandbox host: --host flag > config sandbox_host, unless --local.
		// Reachability is settled before the size check so the check is exact.
		sandboxHost := hostCfg.SandboxHost
		if hostOverride != "" {
			sandboxHost = hostOverride
		}
		if forceLocal {
			sandboxHost = ""
		}
		if sandboxHost != "" && !CheckSandboxReachable(sandboxHost) {
			fmt.Fprintf(os.Stderr, "warning: sandbox %s unreachable, falling back to local execution\n", sandboxHost)
			sandboxHost = ""
		}
		// Fail before a worktree exists. A resume ID (Claude session or Codex
		// thread, both UUIDs) is only known later, so size one in; a sandbox
		// run can still fall back to local, so the local command must fit too.
		sizeCheck := workerOpts
		if resumeFrom != "" || isPRFix || onChange != nil {
			sizeCheck.ResumeID = "00000000-0000-0000-0000-000000000000"
		}
		hosts := []string{sandboxHost}
		if sandboxHost != "" {
			hosts = append(hosts, "")
		}
		for _, host := range hosts {
			if _, err := agentPaneCommand(kind, sizeCheck, inputPath, host, worktree, logFile, finalizePrefix, id, dirs); err != nil {
				return err
			}
		}
		// A local worker that can't run its commands doesn't start: an agy
		// worker without bash can edit files and nothing else. A sandbox
		// worker runs on its host's PATH.
		if sandboxHost == "" {
			if err := checkWorkerPath(kind, worktree); err != nil {
				return err
			}
		}

		switch {
		case isPRFix:
			fmt.Printf("Launching agent %s (PR #%s fix)...\n", id, prNumber)
		case onChange != nil:
			fmt.Printf("Launching agent %s (continuing change %s)...\n", id, onChange.Name)
		default:
			fmt.Printf("Launching agent %s...\n", id)
		}
		if targetRepo != nil {
			fmt.Printf("  target:   %s\n", *targetRepo)
		}
		if isPRFix || onChange != nil {
			// A dispatcher's expectations hold, the valley asked now and
			// origin's branch as fetched under the lock, before anything
			// is created.
			if expect.set() {
				if err := expect.check(onChange, onChange.Tip, listValley); err != nil {
					return fmt.Errorf("--change: %w", err)
				}
			}
			// Create worktree tracking the PR's or the change's branch
			if err := gitClient.WorktreeAddTrack(ctx, repoRoot, worktree, branch, forceTakeover); err != nil {
				var lw *git.LocalWorkError
				var inUse *git.WorktreeInUseError
				switch {
				case errors.As(err, &lw):
					return fmt.Errorf("%s: creating worktree: %w; push or remove that work, or pass --force-takeover to discard it", continuing, err)
				case errors.As(err, &inUse):
					states, _ := run.ListAllSessions()
					return fmt.Errorf("%s: %w", continuing, occupantError(branch, inUse.Path, inUse.Work, worktreeOwner(states, inUse.Path)))
				}
				return fmt.Errorf("%s: creating worktree: %w", continuing, err)
			}
		} else {
			if err := gitClient.WorktreeAdd(ctx, repoRoot, worktree, branch, "origin/"+defaultBranch); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
		}

		// Clean up the worktree if the launch fails after this point.
		// This prevents stale worktrees from blocking future dispatch retries.
		var launchSucceeded bool
		var keepWorktree bool // a worker may run in it
		var knotRec *run.Knot // the knot's record once launch claimed it
		defer func() {
			if !launchSucceeded && !keepWorktree {
				if knotRec != nil {
					releaseKnot(context.Background(), repoRoot, knotRec, id)
				}
				fmt.Fprintf(os.Stderr, "cleaning up worktree after failed launch: %s\n", worktree)
				if rmErr := gitClient.WorktreeRemove(context.Background(), repoRoot, worktree); rmErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to remove worktree %s: %v\n", worktree, rmErr)
				}
				os.Remove(promptPath)
				os.Remove(sysPromptPath)
				os.Remove(dirs.tmux)
				os.Remove(dirs.scratch)
			}
		}()

		if expect.Head != "" {
			if head := gitHead(ctx, worktree); head != expect.Head {
				return fmt.Errorf("--change: the worktree starts at %s, not the expected head %s", shortSHA(head), shortSHA(expect.Head))
			}
		}

		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)
		switch {
		case continuing == "--resume-from":
			fmt.Printf("  resume:   continuing the branch run %s pushed\n", resumeFrom)
		case resumeNote != "":
			fmt.Printf("  resume:   new branch: %s\n", resumeNote)
		}
		if directPush {
			fmt.Printf("  delivery: push branch, no PR (origin is not on GitHub)\n")
		}

		change, err := chooseChangeID(ctx, store, changeIDInput{
			IsPRFix:       isPRFix,
			PRURL:         prURL,
			PRBranch:      branch,
			TargetRepo:    normalizedTarget,
			ResumeFrom:    resumeFrom,
			Change:        onChange,
			Worktree:      worktree,
			DefaultBranch: defaultBranch,
		})
		if err != nil {
			return fmt.Errorf("choosing change ID: %w", err)
		}
		fmt.Printf("  change:   %s (%s)\n", change.ID, change.From)

		// The runs of the change needing attention whose pushed work the
		// branch holds are this run's to carry on.
		var tookOver []string
		if isPRFix || onChange != nil {
			states, _ := run.ListAllSessions()
			holder := &run.State{ID: id}
			candidates := takeoverCandidates(states, holder, change.ID, targetName(normalizedTarget), change.Repo)
			tookOver = recordTakeovers(ctx, worktree, "HEAD", holder, candidates)
			if len(tookOver) > 0 {
				fmt.Printf("  takes on: the work of %s, which needed attention\n", strings.Join(tookOver, ", "))
			}
		}

		// Every commit the agent makes carries the provenance trailers, so a
		// run that cannot stamp them does not start.
		if err := gitClient.InstallCommitMsgHook(ctx, worktree, git.Provenance{RunID: id, ChangeID: change.ID}); err != nil {
			return fmt.Errorf("installing commit-msg hook: %w", err)
		}

		// A follow-up on a valley change takes it back to draft before the
		// agent starts, so the operator can't approve a head it is replacing.
		unready := unreadyChange(ctx, worktree, change)
		// A dispatcher's expectations need the change a draft.
		if expect.set() {
			switch {
			case unready == nil:
				return fmt.Errorf("--change: could not return change %s to draft: the valley on PATH has no unready, or the worktree's head carries no valley policy", onChange.Name)
			case unready.Error != "":
				return fmt.Errorf("--change: could not return change %s to draft: %s", onChange.Name, unready.Error)
			}
		}

		// A --pr launch on a budget-paused PR resumes it.
		var budgetPaused bool
		if isPRFix {
			paused, perr := draft.HasBudgetPausedLabel(ctx, draft.ExecRunner{}, worktree, resolveGHRepo(repoRef, repoRoot), prNumber)
			budgetPaused = perr == nil && paused
		}

		// Set up Nix dev environment if flake.nix exists
		nix.SetupDevEnvironment(worktree)

		if err := run.WritePromptFile(promptPath, prompt); err != nil {
			return err
		}
		if err := run.WritePromptFile(sysPromptPath, sysPrompt); err != nil {
			return err
		}
		if inputPath != promptPath {
			if err := run.WritePromptFile(inputPath, kind.Stdin(sysPrompt, prompt)); err != nil {
				return err
			}
		}

		// The conversation the worker continues. --resume-from names it: a
		// Claude transcript staged into the new worktree, or a local Codex
		// thread to fork. Otherwise a run that continues a budget-paused PR
		// (issue #261), or a change (--change), continues that of the most
		// recent run on it, when the new run's backend can (resolveReplay):
		// a Claude conversation is restored from the data ref for claude
		// --resume, a Codex session is forked, and agy starts fresh. Any miss
		// starts a fresh agent. Replay restores onto the local machine, so it
		// only applies to local execution: a run destined for a sandbox host
		// starts fresh there. An explicit --resume-from that can't be resumed
		// starts fresh rather than replaying another conversation.
		intendsSandbox := sandboxHost != ""
		var resolvedResume, replayedFromRunID string
		if resumeFrom != "" && (kind == backend.Claude || kind == backend.Codex && !intendsSandbox) {
			if prior, err := store.Load(resumeFrom); err == nil {
				resolvedResume = resolveResumeID(prior, kind, worktree)
			}
		}
		if (isPRFix || onChange != nil) && !intendsSandbox {
			switch {
			case resumeFrom != "" && resolvedResume != "":
				fmt.Printf("  replay:   resuming conversation from run %s (--resume-from)\n", resumeFrom)
			case resumeFrom != "":
				fmt.Printf("  replay:   fresh agent (--resume-from %s: its conversation can't be resumed)\n", resumeFrom)
			default:
				threshold := replayThresholdKB
				if threshold == 0 {
					threshold = hostCfg.ReplayThresholdKB
				}
				decision := replayDecision{Reason: "--no-replay"}
				if !noReplay {
					decision = resolveReplay(ctx, replayParams{
						Kind:        kind,
						GitClient:   gitClient,
						Store:       store,
						RepoRoot:    repoRoot,
						DataRef:     hostCfg.DataRef,
						Worktree:    worktree,
						PRBranch:    branch,
						PRNumber:    prNumber,
						GHRepo:      resolveGHRepo(repoRef, repoRoot),
						Change:      onChange,
						ForceReplay: replayFlag,
						ThresholdKB: threshold,
					})
				}
				if decision.SessionUUID != "" {
					resolvedResume = decision.SessionUUID
					replayedFromRunID = decision.SourceRunID
					fmt.Printf("  replay:   %s\n", decision.Reason)
				} else {
					fmt.Printf("  replay:   fresh agent (%s)\n", decision.Reason)
				}
			}
		}

		// Build the selected backend command.
		if kind != backend.Claude && resumeFrom != "" && resolvedResume == "" {
			if onChange != nil || isPRFix {
				fmt.Fprintf(os.Stderr, "warning: %s starts a fresh worker conversation on the branch\n", kind)
			} else {
				fmt.Fprintf(os.Stderr, "warning: %s starts a fresh worker conversation; prior branch changes remain available with --pr or --change\n", kind)
			}
		}
		workerOpts.ResumeID = resolvedResume

		// Sync worktree to the sandbox before launching
		var useSandbox bool
		var sandboxHostName string
		if sandboxHost != "" {
			if err := syncWorktreeToSandbox(sandboxHost, worktree); err != nil {
				fmt.Fprintf(os.Stderr, "warning: sandbox sync failed, falling back to local: %v\n", err)
				if err := checkWorkerPath(kind, worktree); err != nil {
					return err
				}
			} else {
				useSandbox = true
				sandboxHostName = sandboxHost
			}
		}

		paneCmd, err := agentPaneCommand(kind, workerOpts, inputPath, sandboxHostName, worktree, logFile, finalizePrefix, id, dirs)
		if err != nil {
			return err
		}
		if !useSandbox {
			// Owner-only, like the prompts: the tmux directory will hold any
			// tmux socket the worker opens.
			if err := os.MkdirAll(dirs.tmux, 0o700); err != nil {
				return fmt.Errorf("creating the worker's tmux directory: %w", err)
			}
			if err := os.MkdirAll(dirs.scratch, 0o700); err != nil {
				return fmt.Errorf("creating the worker's scratch directory: %w", err)
			}
		}

		// The knot is the run's before its agent starts.
		if knot != nil {
			if err := knot.claim(ctx, repoRoot, id); err != nil {
				return fmt.Errorf("--issue: claiming %s: %w", knotRef, err)
			}
			knotRec = knot.record()
		}

		// What the clone shares across worktrees, as the agent finds it.
		sharedGit := snapshotSharedGit(ctx, worktree)

		// A dispatcher's expectations hold still as the worker starts,
		// after everything else: the valley and origin, asked now. Nothing
		// here excludes another client meanwhile, but the change is a
		// draft, which approve refuses, and the worker's push is leased
		// on the head it starts from.
		if expect.set() {
			tip, err := gitClient.RemoteBranchSHA(ctx, repoRoot, branch)
			if err != nil {
				return fmt.Errorf("--change: asking origin for %s: %w", branch, err)
			}
			if err := expect.check(onChange, tip, listValley); err != nil {
				return fmt.Errorf("--change: %w", err)
			}
		}

		// Launch the agent's tmux pane. Detached by default (its own window in
		// the agents session); splitFrom is non-empty only when agent_display
		// is "pane" and the coordinator's window was split.
		workerStarting = true
		paneID, splitFrom, err := startAgentPane(ctx, tmuxClient, hostCfg.AgentDisplayMode(),
			id, worktree, paneCmd, FormatPaneTitle(id, issue, prompt))
		if err != nil {
			return err
		}

		// Keep the dashboard pane pinned at the bottom. RebalanceLayout uses
		// even-vertical which treats all panes equally, so the dashboard may
		// end up in the middle. Load the session state to find the dashboard
		// pane, then swap it to the last position if needed.
		if splitFrom != "" {
			pinDashboardToBottom(ctx, splitFrom, store, tmuxClient)
		}

		// Write state
		agentStart := time.Now()
		createdAt := agentStart.Format(time.RFC3339)
		issuePtr := stringPtr(issue)
		var budgetPtr *string
		if kind == backend.Claude {
			budgetPtr = &budget
		}
		logFilePtr := &logFile

		var hostPtr *string
		if useSandbox {
			hostPtr = &sandboxHostName
		}

		state := &run.State{
			Backend:     string(kind),
			ID:          id,
			Prompt:      prompt,
			Issue:       issuePtr,
			PR:          stringPtr(prNumber),
			Branch:      branch,
			Worktree:    worktree,
			TmuxPane:    &paneID,
			Budget:      budgetPtr,
			LogFile:     logFilePtr,
			CreatedAt:   createdAt,
			Host:        hostPtr,
			TargetRepo:  normalizedTarget,
			CloneDir:    cloneDirPtr,
			SessionName: &id,
			Model:       stringPtr(model),
			Effort:      stringPtr(effort),
			ChangeID:    change.ID,
			TookOver:    tookOver,
			Unready:     unready,
			ChangeRepo:  change.Repo,
			Delivery:    delivery,
			Knot:        knotRec,
			HeadSHA:     gitHead(ctx, worktree),
			SharedGit:   sharedGit,
		}
		state.DispatchReason = dispatchReason(reasonFlag, isPRFix, budgetPaused, changeRef != "", resumeFrom)
		state.RepoKey = launchRepoKey(ctx, state, repoRoot)
		if resumeFrom != "" {
			state.OriginalRunID = &resumeFrom
		} else if replayedFromRunID != "" {
			state.OriginalRunID = &replayedFromRunID
		}
		if isPRFix {
			state.Type = "pr-fix"
			if prURL != "" {
				state.PRURL = &prURL
			}
		}

		if err := store.Save(state); err != nil {
			// A worker no recorded run names escapes every check for one
			// (a live holder, a dispatcher's pending launch): stop it. One
			// that can't be stopped keeps its worktree.
			if kerr := tmuxClient.KillPane(context.Background(), paneID); kerr != nil {
				keepWorktree = true
				return fmt.Errorf("saving state: %w; and its worker in pane %s couldn't be stopped: %v", err, paneID, kerr)
			}
			return fmt.Errorf("saving state: %w; its worker was stopped", err)
		}

		// Emit agent:started event
		if hds, ok := store.(*run.HomeDirStore); ok {
			startedData := map[string]interface{}{
				"id":     id,
				"prompt": prompt,
				"model":  runBackendModel(state),
			}
			if issue != "" {
				startedData["issue"] = issue
			}
			if normalizedTarget != nil {
				startedData["target_repo"] = *normalizedTarget
			}
			emitRunEvent(hds.BaseDir(), state, event.AgentStarted, startedData)

			// If this is a launch against a budget-paused PR, emit
			// agent:resumed so the coordinator (and dashboard) know the
			// pause is being acted on.
			if budgetPaused && prNumber != "" {
				emitRunEvent(hds.BaseDir(), state, event.AgentResumed, map[string]interface{}{
					"id":        id,
					"pr_number": prNumber,
					"pr_url":    prURL,
				})
			}

			// Setup ends, and the agent's work begins, as its pane starts.
			emitRunEvent(hds.BaseDir(), state, event.PhaseCompleted, phaseData(event.PhaseSetup, setupStart, agentStart))
			emitRunEvent(hds.BaseDir(), state, event.PhaseStarted, map[string]interface{}{
				"phase":      event.PhaseAgent,
				"started_at": createdAt,
			})
		}

		fmt.Printf("  pane:     %s\n", paneID)
		if useSandbox {
			fmt.Printf("  host:     %s (sandbox)\n", sandboxHostName)
		} else {
			fmt.Printf("  host:     local\n")
		}
		if kind == backend.Claude {
			fmt.Printf("  budget:   $%s\n", budget)
		} else {
			fmt.Println("  budget:   unavailable for this backend")
		}
		if model != "" {
			fmt.Printf("  model:    %s\n", model)
		}
		if effort != "" {
			fmt.Printf("  effort:   %s\n", effort)
		}
		fmt.Printf("  backend:  %s\n", kind)
		fmt.Printf("  log:      %s\n", logFile)
		fmt.Println()
		fmt.Printf("Agent %s is running. Use 'klaus status' to check progress.\n", id)
		launchSucceeded = true
		return nil
	},
}

// unreadyChange returns the change a follow-up run continues to draft on the
// valley its repo delivers to, so approve refuses the change while the agent
// works on it; finalize marks the pushed head ready again. It runs `valley
// unready`, bounded by valley.UnreadyTimeout, only for an inherited change in
// a repo whose worktree HEAD carries the valley's policy, and only when the
// valley on PATH lists unready (valley.DetectUnready). A change that wasn't
// ready is noted; a failure is recorded and printed as a warning. Neither
// fails the launch. It returns nil when it didn't run valley.
func unreadyChange(ctx context.Context, worktree string, change changeChoice) *run.Unready {
	if !change.Inherited {
		return nil
	}
	head, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil || !valley.DetectUnready(ctx, worktree, strings.TrimSpace(string(head))) {
		return nil
	}
	rec := &run.Unready{ChangeID: change.ID}
	switch r, err := valley.Unready(ctx, worktree, change.ID); {
	case err != nil:
		rec.Error = err.Error()
		fmt.Fprintf(os.Stderr, "warning: could not return the change to draft: %s\n", rec.Error)
	case r.Draft:
		rec.Repo, rec.Handle = r.Repo, r.Handle
		fmt.Printf("  valley:   %s %s is a draft until the agent pushes\n", r.Repo, r.Handle)
	default:
		rec.Note = r.Note
		fmt.Printf("  valley:   the change wasn't ready: %s\n", r.Note)
	}
	return rec
}

// checkNoLiveHolder returns an error naming the run when a live run, in any
// session, holds the branch a --pr or --change launch is about to work on,
// or the change it continues, in the target repo. A --pr launch's change is
// that of the run that opened the PR (prChangeSource). Runs it can't read
// are an error too: it can't say none of them holds the branch.
func checkNoLiveHolder(ctx context.Context, tc tmux.Client, branch, prURL string, target *string, onChange *changeTarget) error {
	states, err := run.ListAllSessions()
	if err != nil {
		return fmt.Errorf("reading the recorded runs, to find one still working on branch %s: %w", branch, err)
	}
	repos := []string{targetName(target)}
	var changeID string
	if onChange != nil {
		changeID = onChange.ID
		repos = append(repos, onChange.Repo)
	} else if s := prChangeSource(states, prURL, branch, target); s != nil {
		changeID = s.ChangeID
		repos = append(repos, integration.RepoName(s))
	}
	td := run.TmuxDepsFor(ctx, tc)
	s := liveHolder(states, func(s *run.State) bool { return s.IsAgentRunningWith(td) }, branch, changeID, repos...)
	what := "change " + changeID
	switch {
	case s == nil:
		return nil
	case onChange != nil && s.ChangeID == onChange.ID:
		what = "change " + onChange.Name
	case s.Branch == branch:
		what = "branch " + branch
	}
	return fmt.Errorf("run %s is still working on %s; wait for it to finish, or stop it and clean it up (klaus cleanup --force %s, which discards its uncommitted work) and launch again; --force-takeover launches past it, but never removes a worktree that has the branch checked out", s.ID, what, s.ID)
}

// snapshotSharedGit records the files worktree's clone shares across its
// worktrees, for finalize to compare; nil when git can't find the clone.
func snapshotSharedGit(ctx context.Context, worktree string) *run.SharedGit {
	dir, err := git.SharedGitDir(ctx, worktree)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: can't snapshot the clone's shared git files: %v\n", err)
		return nil
	}
	return &run.SharedGit{Dir: dir, Files: git.SharedSnapshot(ctx, dir)}
}

// checkWorkerPath fails when a command kind's local worker, which runs in
// worktree, runs by name isn't on the PATH it gets. That is launch's own:
// tmux gives a pane the PATH of the client that creates it, here klaus
// launch, before the pane's shell starts.
func checkWorkerPath(kind backend.Kind, worktree string) error {
	if err := kind.CheckPath(os.Getenv("PATH"), worktree); err != nil {
		return fmt.Errorf("%w; a worker gets the PATH klaus launch runs with, so launch it with one that has the command", err)
	}
	return nil
}

// resolveResumeID preserves resumable conversations even after an error.
func resolveResumeID(prior *run.State, kind backend.Kind, worktree string) string {
	if prior == nil {
		return ""
	}
	var resumeID string
	switch kind {
	case backend.Claude:
		if prior.LogFile == nil || (prior.Backend != "" && prior.Backend != string(kind)) {
			return ""
		}
		resumeID = ExtractClaudeSessionID(*prior.LogFile)
		if resumeID != "" && !stageResumeTranscript(prior, resumeID, worktree) {
			fmt.Fprintf(os.Stderr, "warning: could not stage prior Claude conversation %s for resume, starting fresh\n", resumeID)
			return ""
		}
	case backend.Codex:
		if prior.Backend != string(kind) || prior.Host != nil || prior.BackendSessionID == nil {
			return ""
		}
		resumeID = *prior.BackendSessionID
	}
	if resumeID != "" && prior.FailureReason != nil {
		fmt.Fprintf(os.Stderr, "warning: prior run %s ended with %s; resuming its conversation\n", prior.ID, *prior.FailureReason)
	}
	return resumeID
}

// resolvePrompt requires exactly one prompt source and preserves file contents.
func resolvePrompt(args []string, promptFile string) (string, error) {
	switch {
	case len(args) > 0 && promptFile != "":
		return "", fmt.Errorf("prompt given twice: pass it as an argument or with --prompt-file, not both")
	case promptFile != "":
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return "", fmt.Errorf("reading prompt file: %w", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("prompt file %s is empty", promptFile)
		}
		return string(data), nil
	case len(args) > 0:
		return args[0], nil
	default:
		return "", fmt.Errorf("no prompt: pass it as an argument or with --prompt-file <path>")
	}
}

// maxPaneCommandBytes caps the command handed to tmux new-window/split-window.
// tmux rejects a request over its 16KB imsg limit (MAX_IMSGSIZE) with "command
// too long", and the pane's other arguments share that budget.
const maxPaneCommandBytes = 12000

// agentPaneCommand builds a worker's pane command; sandboxHost "" runs locally.
// The backend reads inputFile, which holds kind.Stdin(o.SystemPrompt,
// o.Prompt), on stdin, so the prompt's length never counts against tmux's
// limit. A local worker runs under workerEnv(dirs), out of reach of the
// pane's tmux server; a sandbox worker runs on another host, which has no way
// to it, and gets neither directory.
func agentPaneCommand(kind backend.Kind, o backend.Options, inputFile, sandboxHost, worktree, logFile, finalizePrefix, id string, dirs workerDirs) (string, error) {
	if sandboxHost != "" {
		// The remote side cannot read local files; only stdin crosses ssh.
		o.SystemPromptFile = ""
	}
	args := kind.Worker(o)
	// Propagate worker identity to nested klaus commands such as _pre-review.
	agentCmd := "KLAUS_BACKEND=" + string(kind) + " " + backend.ShellCommand(args)
	const selfBin = "klaus" // assumes klaus is in PATH
	var paneCmd string
	if sandboxHost != "" {
		paneCmd = buildSandboxPaneCommand(sandboxHost, worktree, agentCmd, inputFile, logFile, selfBin, finalizePrefix, id)
	} else {
		if dirs.tmux == "" {
			// An empty TMUX_TMPDIR is tmux's default, likely the shared server's.
			return "", fmt.Errorf("no private tmux directory for the %s worker", kind)
		}
		if dirs.scratch == "" {
			// An empty KLAUS_SCRATCH puts "$KLAUS_SCRATCH/<name>" at the root.
			return "", fmt.Errorf("no scratch directory for the %s worker", kind)
		}
		paneCmd = buildPaneCommand(worktree, workerEnv(dirs)+agentCmd, inputFile, logFile, selfBin, finalizePrefix, id)
	}
	if len(paneCmd) <= maxPaneCommandBytes {
		return paneCmd, nil
	}
	msg := fmt.Sprintf("agent pane command is %d bytes, over the %d-byte limit for a tmux command", len(paneCmd), maxPaneCommandBytes)
	// Codex, and Claude without its file flag, take the system prompt in
	// argv; agy reads it on stdin with the prompt.
	if kind == backend.Codex || kind == backend.Claude && o.SystemPromptFile == "" {
		where, fix := string(kind), "shorten .klaus/prompt.md (.klaus/pr-fix-prompt.md for --pr)"
		if kind == backend.Claude {
			where, fix = "sandbox", fix+", or use --local, where claude reads it from a file"
		}
		return "", fmt.Errorf("%s: the worker system prompt is %d bytes and goes inline for %s runs; %s", msg, len(o.SystemPrompt), where, fix)
	}
	return "", fmt.Errorf("%s", msg)
}

func withExitEvent(command string) string {
	return "( " + command + "; klaus_backend_exit=$?; printf '\\n{\"type\":\"klaus_exit\",\"exit_code\":%s}\\n' \"$klaus_backend_exit\" )"
}

// stdinFrom redirects promptFile into the command it follows; "" leaves stdin alone.
func stdinFrom(promptFile string) string {
	if promptFile == "" {
		return ""
	}
	return " < " + shellQuote(promptFile)
}

func buildPaneCommand(worktree, agentCmd, promptFile, logFile, selfBin, finalizePrefix, id string) string {
	return fmt.Sprintf(
		"%s%scd %s && %s | tee %s | %s _format-stream; %s%s _finalize %s",
		tmuxSessionEnvPrefix(),
		runEnvPrefix(id),
		shellQuote(worktree),
		withExitEvent(agentCmd+stdinFrom(promptFile)),
		shellQuote(logFile),
		selfBin,
		finalizePrefix,
		selfBin,
		shellQuote(id),
	)
}

func shellQuote(s string) string {
	// Use single quotes, escaping any existing single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// FormatPaneTitle builds a compact pane title for an agent.
// Format with issue:    "#<issue> <short-desc>"
// Format with a knot:   "<knot> <short-desc>"
// Format without issue: "<short-id> <short-desc>"
// Short desc is up to 40 characters of the prompt, trimmed to a word boundary.
func FormatPaneTitle(id, issue, prompt string) string {
	const (
		shortIDLength = 4
		maxDescLength = 40
	)

	var title string
	if _, ok := valley.ParseKnotRef(issue); ok {
		title = issue
	} else if issue != "" {
		title = "#" + issue
	} else {
		title = id
		if len(id) > shortIDLength {
			title = id[len(id)-shortIDLength:]
		}
	}

	desc := strings.TrimSpace(prompt)
	if len(desc) > maxDescLength {
		desc = desc[:maxDescLength]
		// Trim to last word boundary
		if i := strings.LastIndex(desc, " "); i > 0 {
			desc = desc[:i]
		}
	}
	if desc != "" {
		title += " " + desc
	}

	return title
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// normalizeTargetRepo resolves the target repo to a canonical short name.
// If targetRepo is set, normalizes it against registered projects.
// If targetRepo is nil but hostRoot is a git repo matching a registered project,
// uses the project name instead of leaving it nil (which shows as "(local)").
func normalizeTargetRepo(targetRepo *string, hostRoot string) *string {
	reg, _ := project.Load()

	if targetRepo != nil && *targetRepo != "" {
		n := project.NormalizeRepoName(*targetRepo, reg)
		return &n
	}

	// No explicit target — try to detect from the host repo's git remote
	if hostRoot != "" && reg != nil {
		remote := gitRemoteURL(hostRoot)
		if remote != "" {
			n := project.NormalizeRepoName(remote, reg)
			return &n
		}
	}

	return targetRepo
}

// resolveRepoTarget determines which repo to use for the agent worktree.
// Priority: explicit --repo flag > session target > (caller falls back to hostRoot).
// If the resolved ref matches a registered project name (no "/" in the ref),
// projectLocalPath is set to the project's local directory.
func resolveRepoTarget(repoFlag, sessionTarget string, reg *project.Registry) (repoRef, projectLocalPath string) {
	repoRef = repoFlag

	// If no --repo flag, use the session target. This takes priority over the
	// current git repo (hostRoot) because the coordinator session may be
	// running inside one repo while targeting another.
	if repoRef == "" && sessionTarget != "" {
		repoRef = sessionTarget
	}

	// Resolve against project registry: bare names (no "/") may map to a
	// local clone, avoiding a fresh GitHub clone.
	if repoRef != "" && !strings.Contains(repoRef, "/") && reg != nil {
		if localPath, ok := reg.Get(repoRef); ok {
			projectLocalPath = localPath
		}
	}

	return repoRef, projectLocalPath
}

// isDirectPushRepo reports whether agents deliver to repoRoot by pushing their
// branch for the operator to integrate, rather than opening a PR: true unless
// its origin is a github.com URL. Sandbox runs sync a worktree made from
// repoRoot, so they are decided the same way.
func isDirectPushRepo(repoRoot string) bool {
	return !git.IsGitHubRemote(gitRemoteURL(repoRoot))
}

// runDelivery decides how a run on repoRoot delivers its work: a run that
// continues a PR (--pr) delivers to that PR, and any other run pushes its
// branch when repoRoot is a direct-push repo, or else opens a PR. Launch
// records it on the run; finalize decides state that predates the record
// the same way.
func runDelivery(isPRFix bool, repoRoot string) string {
	if !isPRFix && isDirectPushRepo(repoRoot) {
		return run.DeliveryPush
	}
	return run.DeliveryPR
}

// resolveGHRepo returns an owner/repo string suitable for --repo flags on gh CLI calls.
// If repoRef already contains '/' (owner/repo format), it is returned directly.
// Otherwise, it extracts owner/repo from the git remote of repoRoot.
func resolveGHRepo(repoRef, repoRoot string) string {
	if strings.Contains(repoRef, "/") {
		return repoRef
	}
	if repoRoot != "" {
		remote := gitRemoteURL(repoRoot)
		if remote != "" {
			// Parse owner/repo from remote URL (e.g. git@github.com:owner/repo.git or https://...)
			owner, repo, _, err := git.ParseRepoRef(remote)
			if err == nil {
				return owner + "/" + repo
			}
		}
	}
	return ""
}

// CheckSandboxReachable tests whether a sandbox host is reachable via SSH.
func CheckSandboxReachable(host string) bool {
	cmd := exec.Command("ssh", "-o", "ConnectTimeout=3", "-o", "BatchMode=yes", host, "true")
	return cmd.Run() == nil
}

// syncWorktreeToSandbox syncs a local worktree to a sandbox host via rsync.
func syncWorktreeToSandbox(host, worktree string) error {
	// Create remote directory
	mkdirCmd := exec.Command("ssh", host, fmt.Sprintf("mkdir -p %s", shellQuote(worktree)))
	if out, err := mkdirCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating remote dir: %w: %s", err, string(out))
	}

	// Sync worktree contents
	rsyncCmd := exec.Command("rsync", "-az", "--delete", worktree+"/", fmt.Sprintf("%s:%s/", host, worktree))
	if out, err := rsyncCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync to sandbox: %w: %s", err, string(out))
	}

	return nil
}

func buildSandboxPaneCommand(host, worktree, agentCmd, promptFile, logFile, selfBin, finalizePrefix, id string) string {
	// Run the agent on sandbox via SSH, pipe output locally through tee + formatter,
	// then finalize locally and rsync results back. ssh forwards the local
	// prompt file as the remote command's stdin. The remote command gets the
	// run's ID too: the sandbox has no event log for the run, so _pre-review
	// there reports its check in its output, which the stream brings back to
	// _format-stream --sandbox.
	rsyncBack := fmt.Sprintf("rsync -az %s:%s/ %s/",
		shellQuote(host), shellQuote(worktree), shellQuote(worktree))
	remoteCmd := remoteRunEnvPrefix(id) + "cd " + shellQuote(worktree) + " && " + agentCmd
	return fmt.Sprintf(
		"%s%s%s | tee %s | %s _format-stream --sandbox; %s%s _finalize %s; %s",
		tmuxSessionEnvPrefix(),
		runEnvPrefix(id),
		withExitEvent("ssh "+shellQuote(host)+" "+shellQuote(remoteCmd)+stdinFrom(promptFile)),
		shellQuote(logFile),
		selfBin,
		finalizePrefix,
		selfBin,
		shellQuote(id),
		rsyncBack,
	)
}

// pinDashboardToBottom ensures the dashboard pane is the last (bottom-most)
// pane in the window. This is called after RebalanceLayout which may have
// moved the dashboard out of position.
func pinDashboardToBottom(ctx context.Context, currentPane string, store run.StateStore, tc tmux.Client) {
	// Load the session state directly via KLAUS_SESSION_ID
	sessionID := os.Getenv("KLAUS_SESSION_ID")
	if sessionID == "" {
		return
	}
	s, err := store.Load(sessionID)
	if err != nil || s.Type != "session" || s.DashboardPane == nil {
		return
	}
	dashPane := *s.DashboardPane

	panes, err := tc.ListWindowPanes(ctx, currentPane)
	if err != nil || len(panes) < 2 {
		return
	}

	lastPane := panes[len(panes)-1]
	if lastPane == dashPane {
		return // already at the bottom
	}

	// Ensure the dashboard is actually in this window before swapping
	var found bool
	for _, p := range panes {
		if p == dashPane {
			found = true
			break
		}
	}
	if !found {
		return
	}

	if err := tc.SwapPane(ctx, dashPane, lastPane); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not pin dashboard to bottom: %v\n", err)
	}

	// Pin the coordinator pane to the top. After even-vertical rebalancing,
	// agents may end up above the coordinator.
	if s.TmuxPane != nil {
		panes, err = tc.ListWindowPanes(ctx, currentPane)
		if err == nil && len(panes) > 1 && panes[0] != *s.TmuxPane {
			for _, p := range panes {
				if p == *s.TmuxPane {
					_ = tc.SwapPane(ctx, p, panes[0])
					break
				}
			}
		}
	}
}

func init() {
	launchCmd.Flags().String("backend", "", "Worker backend: claude, codex, or agy (default from session/config)")
	launchCmd.Flags().String("prompt-file", "", "Read the prompt from a file instead of the positional argument: verbatim (no shell mangling of backticks) and any length; mutually exclusive with it")
	launchCmd.Flags().String("issue", "", "GitHub issue number to reference; on a valley project, a knot to work: oc-, bd-, ida- or qn- and 7 hex digits, or <project>:<id> (claimed for the run, linked to its change, closed once it lands)")
	launchCmd.Flags().String("pr", "", "Push fixes to an existing PR's branch instead of creating a new PR (also the way to resume a budget-paused PR — the agent picks up from the WIP commit)")
	launchCmd.Flags().String("budget", "", "Claude only: max spend in USD (default from config)")
	launchCmd.Flags().String("repo", "", "Target repo: registered project name, owner/repo, or full URL")
	launchCmd.Flags().Bool("local", false, "Force local execution even when sandbox is configured")
	launchCmd.Flags().String("host", "", "Override sandbox host (ignores config sandbox_host)")
	launchCmd.Flags().String("resume-from", "", "Resume from a previous agent's session (run ID); without --pr, --change or --branch, continue the branch it pushed without a PR when nothing took its change further")
	launchCmd.Flags().String("change", "", "Continue an existing change in a repo without PRs, named by run ID, change ID or valley handle: work on its branch and keep its change ID, on any backend; Claude replays the latest run's stored conversation, Codex forks its own session on the same machine, agy starts fresh; not with --pr or --branch")
	launchCmd.Flags().Bool("force-takeover", false, "With --pr, --change or a --resume-from that continues its run's branch, launch even while another run is still working on the branch or change, and discard commits on the local branch that origin lacks; never removes a worktree that has the branch checked out (klaus cleanup that run first)")
	launchCmd.Flags().String("branch", "", "Name of the new run's branch (default: <slug>-<run-id tail>, the slug taken from the --issue title or the prompt's first '# ' heading or line); not with --pr or --change")
	launchCmd.Flags().Bool("replay", false, "Force trajectory replay for --pr or --change (continue the prior conversation, bypassing the size threshold, and for --pr the budget-paused check)")
	launchCmd.Flags().Bool("no-replay", false, "Disable trajectory replay for --pr or --change; dispatch a fresh agent instead")
	launchCmd.Flags().Int("replay-threshold-kb", 0, "Max stored trajectory size (KB) eligible for replay; 0 uses config replay_threshold_kb (default 300)")
	launchCmd.Flags().String("model", "", "Model for the selected backend (default from backends.<backend>.model; legacy default_agent_model for Claude)")
	launchCmd.Flags().String("effort", "", "Reasoning effort for the selected backend (default from backends.<backend>.effort; valid levels depend on backend)")
	launchCmd.Flags().String("reason", "", "Why this run is dispatched, recorded on the run and its events, e.g. docs-timer (default: launch, pr-follow-up, change-follow-up, budget-resume or resume, from the other flags)")
	launchCmd.Flags().String("run-id", "", "Name the run (YYYYMMDD-HHMM- and 8 hex digits) instead of minting an ID, for a dispatcher that records it first; refused if a run has it. Failing before the worker starts then exits with status 3")
	launchCmd.Flags().String("expect-owner", "", "With --change <change-id>: launch only if the valley lists the change with this record owner")
	launchCmd.Flags().String("expect-branch", "", "With --change <change-id>: launch only if the change's active version, and the launch, are on this branch")
	launchCmd.Flags().String("expect-head", "", "With --change <change-id>: launch only if the change's active version, origin's branch and the new worktree are at this commit (full SHA)")
	launchCmd.Flags().Bool("expect-draft", false, "With --change <change-id>: launch only if the valley lists the change as a draft, before the worktree and again as the worker starts")
	rootCmd.AddCommand(launchCmd)
}
