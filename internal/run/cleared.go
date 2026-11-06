package run

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// clearedFile is the session's ledger of the runs cleanup removed, one JSON
// line per run, so the session's spend outlasts their state files.
const clearedFile = "cleared.jsonl"

type clearedEntry struct {
	ID         string   `json:"id"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
}

// Cleared sums the runs cleanup removed from a session.
type Cleared struct {
	Runs       int
	CostUSD    float64
	DurationMS int64
	// FirstCreated is when the earliest of them was created; zero when none
	// recorded it.
	FirstCreated time.Time
}

// RecordCleared appends s's cost and duration to the ledger in the session
// directory dir. Each line is one append, so concurrent cleanups don't
// interleave; a run recorded twice counts once.
func RecordCleared(dir string, s *State) error {
	line, err := json.Marshal(clearedEntry{ID: s.ID, CostUSD: s.CostUSD, DurationMS: s.DurationMS, CreatedAt: s.CreatedAt})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, clearedFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadCleared sums the ledger in the session directory dir, leaving out the
// runs in live, whose state cleanup recorded but failed to remove; zero when
// there is no ledger.
func ReadCleared(dir string, live []*State) (Cleared, error) {
	var c Cleared
	f, err := os.Open(filepath.Join(dir, clearedFile))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	seen := make(map[string]bool)
	for _, s := range live {
		seen[s.ID] = true
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e clearedEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		c.Runs++
		if e.CostUSD != nil {
			c.CostUSD += *e.CostUSD
		}
		if e.DurationMS != nil {
			c.DurationMS += *e.DurationMS
		}
		if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil && (c.FirstCreated.IsZero() || t.Before(c.FirstCreated)) {
			c.FirstCreated = t
		}
	}
	return c, sc.Err()
}
