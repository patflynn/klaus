package event

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEmitAndRead(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	evt1 := New("run-001", AgentStarted, map[string]interface{}{
		"id":     "run-001",
		"prompt": "fix the bug",
	})
	evt2 := New("run-001", AgentCompleted, map[string]interface{}{
		"id":       "run-001",
		"cost_usd": 1.23,
	})

	if err := log.Emit(evt1); err != nil {
		t.Fatalf("Emit evt1: %v", err)
	}
	if err := log.Emit(evt2); err != nil {
		t.Fatalf("Emit evt2: %v", err)
	}

	events, err := log.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != AgentStarted {
		t.Errorf("events[0].Type = %q, want %q", events[0].Type, AgentStarted)
	}
	if events[1].Type != AgentCompleted {
		t.Errorf("events[1].Type = %q, want %q", events[1].Type, AgentCompleted)
	}
	if events[0].RunID != "run-001" {
		t.Errorf("events[0].RunID = %q, want %q", events[0].RunID, "run-001")
	}
}

func TestReadSince(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	// Emit 3 events
	for i := 0; i < 3; i++ {
		if err := log.Emit(New("run-001", AgentStarted, nil)); err != nil {
			t.Fatal(err)
		}
	}

	// Read from beginning
	events, marker, err := log.ReadSince("")
	if err != nil {
		t.Fatalf("ReadSince empty: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if marker != "3" {
		t.Errorf("marker = %q, want %q", marker, "3")
	}

	// Read since marker — should be empty
	events, marker2, err := log.ReadSince(marker)
	if err != nil {
		t.Fatalf("ReadSince marker: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}

	// Emit 2 more
	for i := 0; i < 2; i++ {
		if err := log.Emit(New("run-002", AgentCompleted, nil)); err != nil {
			t.Fatal(err)
		}
	}

	// Read since previous marker
	events, marker3, err := log.ReadSince(marker2)
	if err != nil {
		t.Fatalf("ReadSince marker2: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 new events, got %d", len(events))
	}
	if marker3 != "5" {
		t.Errorf("marker3 = %q, want %q", marker3, "5")
	}
}

func TestReadEmpty(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	events, err := log.Read()
	if err != nil {
		t.Fatalf("Read empty: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestConcurrentEmit(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = log.Emit(New("run-001", AgentStarted, nil))
		}()
	}
	wg.Wait()

	events, err := log.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != n {
		t.Errorf("expected %d events, got %d", n, len(events))
	}
}

func TestEventsFileLocation(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	if err := log.Emit(New("run-001", AgentStarted, nil)); err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(dir, "events.jsonl")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("events file not at expected location %s: %v", expected, err)
	}
}

func TestEventDataPreserved(t *testing.T) {
	dir := t.TempDir()
	log := NewLog(dir)

	data := map[string]interface{}{
		"cost_usd":    3.42,
		"duration_ms": float64(15000),
		"pr_url":      "https://github.com/owner/repo/pull/42",
	}
	if err := log.Emit(New("run-001", AgentCompleted, data)); err != nil {
		t.Fatal(err)
	}

	events, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	evt := events[0]
	if cost, ok := evt.Data["cost_usd"].(float64); !ok || cost != 3.42 {
		t.Errorf("cost_usd = %v, want 3.42", evt.Data["cost_usd"])
	}
	if prURL, ok := evt.Data["pr_url"].(string); !ok || prURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("pr_url = %v, want https://github.com/owner/repo/pull/42", evt.Data["pr_url"])
	}
}
