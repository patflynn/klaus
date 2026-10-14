// Package selfupdate brings the installed klaus up to the tip of a configured
// branch. It resolves the branch with git ls-remote, builds exactly that
// commit with nix, smoke-tests the build, and installs it into the user's
// default nix profile in place of the profile element named klaus. Every
// step prints one short line, so a journal of timer runs reads as a history.
package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/git"
	"github.com/patflynn/klaus/internal/nix"
)

// Element is the nix profile element self-update replaces. The profile
// names an element after its package, so a klaus store path installs as it.
const Element = "klaus"

const smokeTimeout = 2 * time.Minute

// State is what self-update.json records between runs.
type State struct {
	Current  *Install `json:"current,omitempty"`  // the klaus self-update last put in the profile
	Previous *Install `json:"previous,omitempty"` // the one it replaced: --rollback reinstalls it
	Failed   *Failure `json:"failed,omitempty"`   // the last commit that failed to build or test
	Pinned   bool     `json:"pinned,omitempty"`   // --pin: leave Current alone until --unpin
	// UpdatedAt is when a run last changed this state.
	UpdatedAt time.Time `json:"updated_at"`
}

// Install is a klaus build in the profile.
type Install struct {
	Commit    string    `json:"commit,omitempty"` // full SHA; a short one when read from a store path's name
	StorePath string    `json:"store_path"`
	At        time.Time `json:"at"` // when it went into the profile
}

// Failure is a commit that failed to build or pass the smoke test.
type Failure struct {
	Commit string    `json:"commit"`
	Error  string    `json:"error"`
	At     time.Time `json:"at"`
}

// Updater runs self-update against one repo and branch.
type Updater struct {
	Repo    string    // git URL (or absolute path) of the klaus repository
	Branch  string    // the branch to install, e.g. main
	Dir     string    // state directory, holding the lock and self-update.json
	Version string    // the running klaus's version, e.g. 0.4.1-e949dba
	Out     io.Writer // where each step's line goes
	Verbose bool      // also report runs that find klaus up to date
}

// DefaultDir returns $XDG_STATE_HOME/klaus, or ~/.local/state/klaus.
func DefaultDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "klaus"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "klaus"), nil
}

// StatePath is the state file in dir.
func StatePath(dir string) string { return filepath.Join(dir, "self-update.json") }

var safeBranch = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// gitEnv restricts every git self-update starts, directly or through nix,
// to the transports Validate accepts, and keeps it from prompting.
var gitEnv = []string{"GIT_ALLOW_PROTOCOL=https:ssh:git:file", "GIT_TERMINAL_PROMPT=0"}

// scpLike is user@host:path, git's shorthand for an ssh URL.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:.`)

// Validate checks the repo and branch before any of them reach git or nix.
// The repo must be an https://, ssh://, git:// or file:// URL, an absolute
// path, or scp-like user@host:path: no other git transport (ext::, fd::)
// and only characters nix's flake-ref parser leaves as they are (urlSafe).
func Validate(repo, branch string) error {
	if err := validRepo(repo); err != nil {
		return fmt.Errorf("self_update.repo %q is not a repo self-update accepts: %v; use an https://, ssh://, git:// or file:// URL, an absolute path, or user@host:path", repo, err)
	}
	if !safeBranch.MatchString(branch) || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
		return fmt.Errorf("self_update.branch %q is not a branch name self-update accepts ([A-Za-z0-9._/-])", branch)
	}
	return nil
}

func validRepo(repo string) error {
	switch {
	case repo == "":
		return errors.New("it is empty")
	case strings.HasPrefix(repo, "-"):
		return errors.New("it starts with -")
	}
	if scheme, _, ok := strings.Cut(repo, "://"); ok {
		u, err := url.Parse(repo)
		if err != nil {
			return err
		}
		// An IPv6 host's brackets pass through nix unchanged.
		if err := urlSafe(strings.Replace(repo, u.Host, strings.NewReplacer("[", "", "]", "").Replace(u.Host), 1)); err != nil {
			return err
		}
		switch scheme {
		case "https", "ssh", "git":
			if u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") {
				return errors.New("it names no host")
			}
		case "file":
			if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
				return errors.New("a file:// URL must be file:///<absolute path>")
			}
		default:
			return fmt.Errorf("transport %s:// is not allowed", scheme)
		}
		return nil
	}
	if err := urlSafe(repo); err != nil {
		return err
	}
	switch {
	case filepath.IsAbs(repo):
		return nil
	case strings.Contains(repo, "::"):
		return errors.New("transport-style <transport>::<address> is not allowed")
	case scpLike.MatchString(repo):
		return nil
	}
	return errors.New("it is not a URL, an absolute path or user@host:path")
}

