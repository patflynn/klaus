// Package prose is _pre-review's prose pass: a model of another family
// rewrites the prose a change adds, a third fact-checks the rewrite against
// the change, and the author decides what to apply.
package prose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/consult"
)

// CommitFile is the file name edits to the head commit's message use.
const CommitFile = "COMMIT_EDITMSG"

// shortMessage is the length under which a commit message alone isn't worth
// a rewrite.
const shortMessage = 200

// Options configure one pass.
type Options struct {
	Dir, Base string // the worktree, and the ref the change is diffed against
	Author    backend.Kind
	Order     []string // writer preference
	Patterns  []string // more prose paths, beyond Markdown and READMEs
	MaxInput  int      // bytes of diff sent
	Timeout   time.Duration
	// HouseRules are the operator's house rules, added to both models'
	// context beside the repository's own writing rules.
	HouseRules string
	// Defaults gives a backend's model and effort.
	Defaults func(backend.Kind) (model, effort string)
	// Started, if set, is called when the writer is about to be asked.
	Started func(*Result)
}

// Claim is one ledger entry: a claim a passage makes and the conditions it
// holds under.
type Claim struct {
	ID         string   `json:"id"`
	Where      string   `json:"where"`
	Claim      string   `json:"claim"`
	Conditions []string `json:"conditions,omitempty"`
}

// Edit replaces Old, text the change added to File, with New.
type Edit struct {
	File   string   `json:"file"`
	Old    string   `json:"old"`
	New    string   `json:"new"`
	Claims []string `json:"claims,omitempty"`
	// Skipped says why klaus left the edit out of the patch, unchecked.
	Skipped string `json:"skipped,omitempty"`
	// Outcome is applied or rejected, judged from the worktree when the run
	// finalizes.
	Outcome string `json:"outcome,omitempty"`
}

// Failure is a fact-check finding against edit number Edit (1-based).
type Failure struct {
	Edit    int    `json:"edit"`
	Claim   string `json:"claim,omitempty"`
	Problem string `json:"problem"`
}

// Result is what a pass found; it is also the job record's prose.
type Result struct {
	Skipped      string    `json:"skipped,omitempty"` // why no rewrite was asked for
	Files        []string  `json:"files,omitempty"`
	Omitted      []string  `json:"omitted,omitempty"` // prose files over the input limit
	Writer       string    `json:"writer,omitempty"`
	WriterModel  string    `json:"writer_model,omitempty"`
	Checker      string    `json:"checker,omitempty"`
	CheckerModel string    `json:"checker_model,omitempty"`
	Error        string    `json:"error,omitempty"`       // the rewrite failed
	CheckError   string    `json:"check_error,omitempty"` // the fact check failed
	Ledger       []Claim   `json:"ledger,omitempty"`
	Edits        []Edit    `json:"edits,omitempty"`
	Failures     []Failure `json:"failures,omitempty"`
	Patch        string    `json:"-"` // the edits that passed, as a diff git apply takes
	Message      string    `json:"-"` // the commit message with its passing edits
	PatchFile    string    `json:"patch_file,omitempty"`
	MessageFile  string    `json:"message_file,omitempty"`
}

// Failed reports whether the fact check failed edit i (0-based). A failure
// that names no edit fails them all.
func (r *Result) Failed(i int) bool {
	for _, f := range r.Failures {
		if f.Edit == i+1 || f.Edit < 1 || f.Edit > len(r.Edits) {
			return true
		}
	}
	return false
}

// Passed reports whether edit i passed the fact check, so goes in the patch.
func (r *Result) Passed(i int) bool {
	return r.Edits[i].Skipped == "" && r.CheckError == "" && !r.Failed(i)
}

// input is the prose a change adds.
type input struct {
	diff    string                  // prose hunks with context
	added   map[string]map[int]bool // file → the line numbers it adds
	message string                  // head commit message; "" when the change has no commit
}

