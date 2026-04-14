package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	gh "github.com/patflynn/klaus/internal/github"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/webhook"
)

// Messages for the bubbletea event loop.

type statesLoadedMsg struct {
	states []*run.State
}

type ghStatusMsg struct {
	statuses map[string]*prStatus
}

type fsEventMsg struct{}

type tickMsg struct{}

type sandboxStatusMsg struct {
	hosts map[string]bool // host -> reachable
}

type pipelineActionMsg struct {
	actions []pipeline.Action
}

type errMsg struct {
	err error
}

type webhookMsg struct {
	event webhook.Event
}

type trustedCommentsMsg struct {
	prNumber             string
	hasNewTrustedComments bool
}

// prStatus holds the GitHub-fetched status for a single PR.
type prStatus struct {
	PRNumber              string
	State                 string // OPEN, MERGED, CLOSED
	CI                    string // passing, failing, pending, unknown
	Conflicts             string // yes, none, unknown
	ReviewDecision        string // APPROVED, CHANGES_REQUESTED, etc.
	HasNewTrustedComments bool   // unaddressed comments from trusted reviewers
}

// Commands for the bubbletea event loop.

func loadStatesCmd(store run.StateStore) tea.Cmd {
	return func() tea.Msg {
		states, err := store.List()
		if err != nil {
			return errMsg{err: err}
		}
		return statesLoadedMsg{states: states}
	}
}

func fetchGHStatusCmd(client gh.Client, states []*run.State) tea.Cmd {
	return func() tea.Msg {
		statuses := make(map[string]*prStatus)
		seen := make(map[string]bool)
		for _, s := range states {
			prNum := extractPRNumber(s)
			prRef := extractPRRef(s)
			if prNum == "" || seen[prNum] {
				continue
			}
			seen[prNum] = true
			statuses[prNum] = fetchPRStatus(client, prNum, prRef)
		}
		return ghStatusMsg{statuses: statuses}
	}
}

func waitForWebhookCmd(ch <-chan webhook.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return webhookMsg{event: ev}
	}
}

func tickCmd() tea.Cmd {
	return func() tea.Msg {
		return tickMsg{}
	}
}

func tickAfterCmd() tea.Cmd {
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func startWatcherCmd(store run.StateStore) tea.Cmd {
	return func() tea.Msg {
		stateDir := store.StateDir()
		// Ensure the directory exists before watching
		os.MkdirAll(stateDir, 0o755)

		w, err := fsnotify.NewWatcher()
		if err != nil {
			return errMsg{err: fmt.Errorf("creating file watcher: %w", err)}
		}
		if err := w.Add(stateDir); err != nil {
			w.Close()
			return errMsg{err: fmt.Errorf("watching state dir: %w", err)}
		}
		return w
	}
}

func watchFSCmd(w *fsnotify.Watcher) tea.Cmd {
	return func() tea.Msg {
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return nil
				}
				if filepath.Ext(event.Name) == ".json" {
					return fsEventMsg{}
				}
			case _, ok := <-w.Errors:
				if !ok {
					return nil
				}
				// Ignore watcher errors and keep watching
			}
		}
	}
}

func checkSandboxCmd(states []*run.State) tea.Cmd {
	return func() tea.Msg {
		hosts := make(map[string]bool)
		for _, s := range states {
			if s.Host != nil && *s.Host != "" {
				if _, ok := hosts[*s.Host]; !ok {
					hosts[*s.Host] = CheckSandboxReachable(*s.Host)
				}
			}
		}
		if len(hosts) == 0 {
			return nil
		}
		return sandboxStatusMsg{hosts: hosts}
	}
}

// fetchPRStatus queries GitHub for a single PR's status.
// prRef should be a full PR URL so gh can resolve it from any directory.
func fetchPRStatus(client gh.Client, prNumber, prRef string) *prStatus {
	ctx := context.TODO()
	ps := &prStatus{PRNumber: prNumber}
	ps.State = client.GetState(ctx, prRef)
	if ps.State == "MERGED" || ps.State == "CLOSED" {
		return ps
	}
	ps.CI = client.GetCI(ctx, prRef)
	ps.Conflicts = client.GetConflicts(ctx, prRef)
	ps.ReviewDecision = client.GetReviewDecision(ctx, prRef)

	// When reviewDecision is not CHANGES_REQUESTED, check for unaddressed
	// trusted reviewer comments that GitHub doesn't reflect in reviewDecision.
	if !strings.EqualFold(ps.ReviewDecision, "CHANGES_REQUESTED") &&
		!strings.EqualFold(ps.ReviewDecision, "APPROVED") {
		ownerRepo := ownerRepoFromPRURL(prRef)
		if ownerRepo != "" {
			ps.HasNewTrustedComments = hasUnaddressedTrustedComments(ownerRepo, prNumber)
		}
	}
	return ps
}
