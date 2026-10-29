// Package jobs records the read-only background jobs a coordinator runs
// beside its agents: one record per klaus consult backend call (a panel is
// one per member, grouped by a panel ID) and one per klaus review. A record
// is written when the job starts and again when it ends, so klaus status
// and the dashboard can show what is running now and how recent jobs ended.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/review"
)

// Kinds of job.
const (
	KindConsult = "consult"
	KindReview  = "review"
)

// Status is how a job stands.
type Status string

const (
	Running     Status = "running"
	Succeeded   Status = "succeeded"
	Failed      Status = "failed"
	TimedOut    Status = "timed out"
	Interrupted Status = "interrupted"
	// Died is never written. A record still running whose process is gone,
	// or whose PID now names another process, reads as died: a SIGKILLed job
	// can't write its end.
	Died Status = "died"
)

// Retention is how long a finished record is kept. Older ones are pruned
// when a new job starts.
const Retention = 7 * 24 * time.Hour

// Job is the record of one job, stored as jobs/<id>.json in the session
// directory. A consult's ID is the run_id of its consult:started and
// consult:completed events; a review's, that of its review events.
type Job struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`            // consult or review
	Panel string `json:"panel,omitempty"` // shared by the members of one --panel consult

	Thread  string `json:"thread,omitempty"`
	Turn    int    `json:"turn,omitempty"` // the thread's turn, once it began
	Role    string `json:"role,omitempty"`
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Dir     string `json:"dir,omitempty"`    // a consult's workspace
	Repo    string `json:"repo,omitempty"`   // a consult's --repo, or a review's owner/repo
	Commit  string `json:"commit,omitempty"` // the commit a --repo consult read
	PR      string `json:"pr,omitempty"`     // a review's PR, owner/repo#N
	Prompt  string `json:"prompt,omitempty"` // first line of a consult's question

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// PID is the klaus process running the job, and PIDStart its start time
	// in clock ticks after boot (/proc/<pid>/stat), so a reused PID isn't
	// taken for the job's process. PIDStart is empty off Linux.
	PID      int    `json:"pid"`
	PIDStart string `json:"pid_start,omitempty"`

	Status Status `json:"status"`
	Error  string `json:"error,omitempty"` // short reason for a job that didn't succeed

	// Where the answer is: a threaded consult's is its turn in the thread's
	// log (Thread, Turn); an unthreaded consult's, AnswerFile; a review's,
	// Review. RawOutput holds what a failed consult's backend wrote.
	AnswerFile string  `json:"answer_file,omitempty"`
	RawOutput  string  `json:"raw_output,omitempty"`
	Review     *Review `json:"review,omitempty"`
}

// Review is what a review found.
type Review struct {
	Verdict   string           `json:"verdict,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	Findings  []review.Finding `json:"findings"`
	ReviewURL string           `json:"review_url,omitempty"`
}

// Finished reports whether the job ended, by its record or by dying.
func (j *Job) Finished() bool { return j.Status != Running }

// Settled is when the job ended: its finish time, or for one that died, when
// it started, the last time it is known to have been alive.
func (j *Job) Settled() time.Time {
	if j.FinishedAt != nil {
		return *j.FinishedAt
	}
	return j.StartedAt
}

// Store keeps job records in a session directory.
type Store struct{ Dir string }

func NewStore(sessionDir string) Store { return Store{Dir: filepath.Join(sessionDir, "jobs")} }

var jobID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (s Store) path(id, ext string) (string, error) {
	if !jobID.MatchString(id) || len(id) > 120 {
		return "", fmt.Errorf("invalid job ID %q", id)
	}
	return filepath.Join(s.Dir, id+ext), nil
}

// Start records j as running in this process from now, then prunes records
// that finished more than Retention ago.
func (s Store) Start(j *Job) error {
	now := time.Now().UTC()
	j.Status, j.StartedAt, j.FinishedAt = Running, now, nil
	j.PID = os.Getpid()
	j.PIDStart, _ = procStart(j.PID)
	err := s.Save(j)
	s.Prune(now)
	return err
}

// Finish records how j ended.
func (s Store) Finish(j *Job, status Status, errText string) error {
	now := time.Now().UTC()
	j.Status, j.Error, j.FinishedAt = status, errText, &now
	return s.Save(j)
}

