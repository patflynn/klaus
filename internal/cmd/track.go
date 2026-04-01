package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

var trackCmd = &cobra.Command{
	Use:   "track <pr-ref> [<pr-ref>...]",
	Short: "Track existing PRs on the dashboard",
	Long: `Adds existing PRs to the klaus dashboard for monitoring.

Accepts PR numbers, full GitHub PR URLs, or owner/repo#number references.
Use --repo to specify the repository when using bare PR numbers.

Examples:
  klaus track 405                              # uses session target repo
  klaus track 405 --repo cosmo                 # explicit repo
  klaus track https://github.com/org/repo/pull/405
  klaus track 405 406 407                      # multiple PRs`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		repoFlag, _ := cmd.Flags().GetString("repo")

		store, err := sessionStore()
		if err != nil {
			return err
		}
		if err := store.EnsureDirs(); err != nil {
			return err
		}

		// Load existing states to check for duplicates
		existingStates, err := store.List()
		if err != nil {
			return err
		}

		// Resolve repo context for bare PR numbers
		resolvedRepo := resolveTrackRepo(repoFlag)

		for _, ref := range args {
			if err := trackPR(ref, resolvedRepo, store, existingStates); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s: %v\n", ref, err)
			}
		}
		return nil
	},
}

// ownerRepoPRPattern matches owner/repo#number references.
var ownerRepoPRPattern = regexp.MustCompile(`^([^/]+/[^#]+)#(\d+)$`)

// parsePRRef parses a PR reference into (repo, prNumber) where repo may be empty.
// Supported formats:
//   - "405" → ("", "405")
//   - "https://github.com/org/repo/pull/405" → ("org/repo", "405")
//   - "owner/repo#405" → ("owner/repo", "405")
func parsePRRef(ref string) (repo, prNumber string, err error) {
	// Full GitHub URL
	if strings.HasPrefix(ref, "https://github.com/") {
		// https://github.com/owner/repo/pull/123
		trimmed := strings.TrimPrefix(ref, "https://github.com/")
		parts := strings.Split(trimmed, "/")
		if len(parts) >= 4 && parts[2] == "pull" {
			return parts[0] + "/" + parts[1], parts[3], nil
		}
		return "", "", fmt.Errorf("invalid GitHub PR URL: %s", ref)
	}

	// owner/repo#number
	if m := ownerRepoPRPattern.FindStringSubmatch(ref); m != nil {
		return m[1], m[2], nil
	}

	// Bare number
	for _, c := range ref {
		if c < '0' || c > '9' {
			return "", "", fmt.Errorf("invalid PR reference: %s", ref)
		}
	}
	if ref == "" {
		return "", "", fmt.Errorf("empty PR reference")
	}
	return "", ref, nil
}

// resolveTrackRepo determines which repo to use for bare PR numbers.
// Priority: --repo flag > session target.
func resolveTrackRepo(repoFlag string) string {
	if repoFlag != "" {
		return repoFlag
	}
	// Try session target
	if s, storeErr := sessionStore(); storeErr == nil {
		if hds, ok := s.(*run.HomeDirStore); ok {
			if target, loadErr := run.LoadTarget(hds.BaseDir()); loadErr == nil && target != "" {
				return target
			}
		}
	}
	return ""
}

// trackPR tracks a single PR reference.
func trackPR(ref, defaultRepo string, store run.StateStore, existingStates []*run.State) error {
	repo, prNumber, err := parsePRRef(ref)
	if err != nil {
		return err
	}

	// If no repo from the reference, use the default
	if repo == "" {
		repo = defaultRepo
	}
	if repo == "" {
		return fmt.Errorf("no repo specified for PR #%s — use --repo or set a session target", prNumber)
	}

	// Fetch PR metadata from GitHub
	prURL, title, headBranch, state, err := fetchPRMetadata(prNumber, repo)
	if err != nil {
		return fmt.Errorf("fetching PR #%s metadata: %w", prNumber, err)
	}

	// Check for duplicates
	for _, s := range existingStates {
		if s.PRURL != nil && *s.PRURL == prURL {
			fmt.Printf("Already tracking PR #%s (%s)\n", prNumber, repo)
			return nil
		}
	}

	// Warn if PR is closed/merged
	if strings.EqualFold(state, "MERGED") || strings.EqualFold(state, "CLOSED") {
		fmt.Fprintf(os.Stderr, "warning: PR #%s is %s\n", prNumber, strings.ToLower(state))
	}

	// Normalize repo name
	reg, _ := project.Load()
	normalizedRepo := project.NormalizeRepoName(repo, reg)

	id, err := run.GenID()
	if err != nil {
		return err
	}

	st := &run.State{
		ID:         id,
		Prompt:     title,
		Branch:     headBranch,
		PRURL:      &prURL,
		Type:       "track",
		TargetRepo: &normalizedRepo,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	if err := store.Save(st); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	fmt.Printf("Tracking PR #%s (%s) — %s\n", prNumber, normalizedRepo, title)
	return nil
}

// fetchPRMetadata fetches PR URL, title, head branch, and state from GitHub via gh CLI.
func fetchPRMetadata(prNumber, repo string) (prURL, title, headBranch, state string, err error) {
	args := []string{
		"pr", "view", prNumber,
		"--repo", repo,
		"--json", "url,title,headRefName,state",
		"-q", `(.url) + "\t" + (.title) + "\t" + (.headRefName) + "\t" + (.state)`,
	}
	cmd := exec.Command("gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", "", "", fmt.Errorf("gh pr view: %s", strings.TrimSpace(stderr.String()))
	}

	parts := strings.SplitN(strings.TrimSpace(stdout.String()), "\t", 4)
	if len(parts) < 4 {
		return "", "", "", "", fmt.Errorf("unexpected gh output: %s", stdout.String())
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func init() {
	trackCmd.Flags().String("repo", "", "Target repo: registered project name, owner/repo, or full URL")
	rootCmd.AddCommand(trackCmd)
}
