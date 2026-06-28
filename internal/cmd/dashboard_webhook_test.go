package cmd

import (
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

func TestWebhookFreshnessText(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		last     time.Time
		wantText string
		wantSev  webhookSeverity
	}{
		{"zero", time.Time{}, "no events yet", webhookFresh},
		{"10s fresh", now.Add(-10 * time.Second), "last event 10s", webhookFresh},
		{"just under stale", now.Add(-(webhookStaleAfter - time.Second)), "last event 29m", webhookFresh},
		{"at stale boundary", now.Add(-webhookStaleAfter), "last event 30m", webhookStale},
		{"45m stale", now.Add(-45 * time.Minute), "last event 45m", webhookStale},
		{"just under dead", now.Add(-(webhookDeadAfter - time.Second)), "last event 1h", webhookStale},
		{"at dead boundary", now.Add(-webhookDeadAfter), "last event 2h", webhookDead},
		{"5h dead", now.Add(-5 * time.Hour), "last event 5h", webhookDead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, sev := webhookFreshnessText(c.last, now)
			if text != c.wantText {
				t.Errorf("text = %q, want %q", text, c.wantText)
			}
			if sev != c.wantSev {
				t.Errorf("severity = %d, want %d", sev, c.wantSev)
			}
		})
	}
}

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{0, "0s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{3 * time.Hour, "3h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{50 * time.Hour, "2d"},
	}
	for _, c := range cases {
		if got := humanizeDuration(c.d); got != c.want {
			t.Errorf("humanizeDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestSelectedPRURLForOpenHandler verifies the path the 'o' key uses:
// selectablePRs + clampCursor yields the entry whose state carries the URL to
// open in the browser.
func TestSelectedPRURLForOpenHandler(t *testing.T) {
	states := []*run.State{
		{ID: "a", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/1"), CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "b", Prompt: "p", TargetRepo: strPtr("r"), PRURL: strPtr("https://github.com/o/r/pull/2"), CreatedAt: "2026-01-01T00:01:00Z"},
	}
	m := dashboardModel{states: states, cursor: 1}

	entries := selectablePRs(m.states)
	e := entries[clampCursor(m.cursor, len(entries))]
	if e.state == nil || e.state.PRURL == nil {
		t.Fatal("selected entry has no PRURL")
	}
	if got := *e.state.PRURL; got != "https://github.com/o/r/pull/2" {
		t.Errorf("selected PR URL = %q, want pull/2", got)
	}
}
