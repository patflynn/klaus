package cmd

import (
	"github.com/patflynn/klaus/internal/backend"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteBackendShellBoundary(t *testing.T) {
	dir := t.TempDir()
	worktree := filepath.Join(dir, "worktree with ' quotes")
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"ssh":   "#!/bin/sh\nshift\nexec sh -c \"$1\"\n",
		"rsync": "#!/bin/sh\nexit 0\n",
		"klaus": "#!/bin/sh\nif [ \"$1\" = _format-stream ]; then cat; fi\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv(sessionIDEnv, "")
	prompt := "a 'quoted' prompt\n$(exit 17) `exit 18`"
	agent := backend.ShellCommand([]string{"printf", "%s\n", prompt})
	command := buildSandboxPaneCommand("test-host", worktree, agent, filepath.Join(dir, "log.jsonl"), "klaus", "", "test-run")
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), prompt) || !strings.Contains(string(out), `"exit_code":0`) {
		t.Fatalf("transport corrupted output: %s", out)
	}
}