// Choose picks the writer, the first installed family in order that isn't
// the author's, and the checker, the next one, else the author's. ok is
// false when there is no writer.
func Choose(order []string, author backend.Kind, installed func(string) bool) (writer, checker backend.Kind, ok bool) {
	var others []backend.Kind
	for _, name := range order {
		k, err := backend.Parse(name)
		if err != nil || name == "" || k == author || contains(others, k) || !installed(name) {
			continue
		}
		others = append(others, k)
	}
	switch len(others) {
	case 0:
		return "", "", false
	case 1:
		return others[0], author, true
	}
	return others[0], others[1], true
}

func contains(ks []backend.Kind, k backend.Kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// Run runs a pass. Its failures are in the result, never returned: the pass
// must not stop _pre-review.
func Run(ctx context.Context, o Options) *Result {
	r := &Result{}
	in, err := collect(ctx, o, r)
	if err != nil {
		r.Error = "reading the change: " + err.Error()
		return r
	}
	if len(r.Files) == 0 && len(strings.TrimSpace(in.message)) < shortMessage {
		r.Skipped = "no prose beyond a short commit message"
		return r
	}
	writer, checker, ok := Choose(o.Order, o.Author, func(name string) bool { _, err := consult.LookupBinary(name); return err == nil })
	if !ok {
		r.Skipped = "no model family other than the author's (" + string(o.Author) + ") is installed"
		return r
	}
	r.Writer, r.Checker = string(writer), string(checker)
	var wEffort, cEffort string
	if o.Defaults != nil {
		r.WriterModel, wEffort = o.Defaults(writer)
		r.CheckerModel, cEffort = o.Defaults(checker)
	}

	if o.Started != nil {
		o.Started(r)
	}
	answer, err := ask(ctx, o, writer, r.WriterModel, wEffort, writerRole, rewritePrompt(o.Dir, in))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	var p struct {
		Ledger []Claim `json:"ledger"`
		Edits  []Edit  `json:"edits"`
	}
	if err := decodeJSON(answer, "edits", &p); err != nil {
		r.Error = fmt.Sprintf("%s's rewrite: %v", writer, err)
		return r
	}
	r.Ledger, r.Edits = p.Ledger, p.Edits
	for i := range r.Edits {
		r.Edits[i].Outcome = ""
		r.Edits[i].Skipped = validate(o.Dir, in, r.Edits[i])
	}

	if checkable := checkPrompt(in, r); checkable != "" {
		answer, err := ask(ctx, o, checker, r.CheckerModel, cEffort, checkerRole, checkable)
		var c struct {
			Failures []Failure `json:"failures"`
		}
		if err == nil {
			err = decodeJSON(answer, "failures", &c)
		}
		if err != nil {
			r.CheckError = err.Error()
		}
		r.Failures = c.Failures
	}
	r.Patch, r.Message = build(o.Dir, in, r)
	return r
}

// collect reads the change's prose: the hunks of the prose files it changes,
// as many whole files as fit in o.MaxInput, and its head commit message.
func collect(ctx context.Context, o Options, r *Result) (*input, error) {
	in := &input{added: map[string]map[int]bool{}}
	names, err := git(ctx, o.Dir, "diff", "--name-only", "-z", "--diff-filter=AM", "--no-renames", o.Base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var diff strings.Builder
	for _, name := range strings.Split(names, "\x00") {
		if name == "" || !IsProse(name, o.Patterns) {
			continue
		}
		hunks, err := git(ctx, o.Dir, "diff", "-U8", "--no-color", "--no-ext-diff", o.Base+"...HEAD", "--", ":(literal)"+name)
		if err != nil {
			return nil, err
		}
		if diff.Len()+len(hunks) > o.MaxInput {
			r.Omitted = append(r.Omitted, name)
			continue
		}
		diff.WriteString(hunks)
		r.Files = append(r.Files, name)
		in.added[name] = addedLines(hunks)
	}
	in.diff = diff.String()
	if n, err := git(ctx, o.Dir, "rev-list", "--count", o.Base+"..HEAD"); err == nil && strings.TrimSpace(n) != "0" {
		if msg, err := git(ctx, o.Dir, "log", "-1", "--format=%B"); err == nil && len(msg) > o.MaxInput {
			r.Omitted = append(r.Omitted, "commit message")
		} else if err == nil {
			in.message = strings.TrimSpace(msg)
			in.added[CommitFile] = map[int]bool{}
			for n := range strings.Split(in.message, "\n") {
				in.added[CommitFile][n+1] = true
			}
		}
	}
	return in, nil
}

// IsProse reports whether a changed file's added lines are prose: Markdown,
// a README, or a match for one of patterns. A pattern with no slash matches
// a file's name in any directory; a pattern ending in /** matches everything
// under a directory.
func IsProse(name string, patterns []string) bool {
	base := path.Base(name)
	ext := strings.ToLower(path.Ext(name))
	if ext == ".md" || ext == ".markdown" || strings.HasPrefix(strings.ToUpper(base), "README") {
		return true
	}
	for _, p := range patterns {
		if dir, ok := strings.CutSuffix(p, "/**"); ok && strings.HasPrefix(name, dir+"/") {
			return true
		}
		target := name
		if !strings.Contains(p, "/") {
			target = base
		}
		if ok, _ := path.Match(p, target); ok {
			return true
		}
	}
	return false
}

var hunkStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

// addedLines is the line numbers a file's diff adds.
func addedLines(diff string) map[int]bool {
	added := map[int]bool{}
	line := 0 // the next new-side line; 0 in the header
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := hunkStart.FindStringSubmatch(l); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
		case line == 0, strings.HasPrefix(l, "-"), strings.HasPrefix(l, `\`):
		case strings.HasPrefix(l, "+"):
			added[line] = true
			line++
		default:
			line++
		}
	}
	return added
}

// validate says why an edit can't go in the patch, or "".
func validate(dir string, in *input, e Edit) string {
	added, ok := in.added[e.File]
	switch {
	case !ok:
		return "not prose this change adds"
	case e.Old == "" || e.Old == e.New:
		return "no change"
	}
	text, err := current(dir, in, e.File)
	if err != nil {
		return err.Error()
	}
	if strings.Count(text, e.Old) != 1 {
		return "its old text isn't in the file exactly once"
	}
	first := strings.Count(text[:strings.Index(text, e.Old)], "\n") + 1
	for n, l := range strings.Split(e.Old, "\n") {
		if strings.TrimSpace(l) != "" && !added[first+n] {
			return "it rewrites text the change didn't add"
		}
	}
	return ""
}

// current is a file's text in the worktree, or the commit message.
func current(dir string, in *input, file string) (string, error) {
	if file == CommitFile {
		return in.message, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
	return string(data), err
}

// build applies the passing edits to the original text: the files' as a
// patch, the commit message's to the message. An edit that overlaps an
// earlier one in its file is marked skipped.
func build(dir string, in *input, r *Result) (patch, message string) {
	type span struct{ edit, start, end int }
	spans := map[string][]span{}
	origs := map[string]string{}
	var order []string
	for i, e := range r.Edits {
		if !r.Passed(i) {
			continue
		}
		if _, ok := origs[e.File]; !ok {
			text, err := current(dir, in, e.File)
			if err != nil {
				r.Edits[i].Skipped = err.Error()
				continue
			}
			origs[e.File] = text
			order = append(order, e.File)
		}
		at := strings.Index(origs[e.File], e.Old)
		spans[e.File] = append(spans[e.File], span{i, at, at + len(e.Old)})
	}
	var b strings.Builder
	for _, file := range order {
		ss, orig := spans[file], origs[file]
		sort.Slice(ss, func(a, b int) bool { return ss[a].start < ss[b].start })
		var text strings.Builder
		pos := 0
		for _, sp := range ss {
			if sp.start < pos {
				r.Edits[sp.edit].Skipped = "an earlier rewrite overlaps it"
				continue
			}
			text.WriteString(orig[pos:sp.start] + r.Edits[sp.edit].New)
			pos = sp.end
		}
		text.WriteString(orig[pos:])
		if file == CommitFile {
			message = text.String() + "\n"
			continue
		}
		d, err := diffFile(file, orig, text.String())
		if err != nil {
			for _, sp := range ss {
				r.Edits[sp.edit].Skipped = "building the patch: " + err.Error()
			}
			continue
		}
		b.WriteString(d)
	}
	return b.String(), message
}

// diffFile is a git-style diff of file from old to new, for git apply.
func diffFile(file, old, new string) (string, error) {
	tmp, err := os.MkdirTemp("", "klaus-prose-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for side, text := range map[string]string{"a": old, "b": new} {
		p := filepath.Join(tmp, side, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("git", "diff", "--no-index", "--no-prefix", "--no-color", "--no-ext-diff", "a/"+file, "b/"+file)
	cmd.Dir = tmp
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 { // 1: the files differ
		err = nil
	}
	return string(out), err
}

// Settle records whether each of a pass's edits was applied, judged from
// the worktree dir: applied when the file, or the head commit's message,
// holds its new text and not its old.
func Settle(dir string, r *Result) {
	msg, _ := git(context.Background(), dir, "log", "-1", "--format=%B")
	for i, e := range r.Edits {
		if e.Skipped != "" || e.Outcome != "" {
			continue
		}
		text := msg
		if e.File != CommitFile {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.File)))
			if err != nil {
				continue
			}
			text = string(data)
		}
		applied := strings.Contains(text, strings.TrimSpace(e.New))
		if !strings.Contains(e.New, e.Old) && strings.Contains(text, e.Old) {
			applied = false // the old passage is still there
		}
		r.Edits[i].Outcome = "rejected"
		if applied {
			r.Edits[i].Outcome = "applied"
		}
	}
}

func ask(ctx context.Context, o Options, kind backend.Kind, model, effort, role, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	t := &consult.Thread{Backend: kind, Model: model, Effort: effort, Role: role, Dir: o.Dir, HouseRules: o.HouseRules}
	res, err := consult.Ask(ctx, t, prompt, false, "", io.Discard, io.Discard)
	if err != nil {
		if s := lastLine(res.Stderr); s != "" {
			err = fmt.Errorf("%w: %s", err, s)
		}
		return "", err
	}
	return res.Answer, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

// decodeJSON reads the first JSON object with key in a model's answer,
// which may wrap it in fences or prose.
func decodeJSON(text, key string, v any) error {
	for i := strings.IndexByte(text, '{'); i >= 0; {
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var probe map[string]json.RawMessage
		if dec.Decode(&probe) == nil && probe[key] != nil {
			return json.Unmarshal([]byte(text[i:i+int(dec.InputOffset())]), v)
		}
		next := strings.IndexByte(text[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return fmt.Errorf("no JSON object with %q in the answer", key)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

var writingHeading = regexp.MustCompile(`(?i)^#+ .*(writ|prose|style|doc)`)

// Rules are the sections on writing of the repo's AGENTS.md and CLAUDE.md:
// those whose heading names writing, prose, style or docs.
func Rules(dir string) string {
	var b strings.Builder
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		level := 0 // the heading level of the section being kept; 0 when none
		for _, l := range strings.Split(string(data), "\n") {
			if h := len(l) - len(strings.TrimLeft(l, "#")); h > 0 && strings.HasPrefix(l[h:], " ") {
				switch {
				case writingHeading.MatchString(l) && (level == 0 || h <= level):
					level = h
				case level > 0 && h <= level:
					level = 0
				}
			}
			if level > 0 {
				b.WriteString(l + "\n")
			}
		}
	}
	if b.Len() > 8000 {
		return b.String()[:8000]
	}
	return b.String()
}
