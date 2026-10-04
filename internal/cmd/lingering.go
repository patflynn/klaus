package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// lingeringProcesses names the commands still running with a working directory
// inside dir: background work a worker left when its turn ended. Workers'
// tools often start commands in a new session, so the worktree, not the
// process group, ties them to the run. This process, its ancestors (the
// pane's shell), and processes attached to another terminal (someone's
// interactive shell) are skipped. It reads /proc, so it finds nothing where
// /proc is absent (macOS).
func lingeringProcesses(dir string) []string {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	skip := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !skip[pid]; {
		skip[pid] = true
		pid, _ = procStat(pid)
	}
	_, ownTTY := procStat(os.Getpid())

	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || (cwd != root && !strings.HasPrefix(cwd, root+string(filepath.Separator))) {
			continue
		}
		if _, tty := procStat(pid); tty != 0 && tty != ownTTY {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		name := strings.TrimSpace(string(comm))
		if err != nil || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// procStat returns a process's parent PID and controlling terminal (0 for
// none) from /proc/<pid>/stat, or zeros when it can't be read.
func procStat(pid int) (ppid, tty int) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0
	}
	// The command name may contain spaces and parentheses; fields resume
	// after the last ')': state, ppid, pgrp, session, tty_nr, ...
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 5 {
		return 0, 0
	}
	ppid, _ = strconv.Atoi(f[1])
	tty, _ = strconv.Atoi(f[4])
	return ppid, tty
}
