package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// Trajectory replay continues a budget-paused PR's prior Claude conversation
// instead of dispatching a fresh agent that must re-explore the repo.
//
// Mechanism: klaus stores the resume-able conversation JSONL (the file
// claude itself writes under ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl)
// on refs/klaus/data at sessions/<run-id>.jsonl. On 'klaus launch --pr'
// against a paused PR, we fetch that blob and restore it into the *new*
// worktree's project dir, then invoke 'claude --resume <uuid>'. claude
// re-anchors to the new cwd; the conversation picks up where it paused.
//
// IMPORTANT: this is NOT the stream-json log at logs/<run-id>.jsonl —
// claude --resume rejects that format ("No conversation found"). The two
// are different schemas; see findResumeConversation.

// encodeProjectPath converts an absolute working directory into the directory
// segment Claude Code uses under ~/.claude/projects. Claude replaces both '/'
// and '.' with '-'. Verified empirically against Claude Code 2.1.x.
func encodeProjectPath(cwd string) string {
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}

// claudeProjectsDir returns ~/.claude/projects, or "" if the home dir is
// unknown.
func claudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// claudeConversationPath returns the path claude --resume reads for a session
// whose conversation runs in worktree cwd.
func claudeConversationPath(cwd, sessionUUID string) string {
	base := claudeProjectsDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, encodeProjectPath(cwd), sessionUUID+".jsonl")
}

// validSessionUUID reports whether s is safe to interpolate into a filepath
// glob/join (UUID-shaped: alphanumerics and hyphens only).
func validSessionUUID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// findClaudeConversationFile globs every project dir for a session UUID's
// conversation file, returning the first match or "".
func findClaudeConversationFile(sessionUUID string) string {
	if !validSessionUUID(sessionUUID) {
		return ""
	}
	base := claudeProjectsDir()
	if base == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(base, "*", sessionUUID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// findResumeConversation locates the resume-able Claude conversation file for
// a finalized run. It prefers the run's own worktree project dir, then falls
// back to globbing all project dirs. Returns "" if the UUID is unknown or no
// file exists.
func findResumeConversation(state *run.State) string {
	if state == nil {
		return ""
	}
	uuid := ""
	if state.ClaudeSessionID != nil {
		uuid = *state.ClaudeSessionID
	}
	if uuid == "" && state.LogFile != nil {
		uuid = ExtractClaudeSessionID(*state.LogFile)
	}
	if !validSessionUUID(uuid) {
		return ""
	}
	if state.Worktree != "" {
		if p := claudeConversationPath(state.Worktree, uuid); p != "" {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return findClaudeConversationFile(uuid)
}

// extractConversationSessionID reads the sessionId (camelCase) from a
// conversation-format JSONL blob. Returns "" if none is found.
func extractConversationSessionID(data []byte) string {
	// bufio.Reader.ReadBytes avoids the fixed buffer cap of bufio.Scanner,
	// which would silently stop (bufio.ErrTooLong) on a single line larger
	// than the cap — tool outputs and system prompts can exceed many MB.
	r := bufio.NewReader(bytes.NewReader(data))
	var ev struct {
		SessionID string `json:"sessionId"`
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if uerr := json.Unmarshal(line, &ev); uerr == nil && ev.SessionID != "" {
				return ev.SessionID
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

// replayParams carries everything resolveBudgetPausedReplay needs.
type replayParams struct {
	GitClient   git.Client
	Store       run.StateStore
	RepoRoot    string // repo dir holding refs/klaus/data (clone dir or host repo)
	DataRef     string
	Worktree    string // the fresh worktree the resumed agent will run in
	PRBranch    string
	PRNumber    string
	GHRepo      string // owner/repo for gh calls (may be "")
	ForceReplay bool   // --replay: skip the size threshold and the paused gate
	ThresholdKB int    // max trajectory size in KB (0 disables the size check)
}

// replayDecision is the outcome of attempting trajectory replay.
type replayDecision struct {
	// SessionUUID is non-empty when the caller should run claude --resume
	// against it; empty means fall back to the fresh-agent flow.
	SessionUUID string
	// SourceRunID is the run whose trajectory was restored (for logging).
	SourceRunID string
	// Reason explains the decision (always set, for user-facing logging).
	Reason string
}

// resolveBudgetPausedReplay decides whether to continue a budget-paused PR's
// prior Claude conversation. On success it restores the stored trajectory into
// the new worktree's project dir and returns the session UUID to resume. On
// any miss it returns an empty SessionUUID with a Reason, signalling the caller
// to fall back to the fresh-agent flow.
func resolveBudgetPausedReplay(ctx context.Context, p replayParams) replayDecision {
	// Replay only targets budget-paused PRs unless the user forces it.
	if !p.ForceReplay {
		paused, err := draft.HasBudgetPausedLabel(ctx, budgetPauseRunner, p.Worktree, p.GHRepo, p.PRNumber)
		if err != nil {
			return replayDecision{Reason: fmt.Sprintf("could not check budget-paused label: %v", err)}
		}
		if !paused {
			return replayDecision{Reason: "PR is not budget-paused"}
		}
	}

	// Find candidate runs on this branch, newest first (List() is sorted by
	// CreatedAt descending).
	states, err := p.Store.List()
	if err != nil {
		return replayDecision{Reason: fmt.Sprintf("listing runs: %v", err)}
	}
	var candidates []*run.State
	for _, s := range states {
		if s.Branch == p.PRBranch {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return replayDecision{Reason: "no prior run recorded for this branch"}
	}

	// Best-effort: pull the latest data ref so the trajectory blob is present
	// even on a fresh machine. Errors are expected (no remote, ref absent).
	_ = p.GitClient.FetchDataRef(ctx, p.RepoRoot, p.DataRef)

	// Use the most recent candidate that actually has a stored trajectory.
	for _, s := range candidates {
		treePath := "sessions/" + s.ID + ".jsonl"
		blob, err := p.GitClient.ReadDataRefFile(ctx, p.RepoRoot, p.DataRef, treePath)
		if err != nil {
			// Sensitive-skipped, never pushed, or pre-dates this feature.
			continue
		}

		if !p.ForceReplay && p.ThresholdKB > 0 && len(blob) > p.ThresholdKB*1024 {
			return replayDecision{Reason: fmt.Sprintf("trajectory for %s is %dKB, over the %dKB threshold (use --replay to force)", s.ID, len(blob)/1024, p.ThresholdKB)}
		}

		uuid := extractConversationSessionID(blob)
		if !validSessionUUID(uuid) {
			return replayDecision{Reason: fmt.Sprintf("stored trajectory for %s has no usable session UUID", s.ID)}
		}

		dest := claudeConversationPath(p.Worktree, uuid)
		if dest == "" {
			return replayDecision{Reason: "could not resolve Claude projects dir"}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return replayDecision{Reason: fmt.Sprintf("creating project dir: %v", err)}
		}
		if err := os.WriteFile(dest, blob, 0o600); err != nil {
			return replayDecision{Reason: fmt.Sprintf("restoring trajectory: %v", err)}
		}
		return replayDecision{
			SessionUUID: uuid,
			SourceRunID: s.ID,
			Reason:      fmt.Sprintf("resuming conversation from run %s (%dKB)", s.ID, len(blob)/1024),
		}
	}

	return replayDecision{Reason: "no stored trajectory found on the data ref (sensitive-skipped or pre-dates replay)"}
}
