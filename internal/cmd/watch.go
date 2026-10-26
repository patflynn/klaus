package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

// defaultWatchFilter is the set of event types emitted as notifications when
// 'klaus watch' is run with no --filter flags. It intentionally includes some
// types that aren't emitted yet (reserved entries) so the filter remains
// forward-compatible as the pipeline grows.
var defaultWatchFilter = []string{
	event.ConsultStarted,      // live
	event.ConsultCompleted,    // live
	event.ReviewStarted,       // live
	event.ReviewCompleted,     // live
	event.AgentPRCreated,      // live
	event.AgentBranchPushed,   // live
	event.AgentChangeReady,    // live
	event.AgentChangeDraft,    // live
	event.AgentNeedsAttention, // live
	"agent:error",             // reserved
	event.PRApproved,          // live
	event.PRMerged,            // live
	event.PipelineStalled,     // live
	"ci:failed",               // reserved (closest live equivalent: agent:ci-failed)
	"ci:passed",               // reserved (closest live equivalent: agent:ci-passed)
	"pr:comment",              // reserved
}

// knownEventTypes maps event types to a one-line description and whether the
// type is currently emitted somewhere in klaus ("live") or reserved for future
// use. --list-types renders this table.
var knownEventTypes = []eventTypeInfo{
	{event.ConsultStarted, "live", "A read-only model consultation started"},
	{event.ConsultCompleted, "live", "A read-only model consultation finished (success, failure, timeout or interrupt)"},
	{event.ReviewStarted, "live", "A cross-model PR review (klaus review) started"},
	{event.ReviewCompleted, "live", "A cross-model PR review finished (success, failure, timeout or interrupt)"},
	{event.AgentStarted, "live", "An agent run started"},
	{event.AgentCompleted, "live", "An agent run finished (success or failure)"},
	{event.AgentPRCreated, "live", "An agent published a PR"},
	{event.AgentBranchPushed, "live", "An agent finished with its branch on origin and no PR (awaiting integration)"},
	{event.AgentChangeReady, "live", "Finalize, or klaus ready, marked a pushed branch ready on its valley as a version of the run's change (awaiting approval)"},
	{event.AgentChangeDraft, "live", "Finalize recorded a pushed branch on its valley as a version of the run's change but left it a draft (awaiting review; ready_at_finalize off)"},
	{event.AgentCIPassed, "live", "CI passed on an agent-owned PR"},
	{event.AgentCIFailed, "live", "CI failed on an agent-owned PR"},
	{event.AgentNeedsAttention, "live", "An agent stopped and needs operator input"},
	{event.PRAwaitingApproval, "live", "A PR is ready for human approval"},
	{event.PRApproved, "live", "A PR was approved"},
	{event.PRMerged, "live", "A PR merged"},
	{event.PRApprovalChanged, "live", "Klaus-internal approval state for a PR changed (e.g. via klaus approve)"},
	{event.PipelineStalled, "live", "The pipeline gave up on a PR after exhausting its retry budget"},
	{event.PhaseStarted, "live", "A run entered a phase (agent, finalize); not in the default filter"},
	{event.PhaseCompleted, "live", "A run finished a phase (setup, agent, finalize), with its duration; not in the default filter"},
	{event.CheckStarted, "live", "An agent started a check klaus can see (_pre-review, or a build, test or lint command); not in the default filter"},
	{event.CheckCompleted, "live", "An agent's check finished, with its duration; not in the default filter"},
	{"agent:error", "reserved", "Reserved for unrecoverable agent failures (not currently emitted; use agent:needs-attention)"},
	{"ci:failed", "reserved", "Reserved short name (currently emitted as agent:ci-failed)"},
	{"ci:passed", "reserved", "Reserved short name (currently emitted as agent:ci-passed)"},
	{"pr:comment", "reserved", "Reserved for trusted-reviewer comment notifications (not currently emitted)"},
}

