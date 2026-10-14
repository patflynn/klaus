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

// Validate checks the repo and branch before any of them reach git or nix.
func Validate(repo, branch string) error {
	if repo == "" || strings.HasPrefix(repo, "-") {
		return fmt.Errorf("self_update.repo %q is not a git URL", repo)
	}
	if !safeBranch.MatchString(branch) || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") {
		return fmt.Errorf("self_update.branch %q is not a branch name self-update accepts ([A-Za-z0-9._/-])", branch)
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
	sha, err := git.LsRemote(ctx, u.Repo, ref)
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
	st.Previous = u.replaced(st, old)
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
	paths, err := nix.Build(ctx, ref)
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
		return nil
	case st.Current != nil && st.Current.StorePath == old:
		return st.Current
	default:
		// Installed by hand: its name carries the version stamp.
		return &Install{Commit: versionCommit(filepath.Base(old)), StorePath: old}
	}
}

// FlakeRef names the flake's default package at exactly sha on branch.
func FlakeRef(repo, branch, sha string) string {
	u := repo
	switch {
	case strings.Contains(u, "://"):
	case filepath.IsAbs(u):
		u = "file://" + u
	default:
		// scp-like user@host:path
		if host, path, ok := strings.Cut(u, ":"); ok && !strings.Contains(host, "/") {
			u = "ssh://" + host + "/" + strings.TrimPrefix(path, "/")
		}
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return fmt.Sprintf("git+%s%sref=refs/heads/%s&rev=%s#packages.%s.default", u, sep, branch, sha, nix.System())
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
