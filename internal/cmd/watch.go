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
	event.AgentPRCreated, // live
	"agent:error",        // reserved
	event.PRApproved,     // live
	event.PRMerged,       // live
	"ci:failed",          // reserved (closest live equivalent: agent:ci-failed)
	"ci:passed",          // reserved (closest live equivalent: agent:ci-passed)
	"pr:comment",         // reserved
}

// knownEventTypes maps event types to a one-line description and whether the
// type is currently emitted somewhere in klaus ("live") or reserved for future
// use. --list-types renders this table.
var knownEventTypes = []eventTypeInfo{
	{event.AgentStarted, "live", "An agent run started"},
	{event.AgentCompleted, "live", "An agent run finished (success or failure)"},
	{event.AgentPRCreated, "live", "An agent published a PR"},
	{event.AgentCIPassed, "live", "CI passed on an agent-owned PR"},
	{event.AgentCIFailed, "live", "CI failed on an agent-owned PR"},
	{event.AgentNeedsAttention, "live", "An agent stopped and needs operator input"},
	{event.PRAwaitingApproval, "live", "A PR is ready for human approval"},
	{event.PRApproved, "live", "A PR was approved"},
	{event.PRMerged, "live", "A PR merged"},
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

  agent:pr-created, agent:error, pr:approved, pr:merged,
  ci:failed, ci:passed, pr:comment

Some of those types are reserved (not currently emitted) but kept in the
default filter so this command stays forward-compatible. Run 'klaus watch
--list-types' to see which types are live vs reserved.

Passing --filter REPLACES the default. --filter-out is always subtracted.
Both flags are repeatable and use OR semantics within each flag.`,
	Example: `  # Default filter, follow new events from now:
  klaus watch

  # Only react when a PR merges:
  klaus watch --filter pr:merged

  # Default filter minus pr:approved (e.g. don't notify on self-approvals):
  klaus watch --filter-out pr:approved

  # Replay everything in the file, then keep following:
  klaus watch --since-start

  # Raw JSONL passthrough for downstream tools:
  klaus watch --json`,
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().StringSlice("filter", nil, "Event type to include (repeatable, OR semantics). Replaces the default filter when set.")
	watchCmd.Flags().StringSlice("filter-out", nil, "Event type to exclude (repeatable). Applied after --filter.")
	watchCmd.Flags().Bool("since-start", false, "Emit events already in the file before tailing new ones.")
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
	sinceStart, _ := cmd.Flags().GetBool("since-start")
	asJSON, _ := cmd.Flags().GetBool("json")
	sessionOverride, _ := cmd.Flags().GetString("session-id")

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
	eventsPath := filepath.Join(sessionsDir, sessionID, "events.jsonl")

	flt := buildFilter(filterIn, filterOut)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return streamEvents(ctx, eventsPath, flt, sinceStart, asJSON, cmd.OutOrStdout())
}

// watchFilter decides whether a given event type should be emitted.
type watchFilter struct {
	include map[string]struct{} // empty means "match anything"
	exclude map[string]struct{}
}

func buildFilter(includeFlags, excludeFlags []string) watchFilter {
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
	return watchFilter{include: inc, exclude: exc}
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
// to out as either formatted summaries or raw JSON.
func streamEvents(ctx context.Context, path string, flt watchFilter, sinceStart, asJSON bool, out io.Writer) error {
	if err := waitForFile(ctx, path, 10*time.Second); err != nil {
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
					if err := emitLine(line, flt, asJSON, out); err != nil {
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

func emitLine(line []byte, flt watchFilter, asJSON bool, out io.Writer) error {
	var evt event.Event
	if err := json.Unmarshal(line, &evt); err != nil {
		// Skip malformed lines silently — matches event.Log.Read behavior.
		return nil
	}
	if !flt.matches(evt.Type) {
		return nil
	}
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
		reason := get("reason")
		if reason != "" {
			return fmt.Sprintf("needs attention — %s", reason)
		}
		return "needs attention"
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
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// waitForFile blocks until path exists or timeout elapses. Polls every 500ms.
// Returns nil if the file becomes available.
func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat events file: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("events file did not appear within %s: %s", timeout, path)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
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
