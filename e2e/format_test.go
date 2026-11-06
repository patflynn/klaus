//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

// TestPreCommitHookFormats commits through the pre-commit hook a real launch
// installs, with the project's format command set. A staged file is committed
// formatted, a file not staged stays out of the commit as it was, a file
// partly staged is committed as staged, and a formatter failure stops the
// commit with its output shown.
func TestPreCommitHookFormats(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)

	// Upper-cases every top-level .md file; refuses one that says
	// UNFORMATTABLE.
	formatter := filepath.Join(h.E2EDir, "fmt.sh")
	script := `#!/bin/sh
for f in *.md; do
  if grep -q UNFORMATTABLE "$f"; then echo "fmt: cannot format $f" >&2; exit 3; fi
  tr a-z A-Z < "$f" > "$f.tmp" && mv "$f.tmp" "$f"
done
`
	if err := os.WriteFile(formatter, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h.AmendRepoConfig(map[string]any{"format": []string{formatter}})

	res := h.RunKlaus("launch", "write some notes")
	if res.ExitCode != 0 {
		t.Fatalf("launch exited %d\nstdout:\n%s\nstderr:\n%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	st, err := h.ReadState(h.RunIDs()[0])
	if err != nil {
		t.Fatal(err)
	}
	h.WaitForClaudeStart(30 * time.Second)
	wt := st.Worktree
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("notes.md", "hello\n")
	h.git(wt, "add", "notes.md")
	write("README.md", "# widget\nnot staged\n")
	write("todo.md", "staged\n")
	h.git(wt, "add", "todo.md")
	write("todo.md", "staged\nnot staged\n")
	h.git(wt, "commit", "-qm", "Add notes")

	if got := h.GitOutput(wt, "show", "HEAD:notes.md"); got != "HELLO" {
		t.Errorf("committed notes.md = %q, want the formatted %q", got, "HELLO")
	}
	if got := h.GitOutput(wt, "show", "HEAD:README.md"); got != "# widget" {
		t.Errorf("committed README.md = %q, want it unchanged", got)
	}
	if got := h.GitOutput(wt, "show", "HEAD:todo.md"); got != "staged" {
		t.Errorf("committed todo.md = %q, want only its staged content %q", got, "staged")
	}
	if staged, unstaged := h.GitOutput(wt, "diff", "--cached", "--name-only"), h.GitOutput(wt, "diff", "--name-only"); staged != "" || unstaged != "README.md\ntodo.md" {
		t.Errorf("after the commit, staged = %q and not staged = %q, want none staged and README.md, todo.md not staged", staged, unstaged)
	}

	head := h.GitOutput(wt, "rev-parse", "HEAD")
	write("bad.md", "UNFORMATTABLE\n")
	h.git(wt, "add", "bad.md")
	cmd := exec.Command("git", "commit", "-qm", "Add bad notes")
	cmd.Dir = wt
	cmd.Env = append(os.Environ(), "HOME="+h.Home, "GIT_CONFIG_GLOBAL="+filepath.Join(h.Home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("commit with a failing formatter should fail:\n%s", out)
	}
	if !strings.Contains(string(out), "fmt: cannot format bad.md") {
		t.Errorf("failed commit should show the formatter's output:\n%s", out)
	}
	if got := h.GitOutput(wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s after a failed format, want %s", got, head)
	}

	h.ReleaseClaude()
	h.WaitForState(st.ID, func(s *run.State) bool { return s.TmuxPane == nil && s.Worktree == "" }, 30*time.Second)
}
