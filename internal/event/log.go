package event

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const eventsFile = "events.jsonl"

// Log provides append-only, file-locked access to an event log.
type Log struct {
	baseDir string // session directory (e.g. ~/.klaus/sessions/<id>)
}

// NewLog creates a Log rooted at the given session base directory.
func NewLog(baseDir string) *Log {
	return &Log{baseDir: baseDir}
}

// Path returns the full path to the events file.
func (l *Log) Path() string {
	return filepath.Join(l.baseDir, eventsFile)
}

// Emit appends a single event to the log file.
// Uses file-level locking so multiple concurrent writers are safe.
func (l *Log) Emit(evt Event) error {
	if err := os.MkdirAll(l.baseDir, 0o755); err != nil {
		return fmt.Errorf("creating event log dir: %w", err)
	}

	f, err := os.OpenFile(l.Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	// Exclusive lock for the duration of the write
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking event log: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing event: %w", err)
	}
	return nil
}

// Read returns all events in the log.
func (l *Log) Read() ([]Event, error) {
	f, err := os.Open(l.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt Event
		if err := json.Unmarshal(line, &evt); err != nil {
			continue // skip malformed lines
		}
		events = append(events, evt)
	}
	return events, scanner.Err()
}

// ReadSince reads events after the given marker (a line offset as a string).
// Returns the events, a new marker for subsequent calls, and any error.
// Pass "" as marker to read from the beginning.
func (l *Log) ReadSince(marker string) ([]Event, string, error) {
	var offset int
	if marker != "" {
		var err error
		offset, err = strconv.Atoi(marker)
		if err != nil {
			return nil, "", fmt.Errorf("invalid marker %q: %w", marker, err)
		}
	}

	all, err := l.Read()
	if err != nil {
		return nil, "", err
	}

	if offset > len(all) {
		offset = len(all)
	}

	newEvents := all[offset:]
	newMarker := strconv.Itoa(len(all))
	return newEvents, newMarker, nil
}
