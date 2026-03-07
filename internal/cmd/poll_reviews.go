package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/spf13/cobra"
)

var saveReviewBaselineCmd = &cobra.Command{
	Use:    "_save-review-baseline <pr-number> <output-file>",
	Short:  "Save current review comment IDs to a file",
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		outputFile := args[1]

		ids, err := fetchTrustedCommentIDs(prNumber)
		if err != nil {
			// Non-fatal: write empty baseline so polling still works
			fmt.Fprintf(os.Stderr, "warning: could not fetch review comments: %v\n", err)
			return os.WriteFile(outputFile, nil, 0o644)
		}

		return writeIDsToFile(outputFile, ids)
	},
}

var pollReviewsCmd = &cobra.Command{
	Use:    "_poll-reviews <pr-number> <baseline-file>",
	Short:  "Poll for new review comments after CI passes",
	Long: `Polls for new review comments on a PR that were not present in the baseline file.
Exits 0 if new comments are found (caller should re-enter the CI loop).
Exits 1 if the wait period expires with no new comments.`,
	Hidden: true,
	Args:   cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		baselineFile := args[1]
		waitSecs, _ := cmd.Flags().GetInt("wait")

		baseline, err := readIDsFromFile(baselineFile)
		if err != nil {
			// If we can't read the baseline, treat as empty
			baseline = map[int64]bool{}
		}

		newComments := pollForNewComments(prNumber, baseline, waitSecs)
		if len(newComments) > 0 {
			fmt.Printf("New review comments found:\n")
			for _, c := range newComments {
				fmt.Printf("  - [%d] %s: %s\n", c.ID, c.Path, truncate(c.Body, 120))
			}
			return nil // exit 0
		}

		// No new comments — signal caller to stop looping
		return fmt.Errorf("no new review comments after %ds", waitSecs)
	},
}

// pollForNewComments checks for new trusted review comments over the wait period.
// It polls every 15 seconds until the wait duration expires or new comments are found.
func pollForNewComments(prNumber string, baseline map[int64]bool, waitSecs int) []gh.PRReviewComment {
	if waitSecs <= 0 {
		return nil
	}

	interval := 15 * time.Second
	deadline := time.Now().Add(time.Duration(waitSecs) * time.Second)

	fmt.Printf("Waiting up to %ds for new review comments on PR #%s...\n", waitSecs, prNumber)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// Wait for the interval (or remaining time, whichever is shorter)
		wait := interval
		if remaining < wait {
			wait = remaining
		}
		time.Sleep(wait)

		comments, err := fetchTrustedComments(prNumber)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: polling review comments: %v\n", err)
			continue
		}

		var newComments []gh.PRReviewComment
		for _, c := range comments {
			if !baseline[c.ID] {
				newComments = append(newComments, c)
			}
		}

		if len(newComments) > 0 {
			return newComments
		}
	}

	return nil
}

// fetchTrustedCommentIDs returns the IDs of all review comments from trusted reviewers.
func fetchTrustedCommentIDs(prNumber string) ([]int64, error) {
	comments, err := fetchTrustedComments(prNumber)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(comments))
	for i, c := range comments {
		ids[i] = c.ID
	}
	return ids, nil
}

// fetchTrustedComments returns review comments from trusted reviewers only.
func fetchTrustedComments(prNumber string) ([]gh.PRReviewComment, error) {
	owner, repo, err := gh.GetRepoOwnerAndName()
	if err != nil {
		return nil, fmt.Errorf("getting repo info: %w", err)
	}

	comments, err := gh.FetchPRReviewComments(owner, repo, prNumber)
	if err != nil {
		return nil, err
	}

	root, err := git.RepoRoot()
	if err != nil {
		// Can't load config for trusted filtering; return all comments
		return comments, nil
	}

	cfg, err := config.Load(root)
	if err != nil {
		return comments, nil
	}

	trusted := buildTrustedSet(cfg, owner, repo)
	var filtered []gh.PRReviewComment
	for _, c := range comments {
		if trusted[c.User.Login] {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

func writeIDsToFile(path string, ids []int64) error {
	var sb strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&sb, "%d\n", id)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func readIDsFromFile(path string) (map[int64]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ids := make(map[int64]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		ids[id] = true
	}
	return ids, scanner.Err()
}

func init() {
	pollReviewsCmd.Flags().Int("wait", 120, "Seconds to wait for new review comments")
	rootCmd.AddCommand(saveReviewBaselineCmd)
	rootCmd.AddCommand(pollReviewsCmd)
}
