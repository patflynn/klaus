package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/jobs"
	"github.com/patflynn/klaus/internal/prose"
	"github.com/patflynn/klaus/internal/run"
)

// startProsePass runs _pre-review's prose pass beside the peer review. The
// function it returns waits for the pass and prints its report. The pass
// never fails _pre-review.
func startProsePass(ctx context.Context, dir string, cfg config.Config, author backend.Kind, houseRules string) func(w io.Writer) {
	pc := cfg.Prose()
	if !*pc.Enabled {
		return func(w io.Writer) { fmt.Fprintln(w, "Prose rewrite: off (pre_review.prose.enabled).") }
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProsePass(ctx, &out, dir, cfg, pc, author, houseRules)
	}()
	return func(w io.Writer) {
		<-done
		w.Write(out.Bytes())
		fmt.Fprintln(w)
	}
}

func runProsePass(ctx context.Context, w io.Writer, dir string, cfg config.Config, pc config.ProseConfig, author backend.Kind, houseRules string) {
	// A sandbox host has no session to record the pass in.
	var store *jobs.Store
	if os.Getenv(runEventsEnv) != runEventsOutput {
		if s, err := sessionStore(); err == nil {
			if hds, ok := s.(*run.HomeDirStore); ok {
				js := jobs.NewStore(hds.BaseDir())
				store = &js
			}
		}
	}
	job := &jobs.Job{ID: eventID("prose"), Kind: jobs.KindProse, Dir: dir, Run: os.Getenv(runIDEnv)}
	r := prose.Run(ctx, prose.Options{
		Dir: dir, Base: defaultBranchRef(dir, cfg.DefaultBranch), Author: author,
		Order: pc.Order, Patterns: pc.Paths, MaxInput: pc.MaxInputBytes,
		Timeout:    time.Duration(pc.TimeoutSeconds) * time.Second,
		HouseRules: houseRules,
		Defaults: func(k backend.Kind) (string, string) {
			d := cfg.AgentDefaults(string(k))
			return d.Model, d.Effort
		},
		Started: func(r *prose.Result) {
			if store != nil {
				job.Backend, job.Model = r.Writer, r.WriterModel
				names := r.Files
				if len(names) == 0 {
					names = []string{"commit message"}
				}
				job.Prompt = "rewrite " + strings.Join(names, ", ")
				warnJob(w, job, store.Start(job))
			}
		},
	})
	fmt.Fprintf(w, "Prose rewrite (writer: %s; fact check: %s; author: %s):\n", orDash(r.Writer), orDash(r.Checker), author)
	r.PatchFile = saveProse(w, store, job, ".patch", r.Patch)
	r.MessageFile = saveProse(w, store, job, ".msg", r.Message)
	if store != nil && job.Backend != "" {
		job.Prose = r
		status := jobs.Succeeded
		if r.Error != "" {
			status = jobs.Failed
		}
		warnJob(w, job, store.Finish(job, status, truncateLine(r.Error, 200)))
	}
	printProse(w, r)
	if r.PatchFile != "" || r.MessageFile != "" || len(r.Failures) > 0 {
		fmt.Fprintln(w, "  Apply the rewrites that keep the meaning; fix or reject the rest. Say in your final report what you applied and why you rejected any.")
	}
}

// saveProse writes a pass's patch or commit message beside its job record,
// else in a temporary file, and returns its path; "" when there is nothing
// to write.
func saveProse(w io.Writer, store *jobs.Store, job *jobs.Job, ext, text string) string {
	if text == "" {
		return ""
	}
	if store != nil && job.Backend != "" {
		path, err := store.WriteFile(job.ID, ext, []byte(text))
		if err == nil {
			return path
		}
		warnJob(w, job, err)
	}
	f, err := os.CreateTemp("", "klaus-prose-*"+ext)
	if err != nil {
		fmt.Fprintf(w, "  warning: saving the %s: %v\n", ext[1:], err)
		return ""
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		fmt.Fprintf(w, "  warning: saving the %s: %v\n", ext[1:], err)
		return ""
	}
	return f.Name()
}

// printProse prints a pass's proposal, for _pre-review and klaus jobs show.
func printProse(w io.Writer, r *prose.Result) {
	if len(r.Omitted) > 0 {
		fmt.Fprintf(w, "  Left out, past pre_review.prose.max_input_bytes: %s\n", strings.Join(r.Omitted, ", "))
	}
	switch {
	case r.Skipped != "":
		fmt.Fprintf(w, "  Skipped: %s.\n", r.Skipped)
		return
	case r.Error != "":
		fmt.Fprintf(w, "  The rewrite failed: %s\n", r.Error)
		return
	case len(r.Edits) == 0:
		fmt.Fprintln(w, "  No rewrites proposed.")
		return
	case r.CheckError != "":
		fmt.Fprintf(w, "  The fact check failed: %s\n  No rewrite passed it; check their meaning yourself.\n", r.CheckError)
	}
	for i, e := range r.Edits {
		status := "passed"
		switch {
		case e.Skipped != "":
			status = "skipped: " + e.Skipped
		case r.Failed(i):
			status = "failed the fact check"
		case r.CheckError != "":
			status = "not fact-checked"
		}
		if e.Outcome != "" {
			status += "; " + e.Outcome
		}
		where := e.File
		if where == prose.CommitFile {
			where = "commit message"
		}
		fmt.Fprintf(w, "  [%d] %s: %s\n", i+1, where, status)
		for _, f := range r.Failures {
			if f.Edit == i+1 {
				fmt.Fprintf(w, "      ✗ %s\n", failureText(f))
			}
		}
		for _, l := range strings.Split(strings.TrimRight(e.Old, "\n"), "\n") {
			fmt.Fprintf(w, "      - %s\n", l)
		}
		for _, l := range strings.Split(strings.TrimRight(e.New, "\n"), "\n") {
			fmt.Fprintf(w, "      + %s\n", l)
		}
	}
	for _, f := range r.Failures {
		if f.Edit < 1 || f.Edit > len(r.Edits) {
			fmt.Fprintf(w, "  ✗ %s\n", failureText(f))
		}
	}
	if r.PatchFile != "" {
		fmt.Fprintf(w, "  Patch of the passing rewrites: git apply %s\n", shellQuote(r.PatchFile))
	}
	if r.MessageFile != "" {
		fmt.Fprintf(w, "  Commit message with its passing rewrites: git commit --amend -F %s\n", shellQuote(r.MessageFile))
	}
}

func failureText(f prose.Failure) string {
	if f.Claim != "" {
		return f.Claim + ": " + f.Problem
	}
	return f.Problem
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// settleProseJobs records, in run id's prose jobs, which rewrites the
// worktree took. Finalize calls it before removing the worktree.
func settleProseJobs(baseDir, id, worktree string) {
	if baseDir == "" || worktree == "" {
		return
	}
	store := jobs.NewStore(baseDir)
	list, _ := store.List()
	for _, j := range list {
		if j.Kind == jobs.KindProse && j.Run == id && j.Prose != nil && j.Finished() && j.Status != jobs.Died {
			prose.Settle(worktree, j.Prose)
			_ = store.Save(j)
		}
	}
}
