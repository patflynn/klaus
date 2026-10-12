package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/run"
)

const (
	// remoteTimeout bounds ls-remote and fetch.
	remoteTimeout = time.Minute
	// localTimeout bounds each local git command.
	localTimeout = time.Minute
	// maxLandedScan bounds how many first-parent commits of the default
	// branch the content check compares against.
	maxLandedScan = 500
)

// GitChecker checks integration with git, in each run's own clone
// (run.State.CloneDir). Per repo and per check it takes one snapshot of
// origin with ls-remote, fetches only the default branch and the pushed
// branches that still exist, and then judges each run:
//
//  1. Integrated: the pushed SHA, or the branch's current tip (the operator
//     may have rebased and force-pushed it), is an ancestor of the default
//     branch.
//  2. IntegratedContent: a first-parent commit on the default branch,
//     committed after the run was created, has the same patch-id as the
//     branch's change (its diff from the merge base with the default branch).
//     This catches an integrator that lands a change as one new commit on top
//     of the default branch.
//  3. Gone: the branch is no longer on origin, neither check matched, and
//     the evidence was complete: the pushed SHA is in the clone, shares
//     history with the default branch, and its change was compared against
//     every commit landed since the run was created.
//  4. Awaiting: the branch is still on origin and neither check matched.
//
// A run without a clone, or whose origin can't be listed or fetched, is
// Unknown, and so is a deleted branch whose evidence was incomplete (the
// pushed SHA was pruned, the clone is shallow, or the scan of landed commits
// hit its cap): an incomplete check never says Gone.
//
// Verdicts other than Unknown are cached in memory by what they were computed
// from (the run, its clone, branch, pushed SHA and creation time, and origin's
// default and branch tips), so a check whose tips haven't moved costs one
// ls-remote per repo. A GitChecker is safe for concurrent use.
type GitChecker struct {
	logger *slog.Logger
	// scanLimit bounds the walk of the default branch (maxLandedScan).
	scanLimit int

	mu       sync.Mutex
	verdicts map[cacheKey]Verdict
	patchIDs map[string]string // default-branch commit SHA -> patch-id ("" when it has no diff)
}

// cacheKey is what a verdict was computed from.
type cacheKey struct {
	runID, cloneDir, pushedBranch, pushedSHA, createdAt, defaultTip, branchTip string
}

// NewGitChecker returns a GitChecker that logs incomplete checks at debug
// level to logger (slog.Default when nil).
func NewGitChecker(logger *slog.Logger) *GitChecker {
	if logger == nil {
		logger = slog.Default()
	}
	return &GitChecker{
		logger:    logger,
		scanLimit: maxLandedScan,
		verdicts:  make(map[cacheKey]Verdict),
		patchIDs:  make(map[string]string),
	}
}

// Check returns a verdict for every run that Applies. Runs are grouped by
// clone so each repo is snapshotted once.
func (c *GitChecker) Check(ctx context.Context, runs []*run.State) map[string]Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]Verdict)
	byDir := make(map[string][]*run.State)
	var dirs []string
	for _, s := range runs {
		if !Applies(s) {
			continue
		}
		if s.CloneDir == nil || *s.CloneDir == "" {
			out[s.ID] = Unknown
			c.logger.Debug("integration check: run has no clone", "run", s.ID)
			continue
		}
		dir := *s.CloneDir
		if _, ok := byDir[dir]; !ok {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], s)
	}

	// Keep only what this check used, so the caches don't grow without bound.
	next := &checkCache{verdicts: make(map[cacheKey]Verdict), patchIDs: make(map[string]string)}
	for _, dir := range dirs {
		c.checkRepo(ctx, dir, byDir[dir], out, next)
	}
	c.verdicts, c.patchIDs = next.verdicts, next.patchIDs
	return out
}

// checkCache collects the cache entries a check used.
type checkCache struct {
	verdicts map[cacheKey]Verdict
	patchIDs map[string]string
}

