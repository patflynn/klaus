package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/project"
	"github.com/spf13/cobra"
)

// webhookEvents is the set of GitHub events the webhook subscribes to.
var webhookEvents = []string{
	"push",
	"check_run",
	"check_suite",
	"pull_request",
	"pull_request_review",
}

// ghHook represents a GitHub repository webhook from the API.
type ghHook struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	} `json:"config"`
}

// webhookStatus holds the check result for a single project.
type webhookStatus struct {
	Project string
	Owner   string
	Repo    string
	HookID  int64  // 0 means not found
	Error   string // non-empty on failure
}

// Function variables for testing.
var (
	webhookListHooks    = listRepoHooks
	webhookCreateHook   = createRepoHook
	webhookResolveRepo  = func(dir string) (string, string, error) {
		return github.NewGHCLIClient("").GetRepoOwnerAndNameFromDir(context.TODO(), dir)
	}
	webhookLoadConfig   = func() (config.Config, error) { return config.Load("") }
	webhookLoadRegistry = project.Load
	webhookReadFile     = os.ReadFile
)

var webhookCmd = &cobra.Command{
	Use:   "webhook",
	Short: "Manage GitHub webhooks for registered projects",
	Long: `Check and configure GitHub webhooks for registered projects.

Webhooks allow Klaus to receive push notifications from GitHub via github-relay
instead of polling. Each project's repo needs a webhook pointing at the relay URL.

Configure relay_url and secret_file in your klaus config webhook section:

  {
    "webhook": {
      "relay_url": "https://example.ts.net",
      "secret_file": "/path/to/webhook-secret"
    }
  }`,
}

var webhookCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check if webhooks are configured for registered projects",
	Args:  cobra.NoArgs,
	RunE:  runWebhookCheck,
}

