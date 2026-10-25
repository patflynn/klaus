package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/project"
	"github.com/spf13/cobra"
)

var forbiddenTermsCmd = &cobra.Command{
	Use:   "_forbidden-terms [<base>] [<head>]",
	Short: "Check a range for the repo's forbidden terms",
	Long: `Checks what <base>...<head> adds to the current checkout's repo for the
terms forbidden_terms in ~/.klaus/config.json forbids there, the scan
_pre-review runs before an agent pushes: the lines the range adds, the
names of files it adds or renames, and the messages of the commits in
<base>..<head>. Each term matches case-insensitively as a literal substring;
files under the entry's allow_paths are skipped.

<head> defaults to HEAD and <base> to the default branch (origin's when it
exists), so the range is the change since its merge base. --repo picks the
entry, by registered project name or owner/repo, when the checkout's own
origin or registration doesn't.

Prints file:line: term for each hit (commit <sha>: term for a message) and
exits 1 when there is one; never prints the term list itself.`,
	Hidden: true,
	Args:   cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		repoRef, _ := cmd.Flags().GetString("repo")
		dir, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("finding the checkout: %w", err)
		}
		cfg, err := config.Load(dir)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		out := cmd.OutOrStdout()
		entries := forbiddenTermsFor(cfg, dir, repoRef)
		if len(entries) == 0 {
			fmt.Fprintln(out, "No forbidden terms are configured for this repo.")
			return nil
		}
		base, head := defaultBranchRef(dir, cfg.DefaultBranch), "HEAD"
		if len(args) > 0 {
			base = args[0]
		}
		if len(args) > 1 {
			head = args[1]
		}
		hits, err := scanForbiddenTerms(dir, base, head, entries)
		if err != nil {
			return err
		}
		printForbiddenTerms(out, hits)
		if len(hits) > 0 {
			return errors.New("forbidden terms found")
		}
		return nil
	},
}

// forbiddenHit is one forbidden term in what a change adds: on Line of File,
// in File's name (Line 0), or in the message of Commit.
type forbiddenHit struct {
	File   string
	Line   int
	Commit string
	Term   string
}

func (h forbiddenHit) String() string {
	switch {
	case h.Commit != "":
		return fmt.Sprintf("commit %s: %s", h.Commit, h.Term)
	case h.Line == 0:
		return fmt.Sprintf("%s: %s (file name)", h.File, h.Term)
	}
	return fmt.Sprintf("%s:%d: %s", h.File, h.Line, h.Term)
}

// printForbiddenTerms reports hits the way _pre-review reports its other
// checks. It names only the terms that hit, never the configured list.
func printForbiddenTerms(w io.Writer, hits []forbiddenHit) {
	if len(hits) == 0 {
		fmt.Fprintln(w, "  ✓ no forbidden terms added")
		return
	}
	label := "term"
	if len(hits) > 1 {
		label = "terms"
	}
	fmt.Fprintf(w, "  ✗ %d forbidden %s added — replace each with a neutral name:\n", len(hits), label)
	for _, h := range hits {
		fmt.Fprintf(w, "    %s\n", h)
	}
}

// forbiddenTermsFor returns the forbidden_terms entries for the repo checked
// out in dir, or for repoRef when it is set. A key is a registered project
// name or owner/repo; it names the repo when it is repoRef itself, when it is
// owner/repo and the repo's origin is that GitHub repo, or when it is a
// registered project that shares the repo's git directory or origin. Every
// entry that names the repo applies, each with its own allow_paths.
func forbiddenTermsFor(cfg config.Config, dir, repoRef string) []config.ForbiddenTermsConfig {
	if len(cfg.ForbiddenTerms) == 0 {
		return nil
	}
	reg, _ := project.Load()
	slug, common := repoSlugForDir(dir), gitCommonDir(dir)
	if repoRef != "" {
		slug, common = repoSlug(repoRef), ""
		if reg != nil {
			if p, ok := reg.Get(repoRef); ok {
				slug, common = repoSlugForDir(p), gitCommonDir(p)
			}
		}
	}
	keys := make([]string, 0, len(cfg.ForbiddenTerms))
	for k := range cfg.ForbiddenTerms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var entries []config.ForbiddenTermsConfig
	for _, key := range keys {
		if repoRef != "" && strings.EqualFold(key, repoRef) || forbiddenKeyNames(key, slug, common, reg) {
			entries = append(entries, cfg.ForbiddenTerms[key])
		}
	}
	return entries
}

// forbiddenKeyNames reports whether key names the repo with origin slug and
// git common directory common (either may be "").
func forbiddenKeyNames(key, slug, common string, reg *project.Registry) bool {
	if strings.Contains(key, "/") {
		return slug != "" && strings.EqualFold(repoSlug(key), slug)
	}
	if reg == nil {
		return false
	}
	p, ok := reg.Get(key)
	if !ok {
		return false
	}
	if common != "" && gitCommonDir(p) == common {
		return true
	}
	return slug != "" && strings.EqualFold(repoSlugForDir(p), slug)
}

