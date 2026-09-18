package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stub CLIs record their argv; the review must reach them only with read-only flags.
func TestReviewSelectedBackend(t *testing.T) {
	for _, tt := range []struct {
		kind string
		want []string
	}{
		{kind: "claude", want: []string{"--safe-mode", "--tools Read,Grep,Glob --permission-mode dontAsk", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--model haiku"}},
		{kind: "codex", want: []string{"--sandbox read-only", "--ignore-user-config", "--ephemeral", "--skip-git-repo-check"}},
		{kind: "agy", want: []string{"--mode plan", "--sandbox", "--agent klaus-consult-", "--print", "Review this diff"}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			stub := `#!/bin/sh
echo "$*" > "$ARGS_FILE"
while [ "$#" -gt 0 ]; do
 if [ "$1" = --agent ]; then shift; cat "$HOME/.gemini/config/agents/$1.md" > "$AGENT_FILE" || exit 21; fi
 if [ "$1" = --output-last-message ]; then shift; printf '%s\n' '{"findings":[],"summary":"reviewed"}' > "$1"; echo 'progress chatter, not JSON'; exit 0; fi
 shift
done
printf '%s\n' '{"findings":[],"summary":"reviewed"}'
`
			for _, bin := range []string{"claude", "codex", "agy"} {
				body := "#!/bin/sh\nexit 20\n"
				if bin == tt.kind {
					body = stub
				}
				if err := os.WriteFile(filepath.Join(dir, bin), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("AGENT_FILE", filepath.Join(dir, "agent"))
			t.Setenv("ARGS_FILE", filepath.Join(dir, "args"))
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			result, err := callReviewInDir(dir, "diff", ReviewConfig{Backend: tt.kind})
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "reviewed" {
				t.Fatalf("result: %+v", result)
			}
			if tt.kind == "agy" {
				definition, err := os.ReadFile(filepath.Join(dir, "agent"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(definition), "tools:\n  - view_file\n  - grep_search\nmainAgent: true\nsubagent: false") {
					t.Fatalf("agent must expose only reading tools: %s", definition)
				}
				leftovers, err := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".gemini", "config", "agents", "klaus-consult-*.md"))
				if err != nil || len(leftovers) != 0 {
					t.Fatalf("agent cleanup: %v, %v", leftovers, err)
				}
			}

			args, _ := os.ReadFile(filepath.Join(dir, "args"))
			for _, w := range tt.want {
				if !strings.Contains(string(args), w) {
					t.Errorf("%s argv missing %q: %s", tt.kind, w, args)
				}
			}
		})
	}
}
