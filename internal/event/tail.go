package event

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Tail follows the events file at path, sending each newly appended Event to
// out. It blocks until ctx is cancelled. New events are emitted starting from
// the end of the file at the time Tail is called (it does NOT replay existing
// history). If the file does not yet exist, Tail waits for it to appear.
//
// Tail is used by the dashboard to subscribe to klaus-internal invalidation
// signals (e.g. PRApprovalChanged) written by other klaus processes such as
// `klaus approve`. Compared with watching the run state directory, tailing
// the event log gives a precise, intent-bearing signal rather than relying on
// fsnotify of a JSON state file as a proxy.
func Tail(ctx context.Context, path string, out chan<- Event) error {
	if err := waitForFile(ctx, path); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(filepath.Dir(path)); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

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
					var evt Event
					if err := json.Unmarshal(line, &evt); err != nil {
						continue
					}
					select {
					case out <- evt:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}

	if err := drain(); err != nil {
		return err
	}

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
			if ev.Op&(fsnotify.Write|fsnotify.Chmod|fsnotify.Create) != 0 {
				if err := drain(); err != nil {
					return err
				}
			}
		case <-watcher.Errors:
			// Non-fatal — keep going.
		case <-ticker.C:
			// Defensive: catch missed write notifications.
			if err := drain(); err != nil {
				return err
			}
		}
	}
}

func waitForFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
