//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubProseBackends replaces agy, codex and claude with stubs that tell the
// prose pass's rewrite and fact check, and the peer review, apart by their
// prompt. Each logs its role to <name>.calls and its last prompt to
// <name>.<role>.stdin, then prints <name>.<role> from E2EDir (a clean review
// by default). <name>.<role>.fail makes it fail; <name>.<role>.sleep makes it
// hang. prose sets the repo's pre_review.prose config; the peer reviewer is
// always claude, and the author is author.
func (h *Harness) stubProseBackends(author string, prose map[string]any) {
	h.t.Helper()
	for _, name := range []string{"agy", "codex", "claude"} {
		h.WriteStub(name, `#!/usr/bin/env bash
dir=`+h.E2EDir+`
input=$(cat)
case "$input" in
  *"Fact-check these rewrites"*) role=check ;;
  *"Rewrite the prose this change adds"*) role=write ;;
  *) role=review ;;
esac
f="$dir/`+name+`.$role"
echo "$role" >> "$dir/`+name+`.calls"
printf '%s\n' "$input" > "$f.stdin"
[ -e "$f.sleep" ] && sleep 60
[ -e "$f.fail" ] && { echo "boom" >&2; exit 3; }
if [ -e "$f" ]; then cat "$f"; else echo '{"findings": [], "summary": "No issues found.", "ledger": [], "edits": [], "failures": []}'; fi
`)
	}
	review := map[string]any{"review_backend": "claude"}
	h.AmendRepoConfig(map[string]any{
		"default_agent_backend": author,
		"backends":              map[string]any{"claude": review, "codex": review, "agy": review},
		"pre_review":            map[string]any{"prose": prose},
	})
}

// stubAnswer sets what stub name answers in role.
func (h *Harness) stubAnswer(name, role string, answer any) {
	h.t.Helper()
	data, err := json.Marshal(answer)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeE2E(name+"."+role, string(data))
}

func (h *Harness) writeE2E(name, content string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.E2EDir, name), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *Harness) readE2E(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.E2EDir, name))
	return string(data)
}

// commitFiles writes files in the sandbox repo and commits them on branch
// work with message.
func (h *Harness) commitFiles(files map[string]string, message string) {
	h.t.Helper()
	h.git(h.RepoDir, "checkout", "-qB", "work")
	for name, content := range files {
		p := filepath.Join(h.RepoDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.git(h.RepoDir, "add", "-A")
	h.git(h.RepoDir, "commit", "-qm", message)
}

const proseReadme = "# widget\n\nDeploy to the build host first, but only from main, once the smoke test passes.\n"

// TestPreReviewProseChoosesFamilies: the writer is the first family in
// pre_review.prose.order that isn't the author's, the fact checker the next,
// else the author's; with no other family the pass is skipped.
func TestPreReviewProseChoosesFamilies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		author, writer, checker string
		order                   []string
		skipped                 bool
	}{
		{author: "claude", writer: "agy", checker: "codex"},
		{author: "agy", writer: "codex", checker: "claude"},
		{author: "codex", writer: "agy", checker: "claude"},
		{author: "claude", order: []string{"agy", "claude"}, writer: "agy", checker: "claude"},
		{author: "codex", order: []string{"codex"}, skipped: true},
	} {
		t.Run(tc.author+"-"+strings.Join(tc.order, ","), func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			prose := map[string]any{}
			if tc.order != nil {
				prose["order"] = tc.order
			}
			h.stubProseBackends(tc.author, prose)
			h.commitFiles(map[string]string{"README.md": proseReadme}, "add deploy note")
			if !tc.skipped {
				h.stubAnswer(tc.writer, "write", map[string]any{"edits": []any{map[string]any{
					"file": "README.md", "old": "Deploy to the build host first, but only from main, once the smoke test passes.",
					"new": "Deploy to the build host first: only from main, after the smoke test passes."}}})
			}

			res := h.RunKlaus("_pre-review")
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "All checks passed.") {
				t.Fatalf("_pre-review exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			if tc.skipped {
				if !strings.Contains(res.Stdout, "Skipped: no model family other than the author's (codex) is installed.") {
					t.Errorf("_pre-review didn't skip the pass:\n%s", res.Stdout)
				}
				return
			}
			if want := "Prose rewrite (writer: " + tc.writer + "; fact check: " + tc.checker + "; author: " + tc.author + ")"; !strings.Contains(res.Stdout, want) {
				t.Errorf("output lacks %q:\n%s", want, res.Stdout)
			}
			if got := h.readE2E(tc.writer + ".calls"); !strings.Contains(got, "write") {
				t.Errorf("%s wasn't asked to rewrite; calls: %q", tc.writer, got)
			}
			if got := h.readE2E(tc.checker + ".calls"); !strings.Contains(got, "check") {
				t.Errorf("%s wasn't asked to fact-check; calls: %q", tc.checker, got)
			}
			if !strings.Contains(res.Stdout, "[1] README.md: passed") {
				t.Errorf("the rewrite didn't pass:\n%s", res.Stdout)
			}
		})
	}
}

