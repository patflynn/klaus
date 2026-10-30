package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/patflynn/klaus/internal/tmux"
)

// State represents the persistent state of a single agent run.
type State struct {
	Backend          string   `json:"backend,omitempty"`       // Empty in old state means Claude.
	AgentBackend     string   `json:"agent_backend,omitempty"` // Session worker default.
	BackendSessionID *string  `json:"backend_session_id,omitempty"`
	ID               string   `json:"id"`
	Prompt           string   `json:"prompt"`
	Issue            *string  `json:"issue"`
	PR               *string  `json:"pr,omitempty"`
	Branch           string   `json:"branch"` // the run's branch: launch's, until finalize finds the run pushed another, as an agent that makes a new change does
	Worktree         string   `json:"worktree"`
	TmuxPane         *string  `json:"tmux_pane"`
	Budget           *string  `json:"budget"`
	LogFile          *string  `json:"log_file"`
	CreatedAt        string   `json:"created_at"`
	CostUSD          *float64 `json:"cost_usd"`
	DurationMS       *int64   `json:"duration_ms"`
	PRURL            *string  `json:"pr_url"`
	Type             string   `json:"type,omitempty"`
	TargetRepo       *string  `json:"target_repo,omitempty"`
	CloneDir         *string  `json:"clone_dir,omitempty"`
	Host             *string  `json:"host,omitempty"`
	MergedAt         *string  `json:"merged_at,omitempty"`
	DashboardPane    *string  `json:"dashboard_pane,omitempty"`
	CoordinatorPane  *string  `json:"coordinator_pane,omitempty"` // tmux pane running the coordinator/claude session
	Approved         *bool    `json:"approved,omitempty"`
	ApprovedAt       *string  `json:"approved_at,omitempty"`
	SessionName      *string  `json:"session_name,omitempty"`      // claude -n name, same as run ID
	OriginalRunID    *string  `json:"original_run_id,omitempty"`   // run ID this was forked from
	ClaudeSessionID  *string  `json:"claude_session_id,omitempty"` // Claude conversation UUID for --resume
	RepoRoot         *string  `json:"repo_root,omitempty"`         // absolute path to base repo for worktree recreation
	FailureReason    *string  `json:"failure_reason,omitempty"`    // set when the agent crashed (e.g. error_during_execution) or, without a PR, left background work running; suppresses success events and blocks resume chaining
	NeedsAttention   *string  `json:"needs_attention,omitempty"`   // set when finalize salvaged unfinished work (no PR, or crash); names the preserved branch
	PushedBranch     *string  `json:"pushed_branch,omitempty"`     // set when finalize found a PR-less run's branch on origin (e.g. direct-push repos)
	PushedSHA        *string  `json:"pushed_sha,omitempty"`        // origin's tip of PushedBranch at finalize
	PushedSubject    string   `json:"pushed_subject,omitempty"`    // the subject of the commit at PushedSHA, when finalize had it
	Model            *string  `json:"model,omitempty"`             // claude --model the agent ran with (unset → claude's default)
	ResolvedModel    string   `json:"resolved_model,omitempty"`    // the model the worker reported using (claude's stream-json init event), set at finalize; preferred over Model
	Effort           *string  `json:"effort,omitempty"`            // claude --effort the agent ran with (unset → claude's default)
	ChangeID         string   `json:"change_id,omitempty"`         // the change the run's commits belong to (their Change-Id trailer): launch's, until finalize finds the run pushed another (see Branch); empty in state that predates change IDs
	Valley           *Valley  `json:"valley,omitempty"`            // set when finalize found the pushed branch's repo delivers to a valley
	Unready          *Unready `json:"unready,omitempty"`           // set when launch returned the change this run continues to draft on a valley, or tried to
	ChangeRepo       string   `json:"change_repo,omitempty"`       // the repo of the change this run continues, as the run it continues named it (integration.RepoName); set at launch when the change ID is inherited from a run
	Delivery         string   `json:"delivery,omitempty"`          // how launch told the agent to deliver: DeliveryPR or DeliveryPush; empty in state that predates it
	Knot             *Knot    `json:"knot,omitempty"`              // set when the run was launched to work a valley knot (--issue <knot>)
	DispatchReason   string   `json:"dispatch_reason,omitempty"`   // why the run was launched (event.Dispatch*, or launch --reason); empty in state that predates it
	RepoKey          string   `json:"repo_key,omitempty"`          // the repo as launch named it for events: owner/repo, valley:<name>, or the repo's short name
	HeadSHA          string   `json:"head_sha,omitempty"`          // the worktree's HEAD when the agent started, then when it finished; for a tracked valley change, its active version's head when klaus track added it
	// TookOver names the runs of this run's change, each needing attention,
	// whose pushed work this run took on: recorded at launch when it
	// continued a branch (--pr, --change, or --resume-from on its run's
	// branch) holding their pushed heads, and at finalize when the head it
	// left on origin contains theirs. Their attention is then this run's.
	TookOver []string `json:"took_over,omitempty"`
	// SharedGit is launch's snapshot of the files the run's clone shares
	// across its worktrees, and what finalize found changed.
	SharedGit *SharedGit `json:"shared_git,omitempty"`
}

