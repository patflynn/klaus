//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAgyLaunchNeedsBash: agy runs every command through bash, so an agy
// worker on a PATH without bash could edit files and nothing else. Launch on
// such a PATH fails up front, naming bash and the PATH it searched, and
// leaves no run or worktree behind; on the harness's PATH, which has bash,
// the same launch starts the worker.
func TestAgyLaunchNeedsBash(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.WriteStub("agy", h.claudeStubScript())

	// klaus, the agy stub and git, and no bash.
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	noBash := filepath.Join(h.E2EDir, "nobash")
	if err := os.Mkdir(noBash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitBin, filepath.Join(noBash, "git")); err != nil {
		t.Fatal(err)
	}
	path := h.BinDir + ":" + noBash

	res := h.runKlausOnPath(path, "launch", "--backend", "agy", "small task")
	if res.ExitCode == 0 {
		t.Fatalf("launch on a PATH without bash succeeded:\n%s%s", res.Stdout, res.Stderr)
	}
	for _, want := range []string{"the agy worker can't run: bash isn't on the PATH it gets", `searched PATH="` + path + `"`} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, res.Stderr)
		}
	}
	if strings.Contains(res.Stdout, "worktree:") || strings.Contains(res.Stderr, "cleaning up worktree") {
		t.Errorf("the check came after the worktree was created:\n%s%s", res.Stdout, res.Stderr)
	}
	if ids := h.RunIDs(); len(ids) != 0 {
		t.Fatalf("a launch that can't run recorded runs: %v", ids)
	}

	res = h.RunKlaus("launch", "--backend", "agy", "small task")
	if res.ExitCode != 0 {
		t.Fatalf("launch with bash on PATH: %s%s", res.Stdout, res.Stderr)
	}
	h.WaitForClaudeStart(30 * time.Second)
	releaseBackendWorkers(t, h)
}

// runKlausOnPath is RunKlaus with PATH replaced by path.
func (h *Harness) runKlausOnPath(path string, args ...string) RunResult {
	h.t.Helper()
	cmd := exec.Command(klausBinary, args...)
	cmd.Dir = h.RepoDir
	for _, kv := range h.env() {
		if strings.HasPrefix(kv, "PATH=") {
			kv = "PATH=" + path
		}
		cmd.Env = append(cmd.Env, kv)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("running klaus %v: %v", args, err)
	}
	return res
}