// checkRepo judges the runs whose clone is dir.
func (c *GitChecker) checkRepo(ctx context.Context, dir string, runs []*run.State, out map[string]Verdict, next *checkCache) {
	unknown := func(rs []*run.State, err error) {
		for _, s := range rs {
			out[s.ID] = Unknown
		}
		c.logger.Debug("integration check incomplete", "clone", dir, "runs", len(rs), "err", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		unknown(runs, err)
		return
	}

	snap, err := snapshotOrigin(ctx, dir)
	if err != nil {
		unknown(runs, err)
		return
	}

	keys := make(map[string]cacheKey, len(runs))
	var pending []*run.State
	for _, s := range runs {
		key := cacheKey{s.ID, dir, *s.PushedBranch, *s.PushedSHA, s.CreatedAt, snap.defaultTip, snap.heads[*s.PushedBranch]}
		keys[s.ID] = key
		if v, ok := c.verdicts[key]; ok {
			out[s.ID] = v
			next.verdicts[key] = v
			continue
		}
		pending = append(pending, s)
	}
	if len(pending) == 0 {
		return
	}

	if err := fetchSnapshot(ctx, dir, snap, pending); err != nil {
		unknown(pending, err)
		return
	}

	// The default branch's recent commits are walked at most once per repo,
	// back to the oldest pending run, and only if a content check needs them.
	var landed []landedCommit
	var truncated bool
	var landedErr error
	walked := false
	landedFn := func() ([]landedCommit, bool, error) {
		if !walked {
			walked = true
			landed, truncated, landedErr = c.landedCommits(ctx, dir, snap.defaultTip, oldestCreated(pending), next)
		}
		return landed, truncated, landedErr
	}

	for _, s := range pending {
		v, err := judge(ctx, dir, snap, s, landedFn)
		if err != nil {
			unknown([]*run.State{s}, err)
			continue
		}
		out[s.ID] = v
		// Unknown is never cached: the next check may have what this one
		// lacked.
		if v != Unknown {
			next.verdicts[keys[s.ID]] = v
		}
	}
}

// judge applies the verdicts, in order, to one run. landed returns the
// default branch's recent commits and whether the walk was cut short by the
// cap.
func judge(ctx context.Context, dir string, snap snapshot, s *run.State, landed func() ([]landedCommit, bool, error)) (Verdict, error) {
	pushed := *s.PushedSHA
	tip := snap.heads[*s.PushedBranch]
	candidates := []string{pushed}
	if tip != "" && tip != pushed {
		candidates = append(candidates, tip)
	}
	// A candidate whose object isn't here after the fetch is skipped.
	var local []string
	for _, x := range candidates {
		ok, err := hasObject(ctx, dir, x)
		if err != nil {
			return Unknown, err
		}
		if ok {
			local = append(local, x)
		}
	}

	for _, x := range local {
		ok, err := isAncestor(ctx, dir, x, snap.defaultTip)
		if err != nil {
			return Unknown, err
		}
		if ok {
			return Integrated, nil
		}
	}

	// complete is set once a candidate's change could be compared: it is
	// here and shares history with the default branch. Without that, a
	// deleted branch is Unknown, not Gone.
	complete := false
	want := make(map[string]bool)
	for _, x := range local {
		id, ok, err := changePatchID(ctx, dir, snap.defaultTip, x)
		if err != nil {
			return Unknown, err
		}
		if !ok {
			continue
		}
		complete = true
		if id != "" {
			want[id] = true
		}
	}
	if len(want) > 0 {
		commits, truncated, err := landed()
		if err != nil {
			return Unknown, err
		}
		since := createdAt(s)
		for _, lc := range commits {
			if lc.when.Before(since) {
				continue
			}
			if lc.patchID != "" && want[lc.patchID] {
				return IntegratedContent, nil
			}
		}
		// A walk cut short by the cap covered this run only if it reached a
		// commit from before the run was created.
		if truncated && (since.IsZero() || len(commits) == 0 || !commits[len(commits)-1].when.Before(since)) {
			complete = false
		}
	}

	switch {
	case tip != "":
		return Awaiting, nil
	case complete:
		return Gone, nil
	default:
		return Unknown, nil
	}
}

// snapshot is origin's default branch and branch tips at one moment.
type snapshot struct {
	defaultBranch string
	defaultTip    string
	heads         map[string]string // branch name -> tip SHA
}

// snapshotOrigin lists origin's HEAD and branches with one ls-remote.
func snapshotOrigin(ctx context.Context, dir string) (snapshot, error) {
	out, err := gitRun(ctx, dir, nil, remoteTimeout, "ls-remote", "--symref", "origin", "HEAD", "refs/heads/*")
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{heads: make(map[string]string)}
	for _, line := range strings.Split(string(out), "\n") {
		left, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if target, isSym := strings.CutPrefix(left, "ref: "); isSym {
			if ref == "HEAD" {
				snap.defaultBranch = strings.TrimPrefix(target, "refs/heads/")
			}
			continue
		}
		if name, isHead := strings.CutPrefix(ref, "refs/heads/"); isHead {
			snap.heads[name] = left
		}
	}
	if snap.defaultBranch == "" {
		// Some servers don't advertise HEAD's target; fall back to the
		// clone's record of it.
		if ref, err := gitRun(ctx, dir, nil, localTimeout, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
			snap.defaultBranch = strings.TrimPrefix(strings.TrimSpace(string(ref)), "refs/remotes/origin/")
		}
	}
	if snap.defaultBranch == "" {
		return snapshot{}, fmt.Errorf("origin's default branch is unknown")
	}
	snap.defaultTip = snap.heads[snap.defaultBranch]
	if snap.defaultTip == "" {
		return snapshot{}, fmt.Errorf("origin has no branch %q", snap.defaultBranch)
	}
	return snap, nil
}

// fetchSnapshot fetches the default branch and the runs' branches that are
// still on origin, with explicit refspecs.
func fetchSnapshot(ctx context.Context, dir string, snap snapshot, runs []*run.State) error {
	refspec := func(b string) string { return "+refs/heads/" + b + ":refs/remotes/origin/" + b }
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		"origin", refspec(snap.defaultBranch)}
	seen := map[string]bool{snap.defaultBranch: true}
	for _, s := range runs {
		b := *s.PushedBranch
		if snap.heads[b] == "" || seen[b] {
			continue
		}
		seen[b] = true
		args = append(args, refspec(b))
	}
	_, err := gitRun(ctx, dir, nil, remoteTimeout, args...)
	return err
}

