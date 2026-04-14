package cmd

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
)

// repoGroup is a set of runs and PRs belonging to the same repository.
type repoGroup struct {
	Repo  string
	Runs  []*run.State
	PRMap map[string]*prStatus
}

// markRunFailed sets sentinel finalization values on a stale run and
// cleans up its worktree and branch. Errors during cleanup are logged
// but do not prevent the state from being saved.
func markRunFailed(store run.StateStore, s *run.State) {
	cost := float64(-1)
	dur := int64(0)
	s.CostUSD = &cost
	s.DurationMS = &dur
	s.TmuxPane = nil
	cleanupWorktree(context.Background(), store, git.NewExecClient(), s)
	if err := store.Save(s); err != nil {
		slog.Warn("failed to save stale run state", "id", s.ID, "err", err)
	}
}

// isAgentRunning checks if a run's agent is currently active in tmux.
func (m *dashboardModel) isAgentRunning(s *run.State) bool {
	return s.IsAgentRunningWith(m.tmuxDeps)
}

// agentStatusLabel returns a display label for a non-running agent.
func agentStatusLabel(s *run.State) string {
	if s.PRURL != nil {
		return "PR"
	}
	return "EXITED"
}

// groupByRepo organizes states into repo groups, sorted by repo name.
func groupByRepo(states []*run.State) []repoGroup {
	reg, _ := project.Load()
	groups := make(map[string]*repoGroup)
	for _, s := range states {
		if s.Type == "session" {
			continue
		}
		repo := repoFromState(s, reg)
		g, ok := groups[repo]
		if !ok {
			g = &repoGroup{
				Repo:  repo,
				PRMap: make(map[string]*prStatus),
			}
			groups[repo] = g
		}
		g.Runs = append(g.Runs, s)
	}

	var result []repoGroup
	for _, g := range groups {
		result = append(result, *g)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Repo < result[j].Repo
	})
	return result
}

// repoFromState extracts the repo identifier from a run state, normalizing
// against the project registry so that different forms of the same repo
// (e.g. "cosmo" vs "patflynn/cosmo") group together.
func repoFromState(s *run.State, reg *project.Registry) string {
	if s.TargetRepo != nil && *s.TargetRepo != "" {
		return project.NormalizeRepoName(*s.TargetRepo, reg)
	}
	if s.PRURL != nil {
		ownerRepo := repoFromPRURL(*s.PRURL)
		if ownerRepo != "(unknown)" {
			return project.NormalizeRepoName(ownerRepo, reg)
		}
		return ownerRepo
	}
	return "(local)"
}

// repoFromPRURL extracts "owner/repo" from a GitHub PR URL.
func repoFromPRURL(prURL string) string {
	// https://github.com/owner/repo/pull/123
	prURL = strings.TrimPrefix(prURL, "https://github.com/")
	prURL = strings.TrimPrefix(prURL, "http://github.com/")
	parts := strings.Split(prURL, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return "(unknown)"
}

// computeTotalCost sums the cost across all runs. For runs without final cost,
// it includes the budget as an upper bound.
func computeTotalCost(states []*run.State) float64 {
	var total float64
	for _, s := range states {
		if s.CostUSD != nil {
			total += *s.CostUSD
		}
	}
	return total
}

// countAgents returns (running, total) agent counts (excludes sessions).
func (m *dashboardModel) countAgents(states []*run.State) (int, int) {
	var running, total int
	for _, s := range states {
		if s.Type == "session" {
			continue
		}
		total++
		if m.isAgentRunning(s) {
			running++
		}
	}
	return running, total
}

// computeSessionDuration returns the duration from the oldest active run's creation time to now.
func computeSessionDuration(states []*run.State) time.Duration {
	if len(states) == 0 {
		return 0
	}
	var oldest time.Time
	for _, s := range states {
		t, err := time.Parse(time.RFC3339, s.CreatedAt)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return time.Since(oldest)
}