type eventTypeInfo struct {
	Type   string
	Status string // "live" or "reserved"
	Desc   string
}

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Stream session pipeline events as a real-time channel",
	Long: `Stream structured pipeline events from the current klaus session as a
line-buffered text channel. Designed for Claude Code's Monitor tool — each
matching event becomes one notification.

Events are read from ~/.klaus/sessions/$KLAUS_SESSION_ID/events.jsonl,
followed via fsnotify, and emitted to stdout one line at a time. The default
filter selects events the coordinator typically wants to react to:

  agent:pr-created, agent:branch-pushed, agent:needs-attention,
  agent:error, pr:approved, pr:merged, pipeline:stalled,
  consult:started, consult:completed, review:started, review:completed,
  ci:failed, ci:passed, pr:comment

Some of those types are reserved (not currently emitted) but kept in the
default filter so this command stays forward-compatible. Run 'klaus watch
--list-types' to see which types are live vs reserved.

Passing --filter REPLACES the default. --filter-out is always subtracted.
Both flags are repeatable and use OR semantics within each flag. --run keeps
only the events of the given runs (repeatable; a pipeline event's run is its
PR's number).

klaus session creates the events file when it creates or resumes a session.
In a session that has none yet, watch waits for it as long as the session's
directory exists, then reads it from the start. An unknown session ID is an
error.

--once is for a coordinator whose background tasks hand it their output only
when they exit (agy, for one): it waits until at least one matching event has
arrived, prints every matching event that has, and exits 0. Its last line is
the command that waits for the events after those, with the stream's position
in --after, e.g.

  next: klaus watch --once --after 18342

(with --json, a JSON object: {"after":18342,"next":"klaus watch ..."}).
Starting each watch from the last one's next: line delivers every event once:
--after is a byte offset into events.jsonl, so events that land between two
watches are past it, and running the same command again prints the same
events. Without --after, --once starts at the end of the file (at its start
with --since-start). Interrupted before any event, it prints just the next:
line. An --after past the end of the file or inside a line is an error, and
so is an events file replaced or removed while --once waits on it.`,
	Example: `  # Default filter, follow new events from now:
  klaus watch

  # Only react when a PR merges:
  klaus watch --filter pr:merged

  # Default filter minus pr:approved (e.g. don't notify on self-approvals):
  klaus watch --filter-out pr:approved

  # Replay everything in the file, then keep following:
  klaus watch --since-start

  # Raw JSONL passthrough for downstream tools:
  klaus watch --json

  # Wait for the next events, print them and exit; then run the next: line:
  klaus watch --once
  klaus watch --once --after 18342

  # Wait for one run's next event:
  klaus watch --once --run 20261026-1701-445ec227`,
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().StringSlice("filter", nil, "Event type to include (repeatable, OR semantics). Replaces the default filter when set.")
	watchCmd.Flags().StringSlice("filter-out", nil, "Event type to exclude (repeatable). Applied after --filter.")
	watchCmd.Flags().StringSlice("run", nil, "Run ID whose events to include (repeatable; a PR number for pipeline events).")
	watchCmd.Flags().Bool("since-start", false, "Emit events already in the file before tailing new ones.")
	watchCmd.Flags().Bool("once", false, "Wait for at least one matching event, print every one that has arrived and the command that waits for the next, then exit.")
	watchCmd.Flags().Int64("after", 0, "With --once: the byte offset in events.jsonl to read from, as printed on the last watch's next: line.")
	watchCmd.Flags().Bool("json", false, "Emit raw JSONL lines instead of human-formatted summaries.")
	watchCmd.Flags().Bool("list-types", false, "Print known event types (live and reserved) and exit.")
	watchCmd.Flags().String("session-id", "", "Override $KLAUS_SESSION_ID (mainly for testing).")
	rootCmd.AddCommand(watchCmd)
}