// landedCommit is a first-parent commit on the default branch.
type landedCommit struct {
	sha     string
	when    time.Time // committer date
	patchID string    // "" for a merge or a commit with no diff
}

// landedCommits lists up to c.scanLimit first-parent commits of tip
// committed since since (all of them when since is zero), newest first, with
// their patch-ids. truncated reports that there were more: the walk stopped
// at the cap, not at since. Patch-ids already computed are reused: commits
// don't change.
func (c *GitChecker) landedCommits(ctx context.Context, dir, tip string, since time.Time, next *checkCache) (commits []landedCommit, truncated bool, err error) {
	// One commit past the cap tells a full walk from a cut-short one.
	args := []string{"log", "--first-parent", "--format=%H %ct", "-n", strconv.Itoa(c.scanLimit + 1)}
	if !since.IsZero() {
		args = append(args, "--since="+strconv.FormatInt(since.Unix(), 10))
	}
	out, err := gitRun(ctx, dir, nil, localTimeout, append(args, tip)...)
	if err != nil {
		return nil, false, err
	}
	var lines []string
	if s := strings.TrimSpace(string(out)); s != "" {
		lines = strings.Split(s, "\n")
	}
	if len(lines) > c.scanLimit {
		lines, truncated = lines[:c.scanLimit], true
	}
	var missing []string
	for _, line := range lines {
		sha, ct, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(ct, 10, 64)
		if err != nil {
			continue
		}
		commits = append(commits, landedCommit{sha: sha, when: time.Unix(secs, 0)})
		if _, ok := c.patchIDs[sha]; !ok {
			missing = append(missing, sha)
		}
	}
	computed, err := commitPatchIDs(ctx, dir, missing)
	if err != nil {
		return nil, false, err
	}
	for i := range commits {
		sha := commits[i].sha
		id, ok := c.patchIDs[sha]
		if !ok {
			id = computed[sha]
		}
		commits[i].patchID = id
		next.patchIDs[sha] = id
	}
	return commits, truncated, nil
}

