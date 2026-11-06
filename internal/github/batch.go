package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PRSnapshot is what one poll reads about a PR.
type PRSnapshot struct {
	Number         int
	State          string // OPEN, MERGED, CLOSED or StateNoAccess
	CI             string // passing, failing or pending
	Conflicts      string // yes, none or unknown
	ReviewDecision string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or ""
	Draft          bool
	Labels         []string
	HeadSHA        string
	BaseSHA        string      // the base branch's tip; "" when the branch is gone
	Activity       *PRActivity // nil unless asked for
}

// PRActivity is the review activity on a PR that decides whether trusted
// reviewers wait on a fix.
type PRActivity struct {
	Operator     string // the account gh runs as
	Author       string
	Reviews      []Review
	Comments     []IssueComment // the PR conversation
	LastCommitAt time.Time      // the head commit's committer date
}

// Review is a submitted review and the bodies of its inline comments.
type Review struct {
	ID          int64
	Author      string
	State       string
	Body        string
	SubmittedAt time.Time
	Comments    []string
}

// IssueComment is a PR conversation comment.
type IssueComment struct {
	Author    string
	Body      string
	UpdatedAt time.Time
}

// RateLimitError is GitHub refusing a call for a primary or secondary rate
// limit. Until is when GitHub says to try again, or a short backoff when it
// doesn't say.
type RateLimitError struct {
	Until time.Time
}

func (e *RateLimitError) Error() string {
	return "GitHub rate-limited until " + e.Until.Format("15:04")
}

// rateLimitBackoff is how long to wait on a rate limit that names no time.
const rateLimitBackoff = 2 * time.Minute

// prFields are the fields of a PR one poll reads; prActivityFields are added
// when review activity is asked for. Bot logins come without GraphQL's
// "[bot]" suffix, so __typename restores it.
const (
	prFields = `state isDraft mergeable reviewDecision headRefOid
baseRef { target { oid } }
labels(first: 50) { nodes { name } }
commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state } } } }`
	prActivityFields = `
author { __typename login }
reviews(last: 30) { nodes { databaseId author { __typename login } state submittedAt body comments(first: 10) { nodes { body } } } }
comments(last: 100) { nodes { author { __typename login } body updatedAt } }`
)

// PRSnapshots reads, in one GraphQL query, the given PRs of the repository
// slug (owner/repo). A PR GitHub can't find, or whose repository it can't,
// reads StateNoAccess. A rate limit is a *RateLimitError.
func (c *GHCLIClient) PRSnapshots(ctx context.Context, slug string, numbers []int, activity bool) (map[int]*PRSnapshot, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || len(numbers) == 0 {
		return nil, fmt.Errorf("PR snapshots: bad request for %q", slug)
	}
	fields := prFields
	if activity {
		fields += prActivityFields
	}
	var q strings.Builder
	q.WriteString("query {")
	if activity {
		q.WriteString(" viewer { login }")
	}
	fmt.Fprintf(&q, " repository(owner: %q, name: %q) {", owner, name)
	for _, n := range numbers {
		fmt.Fprintf(&q, " pr%d: pullRequest(number: %d) { ...f }", n, n)
	}
	q.WriteString(" } } fragment f on PullRequest { " + fields + " }")

	body, err := c.api(ctx, "graphql", "-f", "query="+q.String())
	if err != nil {
		var rl *RateLimitError
		if !errors.As(err, &rl) && IsNoAccess(err.Error()) {
			return noAccess(numbers), nil
		}
		return nil, err
	}
	return parseSnapshots(body, numbers)
}

func noAccess(numbers []int) map[int]*PRSnapshot {
	out := make(map[int]*PRSnapshot, len(numbers))
	for _, n := range numbers {
		out[n] = &PRSnapshot{Number: n, State: StateNoAccess}
	}
	return out
}

type gqlActor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

func (a *gqlActor) login() string {
	if a == nil {
		return ""
	}
	if a.Typename == "Bot" {
		return a.Login + "[bot]"
	}
	return a.Login
}