func runWatch(cmd *cobra.Command, args []string) error {
	listTypes, _ := cmd.Flags().GetBool("list-types")
	if listTypes {
		printKnownEventTypes(cmd.OutOrStdout())
		return nil
	}

	filterIn, _ := cmd.Flags().GetStringSlice("filter")
	filterOut, _ := cmd.Flags().GetStringSlice("filter-out")
	runs, _ := cmd.Flags().GetStringSlice("run")
	sinceStart, _ := cmd.Flags().GetBool("since-start")
	once, _ := cmd.Flags().GetBool("once")
	after, _ := cmd.Flags().GetInt64("after")
	hasAfter := cmd.Flags().Changed("after")
	asJSON, _ := cmd.Flags().GetBool("json")
	sessionOverride, _ := cmd.Flags().GetString("session-id")

	if hasAfter && !once {
		return fmt.Errorf("--after needs --once")
	}
	if hasAfter && sinceStart {
		return fmt.Errorf("--after and --since-start are mutually exclusive")
	}
	if after < 0 {
		return fmt.Errorf("--after must not be negative")
	}

	sessionID := sessionOverride
	if sessionID == "" {
		sessionID = os.Getenv(sessionIDEnv)
	}
	if sessionID == "" {
		return fmt.Errorf("klaus watch must be run inside a klaus session (KLAUS_SESSION_ID is not set; use --session-id to override)")
	}

	sessionsDir, err := run.SessionsDir()
	if err != nil {
		return err
	}
	cmd.SilenceUsage = true // flags are valid; later errors are not usage errors

	sessionDir := filepath.Join(sessionsDir, sessionID)
	if _, err := os.Stat(sessionDir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unknown session %s: %s does not exist", sessionID, sessionDir)
	} else if err != nil {
		return fmt.Errorf("stat session dir: %w", err)
	}
	eventsPath := event.NewLog(sessionDir).Path()

	flt := buildFilter(filterIn, filterOut, runs)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if once {
		start := int64(-1) // the end of the file
		if sinceStart {
			start = 0
		}
		if hasAfter {
			start = after
		}
		next := func(cursor int64) string {
			return nextWatchCommand(sessionOverride, filterIn, filterOut, runs, asJSON, cursor)
		}
		return watchOnce(ctx, eventsPath, flt, start, asJSON, next, cmd.OutOrStdout())
	}
	return streamEvents(ctx, eventsPath, flt, sinceStart, asJSON, cmd.OutOrStdout())
}

// watchFilter decides whether a given event should be emitted.
type watchFilter struct {
	include map[string]struct{} // empty means "match anything"
	exclude map[string]struct{}
	runs    map[string]struct{} // empty means every run
}

func buildFilter(includeFlags, excludeFlags, runFlags []string) watchFilter {
	include := includeFlags
	if len(include) == 0 {
		include = defaultWatchFilter
	}
	inc := make(map[string]struct{}, len(include))
	for _, t := range include {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		inc[t] = struct{}{}
	}
	exc := make(map[string]struct{}, len(excludeFlags))
	for _, t := range excludeFlags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		exc[t] = struct{}{}
	}
	runs := make(map[string]struct{}, len(runFlags))
	for _, r := range runFlags {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		runs[r] = struct{}{}
	}
	return watchFilter{include: inc, exclude: exc, runs: runs}
}

// accepts reports whether evt passes both the type and the run filters.
func (f watchFilter) accepts(evt event.Event) bool {
	if len(f.runs) > 0 {
		if _, ok := f.runs[evt.RunID]; !ok {
			return false
		}
	}
	return f.matches(evt.Type)
}

func (f watchFilter) matches(eventType string) bool {
	if _, blocked := f.exclude[eventType]; blocked {
		return false
	}
	if len(f.include) == 0 {
		return true
	}
	_, ok := f.include[eventType]
	return ok
}

