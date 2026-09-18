package cmd

import (
	"context"
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
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
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

Use --repo to launch an agent against a different repository. If the name
matches a registered project (no owner/ prefix), the project's local path is
used directly. Otherwise, the repo is cloned from GitHub.

Use --pr to push fixes to an existing PR's branch instead of creating a new
PR. The agent will commit and push to the PR branch directly. This is also
how you resume a budget-paused PR: relaunch against the paused PR and the
follow-up picks up from the WIP commit klaus left on the branch. When the
follow-up agent's _finalize runs, the 'klaus:budget-paused' label is cleared
automatically.

Use --resume-from <run-id> to continue a previous run's Claude conversation in
a fresh worktree, paused or not — the follow-up keeps what the earlier agent
learned instead of re-exploring the repo. It starts fresh if that run crashed
or its transcript cannot be located.

For a budget-paused PR, klaus continues the previous agent's Claude
conversation by default (trajectory replay): it restores the stored
conversation and runs 'claude --resume', avoiding a cold re-exploration of
the repo. It falls back to a fresh agent when the trajectory is missing,
oversized, sensitive-skipped, or its session UUID is unknown. Use --no-replay
to force a fresh agent, --replay to force replay (bypassing the size
threshold), and --replay-threshold-kb to tune the per-launch size cap.

Use --backend to select claude, codex, or agy independently of the coordinator.
The saved session worker selection overrides default_agent_backend in config.
Use --model and --effort to override backends.<backend>.model / effort. Legacy
default_agent_model / default_agent_effort apply only to Claude. Models are
passed through verbatim; each CLI validates its own identifiers. agy accepts
low/medium/high effort; Codex also accepts minimal/xhigh, Claude xhigh/max.
Dollar budgets and stored trajectory replay are Claude-only. Codex can fork a
recorded local thread with --resume-from; agy worker follow-ups start fresh.

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
		ctx := cmd.Context()
		tmuxClient := tmux.NewExecClient()

		if !tmux.InSession() {
			return fmt.Errorf("klaus launch must be run inside a tmux session")
		}

		if replayFlag && noReplay {
			return fmt.Errorf("--replay and --no-replay are mutually exclusive")
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
			fmt.Fprintf(os.Stderr, "%s does not enforce dollar budgets; default_budget does not apply\n", kind)
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

		// When --pr is set, track the PR's branch instead of creating a new one
		var branch string
		var isPRFix bool
		var prURL string

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

			fmt.Printf("Launching agent %s (PR #%s fix)...\n", id, prNumber)
			if targetRepo != nil {
				fmt.Printf("  target:   %s\n", *targetRepo)
			}

			// Create worktree tracking the PR branch
			if err := gitClient.WorktreeAddTrack(ctx, repoRoot, worktree, prBranch); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
		} else {
			branch = "agent/" + id

			fmt.Printf("Launching agent %s...\n", id)
			if targetRepo != nil {
				fmt.Printf("  target:   %s\n", *targetRepo)
			}

			// Create worktree
			startPoint := "origin/" + defaultBranch
			if err := gitClient.WorktreeAdd(ctx, repoRoot, worktree, branch, startPoint); err != nil {
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
			}
		}()

		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)

		if err := prepareBackendWorktree(kind, worktree, repoName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
		}

		if err := gitClient.InstallCommitMsgHook(ctx, worktree); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not install commit-msg hook: %v\n", err)
		}

		// Set up Nix dev environment if flake.nix exists
		nix.SetupDevEnvironment(worktree)

		// Build system prompt (from target repo's .klaus/prompt.md if it exists)
		var sysPrompt string
		if isPRFix {
			sysPrompt, err = config.RenderPRFixPrompt(repoRoot, config.PromptVars{
				RunID:    id,
				Issue:    issue,
				PR:       prNumber,
				Branch:   branch,
				RepoName: repoName,
			})
		} else {
			reviewer := hostCfg.PRReviewerOrDefault()
			sysPrompt, err = config.RenderPrompt(repoRoot, config.PromptVars{
				RunID:    id,
				Issue:    issue,
				Branch:   branch,
				RepoName: repoName,
				Reviewer: reviewer,
			})
		}
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}

		logFile := filepath.Join(store.LogDir(), id+".jsonl")

		// Resolve the Claude session UUID from the previous run's JSONL log.
		// claude --resume expects a UUID v4, not the klaus run ID.
		//
		// Claude Code scopes conversation transcripts by working directory
		// (~/.claude/projects/<encoded-cwd>/<uuid>.jsonl). This new agent runs
		// in a fresh worktree, so 'claude --resume' would look in the wrong
		// project dir and exit at startup with 0 turns ("No conversation found
		// with session ID"). To make resume work across worktrees we stage the
		// prior transcript into this worktree's project dir before launching.
		//
		// Resume is an OPTIMIZATION, never a requirement: if the prior run
		// crashed, or its transcript can't be located/copied, we silently
		// start a fresh claude session instead of risking a no-op launch.
		var resolvedResume string
		if resumeFrom != "" && kind == backend.Claude {
			if s, loadErr := store.Load(resumeFrom); loadErr == nil && s != nil && s.LogFile != nil && (s.Backend == "" || s.Backend == string(backend.Claude)) {
				if s.FailureReason != nil {
					// Don't chain onto a crashed conversation.
					fmt.Fprintf(os.Stderr, "warning: prior run %s failed (%s); starting fresh instead of resuming\n", resumeFrom, *s.FailureReason)
				} else if candidate := ExtractClaudeSessionID(*s.LogFile); candidate != "" {
					if stageResumeTranscript(s, candidate, worktree) {
						resolvedResume = candidate
					} else {
						fmt.Fprintf(os.Stderr, "warning: could not stage prior Claude conversation %s for resume, starting fresh\n", candidate)
					}
				}
			}
			// If we couldn't extract a session UUID (e.g., previous agent
			// also failed with no output) or the transcript is missing,
			// skip --resume entirely and let claude start fresh.
		}

		// Budget-paused PR continuation (issue #261): when launching against a
		// paused PR with no explicit --resume-from, try to continue the prior
		// agent's Claude conversation via trajectory replay. The stored
		// conversation is restored into this worktree's project dir and
		// claude --resume picks up where the pause happened, avoiding a costly
		// re-exploration of the repo. Falls back to a fresh agent on any miss.
		// Replay restores the trajectory onto the local machine, so it only
		// applies to local execution. When the run is destined for a sandbox
		// host, claude runs remotely and would not find the restored file —
		// skip replay and let it start fresh there.
		intendsSandbox := !forceLocal && (hostOverride != "" || hostCfg.SandboxHost != "")
		var replayedFromRunID string
		if kind == backend.Claude && resolvedResume == "" && isPRFix && prNumber != "" && !noReplay && !intendsSandbox {
			threshold := replayThresholdKB
			if threshold == 0 {
				threshold = hostCfg.ReplayThresholdKB
			}
			decision := resolveBudgetPausedReplay(ctx, replayParams{
				GitClient:   gitClient,
				Store:       store,
				RepoRoot:    repoRoot,
				DataRef:     hostCfg.DataRef,
				Worktree:    worktree,
				PRBranch:    branch,
				PRNumber:    prNumber,
				GHRepo:      resolveGHRepo(repoRef, repoRoot),
				ForceReplay: replayFlag,
				ThresholdKB: threshold,
			})
			if decision.SessionUUID != "" {
				resolvedResume = decision.SessionUUID
				replayedFromRunID = decision.SourceRunID
				fmt.Printf("  replay:   %s\n", decision.Reason)
			} else {
				fmt.Printf("  replay:   fresh agent (%s)\n", decision.Reason)
			}
		}

		if kind == backend.Codex && resumeFrom != "" && !intendsSandbox {
			if prior, err := store.Load(resumeFrom); err == nil && prior.Backend == string(kind) && prior.Host == nil && prior.FailureReason == nil && prior.BackendSessionID != nil {
				resolvedResume = *prior.BackendSessionID
			}
		}
		// Build the selected backend command.
		if kind != backend.Claude && resumeFrom != "" && resolvedResume == "" {
			fmt.Fprintf(os.Stderr, "warning: %s starts a fresh worker conversation; prior branch changes remain available with --pr\n", kind)
		}
		agentCmd := backend.ShellCommand(kind.Worker(backend.Options{
			SystemPrompt: sysPrompt, Budget: budget, Prompt: prompt, RunID: id,
			ResumeID: resolvedResume, Model: model, Effort: effort,
		}))
		// Propagate worker identity to nested klaus commands such as _pre-review.
		agentCmd = "KLAUS_BACKEND=" + string(kind) + " " + agentCmd

		// Build the pane command: run claude, pipe through tee and formatter, then finalize.
		// For cross-repo launches with a host repo, finalize must run from the
		// host repo context so that data-ref sync works correctly.
		selfBin := "klaus" // assumes klaus is in PATH
		var finalizePrefix string
		if targetRepo != nil && hostRoot != "" {
			finalizePrefix = fmt.Sprintf("cd %s && ", shellQuote(hostRoot))
		}
		// Determine sandbox host: --host flag > config sandbox_host
		sandboxHost := hostCfg.SandboxHost
		if hostOverride != "" {
			sandboxHost = hostOverride
		}

		// Attempt sandbox execution unless --local is set
		var useSandbox bool
		var sandboxHostName string
		if sandboxHost != "" && !forceLocal {
			if CheckSandboxReachable(sandboxHost) {
				useSandbox = true
				sandboxHostName = sandboxHost
				// Sync worktree to sandbox before launching
				if err := syncWorktreeToSandbox(sandboxHost, worktree); err != nil {
					fmt.Fprintf(os.Stderr, "warning: sandbox sync failed, falling back to local: %v\n", err)
					useSandbox = false
				}
			} else {
				fmt.Fprintf(os.Stderr, "warning: sandbox %s unreachable, falling back to local execution\n", sandboxHost)
			}
		}

		var paneCmd string
		if useSandbox {
			paneCmd = buildSandboxPaneCommand(sandboxHostName, worktree, agentCmd, logFile, selfBin, finalizePrefix, id)
		} else {
			paneCmd = buildPaneCommand(worktree, agentCmd, logFile, selfBin, finalizePrefix, id)
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

		// Normalize target repo name against the project registry so that
		// "cosmo", "patflynn/cosmo", and full URLs all resolve to the same
		// canonical name in the dashboard.
		normalizedTarget := normalizeTargetRepo(targetRepo, hostRoot)

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

// resolvePrompt returns the agent prompt from either the positional argument or
// --prompt-file. Exactly one source must be given: both is ambiguous, neither
// leaves the agent with no briefing. File contents are used verbatim so that
// backticks and other shell metacharacters survive intact.
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

func withExitEvent(command string) string {
	return "( " + command + "; klaus_backend_exit=$?; printf '\\n{\"type\":\"klaus_exit\",\"exit_code\":%s}\\n' \"$klaus_backend_exit\" )"
}

func buildPaneCommand(worktree, claudeCmd, logFile, selfBin, finalizePrefix, id string) string {
	return fmt.Sprintf(
		"%scd %s && %s | tee %s | %s _format-stream; %s%s _finalize %s",
		tmuxSessionEnvPrefix(),
		shellQuote(worktree),
		withExitEvent(claudeCmd),
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

func buildSandboxPaneCommand(host, worktree, claudeCmd, logFile, selfBin, finalizePrefix, id string) string {
	// Run claude on sandbox via SSH, pipe output locally through tee + formatter,
	// then finalize locally and rsync results back.
	rsyncBack := fmt.Sprintf("rsync -az %s:%s/ %s/",
		shellQuote(host), shellQuote(worktree), shellQuote(worktree))
	return fmt.Sprintf(
		"%s%s | tee %s | %s _format-stream; %s%s _finalize %s; %s",
		tmuxSessionEnvPrefix(),
		withExitEvent("ssh "+shellQuote(host)+" "+shellQuote("cd "+shellQuote(worktree)+" && "+claudeCmd)),
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
	launchCmd.Flags().String("prompt-file", "", "Read the prompt from a file instead of the positional argument (avoids shell mangling of backticks); mutually exclusive with it")
	launchCmd.Flags().String("issue", "", "GitHub issue number to reference")
	launchCmd.Flags().String("pr", "", "Push fixes to an existing PR's branch instead of creating a new PR (also the way to resume a budget-paused PR — the agent picks up from the WIP commit)")
	launchCmd.Flags().String("budget", "", "Claude only: max spend in USD (default from config)")
	launchCmd.Flags().String("repo", "", "Target repo: registered project name, owner/repo, or full URL")
	launchCmd.Flags().Bool("local", false, "Force local execution even when sandbox is configured")
	launchCmd.Flags().String("host", "", "Override sandbox host (ignores config sandbox_host)")
	launchCmd.Flags().String("resume-from", "", "Resume from a previous agent's session (run ID)")
	launchCmd.Flags().Bool("replay", false, "Force trajectory replay for a budget-paused --pr (continue the prior conversation, bypassing the size threshold)")
	launchCmd.Flags().Bool("no-replay", false, "Disable trajectory replay for a budget-paused --pr; dispatch a fresh agent instead")
	launchCmd.Flags().Int("replay-threshold-kb", 0, "Max stored trajectory size (KB) eligible for replay; 0 uses config replay_threshold_kb (default 300)")
	launchCmd.Flags().String("model", "", "Model for the selected backend (default from backends.<backend>.model; legacy default_agent_model for Claude)")
	launchCmd.Flags().String("effort", "", "Reasoning effort for the selected backend (default from backends.<backend>.effort; valid levels depend on backend)")
	rootCmd.AddCommand(launchCmd)
}
