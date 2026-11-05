package houserules

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/project"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// guidanceProject registers a clone whose origin's main holds the house rules
// file; the clone's working tree is then changed to prove it is never read.
func guidanceProject(t *testing.T) (*project.Registry, string) {
	t.Helper()
	root := t.TempDir()
	origin, clone := filepath.Join(root, "origin.git"), filepath.Join(root, "guidance")
	runGit(t, root, "init", "--bare", "-b", "main", origin)
	runGit(t, root, "clone", origin, clone)
	if err := os.MkdirAll(filepath.Join(clone, "guidance"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(clone, "guidance", "house-rules.md")
	if err := os.WriteFile(file, []byte("Write what a reader can't infer.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "rules")
	runGit(t, clone, "push", "origin", "HEAD:main")
	if err := os.WriteFile(file, []byte("uncommitted taste\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := &project.Registry{Projects: map[string]string{}}
	if err := reg.Add("guidance", clone); err != nil {
		t.Fatal(err)
	}
	return reg, clone
}

func TestLoadReadsOriginsDefaultBranchWithDigest(t *testing.T) {
	reg, _ := guidanceProject(t)
	r := Load(context.Background(), &config.HouseRulesConfig{Repo: "guidance", Path: "guidance/house-rules.md"}, reg)
	if r.Missing || r.Text != "Write what a reader can't infer." {
		t.Fatalf("rules = %+v; want origin's committed text, not the working tree's", r)
	}
	if len(r.Digest) != 64 || !strings.HasPrefix(r.Source, "guidance:guidance/house-rules.md@") {
		t.Errorf("source %q, digest %q", r.Source, r.Digest)
	}
	if rec := r.Record(); rec == nil || rec.Digest != r.Digest || rec.Source != r.Source {
		t.Errorf("record = %+v", rec)
	}
}

func TestMissingFileWarnsAndDoesNotBlock(t *testing.T) {
	reg, _ := guidanceProject(t)
	r := Load(context.Background(), &config.HouseRulesConfig{Repo: "guidance", Path: "nope.md"}, reg)
	if !r.Missing || !strings.Contains(r.Warning(), "continuing without them") {
		t.Fatalf("rules = %+v", r)
	}
	if got := r.Append("prompt"); got != "prompt" {
		t.Errorf("a missing file changed the prompt: %q", got)
	}
	if rec := r.Record(); rec == nil || !rec.Missing || rec.Digest != "" {
		t.Errorf("record = %+v; want missing", rec)
	}
	if un := Load(context.Background(), nil, reg); un.Record() != nil || un.Warning() != "" {
		t.Errorf("unset rules = %+v", un)
	}
	if !Load(context.Background(), &config.HouseRulesConfig{Repo: "other", Path: "x"}, reg).Missing {
		t.Error("an unregistered project should be missing")
	}
}

// TestEveryKindOfRunGetsTheRules appends the rules to each rendered worker
// prompt, with and without an overriding .klaus/prompt.md.
func TestEveryKindOfRunGetsTheRules(t *testing.T) {
	custom := t.TempDir()
	if err := os.MkdirAll(filepath.Join(custom, ".klaus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, ".klaus", "prompt.md"), []byte("Custom for {{.RunID}}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Rules{Source: "g:r.md@abc", Text: "TASTE-MARKER"}
	vars := config.PromptVars{RunID: "r", Branch: "b"}
	for name, render := range map[string]func() (string, error){
		"plain":  func() (string, error) { return config.RenderPrompt(t.TempDir(), vars) },
		"custom": func() (string, error) { return config.RenderPrompt(custom, vars) },
		"pr": func() (string, error) {
			return config.RenderPRFixPrompt(t.TempDir(), config.PromptVars{RunID: "r", PR: "7"})
		},
		"pr-fix-custom": func() (string, error) {
			return config.RenderPRFixPrompt(custom, config.PromptVars{RunID: "r", PR: "7"})
		},
	} {
		prompt, err := render()
		if err != nil {
			t.Fatal(err)
		}
		got := r.Append(config.WithWorkerRules(prompt))
		if !strings.Contains(got, "TASTE-MARKER") || strings.Index(got, "TASTE-MARKER") < strings.Index(got, config.WorkerRules) {
			t.Errorf("%s: house rules missing or ahead of WorkerRules:\n%s", name, got)
		}
	}
}