// TestPreReviewProseRewrite: the writer gets the change's prose hunks and its
// commit message, not its code; a fact-check failure surfaces against its
// rewrite and keeps it out of the patch, which applies the rest. The pass is
// recorded as a prose job.
func TestPreReviewProseRewrite(t *testing.T) {
	t.Parallel()
	h := NewHarness(t)
	h.stubProseBackends("claude", map[string]any{})
	message := "Add a deploy note\n\nThe README now says where to deploy and when, because operators kept deploying straight to production from feature branches before the smoke test had finished running on the build host."
	h.commitFiles(map[string]string{
		"README.md":     proseReadme,
		"docs/guide.md": "# Guide\n\nIn order to be able to run the tool, you will need to install it first.\n",
		"main.go":       "package main\n\nfunc main() { println(\"secret code text\") }\n",
	}, message)
	h.stubAnswer("agy", "write", map[string]any{
		"ledger": []any{
			map[string]any{"id": "C1", "where": "README.md", "claim": "deploy to the build host first"},
			map[string]any{"id": "C2", "where": "README.md", "claim": "only from main", "conditions": []string{"after the smoke test"}},
		},
		"edits": []any{
			map[string]any{"file": "README.md", "old": "Deploy to the build host first, but only from main, once the smoke test passes.", "new": "Deploy to the build host first.", "claims": []string{"C1", "C2"}},
			map[string]any{"file": "COMMIT_EDITMSG", "old": "The README now says where to deploy and when, because operators kept deploying straight to production from feature branches before the smoke test had finished running on the build host.", "new": "Operators deployed from feature branches before the smoke test finished."},
			map[string]any{"file": "docs/guide.md", "old": "In order to be able to run the tool, you will need to install it first.", "new": "Install the tool before running it."},
			map[string]any{"file": "main.go", "old": "secret code text", "new": "other"},
			map[string]any{"file": "README.md", "old": "# widget\n\nDeploy to the build host first, but only from main, once the smoke test passes.", "new": "Deploy from main."},
		},
	})
	h.stubAnswer("codex", "check", map[string]any{"failures": []any{
		map[string]any{"edit": 1, "claim": "C2", "problem": "dropped the condition: only from main, after the smoke test"},
	}})

	res := h.RunKlaus("_pre-review")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "All checks passed.") {
		t.Fatalf("_pre-review exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	prompt := h.readE2E("agy.write.stdin")
	for _, want := range []string{"+Deploy to the build host first, but only from main", "+In order to be able to run the tool", "operators kept deploying straight to production", "Ledger: before rewriting", "Say what a reader can't easily infer"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the writer's prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "secret code text") {
		t.Errorf("the writer got a code file that isn't prose:\n%s", prompt)
	}
	if check := h.readE2E("codex.check.stdin"); !strings.Contains(check, "only from main") || !strings.Contains(check, "Install the tool before running it.") {
		t.Errorf("the fact check didn't get the ledger and rewrites:\n%s", check)
	}
	for _, want := range []string{
		"[1] README.md: failed the fact check",
		"✗ C2: dropped the condition: only from main, after the smoke test",
		"[2] commit message: passed",
		"[3] docs/guide.md: passed",
		"[4] main.go: skipped: not prose this change adds",
		"[5] README.md: skipped: it rewrites text the change didn't add",
		"Say in your final report what you applied",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Stdout)
		}
	}

	patch := field(res.Stdout, "Patch of the passing rewrites: git apply ")
	msgFile := field(res.Stdout, "Commit message with its passing rewrites: git commit --amend -F ")
	if patch == "" || msgFile == "" {
		t.Fatalf("no patch or commit message file:\n%s", res.Stdout)
	}
	if out, err := exec.Command("git", "-C", h.RepoDir, "apply", patch).CombinedOutput(); err != nil {
		t.Fatalf("git apply %s: %v\n%s", patch, err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(h.RepoDir, "docs/guide.md")); !strings.Contains(string(got), "Install the tool before running it.") {
		t.Errorf("the patch didn't rewrite docs/guide.md:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(h.RepoDir, "README.md")); string(got) != proseReadme {
		t.Errorf("the patch has the rewrite that failed the fact check:\n%s", got)
	}
	if got, _ := os.ReadFile(msgFile); !strings.HasPrefix(string(got), "Add a deploy note\n\nOperators deployed from feature branches") {
		t.Errorf("commit message file = %q", got)
	}

	jobsOut := h.RunKlaus("jobs")
	if !strings.Contains(jobsOut.Stdout, "prose") || !strings.Contains(jobsOut.Stdout, "agy") {
		t.Fatalf("klaus jobs doesn't list the prose pass:\n%s%s", jobsOut.Stdout, jobsOut.Stderr)
	}
	id := strings.Fields(strings.Split(jobsOut.Stdout, "\n")[2])[0]
	show := h.RunKlaus("jobs", "show", id)
	if !strings.Contains(show.Stdout, "[1] README.md: failed the fact check") {
		t.Errorf("klaus jobs show lacks the proposal:\n%s%s", show.Stdout, show.Stderr)
	}
}

// field is the rest of out's line that starts, after spaces, with prefix,
// without the single quotes of a quoted path.
func field(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), prefix); ok {
			return strings.Trim(rest, "'")
		}
	}
	return ""
}

