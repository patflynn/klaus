// Package houserules reads the operator's house rules: one file, kept in a
// registered project's repository, that klaus adds to the context of every
// worker, consult and review. The file is read from origin's default branch,
// never from a working tree.
package houserules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
)

// Rules is the house rules as read at one moment. The zero value is no house
// rules configured.
type Rules struct {
	Source  string // "<repo>:<path>@<commit>", or "<repo>:<path>" when unread
	Digest  string // sha256 of Text; empty when Missing
	Text    string
	Missing bool // configured, but the file could not be read
	Reason  string
}

// Load reads the file cfg names. It never fails: a file that cannot be read
// gives Rules with Missing set and the reason, for the caller to warn once and
// carry on.
func Load(ctx context.Context, cfg *config.HouseRulesConfig, reg *project.Registry) Rules {
	if cfg == nil || cfg.Repo == "" || cfg.Path == "" {
		return Rules{}
	}
	r := Rules{Source: cfg.Repo + ":" + cfg.Path, Missing: true}
	if reg == nil {
		r.Reason = "no project registry"
		return r
	}
	clone, ok := reg.Get(cfg.Repo)
	if !ok {
		r.Reason = fmt.Sprintf("project %q is not registered", cfg.Repo)
		return r
	}
	branch := "main"
	if c, err := config.Load(clone); err == nil && c.DefaultBranch != "" {
		branch = c.DefaultBranch
	}
	if err := git.FetchRemoteBranch(ctx, clone, branch); err != nil {
		r.Reason = fmt.Sprintf("fetching origin/%s: %s", branch, strings.TrimSpace(err.Error()))
		return r
	}
	commit := git.RefCommit(ctx, clone, "refs/remotes/origin/"+branch)
	if commit == "" {
		r.Reason = fmt.Sprintf("origin/%s not found", branch)
		return r
	}
	text, err := git.ShowFile(ctx, clone, commit, cfg.Path)
	if err != nil {
		r.Reason = fmt.Sprintf("origin/%s has no readable %s", branch, cfg.Path)
		return r
	}
	sum := sha256.Sum256([]byte(text))
	return Rules{Source: r.Source + "@" + commit[:min(12, len(commit))], Digest: hex.EncodeToString(sum[:]), Text: text}
}

// Warning is what to tell the operator when the rules could not be read, or
// empty.
func (r Rules) Warning() string {
	if !r.Missing {
		return ""
	}
	return fmt.Sprintf("house rules %s could not be read (%s); continuing without them", r.Source, r.Reason)
}

// Section is the rules as a delimited section for a prompt, or empty when
// there are none. The text is the operator's own, so it is instruction, but
// the section says where it came from.
func (r Rules) Section() string {
	if r.Text == "" {
		return ""
	}
	return fmt.Sprintf("<house-rules source=%q>\n%s\n</house-rules>", r.Source, strings.TrimSpace(r.Text))
}

// Append adds the section after prompt.
func (r Rules) Append(prompt string) string {
	if s := r.Section(); s != "" {
		return strings.TrimRight(prompt, "\n") + "\n\n## House rules\nThe operator's rules for all their projects follow. The project's own instructions add to them.\n\n" + s + "\n"
	}
	return prompt
}

// Record is the rules as run state keeps them, or nil when none are
// configured.
func (r Rules) Record() *run.HouseRules {
	if r.Source == "" {
		return nil
	}
	return &run.HouseRules{Source: r.Source, Digest: r.Digest, Missing: r.Missing}
}