// streamEvents waits for the events file to exist, then follows it with
// fsnotify until ctx is cancelled. New lines that pass the filter are written
// to out as either formatted summaries or raw JSON. A file it had to wait for
// is new, so it reads that one from the start.
func streamEvents(ctx context.Context, path string, flt watchFilter, sinceStart, asJSON bool, out io.Writer) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		sinceStart = true
	}
	if err := waitForFile(ctx, path); err != nil {
		return err
	}
	// If ctx was cancelled during the wait, exit cleanly without trying to
	// open the file (it may never have appeared).
	if ctx.Err() != nil {
		return nil
	}

	dir := filepath.Dir(path)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating file watcher: %w", err)
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("watching %s: %w", dir, err)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening events file: %w", err)
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	currentInode := statInode(path)

	if !sinceStart {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("seeking to end of events file: %w", err)
		}
	}

	// Buffer for accumulating partial lines across reads.
	var pending []byte

	drain := func() error {
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				for {
					idx := bytes.IndexByte(pending, '\n')
					if idx < 0 {
						break
					}
					line := pending[:idx]
					pending = pending[idx+1:]
					if len(line) == 0 {
						continue
					}
					if _, err := emitLine(line, flt, asJSON, out); err != nil {
						return err
					}
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading events file: %w", err)
			}
		}
	}

	if err := drain(); err != nil {
		return err
	}

	reopen := func() error {
		if f != nil {
			f.Close()
		}
		nf, err := os.Open(path)
		if err != nil {
			return err
		}
		f = nf
		pending = pending[:0]
		currentInode = statInode(path)
		return nil
	}

	// Even with fsnotify, a periodic inode/size check catches edge cases
	// (e.g. file replaced after a notification was missed).
	const poll = 500 * time.Millisecond
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(ev.Name) != filepath.Clean(path) {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				if _, err := os.Stat(path); err == nil {
					if err := reopen(); err != nil {
						return fmt.Errorf("reopening events file after rotation: %w", err)
					}
					if err := drain(); err != nil {
						return err
					}
				}
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Chmod) != 0 {
				if err := drain(); err != nil {
					return err
				}
			}
		case werr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			// Watcher errors are non-fatal — log to stderr and keep going.
			fmt.Fprintf(os.Stderr, "klaus watch: watcher error: %v\n", werr)
		case <-ticker.C:
			// Defensive: catch missed write notifications and inode swaps.
			newInode := statInode(path)
			if newInode != 0 && newInode != currentInode {
				if err := reopen(); err != nil {
					return fmt.Errorf("reopening events file after inode change: %w", err)
				}
			}
			if err := drain(); err != nil {
				return err
			}
		}
	}
}

// emitLine writes line to out if it is an event that passes flt, and
// reports whether it did.
func emitLine(line []byte, flt watchFilter, asJSON bool, out io.Writer) (bool, error) {
	var evt event.Event
	if err := json.Unmarshal(line, &evt); err != nil {
		// Skip malformed lines silently — matches event.Log.Read behavior.
		return false, nil
	}
	if !flt.accepts(evt) {
		return false, nil
	}
	return true, writeEvent(line, evt, asJSON, out)
}

func writeEvent(line []byte, evt event.Event, asJSON bool, out io.Writer) error {
	if asJSON {
		if _, err := out.Write(line); err != nil {
			return err
		}
		_, err := out.Write([]byte{'\n'})
		return err
	}
	if _, err := fmt.Fprintln(out, formatEvent(evt)); err != nil {
		return err
	}
	return nil
}

// formatEvent renders a single event as a one-line summary. Output shape:
//
//	HH:MM:SS  <run_id>  <type>  <one-line summary>
func formatEvent(evt event.Event) string {
	ts := evt.Timestamp
	if parsed, err := time.Parse(time.RFC3339, evt.Timestamp); err == nil {
		ts = parsed.Local().Format("15:04:05")
	}
	runID := evt.RunID
	if runID == "" {
		runID = "-"
	}
	return fmt.Sprintf("%s  %s  %-22s  %s", ts, runID, evt.Type, eventSummary(evt))
}

