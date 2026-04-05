package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/project"
)

func TestMatchHook(t *testing.T) {
	tests := []struct {
		name     string
		hooks    []ghHook
		relayURL string
		wantID   int64
	}{
		{
			name:     "no hooks",
			hooks:    nil,
			relayURL: "https://relay.example.com",
			wantID:   0,
		},
		{
			name: "exact match",
			hooks: []ghHook{
				{ID: 42, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   42,
		},
		{
			name: "match with trailing slash on relay",
			hooks: []ghHook{
				{ID: 99, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com"}},
			},
			relayURL: "https://relay.example.com/",
			wantID:   99,
		},
		{
			name: "match with trailing slash on hook",
			hooks: []ghHook{
				{ID: 55, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com/"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   55,
		},
		{
			name: "hook URL is subpath of relay",
			hooks: []ghHook{
				{ID: 10, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com/webhook/github"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   10,
		},
		{
			name: "no match - different host",
			hooks: []ghHook{
				{ID: 77, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://other.example.com"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   0,
		},
		{
			name: "multiple hooks, second matches",
			hooks: []ghHook{
				{ID: 1, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://other.example.com"}},
				{ID: 2, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com/webhook"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   2,
		},
		{
			name: "partial hostname should not match",
			hooks: []ghHook{
				{ID: 88, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com.evil.com"}},
			},
			relayURL: "https://relay.example.com",
			wantID:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchHook(tt.hooks, tt.relayURL)
			if got != tt.wantID {
				t.Errorf("matchHook() = %d, want %d", got, tt.wantID)
			}
		})
	}
}

func TestWebhookCheck_NoRelayURL(t *testing.T) {
	orig := webhookLoadConfig
	defer func() { webhookLoadConfig = orig }()

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{}, nil
	}

	var buf bytes.Buffer
	webhookCheckCmd.SetOut(&buf)
	webhookCheckCmd.SetErr(&buf)

	err := webhookCheckCmd.RunE(webhookCheckCmd, nil)
	if err == nil || err.Error() != "webhook.relay_url is not configured; add it to your klaus config" {
		t.Errorf("expected relay_url error, got: %v", err)
	}
}

func TestWebhookCheck_NoProjects(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
	}()

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{RelayURL: "https://relay.example.com"},
		}, nil
	}
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{}}, nil
	}

	var buf bytes.Buffer
	webhookCheckCmd.SetOut(&buf)
	webhookCheckCmd.SetErr(&buf)

	if err := webhookCheckCmd.RunE(webhookCheckCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); out != "No projects registered. Use 'klaus project add' to register one.\n" {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestWebhookCheck_MixedStatuses(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	origResolve := webhookResolveRepo
	origList := webhookListHooks
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
		webhookResolveRepo = origResolve
		webhookListHooks = origList
	}()

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{RelayURL: "https://relay.example.com"},
		}, nil
	}
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{
			"proj-a": "/tmp/proj-a",
			"proj-b": "/tmp/proj-b",
		}}, nil
	}
	webhookResolveRepo = func(dir string) (string, string, error) {
		switch dir {
		case "/tmp/proj-a":
			return "owner", "proj-a", nil
		case "/tmp/proj-b":
			return "owner", "proj-b", nil
		}
		return "", "", nil
	}
	webhookListHooks = func(owner, repo string) ([]ghHook, error) {
		if repo == "proj-a" {
			return []ghHook{
				{ID: 42, Config: struct {
					URL         string `json:"url"`
					ContentType string `json:"content_type"`
				}{URL: "https://relay.example.com/webhook"}},
			}, nil
		}
		return nil, nil
	}

	var buf bytes.Buffer
	webhookCheckCmd.SetOut(&buf)
	webhookCheckCmd.SetErr(&buf)

	if err := webhookCheckCmd.RunE(webhookCheckCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !containsSubstring(out, "configured") {
		t.Errorf("expected 'configured' in output, got: %s", out)
	}
	if !containsSubstring(out, "missing") {
		t.Errorf("expected 'missing' in output, got: %s", out)
	}
}

func TestWebhookSetup_CreatesHook(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	origResolve := webhookResolveRepo
	origList := webhookListHooks
	origCreate := webhookCreateHook
	origRead := webhookReadFile
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
		webhookResolveRepo = origResolve
		webhookListHooks = origList
		webhookCreateHook = origCreate
		webhookReadFile = origRead
	}()

	// Write a temp secret file.
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret")
	os.WriteFile(secretPath, []byte("my-secret\n"), 0o644)

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{
				RelayURL:   "https://relay.example.com",
				SecretFile: secretPath,
			},
		}, nil
	}
	webhookReadFile = os.ReadFile
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{
			"myproject": "/tmp/myproject",
		}}, nil
	}
	webhookResolveRepo = func(dir string) (string, string, error) {
		return "owner", "myproject", nil
	}
	webhookListHooks = func(owner, repo string) ([]ghHook, error) {
		return nil, nil // no existing hooks
	}

	var createdOwner, createdRepo, createdURL, createdSecret string
	webhookCreateHook = func(owner, repo, relayURL, secret string) error {
		createdOwner = owner
		createdRepo = repo
		createdURL = relayURL
		createdSecret = secret
		return nil
	}

	var buf bytes.Buffer
	webhookSetupCmd.SetOut(&buf)
	webhookSetupCmd.SetErr(&buf)

	if err := webhookSetupCmd.RunE(webhookSetupCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createdOwner != "owner" || createdRepo != "myproject" {
		t.Errorf("unexpected repo: %s/%s", createdOwner, createdRepo)
	}
	if createdURL != "https://relay.example.com" {
		t.Errorf("unexpected relay URL: %s", createdURL)
	}
	if createdSecret != "my-secret" {
		t.Errorf("unexpected secret: %q", createdSecret)
	}
	if !containsSubstring(buf.String(), "webhook created") {
		t.Errorf("expected 'webhook created' in output, got: %s", buf.String())
	}
}

