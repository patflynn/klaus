//go:build e2e

// Package e2e contains end-to-end tests that drive the real klaus binary
// against a real (but fully isolated) tmux server and real git repositories.
//
// These tests are guarded by the `e2e` build tag so the default
// `go test ./...` is unaffected. Run them with:
//
//	go test -tags e2e ./e2e/...
//
// See e2e/README.md for the design and how to add new scenarios.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// klausBinary is the path to the klaus binary built once for the whole test
// binary by TestMain. Every Harness shells out to this exact binary.
var klausBinary string

// TestMain builds the klaus binary a single time before running the e2e
// suite, then removes it afterwards. Building once keeps the suite fast even
// though each test spins up its own isolated environment.
func TestMain(m *testing.M) {
	code, err := runMain(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runMain(m *testing.M) (int, error) {
	// Ensure tmux exists — the whole suite is meaningless without it.
	if _, err := exec.LookPath("tmux"); err != nil {
		return 0, fmt.Errorf("tmux not found in PATH: %w", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	// The e2e package lives directly under the module root.
	repoRoot := filepath.Dir(wd)

	binDir, err := os.MkdirTemp("", "klaus-e2e-bin-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(binDir)

	klausBinary = filepath.Join(binDir, "klaus")
	build := exec.Command("go", "build", "-o", klausBinary, "./cmd/klaus/")
	build.Dir = repoRoot
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return 0, fmt.Errorf("building klaus binary: %w", err)
	}

	return m.Run(), nil
}