// urlSafe refuses any character outside [A-Za-z0-9._~/:@-]: nix's flake-ref
// parser keeps exactly these as they are (checked against Nix 2.34 with
// builtins.parseFlakeRef and flakeRefToString), and re-encodes or rejects
// the rest (+ becomes %2B), so with them git ls-remote and nix build could
// name different URLs. ? and # it would read as flake-ref syntax.
func urlSafe(s string) error {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._~/:@-", r):
		default:
			return fmt.Errorf("it has %q; only letters, digits and ._~/:@- are allowed, which nix leaves as they are", r)
		}
	}
	return nil
}

// Update installs the branch's tip unless it is installed already, failed
// before (unless retry), or the install is pinned. A run that finds another
// in progress leaves it to finish and returns nil.
func (u *Updater) Update(ctx context.Context, retry bool) error {
	if err := Validate(u.Repo, u.Branch); err != nil {
		return err
	}
	unlock, err := u.lock(false)
	if errors.Is(err, errBusy) {
		u.say("another self-update is in progress; leaving it to finish")
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	st, err := u.load()
	if err != nil {
		return err
	}
	if st.Pinned {
		u.say("pinned at %s: leaving it alone until klaus self-update --unpin", st.Current.label())
		return nil
	}

	ref := "refs/heads/" + u.Branch
	sha, err := git.LsRemote(ctx, RepoURL(u.Repo), ref, gitEnv...)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", u.Branch, err)
	}
	if !isCommit(sha) {
		return fmt.Errorf("%s has no branch %s", u.Repo, u.Branch)
	}
	short := sha[:7]
	if u.upToDate(st, sha) && u.inProfile(ctx, st.Current) {
		if u.Verbose {
			u.say("%s is at %s: klaus is up to date", u.Branch, short)
		}
		return nil
	}
	if f := st.Failed; f != nil && f.Commit == sha && !retry {
		u.say("skipping %s: it failed at %s: %s (it is retried when %s moves, or with --retry)",
			short, f.At.Local().Format(time.DateTime), f.Error, u.Branch)
		return nil
	}
	u.say("%s is at %s (installed: %s)", u.Branch, short, u.installedLabel(st))

	out, err := u.build(ctx, sha)
	if err == nil {
		err = u.smoke(ctx, out, sha)
	}
	if err != nil {
		st.Failed = &Failure{Commit: sha, Error: err.Error(), At: u.clock()}
		if serr := u.save(st); serr != nil {
			return fmt.Errorf("%s: %w (and recording the failure: %v)", short, err, serr)
		}
		u.say("%s failed; nothing installed", short)
		return fmt.Errorf("%s: %w", short, err)
	}

	old, err := u.replace(ctx, out)
	if err != nil {
		return err
	}
	if old != "" || st.Current == nil || st.Current.Commit != sha {
		st.Previous = u.replaced(st, old)
	} // else reinstalling a klaus removed by hand keeps the rollback target
	st.Current = &Install{Commit: sha, StorePath: out, At: u.clock()}
	st.Failed = nil
	if err := u.save(st); err != nil {
		return err
	}
	u.say("installed %s into the nix profile, replacing %s", short, st.Previous.label())
	return nil
}

