package consult

import (
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
	if err := store.Append("design", "question", "answer"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir, "design.log"))
	if err != nil || !strings.Contains(string(data), "question") || !strings.Contains(string(data), "answer") {
		t.Fatalf("log: %s, %v", data, err)
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
