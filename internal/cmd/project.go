package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/project"
	"github.com/spf13/cobra"
)

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage the project registry",
	Long: `Register, list, and remove projects from the klaus project registry.

The registry maps project names to local paths and is stored in ~/.klaus/projects.json.

  klaus project add owner/repo          Clone and register a project
  klaus project add owner/repo --path . Register with explicit local path
  klaus project list                    Show registered projects
  klaus project remove <name>           Unregister a project
  klaus project set-dir ~/hack          Set the default projects directory`,
}

var projectAddCmd = &cobra.Command{
	Use:   "add <owner/repo | name>",
	Short: "Clone and register a project",
	Long: `Register a project in the klaus project registry.

With owner/repo format, clones the repo into the projects directory and registers it.
With just a name, searches your GitHub repos for a match.

If --path is provided, registers the project at that path (must be an existing git repo).
If the repo is already cloned at the expected path, it is registered without re-cloning.`,
	Args: cobra.ExactArgs(1),
	RunE: runProjectAdd,
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show registered projects",
	Args:  cobra.NoArgs,
	RunE:  runProjectList,
}

var projectRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a project (does not delete the local clone)",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectRemove,
}

var projectSetDirCmd = &cobra.Command{
	Use:   "set-dir <path>",
	Short: "Set the default projects directory",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectSetDir,
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	ref := args[0]
	explicitPath, _ := cmd.Flags().GetString("path")

	reg, err := project.Load()
	if err != nil {
		return err
	}

	var owner, repoName, cloneURL string

	if strings.Contains(ref, "/") {
		// owner/repo format
		parts := strings.SplitN(ref, "/", 2)
		owner = parts[0]
		repoName = parts[1]
		cloneURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repoName)
	} else {
		// Bare name — search GitHub repos
		repoName = ref
		ghOwner, ghURL, err := resolveGitHubRepo(ref)
		if err != nil {
			return err
		}
		owner = ghOwner
		cloneURL = ghURL
	}

	if explicitPath != "" {
		// Register with explicit path
		expanded, err := project.ExpandHome(explicitPath)
		if err != nil {
			return err
		}
		absPath, err := filepath.Abs(expanded)
		if err != nil {
			return fmt.Errorf("resolving path: %w", err)
		}

		if !isGitRepo(absPath) {
			return fmt.Errorf("%s is not a git repository", absPath)
		}

		if err := reg.Add(repoName, absPath); err != nil {
			return err
		}
		if err := reg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Registered %s → %s\n", repoName, absPath)
		return nil
	}

	// Clone to projects_dir/<repo-name>
	projDir, err := reg.ExpandedProjectsDir()
	if err != nil {
		return err
	}
	targetDir := filepath.Join(projDir, repoName)

	if isGitRepo(targetDir) {
		fmt.Fprintf(cmd.OutOrStdout(), "Already cloned at %s\n", targetDir)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Cloning %s/%s into %s...\n", owner, repoName, targetDir)
		if err := gitClone(cloneURL, targetDir); err != nil {
			return fmt.Errorf("cloning: %w", err)
		}
	}

	if err := reg.Add(repoName, targetDir); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Registered %s → %s\n", repoName, targetDir)
	return nil
}

func runProjectList(cmd *cobra.Command, _ []string) error {
	reg, err := project.Load()
	if err != nil {
		return err
	}

	projects := reg.List()
	if len(projects) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No projects registered. Use 'klaus project add' to register one.")
		return nil
	}

	for name, localPath := range projects {
		remote := gitRemoteURL(localPath)
		if remote != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s  (%s)\n", name, localPath, remote)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s\n", name, localPath)
		}
	}
	return nil
}

func runProjectRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	reg, err := project.Load()
	if err != nil {
		return err
	}

	if err := reg.Remove(name); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from registry (local clone was not deleted)\n", name)
	return nil
}

func runProjectSetDir(cmd *cobra.Command, args []string) error {
	dir := args[0]

	expanded, err := project.ExpandHome(dir)
	if err != nil {
		return err
	}
	absDir, err := filepath.Abs(expanded)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}

	reg, err := project.Load()
	if err != nil {
		return err
	}

	reg.SetProjectsDir(absDir)
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Projects directory set to %s\n", absDir)
	return nil
}

// ghRepoEntry represents a repo from gh repo list JSON output.
type ghRepoEntry struct {
	Name          string `json:"name"`
	NameWithOwner string `json:"nameWithOwner"`
	URL           string `json:"url"`
}

// resolveGitHubRepo searches the user's GitHub repos for a name match.
// Returns (owner, cloneURL, error).
var resolveGitHubRepo = func(name string) (string, string, error) {
	out, err := exec.Command("gh", "repo", "list", "--json", "name,nameWithOwner,url", "--limit", "200").Output()
	if err != nil {
		return "", "", fmt.Errorf("running gh repo list: %w", err)
	}

	var repos []ghRepoEntry
	if err := json.Unmarshal(out, &repos); err != nil {
		return "", "", fmt.Errorf("parsing gh output: %w", err)
	}

	var matches []ghRepoEntry
	for _, r := range repos {
		if r.Name == name {
			matches = append(matches, r)
		}
	}

	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("no GitHub repo found matching %q. Use owner/repo format to be explicit", name)
	case 1:
		parts := strings.SplitN(matches[0].NameWithOwner, "/", 2)
		return parts[0], matches[0].URL + ".git", nil
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.NameWithOwner)
		}
		return "", "", fmt.Errorf("multiple repos match %q: %s. Use owner/repo format to be specific", name, strings.Join(names, ", "))
	}
}

// isGitRepo checks whether the given directory exists and contains a .git directory.
func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// gitClone clones a repository to the target directory.
var gitClone = func(url, targetDir string) error {
	c := exec.Command("git", "clone", url, targetDir)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// gitRemoteURL returns the origin remote URL for a repo, or empty string on error.
func gitRemoteURL(repoDir string) string {
	c := exec.Command("git", "-C", repoDir, "remote", "get-url", "origin")
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func init() {
	projectAddCmd.Flags().String("path", "", "Register with an explicit local path instead of cloning")
	projectCmd.AddCommand(projectAddCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectRemoveCmd)
	projectCmd.AddCommand(projectSetDirCmd)
	rootCmd.AddCommand(projectCmd)
}
