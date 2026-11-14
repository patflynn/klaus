package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patflynn/klaus/internal/git"
)

func TestPromptDiff(t *testing.T) {
	const truncated = "\n\n[... diff truncated due to size ...]"
	readme := "diff --git a/README.md b/README.md\nindex 1111111..2222222 100644\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n # widget\n+Deploy with care: déjà vu.\n"
	code := "diff --git a/main.go b/main.go\nindex 4444444..5555555 100644\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-package old\n+package main\n"
	keyHeader := "diff --git a/key.enc b/key.enc\nnew file mode 100644\nindex 0000000..3333333\n--- /dev/null\n+++ b/key.enc\n"
	key := keyHeader + "@@ -0,0 +1,2 @@\n+\xff\xfe\x80sealed\n+\x9a\x01\n"
	badName := "diff --git a/caf\xe9.txt b/caf\xe9.txt\nindex 6666666..7777777 100644\n--- a/caf\xe9.txt\n+++ b/caf\xe9.txt\n@@ -1 +1 @@\n-a\n+b\n"
	for _, tc := range []struct {
		name, diff string
		max        int
		want       string
	}{
		{"valid text is untouched", readme + code, 1 << 20, readme + code},
		{"a file that isn't text keeps its header, the others are untouched", readme + key + code, 1 << 20,
			readme + keyHeader + git.NotText + "\n" + code},
		{"a path that isn't UTF-8 is marked, its hunks kept", badName, 1 << 20,
			"diff --git a/caf\uFFFD.txt b/caf\uFFFD.txt\nindex 6666666..7777777 100644\n--- a/caf\uFFFD.txt\n+++ b/caf\uFFFD.txt\n@@ -1 +1 @@\n-a\n+b\n"},
		{"truncation inside a multi-byte character", "+déjà", 3, "+d" + truncated},
		{"truncation after sanitizing", key, len(keyHeader) + 4, keyHeader + git.NotText[:4] + truncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := promptDiff(tc.diff, tc.max)
			if !utf8.ValidString(got) {
				t.Errorf("promptDiff returned invalid UTF-8: %q", got)
			}
			if got != tc.want {
				t.Errorf("promptDiff:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A changed file git treats as text but that isn't UTF-8 reaches the
// reviewer CLI named, its content left out, and the prompt valid UTF-8.
func TestReviewDiffSendsValidUTF8(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("main.go", "package old\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	run("checkout", "-qb", "work")
	write("main.go", "package main\n")
	write("key.enc", "\xff\xfe\x80sealed\n\x9a\x01\n") // no NUL byte, so git diffs it as text
	run("add", "-A")
	run("commit", "-qm", "change")

	bin := t.TempDir()
	stdin := filepath.Join(bin, "stdin")
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\ncat > '"+stdin+"'\necho '{\"findings\":[],\"summary\":\"clean\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	if _, err := ReviewDiff(dir, ReviewConfig{Backend: "claude"}, "main"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(data)
	if !utf8.ValidString(prompt) {
		t.Fatalf("the reviewer got invalid UTF-8:\n%q", prompt)
	}
	for _, want := range []string{"+++ b/key.enc\n" + git.NotText + "\n", "+package main"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the reviewer's prompt lacks %q:\n%s", want, prompt)
		}
	}
}