// Rollback swaps the current install for the previous one, so a second
// rollback returns to where the first started.
func (u *Updater) Rollback(ctx context.Context) error {
	unlock, err := u.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := u.load()
	if err != nil {
		return err
	}
	prev := st.Previous
	if prev == nil || prev.StorePath == "" {
		return errors.New("nothing to roll back to: self-update has not replaced a klaus yet")
	}
	if _, err := os.Stat(prev.StorePath); err != nil {
		return fmt.Errorf("the previous klaus is gone from the store (garbage-collected?): %w", err)
	}
	old, err := u.replace(ctx, prev.StorePath)
	if err != nil {
		return err
	}
	st.Previous = u.replaced(st, old)
	st.Current = &Install{Commit: prev.Commit, StorePath: prev.StorePath, At: u.clock()}
	if err := u.save(st); err != nil {
		return err
	}
	u.say("rolled back to %s; --rollback again returns to %s", st.Current.label(), st.Previous.label())
	if !st.Pinned {
		u.say("not pinned: the next run installs the tip of %s unless it is this one; klaus self-update --pin keeps this one", u.Branch)
	}
	return nil
}

// Pin sets or clears the pin. While pinned, Update leaves the install alone.
func (u *Updater) Pin(pinned bool) error {
	unlock, err := u.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := u.load()
	if err != nil {
		return err
	}
	st.Pinned = pinned
	if err := u.save(st); err != nil {
		return err
	}
	if pinned {
		u.say("pinned at %s: self-update leaves it alone until klaus self-update --unpin", st.Current.label())
	} else {
		u.say("unpinned: the next run installs the tip of %s if it is not installed", u.Branch)
	}
	return nil
}

// upToDate reports whether the installed klaus was built from sha. Once
// self-update has installed a klaus, the state file says which; before
// that, the running klaus's version stamp does.
func (u *Updater) upToDate(st State, sha string) bool {
	if st.Current != nil {
		return st.Current.Commit == sha
	}
	c := versionCommit(u.Version)
	return c != "" && strings.HasPrefix(sha, c)
}

// inProfile reports whether the profile's klaus element still holds inst,
// so a klaus removed or replaced by hand gets reinstalled. With no install
// recorded, or a profile nix can't list, it trusts the state.
func (u *Updater) inProfile(ctx context.Context, inst *Install) bool {
	if inst == nil {
		return true
	}
	elems, err := nix.ProfileElements(ctx)
	if err != nil {
		return true
	}
	if slices.Contains(elems[Element], inst.StorePath) {
		return true
	}
	u.say("the nix profile's klaus is no longer %s; reinstalling it", inst.label())
	return false
}

func (u *Updater) installedLabel(st State) string {
	if st.Current != nil {
		return st.Current.label()
	}
	return "klaus " + u.Version
}

// build builds the flake's default package at exactly sha.
func (u *Updater) build(ctx context.Context, sha string) (string, error) {
	ref := FlakeRef(u.Repo, u.Branch, sha)
	u.say("building %s", ref)
	paths, err := nix.Build(ctx, ref, gitEnv...)
	if err != nil {
		return "", err
	}
	out := paths[0]
	u.say("built %s", out)
	return out, nil
}

// smoke runs the build's klaus: --version must report sha's short form, and
// status must exit 0, which shows the build reads the existing run state.
func (u *Updater) smoke(ctx context.Context, out, sha string) error {
	bin := filepath.Join(out, "bin", "klaus")
	v, err := runBin(ctx, bin, "--version")
	if err != nil {
		return fmt.Errorf("smoke test: klaus --version: %w", err)
	}
	v = firstLine(v)
	if c := versionCommit(lastField(v)); len(c) < 7 || !strings.HasPrefix(sha, c) {
		return fmt.Errorf("smoke test: klaus --version reports %q, not commit %s", v, sha[:7])
	}
	if _, err := runBin(ctx, bin, "status"); err != nil {
		return fmt.Errorf("smoke test: klaus status: %w", err)
	}
	u.say("smoke test passed: %s; klaus status exits 0", v)
	return nil
}

// replace puts storePath in the profile in place of the klaus element and
// returns the store path that element had, "" if the profile had none.
// Other elements are left alone. If adding fails, it puts the old one back.
func (u *Updater) replace(ctx context.Context, storePath string) (string, error) {
	elems, err := nix.ProfileElements(ctx)
	if err != nil {
		return "", err
	}
	var old string
	if paths, ok := elems[Element]; ok {
		if len(paths) > 0 {
			old = paths[0]
		}
		if err := nix.ProfileRemove(ctx, Element); err != nil {
			return "", err
		}
	}
	if err := nix.ProfileAdd(ctx, storePath); err != nil {
		if old != "" {
			// Put it back even if ctx is what ended the add.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if rerr := nix.ProfileAdd(rctx, old); rerr != nil {
				return "", fmt.Errorf("%w (and putting %s back: %v)", err, old, rerr)
			}
		}
		return "", err
	}
	return old, nil
}