// commitPatchIDs returns the patch-id of each commit's diff against its
// parent. Commits without one (merges, empty commits) are absent.
func commitPatchIDs(ctx context.Context, dir string, shas []string) (map[string]string, error) {
	ids := make(map[string]string)
	if len(shas) == 0 {
		return ids, nil
	}
	diff, err := gitRun(ctx, dir, []byte(strings.Join(shas, "\n")+"\n"), localTimeout, "diff-tree", "--stdin", "-r", "-p")
	if err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		return ids, nil
	}
	out, err := gitRun(ctx, dir, diff, localTimeout, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			ids[f[1]] = f[0]
		}
	}
	return ids, nil
}

// changePatchID returns the patch-id of x's change relative to base: the
// diff from their merge base to x, "" when the change is empty. ok is false
// when the clone has no merge base for them (unrelated or shallow history),
// so the change can't be compared.
func changePatchID(ctx context.Context, dir, base, x string) (id string, ok bool, err error) {
	mb, err := gitRun(ctx, dir, nil, localTimeout, "merge-base", base, x)
	if exitCode(err) == 1 {
		return "", false, nil // no common ancestor
	}
	if err != nil {
		return "", false, err
	}
	diff, err := gitRun(ctx, dir, nil, localTimeout, "diff-tree", "-r", "-p", strings.TrimSpace(string(mb)), x)
	if err != nil {
		return "", false, err
	}
	if len(diff) == 0 {
		return "", true, nil
	}
	out, err := gitRun(ctx, dir, diff, localTimeout, "patch-id", "--stable")
	if err != nil {
		return "", false, err
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		return f[0], true, nil
	}
	return "", true, nil
}

// hasObject reports whether the object named by a full SHA is in the clone.
func hasObject(ctx context.Context, dir, sha string) (bool, error) {
	_, err := gitRun(ctx, dir, nil, localTimeout, "cat-file", "-e", sha)
	switch exitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// isAncestor reports whether x is an ancestor of (or equal to) tip.
func isAncestor(ctx context.Context, dir, x, tip string) (bool, error) {
	_, err := gitRun(ctx, dir, nil, localTimeout, "merge-base", "--is-ancestor", x, tip)
	switch exitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// createdAt parses a run's creation time; zero when it can't.
func createdAt(s *run.State) time.Time {
	t, _ := time.Parse(time.RFC3339, s.CreatedAt)
	return t
}

// oldestCreated returns the earliest creation time among runs, or zero when
// any of them is unknown, so the walk covers every run.
func oldestCreated(runs []*run.State) time.Time {
	var oldest time.Time
	for i, s := range runs {
		t := createdAt(s)
		if t.IsZero() {
			return time.Time{}
		}
		if i == 0 || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

// gitError is a failed git command with its stderr.
type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}

func (e *gitError) Unwrap() error { return e.err }

// exitCode returns the exit status of a failed git command: 0 for no error
// and -1 when the command didn't run to an exit status (e.g. timed out).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return -1
}

// gitRun runs git in dir and returns its stdout. It never prompts: git's
// terminal prompt is off, and the command runs in a new session with no
// controlling terminal, so ssh can't ask for a passphrase or host key on
// the dashboard's screen either.
func gitRun(ctx context.Context, dir string, stdin []byte, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
		return nil, &gitError{args: args, err: err, stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}
