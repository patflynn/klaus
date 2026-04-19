package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/nix"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/tmux"
	"github.com/spf13/cobra"
)

const (
	klausSessionIDEnv = "KLAUS_SESSION_ID"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Start or resume an interactive coordinator session",
	Long: `Resumes the most recent coordinator session, or creates a new one if none exists.
The coordinator runs in an isolated worktree, keeping the base repo clean on
the default branch. Must be run inside a tmux session.

Use 'klaus new' to explicitly start a fresh session.

If run outside a git repository, creates a scratch workspace and uses
~/.klaus/config.json for configuration. Use 'klaus launch --repo owner/repo'
from inside the session to target specific repositories.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSession(cmd, false)
	},
}

var newSessionCmd = &cobra.Command{
	Use:   "new",
	Short: "Start a fresh coordinator session",
	Long: `Creates a new isolated worktree and starts a fresh interactive Claude Code session
with no prior conversation context.

This is the same as the old default behavior of 'klaus' — use this when you
want to start clean instead of resuming the most recent session.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSession(cmd, true)
	},
}

func runSession(cmd *cobra.Command, forceNew bool) error {
	ctx := cmd.Context()
	tmuxClient := tmux.NewExecClient()

	if !tmux.InSession() {
		return fmt.Errorf("klaus session must be run inside a tmux session")
	}

	continueFlag, _ := cmd.Flags().GetBool("continue")
	resumeFlag, _ := cmd.Flags().GetString("resume")

	// Git repo is optional — session can run without one
	root, _ := git.RepoRoot()
	inRepo := root != ""
	gitClient := git.NewExecClient()

	cfg, err := config.Load(root)
	if err != nil {
		return err
	}

	sessionsDir, err := run.SessionsDir()
	if err != nil {
		return err
	}

	var id string
	var resuming bool

	switch {
	case forceNew:
		// Explicit fresh session via 'klaus new'
		baseID, err := run.GenID()
		if err != nil {
			return err
		}
		id = "session-" + baseID
	case continueFlag:
		found, err := run.FindMostRecentSession(sessionsDir)
		if err != nil {
			return fmt.Errorf("finding most recent session: %w", err)
		}
		id = found
		resuming = true
	case resumeFlag != "":
		id = resumeFlag
		dir := filepath.Join(sessionsDir, id)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("session directory does not exist: %s", id)
		}
		resuming = true
	default:
		// Default: resume most recent session if one exists
		found, findErr := run.FindMostRecentSession(sessionsDir)
		if findErr == nil {
			id = found
			resuming = true
		} else {
			// No existing session — create a new one
			baseID, err := run.GenID()
			if err != nil {
				return err
			}
			id = "session-" + baseID
		}
	}

	store, err := run.NewHomeDirStore(id)
	if err != nil {
		return err
	}
	if err := store.EnsureDirs(); err != nil {
		return err
	}

	var branch, repoName, worktree string
	var state *run.State

	if resuming {
		// Load existing state — only carry forward Claude session ID,
		// worktree path, and repo root. All ephemeral state (pane IDs,
		// etc.) starts clean to avoid stale references after tmux restarts.
		prevState, err := store.Load(id)
		if err != nil {
			return fmt.Errorf("loading session state: %w", err)
		}
		worktree = prevState.Worktree
		branch = prevState.Branch
		if branch != "" {
			repoName = filepath.Base(filepath.Dir(worktree))
		} else if inRepo {
			repoName = filepath.Base(root)
		}

		// Verify worktree still exists; recreate if cleaned up
		if _, statErr := os.Stat(worktree); os.IsNotExist(statErr) {
			if branch == "" {
				if err := os.MkdirAll(worktree, 0o755); err != nil {
					return fmt.Errorf("recreating scratch workspace: %w", err)
				}
			} else {
				baseRepo := root
				if prevState.RepoRoot != nil && *prevState.RepoRoot != "" {
					baseRepo = *prevState.RepoRoot
				}
				if baseRepo == "" {
					return fmt.Errorf("session worktree no longer exists and no repo root available: %s", worktree)
				}
				defaultBranch := cfg.DefaultBranch
				if err := gitClient.FetchBranch(ctx, baseRepo, defaultBranch); err != nil {
					return fmt.Errorf("fetching %s: %w", defaultBranch, err)
				}
				startPoint := "origin/" + defaultBranch
				if err := gitClient.WorktreeAdd(ctx, baseRepo, worktree, branch, startPoint); err != nil {
					return fmt.Errorf("recreating worktree: %w", err)
				}
			}
			fmt.Printf("Recreated worktree at %s\n", worktree)
		}

		// Clear stale tmux pane references from all agent runs in this session.
		// After a tmux restart, pane IDs recycle and point to unrelated panes.
		agentStates, _ := store.List()
		for _, as := range agentStates {
			if as.Type == "session" || as.TmuxPane == nil {
				continue
			}
			as.TmuxPane = nil
			if err := store.Save(as); err != nil {
				slog.Warn("failed to clear stale pane from agent", "id", as.ID, "err", err)
			}
		}

		// Build clean state, carrying forward only what matters
		state = &run.State{
			ID:              id,
			Type:            "session",
			Prompt:          "(interactive session)",
			Branch:          branch,
			Worktree:        worktree,
			CreatedAt:       prevState.CreatedAt,
			RepoRoot:        prevState.RepoRoot,
			ClaudeSessionID: prevState.ClaudeSessionID,
		}
		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving refreshed session state: %w", err)
		}

		fmt.Printf("Resuming coordinator session %s...\n", id)
		fmt.Printf("  worktree: %s\n", worktree)
		if branch != "" {
			fmt.Printf("  branch:   %s\n", branch)
		}
	} else {
		if inRepo {
			branch = "session/" + id
			repoName = filepath.Base(root)
			worktree = filepath.Join(cfg.WorktreeBase, repoName, id)
			defaultBranch := cfg.DefaultBranch

			fmt.Printf("Creating coordinator session %s...\n", id)

			if err := gitClient.FetchBranch(ctx, root, defaultBranch); err != nil {
				return fmt.Errorf("fetching %s: %w", defaultBranch, err)
			}

			startPoint := "origin/" + defaultBranch
			if err := gitClient.WorktreeAdd(ctx, root, worktree, branch, startPoint); err != nil {
				return fmt.Errorf("creating worktree: %w", err)
			}
			fmt.Printf("  worktree: %s\n", worktree)
			fmt.Printf("  branch:   %s\n", branch)

			if err := config.WriteClaudeSettings(worktree, repoName); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write .claude/settings.json: %v\n", err)
			}

			// Set up Nix dev environment if flake.nix exists
			nix.SetupDevEnvironment(worktree)
		} else {
			// No repo — use a scratch workspace
			worktree = filepath.Join(store.BaseDir(), "workspace")
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				return fmt.Errorf("creating scratch workspace: %w", err)
			}

			fmt.Printf("Creating coordinator session %s (no repo)...\n", id)
			fmt.Printf("  workspace: %s\n", worktree)
		}

		if err := config.PreTrustWorktree(worktree); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not pre-trust worktree: %v\n", err)
		}

		// Write state
		createdAt := time.Now().Format(time.RFC3339)
		var repoRoot *string
		if inRepo {
			repoRoot = &root
		}
		state = &run.State{
			ID:        id,
			Type:      "session",
			Prompt:    "(interactive session)",
			Branch:    branch,
			Worktree:  worktree,
			CreatedAt: createdAt,
			RepoRoot:  repoRoot,
		}
		if err := store.Save(state); err != nil {
			return fmt.Errorf("saving state: %w", err)
		}
	}

	// Load project list for session prompt
	var projectList string
	if reg, loadErr := project.Load(); loadErr == nil {
		projectList = config.FormatProjectList(reg.List())
	}

	// Render session system prompt
	sessionPrompt, err := config.RenderSessionPrompt(root, config.PromptVars{
		RunID:    id,
		Branch:   branch,
		RepoName: repoName,
		Projects: projectList,
	})
	if err != nil {
		return fmt.Errorf("rendering session prompt: %w", err)
	}

	// Configure tmux window for better situational awareness
	currentPane := os.Getenv("TMUX_PANE")
	if currentPane != "" {
		windowTitle := repoName
		if windowTitle == "" {
			windowTitle = "klaus"
		}
		tmuxClient.SetWindowOption(ctx, currentPane, "automatic-rename", "off")
		tmuxClient.RenameWindow(ctx, currentPane, windowTitle)
		tmuxClient.SetWindowOption(ctx, currentPane, "pane-border-status", "top")
		tmuxClient.SetWindowOption(ctx, currentPane, "pane-border-format", "#{pane_title}")
	}

	fmt.Println()
	fmt.Println("Starting interactive Claude Code session...")
	fmt.Println("  Use 'klaus launch' from inside to spawn workers.")
	fmt.Println()

	// Launch dashboard in a bottom pane before starting Claude.
	var dashPane string
	if currentPane != "" {
		dashCmd := fmt.Sprintf("KLAUS_SESSION_ID=%s klaus dashboard", id)
		paneID, err := tmuxClient.SplitWindowSized(ctx, currentPane, worktree, dashCmd, "-v", "30%")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open dashboard pane: %v\n", err)
		} else {
			dashPane = paneID
			state.DashboardPane = &dashPane
			if err := store.Save(state); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not persist dashboard pane: %v\n", err)
			}
		}
	}

	// Run claude interactively in the worktree, passing session ID to children.
	// Use --session-id to assign a stable UUID so we can resume later.
	claudeArgs := []string{
		"--dangerously-skip-permissions",
		"-n", id,
		"--append-system-prompt", sessionPrompt,
	}
	if resuming && state.ClaudeSessionID != nil && *state.ClaudeSessionID != "" {
		claudeArgs = append(claudeArgs, "--resume", *state.ClaudeSessionID)
	} else if resuming {
		claudeArgs = append(claudeArgs, "--continue")
	} else {
		// New session: generate a UUID and pass it so we can resume by ID later
		csID := genUUIDv4()
		if csID != "" {
			claudeArgs = append(claudeArgs, "--session-id", csID)
			state.ClaudeSessionID = &csID
			if err := store.Save(state); err != nil {
				slog.Warn("failed to save state with claude session ID", "id", state.ID, "err", err)
			}
		}
	}
	claude := exec.Command("claude", claudeArgs...)
	claude.Dir = worktree
	claude.Stdin = os.Stdin
	claude.Stdout = os.Stdout
	claude.Stderr = os.Stderr
	claude.Env = append(os.Environ(), klausSessionIDEnv+"="+id)
	claude.Run() // ignore error — user may exit normally

	fmt.Println()
	fmt.Printf("Session %s ended.\n", id)

	// Install signal handler so Ctrl+C during post-exit work exits promptly.
	sigCtx, sigStop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer sigStop()

	// Check for running agents and print a summary instead of blocking.
	running := checkRunningAgents(sigCtx, store, tmuxClient)

	if dashPane != "" {
		if len(running) > 0 {
			fmt.Println("Dashboard pane left open to monitor running agents.")
		} else {
			if err := tmuxClient.KillPane(sigCtx, dashPane); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not kill dashboard pane %s: %v\n", dashPane, err)
			}
		}
	}

	if inRepo {
		fmt.Printf("  Worktree preserved at: %s\n", worktree)
	}
	fmt.Printf("  To clean up: klaus cleanup %s\n", id)
	return nil
}

