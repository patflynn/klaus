package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

var launchCmd = &cobra.Command{
	Use:   "launch \"<prompt>\" [flags]",
	Short: "Launch an autonomous Claude Code agent",
	Long: `Creates a git worktree, launches Claude Code in autonomous mode in a new
tmux pane, and tracks the run state. Must be run inside a tmux session.

Use --repo to launch an agent against a different repository. If the name
matches a registered project (no owner/ prefix), the project's local path is
used directly. Otherwise, the repo is cloned from GitHub.

Use --pr to push fixes to an existing PR's branch instead of creating a new
PR. The agent will commit and push to the PR branch directly.

When sandbox_host is configured in ~/.klaus/config.json, agents run remotely
via SSH on the sandbox host. The worktree is synced before launch and results
are synced back after completion. Use --local to force local execution, or
--host to override the configured sandbox host.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := args[0]
		issue, _ := cmd.Flags().GetString("issue")
		budget, _ := cmd.Flags().GetString("budget")
		repoRef, _ := cmd.Flags().GetString("repo")
		prNumber, _ := cmd.Flags().GetString("pr")
		forceLocal, _ := cmd.Flags().GetBool("local")
		hostOverride, _ := cmd.Flags().GetString("host")

		if !tmux.InSession() {
			return fmt.Errorf("klaus launch must be run inside a tmux session")
		}

		// Host repo — optional when --repo is specified or session target is set
		hostRoot, _ := git.RepoRoot()

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

		if budget == "" {
			budget = hostCfg.DefaultBudget
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
			repoRoot      string  // repo dir for git ops (clone or host)
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
			if err := git.EnsureClone(cloneURL, cloneDir); err != nil {
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

		// When --pr is set, track the PR's branch instead of creating a new one
		var branch string
		var isPRFix bool
		var prURL string

		if prNumber != "" {
			ghRepo := resolveGHRepo(repoRef, repoRoot)
			prBranch, err := getPRBranch(prNumber, ghRepo)
			if err != nil {
				return fmt.Errorf("getting PR branch: %w", err)
			}
			branch = prBranch
			isPRFix = true

			// Look up the PR URL for state tracking
			prURL, err = getPRURL(prNumber, ghRepo)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not get PR URL for #%s: %v\n", prNumber, err)
			}

			fmt.Printf("Launching agent %s (PR #%s fix)...\n", id, prNumber)
			if targetRepo != nil {
				fmt.Printf("  target:   %s\n", *targetRepo)
			}

			// Fetch the PR branch
			if err := git.FetchBranch(repoRoot, prBranch); err != nil {
				return fmt.Errorf("fetching %s: %w", prBranch, err)
			}

			// Create worktree tracking the PR branch
			if err := git.WorktreeAddTrack(repoRoot, worktree, prBranch); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
		} else {
			branch = "agent/" + id

			fmt.Printf("Launching agent %s...\n", id)
			if targetRepo != nil {
				fmt.Printf("  target:   %s\n", *targetRepo)
			}

			// Fetch latest default branch
			if err := git.FetchBranch(repoRoot, defaultBranch); err != nil {
				return fmt.Errorf("fetching %s: %w", defaultBranch, err)
			}

			// Create worktree
			startPoint := "origin/" + defaultBranch
			if err := git.WorktreeAdd(repoRoot, worktree, branch, startPoint); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
		}
		fmt.Printf("  worktree: %s\n", worktree)
		fmt.Printf("  branch:   %s\n", branch)

		if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
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
			sysPrompt, err = config.RenderPrompt(repoRoot, config.PromptVars{
				RunID:    id,
				Issue:    issue,
				Branch:   branch,
				RepoName: repoName,
			})
		}
		if err != nil {
			return fmt.Errorf("rendering prompt: %w", err)
		}

		logFile := filepath.Join(store.LogDir(), id+".jsonl")

		// Build the claude command
		claudeCmd := buildClaudeCommand(sysPrompt, budget, prompt)

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
			paneCmd = buildSandboxPaneCommand(sandboxHostName, worktree, claudeCmd, logFile, selfBin, finalizePrefix, id)
		} else {
			paneCmd = buildPaneCommand(worktree, claudeCmd, logFile, selfBin, finalizePrefix, id)
		}

		// Launch in tmux pane, targeting the pane that ran this command
		currentPane := os.Getenv("TMUX_PANE")
		paneID, err := tmux.SplitWindow(currentPane, worktree, paneCmd)
		if err != nil {
			return fmt.Errorf("creating tmux pane: %w", err)
		}

		if err := tmux.SetPaneTitle(paneID, FormatPaneTitle(id, issue, prompt)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to set pane title: %v\n", err)
		}
		if err := tmux.SetWindowOption(paneID, "automatic-rename", "off"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to disable automatic rename: %v\n", err)
		}
		if err := tmux.LockPaneTitle(paneID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to lock pane title: %v\n", err)
		}
		if err := tmux.RebalanceLayout(currentPane); err != nil {
			return fmt.Errorf("rebalancing tmux layout: %w", err)
		}

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		issuePtr := stringPtr(issue)
		budgetPtr := &budget
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
			ID:         id,
			Prompt:     prompt,
			Issue:      issuePtr,
			PR:         stringPtr(prNumber),
			Branch:     branch,
			Worktree:   worktree,
			TmuxPane:   &paneID,
			Budget:     budgetPtr,
			LogFile:    logFilePtr,
			CreatedAt:  createdAt,
			Host:       hostPtr,
			TargetRepo: normalizedTarget,
			CloneDir:   cloneDirPtr,
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
		}

		fmt.Printf("  pane:     %s\n", paneID)
		if useSandbox {
			fmt.Printf("  host:     %s (sandbox)\n", sandboxHostName)
		} else {
			fmt.Printf("  host:     local\n")
		}
		fmt.Printf("  budget:   $%s\n", budget)
		fmt.Printf("  log:      %s\n", logFile)
		fmt.Println()
		fmt.Printf("Agent %s is running. Use 'klaus status' to check progress.\n", id)
		return nil
	},
}

func buildPaneCommand(worktree, claudeCmd, logFile, selfBin, finalizePrefix, id string) string {
	return fmt.Sprintf(
		"%scd %s && %s | tee %s | %s _format-stream; %s%s _finalize %s",
		tmuxSessionEnvPrefix(),
		shellQuote(worktree),
		claudeCmd,
		shellQuote(logFile),
		selfBin,
		finalizePrefix,
		selfBin,
		shellQuote(id),
	)
}

func buildClaudeCommand(sysPrompt, budget, prompt string) string {
	parts := []string{
		"claude", "-p",
		"--dangerously-skip-permissions",
		"--verbose",
		"--output-format", "stream-json",
		"--max-budget-usd", shellQuote(budget),
		"--append-system-prompt", shellQuote(sysPrompt),
		shellQuote(prompt),
	}
	return strings.Join(parts, " ")
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

// getPRURL returns the HTML URL for a PR using the gh CLI.
func getPRURL(prNumber string, repo ...string) (string, error) {
	args := []string{"pr", "view", "--json", "url", "-q", ".url"}
	if len(repo) > 0 && repo[0] != "" {
		args = append(args, "--repo", repo[0])
	}
	args = append(args, "--", prNumber)
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
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
		"%sssh %s 'cd %s && %s' | tee %s | %s _format-stream; %s%s _finalize %s; %s",
		tmuxSessionEnvPrefix(),
		shellQuote(host),
		shellQuote(worktree),
		claudeCmd,
		shellQuote(logFile),
		selfBin,
		finalizePrefix,
		selfBin,
		shellQuote(id),
		rsyncBack,
	)
}

func init() {
	launchCmd.Flags().String("issue", "", "GitHub issue number to reference")
	launchCmd.Flags().String("pr", "", "Push fixes to an existing PR's branch instead of creating a new PR")
	launchCmd.Flags().String("budget", "", "Max spend in USD (default from config)")
	launchCmd.Flags().String("repo", "", "Target repo: registered project name, owner/repo, or full URL")
	launchCmd.Flags().Bool("local", false, "Force local execution even when sandbox is configured")
	launchCmd.Flags().String("host", "", "Override sandbox host (ignores config sandbox_host)")
	rootCmd.AddCommand(launchCmd)
}
