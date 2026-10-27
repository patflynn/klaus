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

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/draft"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/run"
)

// Trajectory replay continues the prior Claude conversation on a budget-paused
// PR, or on a change continued with --change, instead of dispatching a fresh
// agent that must re-explore the repo.
//
// Mechanism: klaus stores the resume-able conversation JSONL (the file
// claude itself writes under ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl)
// on the local clone's refs/klaus/data at sessions/<run-id>.jsonl. On 'klaus
// launch --pr' against a paused PR, or 'klaus launch --change', we read that
// blob and restore it into the *new* worktree's project dir, then invoke
// 'claude --resume <uuid>'. claude
// re-anchors to the new cwd; the conversation picks up where it paused. The
// ref reaches origin only when push_data_ref is set, so by default replay
// works only on the machine whose clone ran the paused agent.
//
// IMPORTANT: this is NOT the stream-json log at logs/<run-id>.jsonl —
// claude --resume rejects that format ("No conversation found"). The two
// are different schemas; see findResumeConversation.
//
// Only Claude conversations are stored there. A Codex run continues a Codex
// run by forking its session, which lives in that machine's Codex state
// ('codex exec fork <thread>'); agy workers can't resume a conversation.

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
	if state == nil || !isClaudeRun(state) {
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

// stageResumeTranscript copies a prior run's Claude conversation transcript
// into newWorktree's project dir so 'claude --resume <sessionUUID>' launched
// there can find it. Claude scopes transcripts by working directory, so a
// resume in a different worktree would otherwise fail with 0 turns.
//
// It is best-effort and self-healing: it returns true only when the transcript
// is present at the destination and ready to resume. On any miss (the source
// transcript is gone, the project dir can't be resolved, or the copy fails) it
// returns false so the caller starts a fresh session — a launched agent must
// never crash because a conversation is missing.
func stageResumeTranscript(prevState *run.State, sessionUUID, newWorktree string) bool {
	if !validSessionUUID(sessionUUID) {
		return false
	}
	src := findResumeConversation(prevState)
	if src == "" {
		return false
	}
	dest := claudeConversationPath(newWorktree, sessionUUID)
	if dest == "" {
		return false
	}
	if src == dest {
		// Already in the right place (e.g. same worktree) — nothing to copy.
		return true
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return false
	}
	return true
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

// replayParams carries everything resolveReplay needs.
type replayParams struct {
	Kind      backend.Kind // the new run's worker backend; "" means Claude
	GitClient git.Client
	Store     run.StateStore
	RepoRoot  string // repo dir holding refs/klaus/data (clone dir or host repo)
	DataRef   string
	Worktree  string // the fresh worktree the resumed agent will run in
	// The work the run continues: a PR, or, when Change is set, a change.
	PRBranch    string
	PRNumber    string
	GHRepo      string        // owner/repo for gh calls (may be "")
	Change      *changeTarget // --change: the change, with its runs
	ForceReplay bool          // --replay: skip the size threshold and the paused gate
	ThresholdKB int           // max trajectory size in KB (0 disables the size check)
}

// replayDecision is the outcome of attempting trajectory replay.
type replayDecision struct {
	// SessionUUID is non-empty when the caller should resume it: a Claude
	// session for claude --resume, or a Codex thread for codex exec fork.
	// Empty means fall back to the fresh-agent flow.
	SessionUUID string
	// SourceRunID is the run whose conversation is continued (for logging).
	SourceRunID string
	// Reason explains the decision (always set, for user-facing logging).
	Reason string
}

// resolveReplay decides whether a run that continues a PR or a change
// resumes the conversation of the most recent run on it, and how. A PR's
// runs are those on its branch in this session, and only a budget-paused PR
// replays unless the user forces it; a change's are its runs in every
// session, and replay needs no label. Only the newest run counts: an older
// conversation never saw the work that came after it, so when the newest
// run's can't be resumed the run starts fresh.
//
// A Claude run resumes a Claude run's conversation stored on the data ref,
// restored into the new worktree's project dir; a Codex run forks a Codex
// run's local session; agy can't resume. Any other case returns an empty
// SessionUUID with a Reason naming the run, signalling the caller to fall
// back to the fresh-agent flow.
func resolveReplay(ctx context.Context, p replayParams) replayDecision {
	kind := p.Kind
	if kind == "" {
		kind = backend.Claude
	}
	if kind == backend.Agy {
		return replayDecision{Reason: "agy workers can't resume a conversation"}
	}
	candidates, reason := replayCandidates(ctx, p)
	if len(candidates) == 0 {
		return replayDecision{Reason: reason}
	}
	s := candidates[0]
	from := s.Backend
	if from == "" {
		from = string(backend.Claude)
	}
	latest := fmt.Sprintf("the latest run, %s (%s),", s.ID, from)
	if from != string(kind) {
		return replayDecision{Reason: fmt.Sprintf("%s ran on another backend; a %s worker can't resume its conversation", latest, kind)}
	}

	if kind == backend.Codex {
		thread := resolveResumeID(s, kind, p.Worktree)
		if thread == "" {
			return replayDecision{Reason: latest + " has no local Codex session to fork (it ran on a sandbox host, or recorded none)"}
		}
		return replayDecision{SessionUUID: thread, SourceRunID: s.ID, Reason: fmt.Sprintf("forking Codex session %s from run %s", thread, s.ID)}
	}

	// Best-effort: pull the data ref so a blob another machine pushed (with
	// push_data_ref) is present. Errors are expected: no remote, ref absent
	// from origin (the default), or a local ref ahead of origin's, which the
	// non-forced fetch refuses to rewind. The read below uses the local ref.
	_ = p.GitClient.FetchDataRef(ctx, p.RepoRoot, p.DataRef)

	blob, err := p.GitClient.ReadDataRefFile(ctx, p.RepoRoot, p.DataRef, "sessions/"+s.ID+".jsonl")
	if err != nil {
		return replayDecision{Reason: latest + " has no stored conversation: it was kept off the data ref as potentially sensitive, or is on another machine"}
	}
	if !p.ForceReplay && p.ThresholdKB > 0 && len(blob) > p.ThresholdKB*1024 {
		return replayDecision{Reason: fmt.Sprintf("%s has a %dKB conversation, over the %dKB threshold (use --replay to force)", latest, len(blob)/1024, p.ThresholdKB)}
	}

	uuid := extractConversationSessionID(blob)
	if !validSessionUUID(uuid) {
		return replayDecision{Reason: latest + " has a stored conversation with no usable session UUID"}
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

// replayCandidates returns the runs whose trajectory a continuing run may
// replay, newest first, or why there are none.
func replayCandidates(ctx context.Context, p replayParams) ([]*run.State, string) {
	if p.Change != nil {
		// A track record (`klaus track`) is no run: it holds no conversation.
		var runs []*run.State
		for _, s := range p.Change.Runs {
			if s.Type != "track" {
				runs = append(runs, s)
			}
		}
		if len(runs) == 0 {
			return nil, "no prior run recorded for this change"
		}
		return runs, ""
	}

	// Replay only targets budget-paused PRs unless the user forces it.
	if !p.ForceReplay {
		paused, err := draft.HasBudgetPausedLabel(ctx, budgetPauseRunner, p.Worktree, p.GHRepo, p.PRNumber)
		if err != nil {
			return nil, fmt.Sprintf("could not check budget-paused label: %v", err)
		}
		if !paused {
			return nil, "PR is not budget-paused"
		}
	}

	// Find candidate runs on this branch, newest first (List() is sorted by
	// CreatedAt descending).
	states, err := p.Store.List()
	if err != nil {
		return nil, fmt.Sprintf("listing runs: %v", err)
	}
	var candidates []*run.State
	for _, s := range states {
		if s.Branch == p.PRBranch {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil, "no prior run recorded for this branch"
	}
	return candidates, ""
}