// checkRunningAgents inspects agent states and returns those still running
// in tmux panes. It prints a summary so the user knows what's still active.
// It does not block — agents continue in their tmux panes independently.
func checkRunningAgents(ctx context.Context, store run.StateStore, tc tmux.Client) []*run.State {
	states, err := store.List()
	if err != nil {
		return nil
	}

	td := run.TmuxDeps{
		PaneExists: func(id string) bool { return tc.PaneExists(ctx, id) },
		PaneIsIdle: func(id string) bool { return tc.PaneIsIdle(ctx, id) },
		PaneIsDead: func(id string) bool { return tc.PaneIsDead(ctx, id) },
	}

	var running []*run.State
	for _, s := range states {
		if s.Type == "session" {
			continue
		}
		if s.TmuxPane != nil && tc.PaneExists(ctx, *s.TmuxPane) && s.IsAgentRunningWith(td) {
			running = append(running, s)
		}
	}

	if len(running) == 0 {
		fmt.Println("No agents running.")
		return nil
	}

	fmt.Printf("%d agent(s) still running:\n", len(running))
	for _, s := range running {
		fmt.Printf("  - %s (pane %s)\n", s.ID, *s.TmuxPane)
	}
	return running
}

// genUUIDv4 returns a random UUID v4 string, or "" on error.
func genUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func init() {
	sessionCmd.Flags().Bool("continue", false, "Resume the most recent coordinator session")
	sessionCmd.Flags().String("resume", "", "Resume a specific session by ID")
	rootCmd.AddCommand(sessionCmd)
	rootCmd.AddCommand(newSessionCmd)
}