// SharedGit records the files of a run's clone that every worktree of the
// clone shares and that decide what git does there (git.SharedSnapshot).
type SharedGit struct {
	Dir     string            `json:"dir"`               // the clone's common git dir
	Files   map[string]string `json:"files"`             // launch's digest of each, by path relative to Dir
	Changed []string          `json:"changed,omitempty"` // the files finalize found changed since launch
}

// SharedGitChanged reports whether finalize found the run's clone's shared
// git files changed. Such a run needs attention whatever became of its work,
// and no other run takes that on.
func (s *State) SharedGitChanged() bool {
	return s.SharedGit != nil && len(s.SharedGit.Changed) > 0
}

// How a run delivers its work, as launch decides it from the run's repo.
const (
	DeliveryPR   = "pr"   // the agent opens a GitHub PR, or continues one (--pr)
	DeliveryPush = "push" // the agent pushes its branch; the repo takes no PRs
)

// Unready is what launch's `valley unready` did to the change a follow-up
// run continues, before its agent started.
type Unready struct {
	// ChangeID is the change launch returned to draft. Empty in state that
	// predates it, where it is the run's change; finalize fills it in when
	// the run pushed another change.
	ChangeID string `json:"change_id,omitempty"`
	Repo     string `json:"repo,omitempty"`   // the valley's name for the repo, when the change became a draft
	Handle   string `json:"handle,omitempty"` // the change's handle, when it became a draft
	Note     string `json:"note,omitempty"`   // what the valley said when the change wasn't ready
	Error    string `json:"error,omitempty"`  // why valley unready failed
}

// UnreadyHandle is the handle launch's `valley unready` reported for the
// run's change, or "" when it reported none, or reported it for a change the
// run then left for another.
func (s *State) UnreadyHandle() string {
	if s.Unready == nil || s.Unready.ChangeID != "" && s.Unready.ChangeID != s.ChangeID {
		return ""
	}
	return s.Unready.Handle
}

// Valley is what finalize's `valley ready` recorded for a pushed run: the
// version of its change that the pushed head became, or why it couldn't
// mark it ready. klaus ready updates it when it marks the version ready.
type Valley struct {
	Repo    string `json:"repo,omitempty"`    // the valley's name for the repo
	Handle  string `json:"handle,omitempty"`  // the change's handle when it was marked ready
	Version int    `json:"version,omitempty"` // the version the pushed head became
	Title   string `json:"title,omitempty"`   // the pushed head's subject
	Error   string `json:"error,omitempty"`   // why valley ready (or, for a draft, valley unready) failed; empty when it succeeded
	// Draft is set when finalize left the version a draft awaiting review
	// (ready_at_finalize off), until klaus ready marks it ready.
	Draft bool `json:"draft,omitempty"`
}

// Knot is the knot in a valley project's plan graph that a run was launched
// to work (--issue <knot>), and what klaus did to it: launch claims it,
// finalize links the run's change to it, or comments when the run failed or
// paused, and clears the claim; once the dashboard sees the change landed it
// comments with the landed commit and may close the knot.
type Knot struct {
	ID       string `json:"id"`                 // e.g. oc-897917f
	Project  string `json:"project"`            // the valley project whose plan graph holds it
	Type     string `json:"type"`               // outcome or bug
	Title    string `json:"title,omitempty"`    // its title at launch
	Dir      string `json:"dir,omitempty"`      // the checkout launch ran valley in, where klaus asks about the knot after the run
	Claimed  bool   `json:"claimed,omitempty"`  // launch claimed it for the run
	Released bool   `json:"released,omitempty"` // finalize cleared the run's claim
	Linked   string `json:"linked,omitempty"`   // the change finalize linked to it: valley:<repo>/<change ID>
	Note     string `json:"note,omitempty"`     // why finalize commented instead of linking a change
	Landed   string `json:"landed,omitempty"`   // the commit the change landed as, once klaus commented with it
	Closed   string `json:"closed,omitempty"`   // the status klaus set after the landing: done or closed
	Error    string `json:"error,omitempty"`    // the last knot write that failed
}

