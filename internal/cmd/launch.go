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

The prompt comes either from the positional argument or from --prompt-file
<path>; exactly one of the two is required. Prefer --prompt-file for long or
technical prompts: in zsh a backtick inside a double-quoted argument is command
substitution, so code spans in a shell-quoted prompt are silently mangled before
klaus ever sees them. File contents are used verbatim.

Either way klaus saves the prompt to prompts/<run-id>.md in the session
directory, kept as the record of the brief, and claude and codex workers read it
on stdin, so a prompt of any length fits. agy has no stdin mode and takes it as
an argument. If what still goes on the tmux command line (agy's prompt, or the
worker system prompt for codex, agy and sandbox runs) exceeds tmux's limit,
launch fails before creating a worktree.

Use --repo to launch an agent against a different repository. If the name
matches a registered project (no owner/ prefix), the project's local path is
used directly. Otherwise, the repo is cloned from GitHub.

The agent is told to deliver by opening a PR when the target repo's origin is
on github.com. For any other origin (a plain ssh or local remote) it is told to
push its branch and not open a PR, and to end with a report of the branch, head
SHA, diff summary and checks run, for the operator to integrate from.

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
finalize marks the new head ready. Every backend keeps the branch, the change
ID and the provenance trailers; only the conversation differs: Claude replays
the stored conversation, on any machine that has the data ref, Codex forks
its own session on the same machine, and agy starts fresh.
With --resume-from, that run's conversation is used, and --change still picks
the branch and the change ID. When that conversation can't be resumed, the
agent starts fresh rather than replaying another, for --pr as for --change, and
the replay: line says so.

--pr and --change refuse while a run in any session is still running on the
branch they would work on, or on the change they continue (for --pr, the
change of the run that opened the PR), in the target repo, naming that run.
--force-takeover launches anyway: the new worktree replaces that run's,
discarding its local, uncommitted work and unpushed commits. Without it,
launch never discards work origin lacks: when the branch is checked out in a
worktree with uncommitted changes, or the local branch has commits not on
origin/<branch>, it refuses, naming the worktree or branch.

A run without --pr or --change gets a new branch named <slug>-<tail>. The tail
is the random end of the run ID (bd92f081 for 20261011-1843-bd92f081). The slug
comes from the --issue title, else the prompt's first '# ' heading, else its
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
a fresh worktree on a new branch, paused or not — the follow-up keeps what the
earlier agent learned instead of re-exploring the repo. Prior errors produce a
warning but do not prevent resume. It starts fresh if the transcript cannot be
located or staged.

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