// Save writes j's record atomically.
func (s Store) Save(j *Job) error {
	path, err := s.path(j.ID, ".json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// WriteFile writes a file beside job id's record, such as its answer
// (".answer") or raw output (".raw"), and returns its path.
func (s Store) WriteFile(id, ext string, data []byte) (string, error) {
	path, err := s.path(id, ext)
	if err != nil {
		return "", err
	}
	return path, writeAtomic(path, data)
}

// Load reads job id's record, judging whether a running job died.
func (s Store) Load(id string) (*Job, error) {
	path, err := s.path(id, ".json")
	if err != nil {
		return nil, err
	}
	return load(path)
}

func load(path string) (*Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("reading job %s: %w", filepath.Base(path), err)
	}
	if j.Status == Running && !alive(j.PID, j.PIDStart) {
		j.Status = Died
	}
	return &j, nil
}

// List returns every record, running jobs first, newest first within
// running and finished. Unreadable records are skipped.
func (s Store) List() ([]*Job, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Job
	for _, p := range paths {
		if j, err := load(p); err == nil {
			out = append(out, j)
		}
	}
	Sort(out)
	return out, nil
}

// Sort orders jobs running first, by start, newest first; then finished
// jobs by when they ended, newest first.
func Sort(js []*Job) {
	sort.SliceStable(js, func(a, b int) bool {
		x, y := js[a], js[b]
		if x.Finished() != y.Finished() {
			return !x.Finished()
		}
		tx, ty := x.StartedAt, y.StartedAt
		if x.Finished() {
			tx, ty = x.Settled(), y.Settled()
		}
		if !tx.Equal(ty) {
			return tx.After(ty)
		}
		return x.ID < y.ID
	})
}

// Recent keeps the jobs that are running, or that ended within window of
// now.
func Recent(js []*Job, now time.Time, window time.Duration) []*Job {
	var out []*Job
	for _, j := range js {
		if !j.Finished() || now.Sub(j.Settled()) <= window {
			out = append(out, j)
		}
	}
	return out
}

// Find returns the jobs ref names: the job with that ID, else the members
// of the panel with that ID, else the one job whose ID ends with ref.
func (s Store) Find(ref string) ([]*Job, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var panel, suffix []*Job
	for _, j := range all {
		switch {
		case j.ID == ref:
			return []*Job{j}, nil
		case j.Panel == ref:
			panel = append(panel, j)
		case ref != "" && strings.HasSuffix(j.ID, ref):
			suffix = append(suffix, j)
		}
	}
	switch {
	case len(panel) > 0:
		sort.Slice(panel, func(a, b int) bool { return panel[a].Backend < panel[b].Backend })
		return panel, nil
	case len(suffix) == 1:
		return suffix, nil
	case len(suffix) > 1:
		ids := make([]string, len(suffix))
		for i, j := range suffix {
			ids[i] = j.ID
		}
		return nil, fmt.Errorf("job %q is ambiguous: %s", ref, strings.Join(ids, ", "))
	}
	return nil, fmt.Errorf("no job %q in %s", ref, s.Dir)
}

// Prune removes the records, and the files beside them, of jobs that ended
// more than Retention before now. Errors are ignored: pruning is best effort.
func (s Store) Prune(now time.Time) {
	all, _ := s.List()
	for _, j := range all {
		if !j.Finished() || now.Sub(j.Settled()) <= Retention {
			continue
		}
		for _, ext := range []string{".answer", ".raw", ".json"} {
			if path, err := s.path(j.ID, ext); err == nil {
				_ = os.Remove(path)
			}
		}
	}
}

// alive reports whether pid is still the process that recorded start. With
// no start time (off Linux) any process with the PID counts.
func alive(pid int, start string) bool {
	if pid <= 0 {
		return false
	}
	if start != "" {
		now, ok := procStart(pid)
		return ok && now == start
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// procStart returns pid's start time from /proc/<pid>/stat, in clock ticks
// after boot. ok is false when there is no such live process, or no /proc
// (off Linux). A zombie has exited, so it has none.
func procStart(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	return parseStat(string(data))
}

// parseStat returns the start time from a /proc/<pid>/stat line, unless the
// process has exited. The command name, in parentheses, may itself hold
// spaces and parentheses; the fields after the last ")" start with the
// state (field 3), and the start time is field 22.
func parseStat(stat string) (string, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", false
	}
	return fields[19], true
}

// writeAtomic writes data to path through a synced temporary file in the
// same directory, as the consult thread store does.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".job-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
