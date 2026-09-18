package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/github"
)

// CrossReviewMarker opens the hidden line tagging a posted cross-review; re-exported as pipeline.CrossReviewMarker.
const CrossReviewMarker = "<!-- klaus-cross-review"

const (
	defaultMaxRounds = 2
	maxPRBodyBytes   = 8_000
)

var (
	ErrAlreadyReviewed = errors.New("head commit already cross-reviewed by this backend")
	ErrMaxRounds       = errors.New("cross-review round limit reached")
)

// Marker is the metadata in a cross-review's hidden marker line.
type Marker struct{ Backend, Model, SHA string }

func (m Marker) String() string {
	return fmt.Sprintf("%s backend=%s model=%s sha=%s -->", CrossReviewMarker, m.Backend, m.Model, m.SHA)
}

// ParseMarker reads the first cross-review marker in body.
func ParseMarker(body string) (Marker, bool) {
	_, rest, ok := strings.Cut(body, CrossReviewMarker)
	if !ok {
		return Marker{}, false
	}
	fields, _, ok := strings.Cut(rest, "-->")
	if !ok {
		return Marker{}, false
	}
	var m Marker
	for _, f := range strings.Fields(fields) {
		switch k, v, _ := strings.Cut(f, "="); k {
		case "backend":
			m.Backend = v
		case "model":
			m.Model = v
		case "sha":
			m.SHA = v
		}
	}
	return m, true
}

// PROptions configures RunPRReview.
type PROptions struct {
	Backend   backend.Kind // reviewer family
	Model     string       // "" → CLI default
	Post      bool         // submit one COMMENT review
	MaxRounds int          // posted cross-reviews allowed per PR; ≤0 → 2
}

type prInfo struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// RunPRReview reviews PR prNumber of repo ("owner/repo") from its diff alone, in an empty temp dir, and with opts.Post submits one COMMENT review (never APPROVE/REQUEST_CHANGES).
// A failed post still returns the result alongside the error.
func RunPRReview(ctx context.Context, repo, prNumber string, opts PROptions) (*ReviewResult, error) {
	gh := github.NewGHCLIClient(repo)
	out, err := gh.APIGet(ctx, "repos/"+repo+"/pulls/"+prNumber)
	if err != nil {
		return nil, fmt.Errorf("fetching PR #%s: %w", prNumber, err)
	}
	var pr prInfo
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("parsing PR #%s: %w", prNumber, err)
	}
	if opts.Post {
		if err := checkRounds(ctx, gh, repo, prNumber, pr.Head.SHA, opts); err != nil {
			return nil, err
		}
	}
	diff, err := gh.PRDiff(ctx, prNumber)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(diff) == "" {
		return &ReviewResult{Summary: "No changes to review.", HeadSHA: pr.Head.SHA}, nil
	}

	dir, err := os.MkdirTemp("", "klaus-pr-review-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	text, err := runReviewer(ctx, opts.Backend, opts.Model, prReviewSystemPrompt, buildPRReviewPrompt(pr.Title, pr.Body, truncateDiff(diff)), dir)
	if err != nil {
		return nil, err
	}
	result, err := parseReviewResponse(text)
	if err != nil {
		return nil, err
	}
	result.HeadSHA = pr.Head.SHA
	if !opts.Post {
		return result, nil
	}

	req := buildPRReview(result, DiffLines(diff), Marker{Backend: string(opts.Backend), Model: opts.Model, SHA: pr.Head.SHA})
	resp, err := gh.APIPostJSON(ctx, "repos/"+repo+"/pulls/"+prNumber+"/reviews", req)
	if err != nil {
		return result, fmt.Errorf("posting review: %w", err)
	}
	var posted struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(resp, &posted)
	result.ReviewURL = posted.HTMLURL
	return result, nil
}

