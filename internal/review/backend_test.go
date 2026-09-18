package review

import (
	"errors"
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
		{kind: "codex", want: []string{"--sandbox read-only", "--ignore-user-config", "--ephemeral"}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			stub := `#!/bin/sh
echo "$*" > "$ARGS_FILE"
while [ "$#" -gt 0 ]; do
 if [ "$1" = --output-last-message ]; then shift; printf '%s\n' '{"findings":[],"summary":"reviewed"}' > "$1"; echo 'progress chatter, not JSON'; exit 0; fi
 shift
done
printf '%s\n' '{"findings":[],"summary":"reviewed"}'
`
			for _, bin := range []string{"claude", "codex"} {
				body := "#!/bin/sh\nexit 20\n"
				if bin == tt.kind {
					body = stub
				}
				if err := os.WriteFile(filepath.Join(dir, bin), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ARGS_FILE", filepath.Join(dir, "args"))
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			result, err := callReviewInDir(dir, "diff", ReviewConfig{Backend: tt.kind})
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "reviewed" {
				t.Fatalf("result: %+v", result)
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

func TestReviewRefusesAgy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agy"), []byte("#!/bin/sh\ntouch \"$0.ran\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if _, err := callReviewInDir(dir, "diff", ReviewConfig{Backend: "agy"}); !errors.Is(err, ErrAgyReviewer) {
		t.Fatalf("err = %v, want ErrAgyReviewer", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agy.ran")); err == nil {
		t.Error("agy ran without read-only enforcement")
	}
}
