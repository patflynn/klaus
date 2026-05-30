package event

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestTailDeliversAppendedEvents verifies the Tail helper picks up events
// written after it started. This is the path the dashboard uses to wake on
// PRApprovalChanged signals emitted by `klaus approve`.
func TestTailDeliversAppendedEvents(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)
	// Pre-create the file so Tail's waitForFile returns immediately.
	if err := log.Emit(New("seed", AgentStarted, nil)); err != nil {
		t.Fatalf("seed emit: %v", err)
	}

	out := make(chan Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Tail(ctx, filepath.Join(dir, "events.jsonl"), out)
	}()

	// Give Tail a moment to seek to EOF before we emit.
	time.Sleep(200 * time.Millisecond)

	if err := log.Emit(New("run-1", PRApprovalChanged, map[string]interface{}{
		"pr_number": "42",
	})); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case ev := <-out:
		if ev.Type != PRApprovalChanged {
			t.Errorf("got event type %q, want %q", ev.Type, PRApprovalChanged)
		}
		if ev.Data["pr_number"] != "42" {
			t.Errorf("got pr_number %v, want 42", ev.Data["pr_number"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive event within 3s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Tail returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Tail did not exit within 3s of context cancel")
	}
}

// TestTailIgnoresPreExistingHistory verifies Tail only delivers events
// appended after it starts. Pre-existing events should be skipped so the
// dashboard does not re-process old approvals on every restart.
func TestTailIgnoresPreExistingHistory(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)
	if err := log.Emit(New("run-1", PRApprovalChanged, map[string]interface{}{
		"pr_number": "1",
	})); err != nil {
		t.Fatalf("pre-emit: %v", err)
	}

	out := make(chan Event, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Tail(ctx, filepath.Join(dir, "events.jsonl"), out)
	}()

	time.Sleep(200 * time.Millisecond)

	if err := log.Emit(New("run-2", PRApprovalChanged, map[string]interface{}{
		"pr_number": "2",
	})); err != nil {
		t.Fatalf("emit: %v", err)
	}

	select {
	case ev := <-out:
		if ev.Data["pr_number"] != "2" {
			t.Errorf("got pre-existing event with pr_number %v; Tail should only deliver new events", ev.Data["pr_number"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive event within 3s")
	}

	cancel()
	<-done
}
