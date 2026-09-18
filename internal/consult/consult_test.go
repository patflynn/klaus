package consult

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/backend"
)

func TestThreadStore(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now().UTC()
	want := &Thread{Backend: backend.Codex, Model: "model", BackendSessionID: "id", Dir: "/repo", Role: "critic", CreatedAt: now, LastUsed: now, Turns: 2}
	unlock, err := store.Lock("design")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := store.Lock("design"); err == nil {
		t.Fatal("concurrent turn accepted")
	}
	if err := store.Save("design", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("design")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	turn, err := store.BeginTurn("design", want, "question", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := os.ReadFile(filepath.Join(store.Dir, "design.log"))
	if err != nil {
		t.Fatal(err)
	}
	var record Turn
	if err := json.Unmarshal(pending, &record); err != nil {
		t.Fatal(err)
	}
	if record.Status != "pending" || record.Prompt != "question" || record.Answer != "" || record.RepoRevision != "abc123" || record.ResumeID != "id" {
		t.Fatalf("pending: %+v", record)
	}
	if err := store.FinishTurn("design", turn, Result{Answer: "answer", SessionID: "id", SessionIDSource: "stderr"}, nil); err != nil {
		t.Fatal(err)
	}
	completed, _ := os.ReadFile(filepath.Join(store.Dir, "design.log"))
	if err := json.Unmarshal(completed, &record); err != nil {
		t.Fatal(err)
	}
	if record.Status != "ok" || record.SessionIDSource != "stderr" {
		t.Fatalf("completed: %+v", record)
	}
	failed, err := store.BeginTurn("design", want, "failed question", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn("design", failed, Result{Answer: "partial"}, fmt.Errorf("backend failed")); err != nil {
		t.Fatal(err)
	}
	interrupted, err := store.BeginTurn("design", want, "interrupted question", "")
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginTurn("design", want, "next question", "")
	if err != nil || next.Number != interrupted.Number+1 || interrupted.Number != failed.Number+1 {
		t.Fatalf("attempt numbers: %+v, %v", next, err)
	}
	if err := store.FinishTurn("design", interrupted, Result{}, nil); err == nil {
		t.Fatal("stale completion overwrote newer turn")
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir, "design.log"))
	if err != nil || !strings.Contains(string(data), "question") || !strings.Contains(string(data), "answer") {
		t.Fatalf("log: %s, %v", data, err)
	}
	var statuses []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var persisted Turn
		if err := json.Unmarshal(line, &persisted); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, persisted.Status)
	}
	if !reflect.DeepEqual(statuses, []string{"ok", "error: backend failed", "pending", "pending"}) {
		t.Fatalf("durable statuses: %v", statuses)
	}
	if err := store.Save("../escape", want); err == nil {
		t.Fatal("accepted path traversal")
	}
}

func TestInlineFiles(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "plan"), filepath.Join(dir, "diff")
	if err := os.WriteFile(a, []byte("plan contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("diff contents"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := InlineFiles("question", []string{a, b})
	if err != nil || got != "question\n\n### "+a+"\nplan contents\n\n### "+b+"\ndiff contents" {
		t.Fatalf("inline: %q, %v", got, err)
	}
	if err := os.WriteFile(a, make([]byte, MaxFileBytes), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InlineFiles("q", []string{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := InlineFiles("q", []string{a, b}); err == nil || !strings.Contains(err.Error(), "200KB") {
		t.Fatalf("total cap: %v", err)
	}
	if _, err := InlineFiles("q", []string{filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestInstalled(t *testing.T) {
	old := LookupBinary
	t.Cleanup(func() { LookupBinary = old })
	LookupBinary = func(name string) (string, error) {
		if name == "agy" {
			return "", os.ErrNotExist
		}
		return name, nil
	}
	got, err := Installed([]string{"agy", "claude", "codex", "claude"}, "codex")
	if err != nil || !reflect.DeepEqual(got, []backend.Kind{backend.Claude}) {
		t.Fatalf("selection: %v, %v", got, err)
	}
	if _, err := Installed([]string{"typo"}, ""); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func TestSessionIDPaths(t *testing.T) {
	for _, tt := range []struct {
		name                               string
		kind                               backend.Kind
		stdout, stderr, diagnostic, source string
	}{
		{"codex header", backend.Codex, "answer", "session id: abc123\n", "", "stderr"},
		{"stdout JSON", backend.Codex, `{"thread_id":"abc123"}`, "", "", "stdout-json"},
		{"text fallback", backend.Codex, "session id: abc123\n", "", "", "text"},
		{"agy diagnostic", backend.Agy, "answer", "", "Sending user message to conversation abc123 (items=1)", "diagnostic-log"},
		{"agy JSON fallback", backend.Agy, `{"conversation_id":"abc123"}`, "", "", "stdout-json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id, source, err := extractSessionID(tt.kind, "", "", tt.stdout, tt.stderr, tt.diagnostic)
			if err != nil || id != "abc123" || source != tt.source {
				t.Fatalf("%q %q %v", id, source, err)
			}
		})
	}
	for _, kind := range []backend.Kind{backend.Codex, backend.Agy} {
		_, _, err := extractSessionID(kind, "", "", "answer", "diagnostics", "unrecognized")
		if err == nil {
			t.Fatal("missing ID accepted")
		}
		for _, path := range []string{string(kind), "stderr", "stdout-json", "text"} {
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("missing path %s: %v", path, err)
			}
		}
		if kind == backend.Agy && !strings.Contains(err.Error(), "diagnostic-log") {
			t.Fatal(err)
		}
	}
}

func TestResumeIDRejectsAnswerText(t *testing.T) {
	for _, output := range []string{`{"session_id":"unrelated"}`, "session id: unrelated\n"} {
		id, source, err := extractSessionID(backend.Claude, "known", "", output, "", "")
		if err != nil || id != "known" || source != "resume" {
			t.Fatalf("resume: %s %s %v", id, source, err)
		}
	}
}

func TestLegacyTranscriptBoundary(t *testing.T) {
	store := NewStore(t.TempDir())
	unlock, err := store.Lock("legacy")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	path := filepath.Join(store.Dir, "legacy.log")
	if err := os.WriteFile(path, []byte("legacy without newline"), 0600); err != nil {
		t.Fatal(err)
	}
	thread := &Thread{Backend: backend.Codex}
	turn, err := store.BeginTurn("legacy", thread, "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn("legacy", turn, Result{}, fmt.Errorf("failed")); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginTurn("legacy", thread, "retry", "")
	if err != nil || next.Number != 2 {
		t.Fatalf("next: %+v, %v", next, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 3 || string(lines[0]) != "legacy without newline" {
		t.Fatalf("legacy transcript: %s", data)
	}
	for _, line := range lines[1:] {
		var saved Turn
		if err := json.Unmarshal(line, &saved); err != nil {
			t.Fatal(err)
		}
	}
}
