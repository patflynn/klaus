package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/consult"
	"github.com/patflynn/klaus/internal/jobs"
	"github.com/patflynn/klaus/internal/review"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

// recentJobs is how long a finished job stays in klaus status, klaus jobs
// and the dashboard after it ends.
const recentJobs = 3 * time.Hour

func newJobsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "jobs",
		Short: "List running and recent consults and reviews",
		Long: `Lists the read-only background jobs of the current Klaus session (or the
most recent session outside a pane): each klaus consult backend call, one per
member of a --panel, and each klaus review. Running jobs come first, then the
jobs that finished in the last 3 hours, newest first; --all lists every
record kept.

STATUS is running, succeeded, failed, timed out or interrupted, as the job
recorded it, or died: the job's klaus process is gone (it was killed with
SIGKILL, or the machine restarted) without recording an end.

Records live in ~/.klaus/sessions/<session-id>/jobs/<id>.json. A consult's
ID is the run_id of its consult:started and consult:completed events, and a
review's that of its review:started and review:completed events. Records of
jobs that finished more than 7 days ago are removed when a new job starts.

Use 'klaus jobs show <id>' to read a job's answer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := sessionJobStore()
			if err != nil {
				return err
			}
			list, err := store.List()
			if err != nil {
				return err
			}
			now := time.Now()
			if all, _ := cmd.Flags().GetBool("all"); !all {
				list = jobs.Recent(list, now, recentJobs)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No jobs.")
				return nil
			}
			printJobsTable(cmd.OutOrStdout(), list, now)
			return nil
		},
	}
	c.Flags().Bool("all", false, "List every job record kept, not only running and recent ones")
	c.AddCommand(newJobsShowCmd())
	return c
}

func newJobsShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <job-id>",
		Short: "Print a job's record and its answer",
		Long: `Prints a consult's or review's record, then its answer: for a threaded
consult, its turn's answer from the thread's log; for a one-shot consult,
the answer saved beside the record; for a review, its verdict, findings and
summary. A job that didn't succeed shows its error and where its raw output
was saved.

