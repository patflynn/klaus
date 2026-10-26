package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A clone's common git dir holds files that every worktree of the clone
// shares and that decide what git does there: its config, info/attributes
// and the hooks directory. A worker that writes one of them reaches every
// other worker's git commands, and the operator's, so launch snapshots them
// and finalize compares (SharedSnapshot, SharedChanges). Git can't keep a
// worker from writing them: the worker runs as the operator, git replaces
// its config by rename whatever the file's mode, and a worker's own
// `git push -u` legitimately writes branch tracking there.

// SharedGitDir returns the common git dir of the clone worktreeDir belongs to.
func SharedGitDir(ctx context.Context, worktreeDir string) (string, error) {
	return absGitPath(ctx, worktreeDir, "--git-common-dir")
}

// SharedSnapshot digests the shared files of the common git dir commonDir,
// by path relative to it: "config", "info/attributes" and "hooks/<name>". An
// absent file has no entry. The config's digest is of its entries, without
// the branch tracking that git push -u and launch write there and the
// remote resolution gh records. Nothing klaus or a worker runs writes
// info/attributes or the shared hooks, so theirs include the file's
// modification time, and "hooks" is the directory's: a file written and then
// put back, or a hook added and then removed, still differs.
func SharedSnapshot(ctx context.Context, commonDir string) map[string]string {
	snap := map[string]string{}
	if d, ok := configDigest(ctx, filepath.Join(commonDir, "config")); ok {
		snap["config"] = d
	}
	if d, ok := fileDigest(filepath.Join(commonDir, "info", "attributes")); ok {
		snap["info/attributes"] = d
	}
	hooks := filepath.Join(commonDir, "hooks")
	if info, err := os.Stat(hooks); err == nil {
		snap["hooks"] = fmt.Sprintf("dir %d", info.ModTime().UnixNano())
		entries, _ := os.ReadDir(hooks)
		for _, e := range entries {
			if d, ok := fileDigest(filepath.Join(hooks, e.Name())); ok {
				snap["hooks/"+e.Name()] = d
			}
		}
	}
	return snap
}

// SharedChanges names, sorted, the files whose digests differ between two
// SharedSnapshots: changed, added or removed.
func SharedChanges(before, after map[string]string) []string {
	var changed []string
	for k, v := range before {
		if w, ok := after[k]; !ok || w != v {
			changed = append(changed, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

// fileDigest is a file's content, mode and modification time; ok is false when it
// can't be read.
func fileDigest(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	var data []byte
	if info.Mode().IsRegular() {
		if data, err = os.ReadFile(path); err != nil {
			return "", false
		}
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s %s %d", hex.EncodeToString(sum[:]), info.Mode(), info.ModTime().UnixNano()), true
}

// configDigest is the digest of a git config file's entries, as git reads
// them and in order, leaving out benignConfigKey's; a file git can't read is digested
// whole. ok is false when the file doesn't exist.
func configDigest(ctx context.Context, path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	// --file reads the file alone, includes unfollowed; listing runs nothing.
	out, err := exec.CommandContext(ctx, "git", "config", "--file", path, "--list", "-z").Output()
	if err != nil {
		sum := sha256.Sum256(data)
		return "raw " + hex.EncodeToString(sum[:]), true
	}
	var entries []string
	for _, e := range strings.Split(string(out), "\x00") {
		key, _, _ := strings.Cut(e, "\n")
		if e != "" && !benignConfigKey(key) {
			entries = append(entries, e)
		}
	}
	// In file order: a later entry overrides an earlier one.
	sum := sha256.Sum256([]byte(strings.Join(entries, "\x00")))
	return hex.EncodeToString(sum[:]), true
}

// benignConfigKey reports whether a shared config key is one that runs write
// in the ordinary course: a branch's upstream (git push -u, launch's tracking
// worktrees) and the base remote gh resolves.
func benignConfigKey(key string) bool {
	switch {
	case strings.HasPrefix(key, "branch."):
		return strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".merge")
	case strings.HasPrefix(key, "remote."):
		return strings.HasSuffix(key, ".gh-resolved")
	}
	return false
}
