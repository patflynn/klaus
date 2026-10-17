package cmd

import (
	"os"
	"path/filepath"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

func resolveAgentBackend(cmd *cobra.Command, cfg config.Config) (backend.Kind, error) {
	name, _ := cmd.Flags().GetString("backend")
	if name == "" {
		// Read saved session state so dashboard dispatch works even when its tmux
		// pane did not inherit the coordinator process's environment.
		if store, err := sessionStore(); err == nil {
			if s, err := store.Load(os.Getenv(sessionIDEnv)); err == nil && s != nil {
				name = s.AgentBackend
			}
		}
	}
	if name == "" {
		name = os.Getenv("KLAUS_AGENT_BACKEND")
	}
	if name == "" {
		name = cfg.DefaultAgentBackend
	}
	return backend.Parse(name)
}

// claudeSettingsPath is the file a Claude coordinator loads with --settings
// (backend.Options.SettingsFile), in the session's prompts directory. It is
// never in the worktree, where git would see it.
func claudeSettingsPath(store run.StateStore, id string) string {
	return filepath.Join(run.PromptDir(store), id+".settings.json")
}

// writeClaudeSettings writes the settings that give a Claude coordinator a
// status line naming repo to path.
func writeClaudeSettings(path, repo string) error {
	return run.WritePromptFile(path, backend.ClaudeSettingsWithStatusLine(repo))
}

func trustBackendWorktree(k backend.Kind, dir string) error {
	if k == backend.Claude {
		return config.PreTrustWorktree(dir)
	}
	return nil
}
func linkBackendMemory(k backend.Kind, dir string) error {
	if k == backend.Claude {
		return config.LinkSharedMemory(dir)
	}
	return nil
}

func isClaudeRun(s *run.State) bool { return s.Backend == "" || s.Backend == string(backend.Claude) }