<job-id> is a job's ID, the ID of a panel (every member is shown), or a
unique ending of a job's ID, such as the four characters the dashboard
shows. --answer prints only the answer, and fails when there is none.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := sessionJobStore()
			if err != nil {
				return err
			}
			found, err := store.Find(args[0])
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			out := cmd.OutOrStdout()
			if only, _ := cmd.Flags().GetBool("answer"); only {
				for _, j := range found {
					answer, ok, err := jobAnswer(store, j)
					if err != nil {
						return err
					}
					if !ok {
						return fmt.Errorf("job %s has no answer: %s", j.ID, jobStatusText(j))
					}
					fmt.Fprint(out, answer)
				}
				return nil
			}
			for i, j := range found {
				if i > 0 {
					fmt.Fprintln(out)
				}
				if err := showJob(out, store, j, time.Now()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().Bool("answer", false, "Print only the answer")
	return c
}

func init() { rootCmd.AddCommand(newJobsCmd()) }

// sessionJobStore is the job store of the current session, or the most
// recent one outside a pane.
func sessionJobStore() (jobs.Store, error) {
	store, err := sessionStore()
	if err != nil {
		return jobs.Store{}, err
	}
	return jobs.NewStore(store.(*run.HomeDirStore).BaseDir()), nil
}

// jobOutcome classifies how a consult or review call ended, for its
// completion event and its record: succeeded; interrupted when SIGINT or
// SIGTERM cancelled the command; timed out at its deadline, said as
// timeoutMsg; else failed, with the error's first line.
func jobOutcome(err error, interrupted, timedOut bool, timeoutMsg string) (jobs.Status, string) {
	switch {
	case err == nil:
		return jobs.Succeeded, ""
	case interrupted:
		return jobs.Interrupted, "interrupted"
	case timedOut:
		return jobs.TimedOut, timeoutMsg
	}
	return jobs.Failed, errorLine(err)
}

// errorLine is an error's first line, cut to 200 characters.
func errorLine(err error) string {
	return truncateLine(strings.SplitN(err.Error(), "\n", 2)[0], 200)
}

// warnJob reports a job record klaus couldn't write. It never fails the
// job: the record is for watching it, not part of it.
func warnJob(w io.Writer, j *jobs.Job, err error) {
	if err != nil {
		fmt.Fprintf(w, "warning: recording job %s: %v\n", j.ID, err)
	}
}

// printJobsTable prints jobs as klaus status and klaus jobs show them.
func printJobsTable(w io.Writer, list []*jobs.Job, now time.Time) {
	const format = "%-30s  %-7s  %-20s  %-24s  %-11s  %-7s  %s\n"
	fmt.Fprintf(w, format, "JOB ID", "KIND", "THREAD/PR", "BACKEND/MODEL", "STATUS", "TIME", "PROMPT")
	fmt.Fprintf(w, format, "------", "----", "---------", "-------------", "------", "----", "------")
	for _, j := range list {
		fmt.Fprintf(w, format, j.ID, j.Kind, truncateLine(jobTarget(j), 20), truncateLine(jobBackendModel(j), 24),
			j.Status, jobTime(j, now), truncateLine(j.Prompt, 40))
	}
}

// jobTarget names what a job works on: a consult's thread, a review's PR,
// "panel" for a member of a panel, else "-".
func jobTarget(j *jobs.Job) string {
	switch {
	case j.Thread != "":
		return j.Thread
	case j.PR != "":
		return j.PR
	case j.Panel != "":
		return "panel"
	}
	return "-"
}

// jobBackendModel is a job's backend and model, e.g. codex/gpt-5.5.
func jobBackendModel(j *jobs.Job) string {
	if j.Model == "" {
		return j.Backend
	}
	return j.Backend + "/" + j.Model
}

// jobTime is how long a running job has run, or how long a finished one
// took; "-" for one that died, whose end isn't known.
func jobTime(j *jobs.Job, now time.Time) string {
	switch {
	case j.Status == jobs.Running:
		return formatElapsed(now.Sub(j.StartedAt))
	case j.FinishedAt != nil:
		return formatElapsed(j.FinishedAt.Sub(j.StartedAt))
	}
	return "-"
}

// formatElapsed renders a job's time compactly: "45s", "12m", "1h05m".
func formatElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// jobStatusText is a job's status with its error, e.g.
// "failed: consult codex: exit status 7".
func jobStatusText(j *jobs.Job) string {
	switch {
	case j.Status == jobs.Died:
		return fmt.Sprintf("died: process %d ended without recording how the job ended", j.PID)
	case j.Error != "" && j.Error != string(j.Status):
		return string(j.Status) + ": " + j.Error
	}
	return string(j.Status)
}

// showJob prints a job's record and then its answer, for klaus jobs show
// and the dashboard.
func showJob(w io.Writer, store jobs.Store, j *jobs.Job, now time.Time) error {
	var fields [][2]string
	add := func(k, v string) {
		if v != "" {
			fields = append(fields, [2]string{k, v})
		}
	}
	add("job", j.ID)
	kind := j.Kind
	if j.Panel != "" {
		kind += ", panel " + j.Panel
	}
	add("kind", kind)
	add("status", jobStatusText(j))
	backend := jobBackendModel(j)
	if j.Effort != "" {
		backend += ", effort " + j.Effort
	}
	if j.Role != "" {
		backend += ", role " + truncateLine(j.Role, 60)
	}
	add("backend", backend)
	if j.Thread != "" {
		thread := j.Thread
		if j.Turn > 0 {
			thread += fmt.Sprintf(", turn %d", j.Turn)
		}
		add("thread", thread)
	}
	add("pr", j.PR)
	if j.PR == "" {
		add("repo", j.Repo)
	}
	add("dir", j.Dir)
	add("started", j.StartedAt.Local().Format("2006-01-02 15:04:05"))
	if j.FinishedAt != nil {
		add("finished", fmt.Sprintf("%s, after %s", j.FinishedAt.Local().Format("2006-01-02 15:04:05"), jobTime(j, now)))
	} else if j.Status == jobs.Running {
		add("running", jobTime(j, now))
	}
	add("pid", fmt.Sprint(j.PID))
	add("prompt", j.Prompt)
	if j.Review != nil {
		add("posted", j.Review.ReviewURL)
	}
	add("answer", j.AnswerFile)
	add("raw output", j.RawOutput)
	for _, f := range fields {
		fmt.Fprintf(w, "%-11s %s\n", f[0]+":", f[1])
	}

	answer, ok, err := jobAnswer(store, j)
	switch {
	case err != nil:
		fmt.Fprintf(w, "\n(answer unavailable: %v)\n", err)
	case ok:
		fmt.Fprintln(w)
		fmt.Fprint(w, answer)
		if !strings.HasSuffix(answer, "\n") {
			fmt.Fprintln(w)
		}
	case j.Status == jobs.Running:
		fmt.Fprintln(w, "\n(still running)")
	}
	return nil
}

// jobAnswer is a job's answer: a succeeded consult's, from its thread's log
// or the file beside its record, or a review's findings. ok is false when
// the job has none to show.
func jobAnswer(store jobs.Store, j *jobs.Job) (answer string, ok bool, err error) {
	if j.Kind == jobs.KindReview {
		if j.Review == nil {
			return "", false, nil
		}
		var b bytes.Buffer
		printPRReview(&b, &review.ReviewResult{Verdict: j.Review.Verdict, Summary: j.Review.Summary, Findings: j.Review.Findings})
		return strings.TrimPrefix(b.String(), "\n"), true, nil
	}
	if j.Status != jobs.Succeeded {
		return "", false, nil
	}
	switch {
	case j.AnswerFile != "":
		data, err := os.ReadFile(j.AnswerFile)
		if err != nil {
			return "", false, err
		}
		return string(data), true, nil
	case j.Thread != "" && j.Turn > 0:
		// The job store sits beside consults/ in the session directory.
		turn, err := consult.NewStore(filepath.Dir(store.Dir)).Turn(j.Thread, j.Turn)
		if err != nil {
			return "", false, err
		}
		return turn.Answer, true, nil
	}
	return "", false, nil
}
