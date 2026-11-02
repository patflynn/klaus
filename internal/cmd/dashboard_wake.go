package cmd

import (
	"context"
	"log/slog"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// valleyWakeMsg says a valley host reported a record that can move a
// change. It wakes the integration check; it is never state.
type valleyWakeMsg struct{}

// valleyWakeCheckMsg starts the check a wake asked for, once the burst of
// records that woke it has had wakeSettle to arrive.
type valleyWakeCheckMsg struct{}

// wakeSettle is how long a wake waits for the rest of its burst of records,
// so the burst costs one check.
const wakeSettle = 500 * time.Millisecond

// telemetrySources are the valley telemetry sources whose records can move
// a change: the integrator's decisions, and the check worker's evidence.
var telemetrySources = []string{"integrator", "worker"}

const (
	// followFloor is the least time between two follows of one source, so a
	// host that ends each follow at once isn't asked in a busy loop.
	followFloor = time.Second
	// followTries is how many follows in a row may fail, a little apart,
	// before the dashboard says so and asks only every followRetry.
	followTries = 3
	// followRetry is how long a source that keeps failing waits between
	// follows: the minute's check carries the dashboard until one succeeds.
	followRetry = 10 * time.Minute
)

// valleyWaker follows the telemetry of each valley host the dashboard's
// valley changes live on, from one checkout per host, and says on its wake
// channel when a record arrives that can move a change. Records that arrive
// before the dashboard reads the channel are one wake.
type valleyWaker struct {
	wake   chan struct{}
	want   chan []string // the checkouts to follow from; only the latest counts
	mu     sync.Mutex    // makes Track's replace of want atomic
	logger *slog.Logger

	quietMu sync.Mutex
	quiet   map[string]bool // hosts whose failing telemetry was logged
}

// startValleyWaker starts a valleyWaker that runs until ctx ends, logging
// to logger.
func startValleyWaker(ctx context.Context, logger *slog.Logger) *valleyWaker {
	w := &valleyWaker{
		wake:   make(chan struct{}, 1),
		want:   make(chan []string, 1),
		logger: logger,
		quiet:  make(map[string]bool),
	}
	go w.run(ctx)
	return w
}

// valleyCheckouts returns the clones of the runs whose valley change the
// integration check asks the valley about: a run awaiting its change's
// integration, or a tracked change.
func valleyCheckouts(states []*run.State) []string {
	seen := make(map[string]bool)
	var dirs []string
	for _, s := range states {
		if s.Valley == nil || !(awaitsIntegration(s) || integration.TracksChange(s)) {
			continue
		}
		if s.CloneDir != nil && *s.CloneDir != "" && !seen[*s.CloneDir] {
			seen[*s.CloneDir] = true
			dirs = append(dirs, *s.CloneDir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// Track sets the checkouts whose origins' hosts to follow; none stops every
// follow. It never blocks.
func (w *valleyWaker) Track(dirs []string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-w.want:
	default:
	}
	w.want <- dirs
}

// waitCmd waits for the next wake.
func (w *valleyWaker) waitCmd() tea.Cmd {
	if w == nil {
		return nil
	}
	return func() tea.Msg {
		<-w.wake
		return valleyWakeMsg{}
	}
}

func (w *valleyWaker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// run follows each tracked host's sources, once per host, until ctx ends.
// A host is followed from one of its tracked checkouts; when that checkout
// is no longer tracked, its follows restart from another.
func (w *valleyWaker) run(ctx context.Context) {
	type following struct {
		dir  string
		stop context.CancelFunc
	}
	followers := make(map[string]following) // by host
	hostOf := make(map[string]string)       // checkout -> its origin's host
	for {
		var dirs []string
		select {
		case <-ctx.Done():
			return
		case dirs = <-w.want:
		}
		hosts := make(map[string]string) // host -> the checkout to follow it from
		for _, dir := range dirs {
			host, ok := hostOf[dir]
			if !ok {
				if host = originHost(ctx, dir); host == "" {
					continue // not a checkout now; asked again next time
				}
				hostOf[dir] = host
			}
			if f, ok := followers[host]; ok && f.dir == dir {
				hosts[host] = dir // keep following from where it does
			} else if _, ok := hosts[host]; !ok {
				hosts[host] = dir
			}
		}
		for host, f := range followers {
			if hosts[host] != f.dir {
				f.stop()
				delete(followers, host)
			}
		}
		for host, dir := range hosts {
			if _, ok := followers[host]; ok {
				continue
			}
			fctx, stop := context.WithCancel(ctx)
			followers[host] = following{dir: dir, stop: stop}
			for _, source := range telemetrySources {
				go w.follow(fctx, host, dir, source)
			}
		}
	}
}

// follow follows one source of host's telemetry from dir, resuming each
// follow where the last ended, until ctx ends. A failing follow is retried
// a little later, and after followTries failures in a row, logged once and
// retried only every followRetry.
func (w *valleyWaker) follow(ctx context.Context, host, dir, source string) {
	cursor, failures := "", 0
	for {
		started := time.Now()
		next, err := valley.Follow(ctx, dir, source, cursor, w.poke)
		if ctx.Err() != nil {
			return
		}
		pause := followFloor - time.Since(started)
		if err == nil {
			cursor, failures = next, 0
		} else {
			failures++
			pause = time.Second << (failures - 1)
			if failures >= followTries {
				pause = followRetry
				w.noteQuiet(host, source, err)
			}
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
		}
	}
}

// noteQuiet logs, once per host, that its telemetry can't be followed, so
// its changes are checked each minute only.
func (w *valleyWaker) noteQuiet(host, source string, err error) {
	w.quietMu.Lock()
	defer w.quietMu.Unlock()
	if w.quiet[host] {
		return
	}
	w.quiet[host] = true
	w.logger.Info("can't follow the valley host's telemetry; checking its changes each minute",
		"host", host, "source", source, "err", err)
}

// originHost returns the host of the origin of the checkout in dir: the
// host of an URL or of an scp-like address, or a local path as it is. ""
// when dir has no origin.
func originHost(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return remoteHost(strings.TrimSpace(string(out)))
}

// remoteHost is the host a git remote address names, without its user or
// port: "valley.example" for ssh://git@valley.example:22/repo or
// git@valley.example:repo. A local path has no host, and stands for itself.
func remoteHost(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Hostname()
	}
	if at, rest, ok := strings.Cut(remote, ":"); ok && !strings.Contains(at, "/") {
		if _, host, ok := strings.Cut(at, "@"); ok {
			return host
		}
		if rest != "" {
			return at
		}
	}
	return remote
}