When sandbox_host is configured in ~/.klaus/config.json, agents run remotely
via SSH on the sandbox host. The worktree is synced before launch and results
are synced back after completion. Use --local to force local execution, or
--host to override the configured sandbox host.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
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
		if forceTakeover && prNumber == "" && changeRef == "" {
			return fmt.Errorf("--force-takeover takes over a PR's or a change's branch; use it with --pr or --change")
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

		id, err := run.GenID()
		if err != nil {
			return err
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

		// When --pr or --change is set, track the PR's or the change's
		// branch instead of creating a new one
		var branch string
		var isPRFix bool
		var prURL string
		var onChange *changeTarget

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
			onChange, err = resolveChange(changeRef, changeSources{
				Runs: states,
				Repo: targetName(normalizedTarget),
				Valley: func() (*valley.Changes, error) {
					if !valley.Detect(ctx, repoRoot, "", "origin/"+defaultBranch) {
						return nil, errNoValley
					}
					return valley.ListRepo(ctx, repoRoot)
				},
			})
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
			var issueTitle string
			if issue != "" {
				title, err := gh.NewGHCLIClient(resolveGHRepo(repoRef, repoRoot)).GetIssueTitle(ctx, issue)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not fetch the title of issue %s for the branch name: %v\n", issue, err)
				}
				issueTitle = title
			}
			branch = defaultBranchName(ctx, id, repoName, issueTitle, prompt)
		}

		// Creating the worktree resets the branch and moves it from any
		// worktree that has it checked out, so a run still working on the
		// branch, or on the change, keeps it unless the operator takes over.
		if (isPRFix || onChange != nil) && !forceTakeover {
			if err := checkNoLiveHolder(ctx, tmuxClient, branch, prURL, normalizedTarget, onChange); err != nil {
				if isPRFix {
					return fmt.Errorf("--pr: %w", err)
				}
				return fmt.Errorf("--change: %w", err)
			}
		}
		if onChange != nil {
			if err := onChange.checkBranch(ctx, repoRoot); err != nil {
				return fmt.Errorf("--change: %w", err)
			}
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
			if onChange != nil {
				vars.Change = onChange.Name
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
		// thread, both UUIDs) is only known later, so size one in; the other
		// later change, a local fallback, never makes the command longer.
		sizeCheck := workerOpts
		if resumeFrom != "" || isPRFix || onChange != nil {
			sizeCheck.ResumeID = "00000000-0000-0000-0000-000000000000"
		}
		if _, err := agentPaneCommand(kind, sizeCheck, promptPath, sandboxHost, worktree, logFile, finalizePrefix, id); err != nil {
			return err
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
			// Create worktree tracking the PR's or the change's branch
			if err := gitClient.WorktreeAddTrack(ctx, repoRoot, worktree, branch, forceTakeover); err != nil {
				var lw *git.LocalWorkError
				if errors.As(err, &lw) {
					return fmt.Errorf("creating worktree: %w; push or remove that work, or pass --force-takeover to discard it", err)
				}
				return fmt.Errorf("creating worktree: %w", err)
			}
		} else {
			if err := gitClient.WorktreeAdd(ctx, repoRoot, worktree, branch, "origin/"+defaultBranch); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
		}

		// Clean up the worktree if the launch fails after this point.
		// This prevents stale worktrees from blocking future dispatch retries.
		var launchSucceeded bool
		defer func() {
			if !launchSucceeded {
				fmt.Fprintf(os.Stderr, "cleaning up worktree after failed launch: %s\n", worktree)
				if rmErr := gitClient.WorktreeRemove(context.Background(), repoRoot, worktree); rmErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to remove worktree %s: %v\n", worktree, rmErr)
				}
				os.Remove(promptPath)
				os.Remove(sysPromptPath)
			}
		}()

		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)
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

		// Every commit the agent makes carries the provenance trailers, so a
		// run that cannot stamp them does not start.
		if err := gitClient.InstallCommitMsgHook(ctx, worktree, git.Provenance{RunID: id, ChangeID: change.ID}); err != nil {
			return fmt.Errorf("installing commit-msg hook: %w", err)
		}

		// A follow-up on a valley change takes it back to draft before the
		// agent starts, so the operator can't approve a head it is replacing.
		unready := unreadyChange(ctx, worktree, change)

		// Set up Nix dev environment if flake.nix exists
		nix.SetupDevEnvironment(worktree)

		if err := run.WritePromptFile(promptPath, prompt); err != nil {
			return err
		}
		if err := run.WritePromptFile(sysPromptPath, sysPrompt); err != nil {
			return err
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
			fmt.Fprintf(os.Stderr, "warning: %s starts a fresh worker conversation; prior branch changes remain available with --pr or --change\n", kind)
		}
		workerOpts.ResumeID = resolvedResume

		// Sync worktree to the sandbox before launching
		var useSandbox bool
		var sandboxHostName string
		if sandboxHost != "" {
			if err := syncWorktreeToSandbox(sandboxHost, worktree); err != nil {
				fmt.Fprintf(os.Stderr, "warning: sandbox sync failed, falling back to local: %v\n", err)
			} else {
				useSandbox = true
				sandboxHostName = sandboxHost
			}
		}

		paneCmd, err := agentPaneCommand(kind, workerOpts, promptPath, sandboxHostName, worktree, logFile, finalizePrefix, id)
		if err != nil {
			return err
		}

		// Launch the agent's tmux pane. Detached by default (its own window in
		// the agents session); splitFrom is non-empty only when agent_display
		// is "pane" and the coordinator's window was split.
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
		createdAt := time.Now().Format(time.RFC3339)
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
			Unready:     unready,
			ChangeRepo:  change.Repo,
			Delivery:    delivery,
		}
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
			return fmt.Errorf("saving state: %w", err)
		}

		// Emit agent:started event
		if hds, ok := store.(*run.HomeDirStore); ok {
			startedData := map[string]interface{}{
				"id":     id,
				"prompt": prompt,
			}
			if issue != "" {
				startedData["issue"] = issue
			}
			if normalizedTarget != nil {
				startedData["target_repo"] = *normalizedTarget
			}
			emitEvent(hds.BaseDir(), id, event.AgentStarted, startedData)

			// If this is a launch against a budget-paused PR, emit
			// agent:resumed so the coordinator (and dashboard) know the
			// pause is being acted on.
			if isPRFix && prNumber != "" {
				ghRepoArg := resolveGHRepo(repoRef, repoRoot)
				if paused, perr := draft.HasBudgetPausedLabel(ctx, draft.ExecRunner{}, worktree, ghRepoArg, prNumber); perr == nil && paused {
					emitEvent(hds.BaseDir(), id, event.AgentResumed, map[string]interface{}{
						"id":        id,
						"pr_number": prNumber,
						"pr_url":    prURL,
					})
				}
			}
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
	rec := &run.Unready{}
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
// that of the run that opened the PR (prChangeSource).
func checkNoLiveHolder(ctx context.Context, tc tmux.Client, branch, prURL string, target *string, onChange *changeTarget) error {
	states, _ := run.ListAllSessions()
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
	return fmt.Errorf("run %s is still working on %s; wait for it to finish or stop it, or pass --force-takeover to take over, discarding that run's local, uncommitted work", s.ID, what)
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
// The prompt reaches the backend on stdin from promptFile unless the backend
// only takes it in argv (agy), so its length never counts against tmux's limit.
func agentPaneCommand(kind backend.Kind, o backend.Options, promptFile, sandboxHost, worktree, logFile, finalizePrefix, id string) (string, error) {
	if sandboxHost != "" {
		// The remote side cannot read local files; only stdin crosses ssh.
		o.SystemPromptFile = ""
	}
	args, promptOnStdin := kind.Worker(o)
	if !promptOnStdin {
		promptFile = ""
	}
	// Propagate worker identity to nested klaus commands such as _pre-review.
	agentCmd := "KLAUS_BACKEND=" + string(kind) + " " + backend.ShellCommand(args)
	const selfBin = "klaus" // assumes klaus is in PATH
	var paneCmd string
	if sandboxHost != "" {
		paneCmd = buildSandboxPaneCommand(sandboxHost, worktree, agentCmd, promptFile, logFile, selfBin, finalizePrefix, id)
	} else {
		paneCmd = buildPaneCommand(worktree, agentCmd, promptFile, logFile, selfBin, finalizePrefix, id)
	}
	if len(paneCmd) <= maxPaneCommandBytes {
		return paneCmd, nil
	}
	msg := fmt.Sprintf("agent pane command is %d bytes, over the %d-byte limit for a tmux command", len(paneCmd), maxPaneCommandBytes)
	if !promptOnStdin && len(o.Prompt) > len(o.SystemPrompt) {
		return "", fmt.Errorf("%s: the prompt is %d bytes and %s takes it as an argument; shorten it or use another backend", msg, len(o.Prompt), kind)
	}
	if kind != backend.Claude || o.SystemPromptFile == "" {
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
		"%scd %s && %s | tee %s | %s _format-stream; %s%s _finalize %s",
		tmuxSessionEnvPrefix(),
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
// Format without issue: "<short-id> <short-desc>"
// Short desc is up to 40 characters of the prompt, trimmed to a word boundary.
func FormatPaneTitle(id, issue, prompt string) string {
	const (
		shortIDLength = 4
		maxDescLength = 40
	)

	var title string
	if issue != "" {
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
	// prompt file as the remote command's stdin.
	rsyncBack := fmt.Sprintf("rsync -az %s:%s/ %s/",
		shellQuote(host), shellQuote(worktree), shellQuote(worktree))
	return fmt.Sprintf(
		"%s%s | tee %s | %s _format-stream; %s%s _finalize %s; %s",
		tmuxSessionEnvPrefix(),
		withExitEvent("ssh "+shellQuote(host)+" "+shellQuote("cd "+shellQuote(worktree)+" && "+agentCmd)+stdinFrom(promptFile)),
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
	launchCmd.Flags().String("issue", "", "GitHub issue number to reference")
	launchCmd.Flags().String("pr", "", "Push fixes to an existing PR's branch instead of creating a new PR (also the way to resume a budget-paused PR — the agent picks up from the WIP commit)")
	launchCmd.Flags().String("budget", "", "Claude only: max spend in USD (default from config)")
	launchCmd.Flags().String("repo", "", "Target repo: registered project name, owner/repo, or full URL")
	launchCmd.Flags().Bool("local", false, "Force local execution even when sandbox is configured")
	launchCmd.Flags().String("host", "", "Override sandbox host (ignores config sandbox_host)")
	launchCmd.Flags().String("resume-from", "", "Resume from a previous agent's session (run ID)")
	launchCmd.Flags().String("change", "", "Continue an existing change in a repo without PRs, named by run ID, change ID or valley handle: work on its branch and keep its change ID, on any backend; Claude replays the latest run's stored conversation, Codex forks its own session on the same machine, agy starts fresh; not with --pr or --branch")
	launchCmd.Flags().Bool("force-takeover", false, "With --pr or --change, launch even while another run is still working on the branch or change, discarding that run's local, uncommitted work")
	launchCmd.Flags().String("branch", "", "Name of the new run's branch (default: <slug>-<run-id tail>, the slug taken from the --issue title or the prompt's first '# ' heading or line); not with --pr or --change")
	launchCmd.Flags().Bool("replay", false, "Force trajectory replay for --pr or --change (continue the prior conversation, bypassing the size threshold, and for --pr the budget-paused check)")
	launchCmd.Flags().Bool("no-replay", false, "Disable trajectory replay for --pr or --change; dispatch a fresh agent instead")
	launchCmd.Flags().Int("replay-threshold-kb", 0, "Max stored trajectory size (KB) eligible for replay; 0 uses config replay_threshold_kb (default 300)")
	launchCmd.Flags().String("model", "", "Model for the selected backend (default from backends.<backend>.model; legacy default_agent_model for Claude)")
	launchCmd.Flags().String("effort", "", "Reasoning effort for the selected backend (default from backends.<backend>.effort; valid levels depend on backend)")
	rootCmd.AddCommand(launchCmd)
}