// checkRounds refuses a post that would repeat this backend on the same head or exceed opts.MaxRounds, so review → fix → review cannot spin.
func checkRounds(ctx context.Context, gh *github.GHCLIClient, repo, prNumber, sha string, opts PROptions) error {
	max := opts.MaxRounds
	if max <= 0 {
		max = defaultMaxRounds
	}
	out, err := gh.APIGet(ctx, "repos/"+repo+"/pulls/"+prNumber+"/reviews?per_page=100")
	if err != nil {
		return fmt.Errorf("listing reviews on PR #%s: %w", prNumber, err)
	}
	var reviews []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &reviews); err != nil {
		return fmt.Errorf("parsing reviews on PR #%s: %w", prNumber, err)
	}
	rounds := 0
	for _, r := range reviews {
		m, ok := ParseMarker(r.Body)
		if !ok {
			continue
		}
		if m.SHA == sha && m.Backend == string(opts.Backend) {
			return fmt.Errorf("%w (%s at %.7s)", ErrAlreadyReviewed, opts.Backend, sha)
		}
		rounds++
	}
	if rounds >= max {
		return fmt.Errorf("%w: PR #%s has %d posted cross-reviews (cross_review.max_rounds=%d)", ErrMaxRounds, prNumber, rounds, max)
	}
	return nil
}

type reviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

type reviewRequest struct {
	CommitID string          `json:"commit_id,omitempty"`
	Body     string          `json:"body"`
	Event    string          `json:"event"`
	Comments []reviewComment `json:"comments,omitempty"`
}

// buildPRReview inlines findings on lines shown in the diff and folds the rest into the body.
func buildPRReview(r *ReviewResult, lines map[string]map[int]bool, m Marker) reviewRequest {
	req := reviewRequest{CommitID: m.SHA, Event: "COMMENT"}
	var folded []Finding
	for _, f := range r.Findings {
		if p, ok := diffPath(lines, f.File); ok && lines[p][f.Line] {
			req.Comments = append(req.Comments, reviewComment{
				Path: p, Line: f.Line, Side: "RIGHT",
				Body: fmt.Sprintf("**%s** (%s cross-review): %s", strings.ToUpper(f.Severity), m.Backend, f.Description),
			})
			continue
		}
		folded = append(folded, f)
	}
	req.Body = prReviewBody(r, folded, m)
	return req
}

func prReviewBody(r *ReviewResult, folded []Finding, m Marker) string {
	model := m.Model
	if model == "" {
		model = "default model"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Cross-model review** by %s (%s): a second opinion for the operator, not an approval.\n\n", m.Backend, model)
	if r.Verdict != "" {
		fmt.Fprintf(&b, "**Verdict:** %s\n\n", r.Verdict)
	}
	if r.Summary != "" {
		fmt.Fprintf(&b, "**Summary:** %s\n\n", r.Summary)
	}
	if len(folded) > 0 {
		b.WriteString("**Findings not on a diff line:**\n")
		for _, f := range folded {
			loc := f.File
			if loc == "" {
				loc = "general"
			} else if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(&b, "- **%s** `%s`: %s\n", strings.ToUpper(f.Severity), loc, f.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString(m.String())
	return b.String()
}

const prReviewSystemPrompt = `You are a code reviewer giving a second opinion on a pull request written by a different AI model. Judge whether the change does what its title and description claim, and report real problems only. Be concise.

Respond with ONLY a JSON object (no markdown fences) in this exact format:
{
  "verdict": "2-4 sentences for the human who approves merges: does the change match its stated intent, what is missing or risky, what to check before approving",
  "findings": [
    {"severity": "critical|high|medium|low", "file": "path/in/repo.go", "line": 123, "description": "brief description"}
  ],
  "summary": "one sentence summary"
}

"line" is the line number in the new version of the file and must be an added or context line shown in the diff; use 0 when a finding is not about one line.
If there are no issues, return an empty "findings" array.`

func buildPRReviewPrompt(title, body, diff string) string {
	if strings.TrimSpace(body) == "" {
		body = "(no description)"
	} else if len(body) > maxPRBodyBytes {
		body = body[:maxPRBodyBytes] + "\n[... description truncated ...]"
	}
	return fmt.Sprintf("Pull request title: %s\n\nPull request description:\n%s\n\n"+
		"Check whether the diff implements what the title and description claim, and flag anything claimed but missing or changed but unmentioned. Then review it for:\n%s\n\nDiff:\n%s",
		title, body, reviewChecklist, diff)
}
