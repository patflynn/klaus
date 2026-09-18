package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/backend"
)

// gitDiffFixture returns a real `git diff`: app.go (40 lines) edited at lines 5 and 35, new.go added, old.go deleted.
func gitDiffFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	git("init", "-q")
	write("app.go", strings.Join(lines, "\n")+"\n")
	write("old.go", "package old\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")

	lines[4], lines[34] = "changed 5", "changed 35"
	write("app.go", strings.Join(lines, "\n")+"\n")
	write("new.go", "package app\n\nfunc New() {}\n")
	if err := os.Remove(filepath.Join(dir, "old.go")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	return git("diff", "--cached")
}

func TestBuildPRReviewInlinesOnlyDiffLines(t *testing.T) {
	result := &ReviewResult{
		Verdict: "Does what it says.",
		Summary: "six findings",
		Findings: []Finding{
			{Severity: "high", File: "app.go", Line: 5, Description: "added line"},
			{Severity: "low", File: "app.go", Line: 8, Description: "context line in hunk"},
			{Severity: "medium", File: "app.go", Line: 20, Description: "outside every hunk"},
			{Severity: "low", File: "./new.go", Line: 3, Description: "new file"},
			{Severity: "low", File: "old.go", Line: 1, Description: "deleted file"},
			{Severity: "low", Description: "general concern"},
		},
	}
	m := Marker{Backend: "codex", SHA: "abc123"}
	req := buildPRReview(result, DiffLines(gitDiffFixture(t)), m)

	if req.Event != "COMMENT" || req.CommitID != "abc123" {
		t.Errorf("event/commit = %q/%q, want COMMENT/abc123", req.Event, req.CommitID)
	}
	var inline []string
	for _, c := range req.Comments {
		if c.Side != "RIGHT" {
			t.Errorf("comment side = %q, want RIGHT", c.Side)
		}
		inline = append(inline, fmt.Sprintf("%s:%d", c.Path, c.Line))
	}
	if got, want := strings.Join(inline, " "), "app.go:5 app.go:8 new.go:3"; got != want {
		t.Errorf("inline comments = %s, want %s", got, want)
	}
	for _, want := range []string{"outside every hunk", "`app.go:20`", "deleted file", "general concern", "Does what it says.", "not an approval"} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("body missing %q:\n%s", want, req.Body)
		}
	}
	if strings.Contains(req.Body, "added line") {
		t.Error("inlined finding must not be repeated in the body")
	}
	if got, ok := ParseMarker(req.Body); !ok || got != m {
		t.Errorf("ParseMarker(body) = %+v, %v; want %+v", got, ok, m)
	}
}

func TestParseMarker(t *testing.T) {
	m := Marker{Backend: "agy", Model: "gemini-3-pro", SHA: "d139283"}
	if got, ok := ParseMarker("verdict\n\n" + m.String() + "\n"); !ok || got != m {
		t.Errorf("round trip = %+v, %v; want %+v", got, ok, m)
	}
	if _, ok := ParseMarker("plain review " + CrossReviewMarker); ok {
		t.Error("unterminated marker must not parse")
	}
	if _, ok := ParseMarker("LGTM"); ok {
		t.Error("body without marker must not parse")
	}
}

// Fake gh serves PR o/r#7 and records the posted review; fake codex checks it runs read-only in an empty dir.
func TestRunPRReview(t *testing.T) {
	const sha = "d139283518cdbd801aa20a97868da29a2417178e"
	other := Marker{Backend: "codex", SHA: "0000000"}.String()
	tests := []struct {
		name    string
		reviews []string
		wantErr error
	}{
		{name: "posts one COMMENT review", reviews: []string{"LGTM", other}},
		{name: "ignores markers copied by other users", reviews: []string{other, "drive-by:" + other, "drive-by:" + Marker{Backend: "codex", SHA: sha}.String()}},
		{name: "same backend already reviewed head", reviews: []string{Marker{Backend: "codex", SHA: sha}.String()}, wantErr: ErrAlreadyReviewed},
		{name: "round limit", reviews: []string{other, Marker{Backend: "agy", SHA: "1111111"}.String()}, wantErr: ErrMaxRounds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string, mode os.FileMode) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			var reviews []map[string]any
			for _, b := range tt.reviews {
				login := "operator"
				if rest, ok := strings.CutPrefix(b, "drive-by:"); ok {
					login, b = "drive-by", rest
				}
				reviews = append(reviews, map[string]any{"body": b, "user": map[string]string{"login": login}})
			}
			rj, _ := json.Marshal(reviews)
			write("reviews.json", string(rj), 0o644)
			write("pr.json", `{"title":"Add greeting","body":"Adds New().","head":{"sha":"`+sha+`"}}`, 0o644)
			write("diff", gitDiffFixture(t), 0o644)
			write("gh", `#!/bin/sh
case "$1 $2" in
"pr diff") cat "$FAKE_DIR/diff" ;;
"api repos/o/r/pulls/7") cat "$FAKE_DIR/pr.json" ;;
"api user") echo operator ;;
"api repos/o/r/pulls/7/reviews?per_page=100") cat "$FAKE_DIR/reviews.json" ;;
"api repos/o/r/pulls/7/reviews") cat > "$FAKE_DIR/posted.json"; echo '{"html_url":"https://github.com/o/r/pull/7#pullrequestreview-1"}' ;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
`, 0o755)
			write("codex", `#!/bin/sh
out=; sandbox=
while [ $# -gt 0 ]; do
  case "$1" in
    --output-last-message) shift; out=$1 ;;
    --sandbox) shift; sandbox=$1 ;;
  esac
  shift
done
[ "$sandbox" = read-only ] || exit 3
[ -z "$(ls -A .)" ] || exit 4
cat > "$FAKE_DIR/prompt"
printf '%s' '{"verdict":"Matches intent.","findings":[{"severity":"high","file":"app.go","line":5,"description":"inline one"},{"severity":"low","file":"app.go","line":20,"description":"folded one"}],"summary":"two issues"}' > "$out"
`, 0o755)
			t.Setenv("FAKE_DIR", dir)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			result, err := RunPRReview(context.Background(), "o/r", "7", PROptions{Backend: backend.Codex, Post: true})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if _, statErr := os.Stat(filepath.Join(dir, "prompt")); statErr == nil {
					t.Error("reviewer ran although posting was refused")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Verdict != "Matches intent." || result.ReviewURL == "" {
				t.Errorf("result = %+v", result)
			}
			prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
			for _, want := range []string{"Pull request title: Add greeting", "Adds New().", "+changed 5", `"verdict"`} {
				if !strings.Contains(string(prompt), want) {
					t.Errorf("prompt missing %q", want)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "posted.json"))
			if err != nil {
				t.Fatal(err)
			}
			var posted reviewRequest
			if err := json.Unmarshal(data, &posted); err != nil {
				t.Fatal(err)
			}
			if posted.Event != "COMMENT" || posted.CommitID != sha || len(posted.Comments) != 1 || posted.Comments[0].Line != 5 {
				t.Errorf("posted = %+v", posted)
			}
			if !strings.Contains(posted.Body, "folded one") || !strings.Contains(posted.Body, CrossReviewMarker) {
				t.Errorf("posted body = %q", posted.Body)
			}
		})
	}
}