// TestPreReviewProseNeverBlocks: a writer that fails, hangs past
// timeout_seconds or answers nonsense, or a fact check that fails, is
// reported, offers no patch, and _pre-review passes.
func TestPreReviewProseNeverBlocks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file, content, want string
	}{
		{"fails", "agy.write.fail", "", "The rewrite failed: consult agy: exit status 3: boom"},
		{"hangs", "agy.write.sleep", "", "The rewrite failed: consult agy timed out"},
		{"nonsense", "agy.write", "I rewrote it nicely.", "The rewrite failed: agy's rewrite: no JSON object"},
		{"check fails", "codex.check.fail", "", "The fact check failed: consult codex: exit status 3: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.stubProseBackends("claude", map[string]any{"timeout_seconds": 1})
			h.commitFiles(map[string]string{"README.md": proseReadme}, "add deploy note")
			h.stubAnswer("agy", "write", map[string]any{"edits": []any{map[string]any{
				"file": "README.md", "old": "Deploy to the build host first, but only from main, once the smoke test passes.", "new": "Deploy from main."}}})
			h.writeE2E(tc.file, tc.content) // the nonsense case replaces the answer

			start := time.Now()
			res := h.RunKlaus("_pre-review")
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "All checks passed.") {
				t.Fatalf("_pre-review exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			if !strings.Contains(res.Stdout, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, res.Stdout)
			}
			if strings.Contains(res.Stdout, "git apply") {
				t.Errorf("a pass that didn't finish offered a patch:\n%s", res.Stdout)
			}
			if d := time.Since(start); d > 20*time.Second {
				t.Errorf("_pre-review took %s with a 1s prose timeout", d)
			}
		})
	}
}

// TestPreReviewProseConfig: enabled false turns the pass off; paths makes a
// code file's added lines prose; a change with only code and a short commit
// message is skipped.
func TestPreReviewProseConfig(t *testing.T) {
	t.Parallel()
	code := map[string]string{"main.go": "package main\n\nfunc main() { println(\"Usage: widget [flags]\") }\n"}
	for _, tc := range []struct {
		name   string
		prose  map[string]any
		want   string
		writes bool
	}{
		{"off", map[string]any{"enabled": false}, "Prose rewrite: off (pre_review.prose.enabled).", false},
		{"code only", map[string]any{}, "Skipped: no prose beyond a short commit message.", false},
		{"paths", map[string]any{"paths": []string{"*.go"}}, "No rewrites proposed.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHarness(t)
			h.stubProseBackends("claude", tc.prose)
			files := code
			if tc.name == "off" {
				files = map[string]string{"README.md": proseReadme}
			}
			h.commitFiles(files, "add usage")

			res := h.RunKlaus("_pre-review")
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "All checks passed.") {
				t.Fatalf("_pre-review exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
			}
			if !strings.Contains(res.Stdout, tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, res.Stdout)
			}
			calls := h.readE2E("agy.calls")
			if strings.Contains(calls, "write") != tc.writes {
				t.Errorf("agy calls %q; want a rewrite: %v", calls, tc.writes)
			}
			if tc.writes && !strings.Contains(h.readE2E("agy.write.stdin"), `+func main() { println("Usage: widget [flags]") }`) {
				t.Errorf("the writer didn't get main.go:\n%s", h.readE2E("agy.write.stdin"))
			}
		})
	}
}