// replaced describes the klaus that replace took out of the profile.
func (u *Updater) replaced(st State, old string) *Install {
	switch {
	case old == "":
		// Removed from the profile by hand: still the one to roll back to.
		return st.Current
	case st.Current != nil && st.Current.StorePath == old:
		return st.Current
	default:
		// Installed by hand: its name carries the version stamp.
		return &Install{Commit: versionCommit(filepath.Base(old)), StorePath: old}
	}
}

// RepoURL is the one URL self-update reads repo by, for git ls-remote and
// for nix alike, so both read the same repository. A path becomes a
// file:// URL, and user@host:path becomes ssh://user@host/path: a relative
// path counts from the server's root, as forges read it, so a path in your
// home on a plain server is written user@host:~/path. repo must have passed
// Validate, so it needs no escaping: it has no %, ?, # or whitespace.
func RepoURL(repo string) string {
	switch {
	case strings.Contains(repo, "://"):
		return repo
	case filepath.IsAbs(repo):
		return "file://" + repo
	}
	host, path, _ := strings.Cut(repo, ":")
	return "ssh://" + host + "/" + strings.TrimPrefix(path, "/")
}

// FlakeRef names the flake's default package at exactly sha on branch of
// repo, read by RepoURL.
func FlakeRef(repo, branch, sha string) string {
	return fmt.Sprintf("git+%s?ref=refs/heads/%s&rev=%s#packages.%s.default", RepoURL(repo), branch, sha, nix.System())
}

var errBusy = errors.New("another self-update is in progress")

// lock takes the state directory's lock. Without wait it returns errBusy
// when another run holds it; with wait it says so and waits.
func (u *Updater) lock(wait bool) (func(), error) {
	if err := os.MkdirAll(u.Dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(u.Dir, "self-update.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		if !wait {
			f.Close()
			return nil, errBusy
		}
		u.say("waiting for the self-update in progress to finish")
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil // close drops the flock
}

// Load reads the state file in dir; a missing file is the zero State.
func Load(dir string) (State, error) {
	var st State
	data, err := os.ReadFile(StatePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parsing %s: %w", StatePath(dir), err)
	}
	return st, nil
}

func (u *Updater) load() (State, error) { return Load(u.Dir) }

// save writes the state file through a rename, so a reader never sees half.
func (u *Updater) save(st State) error {
	st.UpdatedAt = u.clock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(u.Dir, "self-update-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), StatePath(u.Dir))
}

func (u *Updater) say(format string, args ...any) {
	if u.Out != nil {
		fmt.Fprintf(u.Out, format+"\n", args...)
	}
}

func (u *Updater) clock() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (i *Install) label() string {
	switch {
	case i == nil:
		return "nothing (the system klaus)"
	case i.Commit != "":
		return shortSHA(i.Commit)
	}
	return i.StorePath
}

// runBin runs a built klaus and returns its stdout; an error carries its
// last line of output.
func runBin(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if line := nix.ErrorLine(stderr.String() + "\n" + stdout.String()); line != "" {
			return "", fmt.Errorf("%w: %s", err, line)
		}
		return "", err
	}
	return stdout.String(), nil
}

var hexRe = regexp.MustCompile(`^[0-9a-f]+$`)

func isCommit(s string) bool { return len(s) >= 40 && hexRe.MatchString(s) }

// versionCommit returns the commit stamped at the end of a klaus version or
// store path name (0.4.1-e949dba gives e949dba), or "" if there is none.
func versionCommit(v string) string {
	i := strings.LastIndex(v, "-")
	if i < 0 {
		return ""
	}
	c := v[i+1:]
	if len(c) < 7 || !hexRe.MatchString(c) {
		return ""
	}
	return c
}

func shortSHA(s string) string { return s[:min(len(s), 7)] }

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

func lastField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}
