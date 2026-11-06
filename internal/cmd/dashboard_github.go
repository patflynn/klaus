package cmd

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/patflynn/klaus/internal/github"
)

// ghPoller reads the GitHub status of tracked PRs at a cost that doesn't grow
// with their number: one GraphQL query per repository per poll, plus a
// compare for a PR whose head or base moved since its last one. A rate limit
// pauses it until GitHub says to try again. A PR it can't read in full is
// left out of a poll, keeping its last status, rather than reported with
// unknown values the pipeline would act on.
type ghPoller struct {
	client  gh.Client
	trusted []string // trusted reviewers; their review activity is read only when there are some

	mu     sync.Mutex
	until  time.Time           // GitHub rate-limited polling until then
	behind map[string]behindAt // PR URL -> its last compare
}

// behindAt is how many commits a PR's base had that its head lacked, at the
// head and base compared.
type behindAt struct {
	head, base string
	n          int
}

// trackedPR is a PR a poll asks about.
type trackedPR struct {
	url, number string
}

func newGHPoller(client gh.Client, trusted []string) *ghPoller {
	return &ghPoller{client: client, trusted: trusted, behind: make(map[string]behindAt)}
}

// poll returns the statuses it read, keyed by PR URL, and the time polling
// is paused until, zero when it isn't.
func (p *ghPoller) poll(ctx context.Context, prs []trackedPR) (map[string]*prStatus, time.Time) {
	statuses := make(map[string]*prStatus)
	bySlug := make(map[string]map[int]string) // slug -> PR number -> URL
	var slugs []string
	for _, pr := range prs {
		slug := gh.OwnerRepoFromPRURL(pr.url)
		n, err := strconv.Atoi(pr.number)
		if slug == "" || err != nil {
			continue
		}
		if bySlug[slug] == nil {
			bySlug[slug] = make(map[int]string)
			slugs = append(slugs, slug)
		}
		bySlug[slug][n] = pr.url
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		if until := p.pausedUntil(); !until.IsZero() {
			return statuses, until
		}
		numbers := make([]int, 0, len(bySlug[slug]))
		for n := range bySlug[slug] {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		snaps, err := p.client.PRSnapshots(ctx, slug, numbers, len(p.trusted) > 0)
		if err != nil {
			p.failed(err)
			continue
		}
		for _, n := range numbers {
			url := bySlug[slug][n]
			if s := snaps[n]; s != nil {
				if ps, ok := p.status(ctx, slug, url, s); ok {
					statuses[url] = ps
				}
			}
		}
	}
	return statuses, p.pausedUntil()
}

// status turns a snapshot into the PR's status; ok is false when part of it
// couldn't be read.
func (p *ghPoller) status(ctx context.Context, slug, url string, s *gh.PRSnapshot) (*prStatus, bool) {
	ps := &prStatus{PRNumber: strconv.Itoa(s.Number), State: s.State}
	if s.State != "OPEN" {
		return ps, true
	}
	ps.CI, ps.Conflicts, ps.ReviewDecision = s.CI, s.Conflicts, s.ReviewDecision
	ps.Labels, ps.Draft, ps.HeadSHA = s.Labels, s.Draft, s.HeadSHA
	n, ok := p.commitsBehind(ctx, slug, url, s)
	if !ok {
		return nil, false
	}
	ps.BehindBy = n
	// GitHub's review decision doesn't reflect comments from trusted
	// reviewers, unless it is already decided.
	if !strings.EqualFold(ps.ReviewDecision, "CHANGES_REQUESTED") && !strings.EqualFold(ps.ReviewDecision, "APPROVED") {
		ps.HasNewTrustedComments = hasUnaddressedTrustedComments(s.Activity, p.trusted)
	}
	return ps, true
}

// commitsBehind returns how far the PR's head is behind its base, comparing
// them only when either moved since the last compare.
func (p *ghPoller) commitsBehind(ctx context.Context, slug, url string, s *gh.PRSnapshot) (int, bool) {
	if s.HeadSHA == "" || s.BaseSHA == "" {
		return 0, true
	}
	p.mu.Lock()
	b, ok := p.behind[url]
	p.mu.Unlock()
	if ok && b.head == s.HeadSHA && b.base == s.BaseSHA {
		return b.n, true
	}
	if !p.pausedUntil().IsZero() {
		return 0, false
	}
	n, err := p.client.CommitsBehindSHA(ctx, slug, s.BaseSHA, s.HeadSHA)
	if err != nil {
		p.failed(err)
		return 0, false
	}
	p.mu.Lock()
	p.behind[url] = behindAt{head: s.HeadSHA, base: s.BaseSHA, n: n}
	p.mu.Unlock()
	return n, true
}

// failed notes a failed call: a rate limit pauses polling, and is logged
// once per pause.
func (p *ghPoller) failed(err error) {
	var rl *gh.RateLimitError
	if !errors.As(err, &rl) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !time.Now().Before(p.until) {
		slog.Warn("GitHub rate limit: PR polling paused", "until", rl.Until.Format(time.RFC3339))
	}
	if rl.Until.After(p.until) {
		p.until = rl.Until
	}
}

// pausedUntil is when a rate limit lets polling resume, zero when it isn't
// paused.
func (p *ghPoller) pausedUntil() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.until) {
		return p.until
	}
	return time.Time{}
}
