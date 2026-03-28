package review

import (
	"os"
	"runtime"
	"testing"
)

func TestRunLinters_empty(t *testing.T) {
	results, err := RunLinters(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %v", results)
	}
}

func TestRunLinters_passingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	results, err := RunLinters(t.TempDir(), []string{"true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Passed {
		t.Error("expected command to pass")
	}
	if results[0].Command != "true" {
		t.Errorf("command = %q, want %q", results[0].Command, "true")
	}
}

func TestRunLinters_failingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	results, err := RunLinters(t.TempDir(), []string{"false"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Passed {
		t.Error("expected command to fail")
	}
}

func TestRunLinters_capturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	results, err := RunLinters(t.TempDir(), []string{"echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Output != "hello" {
		t.Errorf("output = %q, want %q", results[0].Output, "hello")
	}
}

func TestRunLinters_multipleCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	results, err := RunLinters(t.TempDir(), []string{"true", "false", "echo ok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if !results[0].Passed {
		t.Error("expected first command to pass")
	}
	if results[1].Passed {
		t.Error("expected second command to fail")
	}
	if !results[2].Passed {
		t.Error("expected third command to pass")
	}
}

func TestRunLinters_usesDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/marker.txt", []byte("found"), 0644); err != nil {
		t.Fatal(err)
	}
	results, err := RunLinters(dir, []string{"ls marker.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !results[0].Passed {
		t.Errorf("expected ls to find marker.txt in dir, output: %s", results[0].Output)
	}
}

func TestRunLinters_commandNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	results, err := RunLinters(t.TempDir(), []string{"nonexistent-command-xyz"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Passed {
		t.Error("expected command to fail")
	}
}