func eventSummary(evt event.Event) string {
	d := evt.Data
	get := func(k string) string {
		if d == nil {
			return ""
		}
		v, ok := d[k]
		if !ok || v == nil {
			return ""
		}
		switch x := v.(type) {
		case string:
			return x
		case float64:
			// JSON numbers come back as float64 — trim trailing .0 for integer-ish values.
			if x == float64(int64(x)) {
				return fmt.Sprintf("%d", int64(x))
			}
			return fmt.Sprintf("%g", x)
		default:
			return fmt.Sprintf("%v", v)
		}
	}

	prNum := get("pr_number")
	prURL := get("pr_url")

	switch evt.Type {
	case event.ConsultStarted, event.ConsultCompleted:
		parts := []string{get("backend")}
		if m := get("model"); m != "" {
			parts[0] += "/" + m
		}
		for _, k := range []string{"effort", "role"} {
			if v := get(k); v != "" {
				parts = append(parts, v)
			}
		}
		for _, k := range []string{"thread", "repo"} {
			if v := get(k); v != "" {
				parts = append(parts, k+"="+v)
			}
		}
		if get("panel") == "true" {
			parts = append(parts, "panel")
		}
		head := strings.Join(parts, " ")
		if evt.Type == event.ConsultStarted {
			return truncateLine(head+": "+get("prompt"), 160)
		}
		status := "ok"
		if get("success") != "true" {
			status = "failed"
			if e := get("error"); e != "" {
				status += ": " + e
			}
		}
		return truncateLine(fmt.Sprintf("%s (%sms) %s", head, get("duration_ms"), status), 160)
	case event.ReviewStarted, event.ReviewCompleted:
		head := get("backend")
		if m := get("model"); m != "" {
			head += "/" + m
		}
		head += " " + get("repo") + "#" + prNum
		if get("post") == "true" {
			head += " post"
		}
		if evt.Type == event.ReviewStarted {
			return head
		}
		status := "ok: " + get("findings") + " findings"
		if get("success") != "true" {
			status = "failed"
			if e := get("error"); e != "" {
				status += ": " + e
			}
		}
		return truncateLine(fmt.Sprintf("%s (%sms) %s", head, get("duration_ms"), status), 160)
	case event.AgentStarted:
		prompt := get("prompt")
		if prompt != "" {
			return truncateLine(prompt, 80)
		}
		return "agent started"
	case event.AgentCompleted:
		cost := get("cost_usd")
		if cost != "" {
			return fmt.Sprintf("agent completed (cost $%s)", cost)
		}
		return "agent completed"
	case event.AgentPRCreated:
		if prNum != "" && prURL != "" {
			return fmt.Sprintf("PR #%s: %s", prNum, prURL)
		}
		if prURL != "" {
			return prURL
		}
		return "PR created"
	case event.AgentBranchPushed:
		msg := "branch " + get("branch")
		if sha := get("sha"); sha != "" {
			msg += "@" + shortSHA(sha)
		}
		msg += " pushed, awaiting integration"
		if repo := get("repo"); repo != "" {
			msg += " (" + repo + ")"
		}
		return msg
	case event.AgentChangeReady, event.AgentChangeDraft:
		msg := strings.TrimSpace(get("repo") + " " + get("handle"))
		if v := get("version"); v != "" {
			msg += " v" + v
		}
		if evt.Type == event.AgentChangeDraft {
			msg += " draft, awaiting review"
		} else {
			msg += " ready, awaiting approval"
		}
		if title := get("title"); title != "" {
			msg += ": " + title
		}
		return truncateLine(msg, 160)
	case event.AgentCIPassed:
		if prNum != "" {
			return fmt.Sprintf("CI passed on PR #%s", prNum)
		}
		return "CI passed"
	case event.AgentCIFailed:
		if prNum != "" {
			return fmt.Sprintf("CI failed on PR #%s", prNum)
		}
		return "CI failed"
	case event.AgentNeedsAttention:
		msg := "needs attention"
		if reason := get("reason"); reason != "" {
			msg += " — " + reason
		}
		if branch := get("branch"); branch != "" {
			msg += fmt.Sprintf(" (branch %s)", branch)
		}
		return msg
	case event.PRAwaitingApproval:
		if prNum != "" {
			return fmt.Sprintf("PR #%s awaiting approval", prNum)
		}
		return "PR awaiting approval"
	case event.PRApproved:
		if prNum != "" {
			return fmt.Sprintf("PR #%s approved", prNum)
		}
		return "PR approved"
	case event.PRMerged:
		if prNum != "" {
			return fmt.Sprintf("PR #%s merged", prNum)
		}
		return "PR merged"
	case event.PhaseStarted:
		return get("phase")
	case event.PhaseCompleted:
		return fmt.Sprintf("%s (%sms)", get("phase"), get("duration_ms"))
	case event.CheckStarted:
		return truncateLine(get("kind")+": "+get("command"), 160)
	case event.CheckCompleted:
		status := "ok"
		if get("success") == "false" {
			status = "failed"
		}
		return truncateLine(fmt.Sprintf("%s (%sms) %s: %s", get("kind"), get("duration_ms"), status, get("command")), 160)
	case event.PipelineStalled:
		reason := get("reason")
		switch {
		case prNum != "" && reason != "":
			return fmt.Sprintf("PR #%s stalled — %s", prNum, reason)
		case prNum != "":
			return fmt.Sprintf("PR #%s stalled", prNum)
		case reason != "":
			return fmt.Sprintf("pipeline stalled — %s", reason)
		}
		return "pipeline stalled"
	}

	// Generic fallback: list known fields in a stable order.
	if d == nil {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := get(k)
		if v == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, " ")
}