var webhookSetupCmd = &cobra.Command{
	Use:   "setup [project-name]",
	Short: "Create missing webhooks for registered projects",
	Long: `Create GitHub webhooks for projects that don't have one configured.

With no arguments, checks all registered projects and creates webhooks for any
that are missing. With a project name, sets up the webhook for that project only.

Requires relay_url and secret_file in the webhook config.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWebhookSetup,
}

func runWebhookCheck(cmd *cobra.Command, _ []string) error {
	cfg, err := webhookLoadConfig()
	if err != nil {
		return err
	}
	if cfg.Webhook == nil || cfg.Webhook.RelayURL == "" {
		return fmt.Errorf("webhook.relay_url is not configured; add it to your klaus config")
	}

	statuses, err := checkAllProjects(cfg.Webhook.RelayURL)
	if err != nil {
		return err
	}

	if len(statuses) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No projects registered. Use 'klaus project add' to register one.")
		return nil
	}

	for _, s := range statuses {
		if s.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  error: %s\n", s.Project, s.Owner, s.Repo, s.Error)
		} else if s.HookID != 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  ✓ configured (hook %d)\n", s.Project, s.Owner, s.Repo, s.HookID)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  ✗ missing\n", s.Project, s.Owner, s.Repo)
		}
	}
	return nil
}

func runWebhookSetup(cmd *cobra.Command, args []string) error {
	cfg, err := webhookLoadConfig()
	if err != nil {
		return err
	}
	if cfg.Webhook == nil || cfg.Webhook.RelayURL == "" {
		return fmt.Errorf("webhook.relay_url is not configured; add it to your klaus config")
	}
	if cfg.Webhook.SecretFile == "" {
		return fmt.Errorf("webhook.secret_file is not configured; add it to your klaus config")
	}

	secretBytes, err := webhookReadFile(cfg.Webhook.SecretFile)
	if err != nil {
		return fmt.Errorf("reading webhook secret: %w", err)
	}
	secret := strings.TrimSpace(string(secretBytes))
	if secret == "" {
		return fmt.Errorf("webhook secret file is empty: %s", cfg.Webhook.SecretFile)
	}

	reg, err := webhookLoadRegistry()
	if err != nil {
		return err
	}
	projects := reg.List()
	if len(projects) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No projects registered. Use 'klaus project add' to register one.")
		return nil
	}

	// If a specific project was requested, filter to just that one.
	if len(args) == 1 {
		name := args[0]
		path, ok := projects[name]
		if !ok {
			return fmt.Errorf("project %q is not registered", name)
		}
		projects = map[string]string{name: path}
	}

	relayURL := cfg.Webhook.RelayURL
	var created, skipped, errored int

	for name, localPath := range projects {
		owner, repo, err := webhookResolveRepo(localPath)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s error resolving repo: %v\n", name, err)
			errored++
			continue
		}

		hookID, err := findMatchingHook(owner, repo, relayURL)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  error checking hooks: %v\n", name, owner, repo, err)
			errored++
			continue
		}
		if hookID != 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  already configured (hook %d)\n", name, owner, repo, hookID)
			skipped++
			continue
		}

		if err := webhookCreateHook(owner, repo, relayURL, secret); err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  error creating webhook: %v\n", name, owner, repo, err)
			errored++
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s/%s  ✓ webhook created\n", name, owner, repo)
		created++
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\nDone: %d created, %d already configured, %d errors\n", created, skipped, errored)
	return nil
}

// checkAllProjects checks webhook status for all registered projects.
func checkAllProjects(relayURL string) ([]webhookStatus, error) {
	reg, err := webhookLoadRegistry()
	if err != nil {
		return nil, err
	}
	projects := reg.List()

	var statuses []webhookStatus
	for name, localPath := range projects {
		s := webhookStatus{Project: name}

		owner, repo, err := webhookResolveRepo(localPath)
		if err != nil {
			s.Error = fmt.Sprintf("resolving repo: %v", err)
			statuses = append(statuses, s)
			continue
		}
		s.Owner = owner
		s.Repo = repo

		hookID, err := findMatchingHook(owner, repo, relayURL)
		if err != nil {
			s.Error = fmt.Sprintf("listing hooks: %v", err)
			statuses = append(statuses, s)
			continue
		}
		s.HookID = hookID
		statuses = append(statuses, s)
	}
	return statuses, nil
}

// findMatchingHook checks if a repo has a webhook whose URL matches the relay URL.
// Returns the hook ID if found, 0 if not.
func findMatchingHook(owner, repo, relayURL string) (int64, error) {
	hooks, err := webhookListHooks(owner, repo)
	if err != nil {
		return 0, err
	}
	return matchHook(hooks, relayURL), nil
}

// matchHook returns the ID of the first hook whose config URL starts with the
// relay URL, or 0 if none match.
func matchHook(hooks []ghHook, relayURL string) int64 {
	// Normalize: ensure relayURL doesn't have trailing slash for comparison.
	relayURL = strings.TrimRight(relayURL, "/")
	for _, h := range hooks {
		hookURL := strings.TrimRight(h.Config.URL, "/")
		if hookURL == relayURL || strings.HasPrefix(hookURL, relayURL+"/") {
			return h.ID
		}
	}
	return 0
}

// listRepoHooks fetches webhooks for a repository via the GitHub API.
func listRepoHooks(owner, repo string) ([]ghHook, error) {
	client := github.NewGHCLIClient("")
	data, err := client.APIGet(context.TODO(), fmt.Sprintf("repos/%s/%s/hooks", owner, repo))
	if err != nil {
		return nil, err
	}
	var hooks []ghHook
	if err := json.Unmarshal(data, &hooks); err != nil {
		return nil, fmt.Errorf("parsing hooks response: %w", err)
	}
	return hooks, nil
}

// createRepoHook creates a webhook on a GitHub repository.
func createRepoHook(owner, repo, relayURL, secret string) error {
	client := github.NewGHCLIClient("")
	body := map[string]interface{}{
		"name":   "web",
		"active": true,
		"events": webhookEvents,
		"config": map[string]string{
			"url":          relayURL,
			"content_type": "json",
			"secret":       secret,
		},
	}
	_, err := client.APIPostJSON(context.TODO(), fmt.Sprintf("repos/%s/%s/hooks", owner, repo), body)
	return err
}

func init() {
	webhookCmd.AddCommand(webhookCheckCmd)
	webhookCmd.AddCommand(webhookSetupCmd)
	rootCmd.AddCommand(webhookCmd)
}
