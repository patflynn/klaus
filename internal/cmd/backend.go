package cmd

import (
	"os"

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

func prepareBackendWorktree(k backend.Kind, dir, repo string) error {
	if k == backend.Claude {
		return config.WriteClaudeSettings(dir, repo)
	}
	return nil
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