func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// waitForFile blocks until path exists, polling every 500ms, for as long as
// the session directory holding it does: a session created before klaus made
// the file at session start has none until its first event. It returns nil
// once the file exists or ctx is cancelled, and an error if the session
// directory is removed.
func waitForFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat events file: %w", err)
		}
		if err := checkSessionDir(path); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// checkSessionDir returns an error if the session directory holding the
// events file at path no longer exists.
func checkSessionDir(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session directory %s was removed", dir)
	} else if err != nil {
		return fmt.Errorf("stat session dir: %w", err)
	}
	return nil
}

// watchOnce is klaus watch --once. It reads the events file from byte offset
// start (the end of the file if start is negative) and waits until at least
// one complete line there is an event that passes flt. It writes every such
// event in the file at that moment to out, then the next: line, built by
// next from the offset just past the last complete line it read. The offset
// only ever advances past whole lines, all of which it has written or
// filtered out, so starting the next watch from it neither repeats nor skips
// an event. Cancelling ctx ends the wait early with just the next: line.
func watchOnce(ctx context.Context, path string, flt watchFilter, start int64, asJSON bool, next func(int64) string, out io.Writer) error {
	cursor := int64(0)
	if start > 0 {
		cursor = start
	}
	trailer := func() error {
		cmdline := next(cursor)
		if asJSON {
			data, err := json.Marshal(struct {
				After int64  `json:"after"`
				Next  string `json:"next"`
			}{cursor, cmdline})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, string(data))
			return err
		}
		_, err := fmt.Fprintln(out, "next: "+cmdline)
		return err
	}

	// Watch the directory before reading, so a write that lands between a
	// read and the wait still wakes the loop.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating file watcher: %w", err)
	}
	defer watcher.Close()
	if err := watcher.Add(filepath.Dir(path)); err != nil {
		return fmt.Errorf("watching %s: %w", filepath.Dir(path), err)
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// No file yet, so the end of the file is its start: every event
		// that lands in it is new.
		if start < 0 {
			start = 0
		}
		if err := waitForFile(ctx, path); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return trailer()
		}
		f, err = os.Open(path)
	}
	if err != nil {
		return fmt.Errorf("opening events file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat events file: %w", err)
	}
	var inode uint64
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = sys.Ino
	}
	switch {
	case start < 0:
		cursor = info.Size()
	case start > info.Size():
		return fmt.Errorf("--after %d is past the end of %s (%d bytes): pass the offset from the last watch's next: line", start, path, info.Size())
	case start > 0:
		// An offset from a next: line is always just past a newline.
		b := make([]byte, 1)
		if _, err := f.ReadAt(b, start-1); err != nil {
			return fmt.Errorf("reading events file: %w", err)
		}
		if b[0] != '\n' {
			return fmt.Errorf("--after %d is not at the start of a line in %s: pass the offset from the last watch's next: line", start, path)
		}
	}
	if _, err := f.Seek(cursor, io.SeekStart); err != nil {
		return fmt.Errorf("seeking events file: %w", err)
	}

	// read consumes every complete line from the cursor on, writing those
	// that pass flt, and reports how many it wrote. A partial last line,
	// still being written, stays in pending, short of the cursor.
	var pending []byte
	buf := make([]byte, 32*1024)
	read := func() (int, error) {
		written := 0
		for {
			n, err := f.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				for {
					idx := bytes.IndexByte(pending, '\n')
					if idx < 0 {
						break
					}
					line := pending[:idx]
					pending = pending[idx+1:]
					ok, werr := emitLine(line, flt, asJSON, out)
					if werr != nil {
						return written, werr
					}
					cursor += int64(idx) + 1
					if ok {
						written++
					}
				}
			}
			if err == io.EOF {
				return written, nil
			}
			if err != nil {
				return written, fmt.Errorf("reading events file: %w", err)
			}
		}
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		written, err := read()
		if err != nil {
			return err
		}
		if written > 0 {
			return trailer()
		}
		select {
		case <-ctx.Done():
			return trailer()
		case _, ok := <-watcher.Events:
			if !ok {
				return trailer()
			}
		case werr, ok := <-watcher.Errors:
			if !ok {
				return trailer()
			}
			fmt.Fprintf(os.Stderr, "klaus watch: watcher error: %v\n", werr)
		case <-ticker.C:
			// Catches a missed notification, and a session or events file
			// removed under us. Offsets into a file that was replaced say
			// nothing about the new one, so that ends the watch too.
			if err := checkSessionDir(path); err != nil {
				return err
			}
			if inode != 0 && statInode(path) != inode {
				return fmt.Errorf("%s was replaced or removed while this watch read it: its offsets don't apply to the new file", path)
			}
		}
	}
}