type gqlPR struct {
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"reviewDecision"`
	HeadRefOid     string `json:"headRefOid"`
	BaseRef        *struct {
		Target struct {
			Oid string `json:"oid"`
		} `json:"target"`
	} `json:"baseRef"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Author  *gqlActor `json:"author"`
	Reviews *struct {
		Nodes []struct {
			DatabaseID  int64     `json:"databaseId"`
			Author      *gqlActor `json:"author"`
			State       string    `json:"state"`
			SubmittedAt time.Time `json:"submittedAt"`
			Body        string    `json:"body"`
			Comments    struct {
				Nodes []struct {
					Body string `json:"body"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviews"`
	Comments *struct {
		Nodes []struct {
			Author    *gqlActor `json:"author"`
			Body      string    `json:"body"`
			UpdatedAt time.Time `json:"updatedAt"`
		} `json:"nodes"`
	} `json:"comments"`
}

// parseSnapshots reads a PRSnapshots response. A PR, or a repository,
// GraphQL reports NOT_FOUND or FORBIDDEN beside the others' data reads
// StateNoAccess; one missing for another error is left out, so it keeps its
// last status.
func parseSnapshots(body []byte, numbers []int) (map[int]*PRSnapshot, error) {
	var resp struct {
		Data *struct {
			Viewer *struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Repository map[string]*gqlPR `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string        `json:"type"`
			Path    []interface{} `json:"path"`
			Message string        `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing PR snapshots: %w", err)
	}
	unreadable := make(map[string]bool) // "repository" or a PR's alias
	for _, e := range resp.Errors {
		if (e.Type == "NOT_FOUND" || e.Type == "FORBIDDEN") && len(e.Path) > 0 {
			unreadable[fmt.Sprint(e.Path[len(e.Path)-1])] = true
		}
	}
	if resp.Data == nil || resp.Data.Repository == nil {
		if unreadable["repository"] {
			return noAccess(numbers), nil
		}
		return nil, fmt.Errorf("PR snapshots: no data in %.200s", body)
	}
	operator := ""
	if resp.Data.Viewer != nil {
		operator = resp.Data.Viewer.Login
	}
	out := make(map[int]*PRSnapshot, len(numbers))
	for _, n := range numbers {
		alias := "pr" + strconv.Itoa(n)
		pr := resp.Data.Repository[alias]
		if pr == nil {
			if unreadable[alias] {
				out[n] = &PRSnapshot{Number: n, State: StateNoAccess}
			}
			continue
		}
		s := &PRSnapshot{Number: n}
		out[n] = s
		s.State = strings.ToUpper(pr.State)
		s.Draft = pr.IsDraft
		s.ReviewDecision = pr.ReviewDecision
		s.HeadSHA = pr.HeadRefOid
		if pr.BaseRef != nil {
			s.BaseSHA = pr.BaseRef.Target.Oid
		}
		switch pr.Mergeable {
		case "CONFLICTING":
			s.Conflicts = "yes"
		case "MERGEABLE":
			s.Conflicts = "none"
		default:
			s.Conflicts = "unknown"
		}
		for _, l := range pr.Labels.Nodes {
			s.Labels = append(s.Labels, l.Name)
		}
		// A PR without checks has nothing to fail.
		s.CI = "passing"
		var lastCommitAt time.Time
		if len(pr.Commits.Nodes) > 0 {
			head := pr.Commits.Nodes[len(pr.Commits.Nodes)-1].Commit
			lastCommitAt = head.CommittedDate
			if head.StatusCheckRollup != nil {
				s.CI = rollupCI(head.StatusCheckRollup.State)
			}
		}
		if pr.Reviews != nil || pr.Comments != nil {
			a := &PRActivity{Operator: operator, Author: pr.Author.login(), LastCommitAt: lastCommitAt}
			if pr.Reviews != nil {
				for _, r := range pr.Reviews.Nodes {
					rv := Review{ID: r.DatabaseID, Author: r.Author.login(), State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt}
					for _, c := range r.Comments.Nodes {
						rv.Comments = append(rv.Comments, c.Body)
					}
					a.Reviews = append(a.Reviews, rv)
				}
			}
			if pr.Comments != nil {
				for _, c := range pr.Comments.Nodes {
					a.Comments = append(a.Comments, IssueComment{Author: c.Author.login(), Body: c.Body, UpdatedAt: c.UpdatedAt})
				}
			}
			s.Activity = a
		}
	}
	return out, nil
}

// rollupCI maps a commit's check rollup state to passing, failing or pending.
func rollupCI(state string) string {
	switch state {
	case "SUCCESS":
		return "passing"
	case "FAILURE", "ERROR":
		return "failing"
	default:
		return "pending"
	}
}

// CommitsBehindSHA returns how many commits base has that head lacks, in the
// repository slug. A rate limit is a *RateLimitError.
func (c *GHCLIClient) CommitsBehindSHA(ctx context.Context, slug, base, head string) (int, error) {
	body, err := c.api(ctx, fmt.Sprintf("repos/%s/compare/%s...%s", slug, base, head))
	if err != nil {
		return 0, err
	}
	var cmp struct {
		BehindBy *int `json:"behind_by"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil || cmp.BehindBy == nil {
		return 0, fmt.Errorf("comparing %s...%s: unexpected response %.200s", base, head, body)
	}
	return *cmp.BehindBy, nil
}

// api runs `gh api --include` and returns the response body. It reads the
// status line and headers --include adds, so a rate limit comes back as a
// *RateLimitError with the time GitHub says to wait for. A GraphQL response
// that carries data succeeds even though gh exits non-zero for the errors
// beside it.
func (c *GHCLIClient) api(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := ensureTimeout(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", append([]string{"api", "--include"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	status, hdr, body := splitInclude(stdout.Bytes())
	if rateLimited(status, body, stderr.String()) {
		return nil, &RateLimitError{Until: rateLimitUntil(hdr, time.Now())}
	}
	if runErr != nil && (status < 200 || status > 299) {
		return nil, fmt.Errorf("gh api: %w: %s", wrapTimeoutErr(ctx, "gh api", runErr), strings.TrimSpace(stderr.String()))
	}
	return body, nil
}

// splitInclude splits `gh api --include` output into the status code, the
// headers and the body. Output without a status line is all body, status 0.
func splitInclude(out []byte) (int, http.Header, []byte) {
	hdr := http.Header{}
	s := strings.ReplaceAll(string(out), "\r\n", "\n")
	head, body, ok := strings.Cut(s, "\n\n")
	if !ok || !strings.HasPrefix(head, "HTTP/") {
		return 0, hdr, out
	}
	lines := strings.Split(head, "\n")
	status := 0
	if f := strings.Fields(lines[0]); len(f) >= 2 {
		status, _ = strconv.Atoi(f[1])
	}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return status, hdr, []byte(body)
}

// rateLimited reports whether a response is GitHub refusing for a rate
// limit: a GraphQL RATE_LIMITED error, or a 403 or 429 (or a failure before
// any response) saying "rate limit". A successful body isn't searched for
// the words, since PR text can contain them.
func rateLimited(status int, body []byte, stderr string) bool {
	if status >= 200 && status <= 299 {
		var resp struct {
			Errors []struct {
				Type string `json:"type"`
			} `json:"errors"`
		}
		if json.Unmarshal(body, &resp) == nil {
			for _, e := range resp.Errors {
				if e.Type == "RATE_LIMITED" {
					return true
				}
			}
		}
		return false
	}
	if status != 0 && status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)+stderr), "rate limit")
}

// rateLimitUntil is when a rate-limited response says to try again: after
// Retry-After, else at X-RateLimit-Reset once the budget is spent, else after
// rateLimitBackoff.
func rateLimitUntil(hdr http.Header, now time.Time) time.Time {
	if s, err := strconv.Atoi(hdr.Get("Retry-After")); err == nil && s > 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	if hdr.Get("X-RateLimit-Remaining") == "0" {
		if r, err := strconv.ParseInt(hdr.Get("X-RateLimit-Reset"), 10, 64); err == nil && r > now.Unix() {
			return time.Unix(r, 0)
		}
	}
	return now.Add(rateLimitBackoff)
}