// gitCommonDir returns the absolute git common directory of the checkout at
// dir, the same for all its worktrees, or "" when dir is not a checkout.
func gitCommonDir(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// defaultBranchRef returns the ref of the default branch to diff a change
// against: origin's, which worktrees start from, when dir has it, else the
// local branch.
func defaultBranchRef(dir, branch string) string {
	if branch == "" {
		branch = "main"
	}
	if git.RefExists(context.Background(), dir, "refs/remotes/origin/"+branch) {
		return "origin/" + branch
	}
	return branch
}

// scanForbiddenTerms returns the forbidden terms of entries in what
// base...head adds in dir: added lines and added or renamed file names,
// outside each entry's allow_paths, and the messages of base..head's
// commits. Deleted lines don't count.
func scanForbiddenTerms(dir, base, head string, entries []config.ForbiddenTermsConfig) ([]forbiddenHit, error) {
	termsFor := func(path string) []string {
		var terms []string
		for _, e := range entries {
			if !allowedPath(path, e.AllowPaths) {
				terms = append(terms, e.Terms...)
			}
		}
		return terms
	}
	var hits []forbiddenHit

	names, err := gitForbiddenScan(dir, "diff", "--name-only", "-z", "--diff-filter=AR", "--no-relative", base+"..."+head)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimRight(names, "\x00"), "\x00") {
		for _, t := range matchTerms(name, termsFor(name)) {
			hits = append(hits, forbiddenHit{File: name, Term: t})
		}
	}

	// --text: a file git takes for binary, or one .gitattributes marks -diff,
	// still has its lines scanned. --no-renames: a file moved out of
	// allow_paths adds all its lines at its new path.
	diff, err := gitForbiddenScan(dir, "diff", "--unified=0", "--text", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--no-relative", "--src-prefix=a/", "--dst-prefix=b/", base+"..."+head)
	if err != nil {
		return nil, err
	}
	for _, l := range addedLines(diff) {
		for _, t := range matchTerms(l.text, termsFor(l.file)) {
			hits = append(hits, forbiddenHit{File: l.file, Line: l.line, Term: t})
		}
	}

	var all []string
	for _, e := range entries {
		all = append(all, e.Terms...)
	}
	log, err := gitForbiddenScan(dir, "log", "-z", "--format=%h%n%B", base+".."+head)
	if err != nil {
		return nil, err
	}
	for _, c := range strings.Split(strings.TrimRight(log, "\x00"), "\x00") {
		sha, msg, _ := strings.Cut(c, "\n")
		for _, t := range matchTerms(msg, all) {
			hits = append(hits, forbiddenHit{Commit: sha, Term: t})
		}
	}
	return hits, nil
}

func gitForbiddenScan(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// matchTerms returns the terms s contains, ignoring case. A term is a
// literal: its dots are dots.
func matchTerms(s string, terms []string) []string {
	var found []string
	lower := strings.ToLower(s)
	for _, t := range terms {
		if strings.TrimSpace(t) != "" && strings.Contains(lower, strings.ToLower(t)) {
			found = append(found, t)
		}
	}
	return found
}

// allowedPath reports whether path is one of allow, or under one that is a
// directory.
func allowedPath(path string, allow []string) bool {
	for _, a := range allow {
		a = strings.TrimSuffix(strings.TrimPrefix(a, "./"), "/")
		if a != "" && (path == a || strings.HasPrefix(path, a+"/")) {
			return true
		}
	}
	return false
}

type addedLine struct {
	file string
	line int
	text string
}

var zeroContextHunk = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// addedLines returns the lines a unified diff adds, with their new-side line
// numbers. Hunk counts, not prefixes, mark a hunk's end, so an added line
// that begins "++ " isn't taken for a file header.
func addedLines(diff string) []addedLine {
	var lines []addedLine
	var file string
	var line, oldLeft, newLeft int
	for _, l := range strings.Split(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(l, "-"):
				oldLeft--
			case strings.HasPrefix(l, "+"):
				if file != "" {
					lines = append(lines, addedLine{file: file, line: line, text: l[1:]})
				}
				line++
				newLeft--
			case strings.HasPrefix(l, `\`): // "\ No newline at end of file"
			default:
				line++
				oldLeft--
				newLeft--
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file = ""
		case strings.HasPrefix(l, "+++ "):
			// git ends an unquoted path that has a space with a tab.
			path := strings.TrimSuffix(strings.TrimPrefix(l, "+++ "), "\t")
			if path == "/dev/null" {
				file = ""
				continue
			}
			if uq, err := strconv.Unquote(path); err == nil {
				path = uq
			}
			file = strings.TrimPrefix(path, "b/")
		default:
			if m := zeroContextHunk.FindStringSubmatch(l); m != nil {
				oldLeft, newLeft = hunkLen(m[1]), hunkLen(m[3])
				line, _ = strconv.Atoi(m[2])
			}
		}
	}
	return lines
}

// hunkLen reads a hunk header's line count, which is 1 when omitted.
func hunkLen(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

func init() {
	forbiddenTermsCmd.Flags().String("repo", "", "Repo whose forbidden_terms entry applies: a registered project name or owner/repo (default: the checkout's)")
	rootCmd.AddCommand(forbiddenTermsCmd)
}