// nextWatchCommand is the klaus watch --once command line that reads on from
// cursor with the given flags.
func nextWatchCommand(sessionID string, filterIn, filterOut, runs []string, asJSON bool, cursor int64) string {
	args := []string{"klaus", "watch", "--once", "--after", strconv.FormatInt(cursor, 10)}
	if sessionID != "" {
		args = append(args, "--session-id", shellWord(sessionID))
	}
	for _, flag := range []struct {
		name   string
		values []string
	}{{"--filter", filterIn}, {"--filter-out", filterOut}, {"--run", runs}} {
		for _, v := range flag.values {
			args = append(args, flag.name, shellWord(v))
		}
	}
	if asJSON {
		args = append(args, "--json")
	}
	return strings.Join(args, " ")
}

// statInode returns the inode for path, or 0 if unavailable.
// Used to detect log rotation when fsnotify drops a notification.
func statInode(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return sys.Ino
}

func printKnownEventTypes(out io.Writer) {
	// Group live first, then reserved, alphabetically within each group.
	live := make([]eventTypeInfo, 0)
	reserved := make([]eventTypeInfo, 0)
	for _, t := range knownEventTypes {
		if t.Status == "reserved" {
			reserved = append(reserved, t)
		} else {
			live = append(live, t)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Type < live[j].Type })
	sort.Slice(reserved, func(i, j int) bool { return reserved[i].Type < reserved[j].Type })

	fmt.Fprintln(out, "Live event types (currently emitted by klaus):")
	for _, t := range live {
		fmt.Fprintf(out, "  %-24s %s\n", t.Type, t.Desc)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Reserved event types (not currently emitted; included in default filter for forward-compat):")
	for _, t := range reserved {
		fmt.Fprintf(out, "  %-24s %s\n", t.Type, t.Desc)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Default filter:")
	def := append([]string{}, defaultWatchFilter...)
	sort.Strings(def)
	fmt.Fprintln(out, "  "+strings.Join(def, ", "))
}