// TmuxDeps abstracts tmux pane operations so callers can inject test doubles.
type TmuxDeps struct {
	PaneExists func(string) bool
	PaneIsIdle func(string) bool
	PaneIsDead func(string) bool
}

// DefaultTmuxDeps returns TmuxDeps wired to the real tmux package.
// Uses a background context for the underlying tmux calls.
func DefaultTmuxDeps() TmuxDeps {
	return TmuxDeps{
		PaneExists: func(paneID string) bool { return tmux.PaneExists(context.Background(), paneID) },
		PaneIsIdle: func(paneID string) bool { return tmux.PaneIsIdle(context.Background(), paneID) },
		PaneIsDead: func(paneID string) bool { return tmux.PaneIsDead(context.Background(), paneID) },
	}
}

// TmuxDepsFor returns TmuxDeps that ask tc, under ctx.
func TmuxDepsFor(ctx context.Context, tc tmux.Client) TmuxDeps {
	return TmuxDeps{
		PaneExists: func(paneID string) bool { return tc.PaneExists(ctx, paneID) },
		PaneIsIdle: func(paneID string) bool { return tc.PaneIsIdle(ctx, paneID) },
		PaneIsDead: func(paneID string) bool { return tc.PaneIsDead(ctx, paneID) },
	}
}

// IsAgentRunning checks if the agent's tmux pane is still active and
// executing its command pipeline.
func (s *State) IsAgentRunning() bool {
	return s.IsAgentRunningWith(DefaultTmuxDeps())
}

// IsAgentRunningWith is like IsAgentRunning but uses the provided TmuxDeps.
func (s *State) IsAgentRunningWith(td TmuxDeps) bool {
	if s.TmuxPane == nil || !td.PaneExists(*s.TmuxPane) {
		return false
	}

	// Finalized runs (cost/duration set) are running only if their pane
	// is not idle (e.g. still showing output before the user closes it).
	if s.CostUSD != nil || s.DurationMS != nil {
		return !td.PaneIsIdle(*s.TmuxPane)
	}

	// Active (unfinalized) runs are running unless the pane is explicitly dead.
	return !td.PaneIsDead(*s.TmuxPane)
}

// StaleGracePeriod is how long after creation before a run can be considered stale.
// This allows time for the agent pipeline to start up.
var StaleGracePeriod = 2 * time.Minute

// IsStale returns true if the run appears to have been orphaned — its pipeline
// never ran _finalize. A run is stale when it has no finalization data (CostUSD
// and DurationMS are both nil), its tmux pane no longer exists, and enough time
// has passed since creation to rule out normal startup delays.
func (s *State) IsStale() bool {
	return s.IsStaleWith(DefaultTmuxDeps())
}

// IsStaleWith is like IsStale but uses the provided TmuxDeps.
func (s *State) IsStaleWith(td TmuxDeps) bool {
	// Already finalized — not stale.
	if s.CostUSD != nil || s.DurationMS != nil {
		return false
	}

	// Sessions are not agent runs.
	if s.Type == "session" {
		return false
	}

	// If the pane reference is nil or the pane still exists, not stale.
	if s.TmuxPane == nil {
		// No pane reference at all — could be stale if old enough, but we
		// require the pane to have existed and then disappeared.
		return false
	}
	if td.PaneExists(*s.TmuxPane) {
		return false
	}

	// Grace period: don't mark very recent runs as stale.
	created, err := time.Parse(time.RFC3339, s.CreatedAt)
	if err != nil {
		return false
	}
	return time.Since(created) > StaleGracePeriod
}

// GenChangeID generates a change ID in jj's form: 16 random bytes in jj's
// "reverse hex", where the hex digits 0-9a-f are written z-k. The result is 32
// characters drawn from k to z, comparable as-is with a jj change-id header.
func GenChangeID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	id := make([]byte, 0, 2*len(b))
	for _, c := range b {
		id = append(id, 'z'-c>>4, 'z'-c&0x0f)
	}
	return string(id), nil
}

// ValidChangeID reports whether id has the form GenChangeID produces.
func ValidChangeID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range []byte(id) {
		if c < 'k' || c > 'z' {
			return false
		}
	}
	return true
}

// GenID generates a run ID in the format YYYYMMDD-HHMM-XXXXXXXX where the suffix is 8 hex chars.
func GenID() (string, error) {
	ts := time.Now().Format("20060102-1504")
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return fmt.Sprintf("%s-%s", ts, hex.EncodeToString(b)), nil
}