func TestWebhookSetup_SkipsExisting(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	origResolve := webhookResolveRepo
	origList := webhookListHooks
	origCreate := webhookCreateHook
	origRead := webhookReadFile
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
		webhookResolveRepo = origResolve
		webhookListHooks = origList
		webhookCreateHook = origCreate
		webhookReadFile = origRead
	}()

	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret")
	os.WriteFile(secretPath, []byte("s3cret"), 0o644)

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{
				RelayURL:   "https://relay.example.com",
				SecretFile: secretPath,
			},
		}, nil
	}
	webhookReadFile = os.ReadFile
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{
			"myproject": "/tmp/myproject",
		}}, nil
	}
	webhookResolveRepo = func(dir string) (string, string, error) {
		return "owner", "myproject", nil
	}
	webhookListHooks = func(owner, repo string) ([]ghHook, error) {
		return []ghHook{
			{ID: 100, Config: struct {
				URL         string `json:"url"`
				ContentType string `json:"content_type"`
			}{URL: "https://relay.example.com"}},
		}, nil
	}

	createCalled := false
	webhookCreateHook = func(owner, repo, relayURL, secret string) error {
		createCalled = true
		return nil
	}

	var buf bytes.Buffer
	webhookSetupCmd.SetOut(&buf)
	webhookSetupCmd.SetErr(&buf)

	if err := webhookSetupCmd.RunE(webhookSetupCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createCalled {
		t.Error("createHook should not have been called for existing webhook")
	}
	if !containsSubstring(buf.String(), "already configured") {
		t.Errorf("expected 'already configured' in output, got: %s", buf.String())
	}
}

func TestWebhookSetup_MissingSecretFile(t *testing.T) {
	origConfig := webhookLoadConfig
	origRead := webhookReadFile
	defer func() {
		webhookLoadConfig = origConfig
		webhookReadFile = origRead
	}()

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{
				RelayURL:   "https://relay.example.com",
				SecretFile: "/nonexistent/secret",
			},
		}, nil
	}
	webhookReadFile = os.ReadFile

	err := webhookSetupCmd.RunE(webhookSetupCmd, nil)
	if err == nil || !containsSubstring(err.Error(), "reading webhook secret") {
		t.Errorf("expected secret file error, got: %v", err)
	}
}

func TestWebhookSetup_SpecificProject(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	origResolve := webhookResolveRepo
	origList := webhookListHooks
	origCreate := webhookCreateHook
	origRead := webhookReadFile
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
		webhookResolveRepo = origResolve
		webhookListHooks = origList
		webhookCreateHook = origCreate
		webhookReadFile = origRead
	}()

	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret")
	os.WriteFile(secretPath, []byte("s3cret"), 0o644)

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{
				RelayURL:   "https://relay.example.com",
				SecretFile: secretPath,
			},
		}, nil
	}
	webhookReadFile = os.ReadFile
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{
			"proj-a": "/tmp/proj-a",
			"proj-b": "/tmp/proj-b",
		}}, nil
	}

	var resolvedDirs []string
	webhookResolveRepo = func(dir string) (string, string, error) {
		resolvedDirs = append(resolvedDirs, dir)
		return "owner", filepath.Base(dir), nil
	}
	webhookListHooks = func(owner, repo string) ([]ghHook, error) {
		return nil, nil
	}
	webhookCreateHook = func(owner, repo, relayURL, secret string) error {
		return nil
	}

	var buf bytes.Buffer
	webhookSetupCmd.SetOut(&buf)
	webhookSetupCmd.SetErr(&buf)

	if err := webhookSetupCmd.RunE(webhookSetupCmd, []string{"proj-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resolvedDirs) != 1 || resolvedDirs[0] != "/tmp/proj-a" {
		t.Errorf("expected only proj-a to be resolved, got: %v", resolvedDirs)
	}
}

func TestWebhookSetup_UnknownProject(t *testing.T) {
	origConfig := webhookLoadConfig
	origRegistry := webhookLoadRegistry
	origRead := webhookReadFile
	defer func() {
		webhookLoadConfig = origConfig
		webhookLoadRegistry = origRegistry
		webhookReadFile = origRead
	}()

	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret")
	os.WriteFile(secretPath, []byte("s3cret"), 0o644)

	webhookLoadConfig = func() (config.Config, error) {
		return config.Config{
			Webhook: &config.WebhookConfig{
				RelayURL:   "https://relay.example.com",
				SecretFile: secretPath,
			},
		}, nil
	}
	webhookReadFile = os.ReadFile
	webhookLoadRegistry = func() (*project.Registry, error) {
		return &project.Registry{Projects: map[string]string{
			"proj-a": "/tmp/proj-a",
		}}, nil
	}

	err := webhookSetupCmd.RunE(webhookSetupCmd, []string{"nonexistent"})
	if err == nil || err.Error() != `project "nonexistent" is not registered` {
		t.Errorf("expected not registered error, got: %v", err)
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && bytes.Contains([]byte(s), []byte(substr))
}
