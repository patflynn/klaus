package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
)

// ReviewConfig configures the peer review agent.
type ReviewConfig struct {
	Backend      string
	Model        string // e.g. "haiku" — passed to claude CLI --model flag
	MaxFixRounds int    // default 2
}

// Finding represents a single review finding.
type Finding struct {
	Severity    string `json:"severity"` // critical, high, medium, low
	File        string `json:"file"`
	Line        int    `json:"line"`
	Description string `json:"description"`
}

// ReviewResult holds the complete review output.
type ReviewResult struct {
	Findings []Finding `json:"findings"`
	Summary  string    `json:"summary"`
	Verdict  string    `json:"verdict,omitempty"` // PR mode only
	// Set by RunPRReview.
	HeadSHA   string `json:"-"`
	ReviewURL string `json:"-"` // posted review, if any
}

// maxDiffBytes is the maximum diff size we send to the review model.
// Haiku has 200k context; we cap the diff well under that.
const maxDiffBytes = 80_000

// ReviewDiff runs a peer review on the diff between the current branch and the given base branch.
func ReviewDiff(dir string, cfg ReviewConfig, baseBranch string) (*ReviewResult, error) {
	if baseBranch == "" {
		baseBranch = "main"
	}
	diff, err := getDiff(dir, baseBranch)
	if err != nil {
		return nil, fmt.Errorf("getting diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return &ReviewResult{Summary: "No changes to review."}, nil
	}

	return callReviewInDir(dir, truncateDiff(diff), cfg)
}

func truncateDiff(diff string) string {
	if len(diff) > maxDiffBytes {
		return diff[:maxDiffBytes] + "\n\n[... diff truncated due to size ...]"
	}
	return diff
}

func getDiff(dir, baseBranch string) (string, error) {
	cmd := exec.Command("git", "diff", baseBranch+"...HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func callReviewAPI(diff string, cfg ReviewConfig) (*ReviewResult, error) {
	return callReviewInDir("", diff, cfg)
}
func callReviewInDir(dir, diff string, cfg ReviewConfig) (*ReviewResult, error) {
	kind, err := backend.Parse(cfg.Backend)
	if err != nil {
		return nil, err
	}
	model := cfg.Model
	if model == "" && kind == backend.Claude {
		model = "haiku"
	}
	text, err := runReviewer(context.Background(), kind, model, reviewSystemPrompt, buildReviewPrompt(diff), dir)
	if err != nil {
		return nil, err
	}
	return parseReviewResponse(text)
}

// ErrAgyReviewer: agy has no read-only mode here yet.
var ErrAgyReviewer = errors.New("agy reviewer requires read-only agent support, pending #307")

// runReviewer runs one read-only, non-persistent review turn with a backend CLI in dir and returns its final message. Empty model → CLI default.
func runReviewer(ctx context.Context, kind backend.Kind, model, systemPrompt, userPrompt, dir string) (string, error) {
	var argv []string
	var stdin, lastMessage string
	switch kind {
	case backend.Claude:
		// Read/search tools only, no customizations, MCP, or slash commands.
		argv = []string{"claude", "-p", "--safe-mode", "--output-format", "text", "--system-prompt", systemPrompt,
			"--tools", "Read,Grep,Glob", "--permission-mode", "dontAsk", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"}
		stdin = userPrompt
	case backend.Codex:
		tmp, err := os.MkdirTemp("", "klaus-review-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		lastMessage = filepath.Join(tmp, "response.json")
		// --skip-git-repo-check: PR reviews run in an empty temp dir. --ignore-user-config: no MCP/hooks/plugins; auth still loads.
		argv = []string{"codex", "exec", "--ephemeral", "--sandbox", "read-only", "--skip-git-repo-check", "--ignore-user-config", "--output-last-message", lastMessage}
		stdin = systemPrompt + "\n\n" + userPrompt
	case backend.Agy:
		return "", ErrAgyReviewer
	default:
		return "", fmt.Errorf("unknown review backend %q", kind)
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if kind == backend.Codex {
		argv = append(argv, "-") // prompt from stdin
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("calling %s CLI: %w; stderr: %s", kind, err, stderr.String())
	}
	if lastMessage != "" {
		data, err := os.ReadFile(lastMessage)
		if err != nil {
			return "", fmt.Errorf("reading Codex review response: %w", err)
		}
		return string(data), nil
	}
	return stdout.String(), nil
}

const reviewSystemPrompt = `You are a code reviewer. Review the given git diff for issues. Be concise and focus only on real problems.

Respond with ONLY a JSON object (no markdown fences) in this exact format:
{
  "findings": [
    {"severity": "critical|high|medium|low", "file": "path/to/file.go", "line": 123, "description": "brief description"}
  ],
  "summary": "one sentence summary"
}

If there are no issues, return {"findings": [], "summary": "No issues found."}`

const reviewChecklist = `- Correctness bugs (logic errors, quoting issues, off-by-one)
- Unchecked errors and type assertions
- Security issues (injection, path traversal, unchecked input)
- Race conditions
- Edge cases and nil pointer dereferences`

func buildReviewPrompt(diff string) string {
	return fmt.Sprintf("Review this diff for:\n%s\n\nDiff:\n%s", reviewChecklist, diff)
}

func parseReviewResponse(text string) (*ReviewResult, error) {
	text = strings.TrimSpace(text)
	// Strip markdown code fences if present
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var result ReviewResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		raw := embeddedReviewObject(text)
		if raw == "" || json.Unmarshal([]byte(raw), &result) != nil {
			return nil, fmt.Errorf("failed to parse review response: %w; response text: %q", err, text)
		}
	}

	// Normalize severity values
	for i := range result.Findings {
		result.Findings[i].Severity = strings.ToLower(result.Findings[i].Severity)
	}

	return &result, nil
}

// embeddedReviewObject finds the first JSON object with a findings or summary key in prose-wrapped output; trailing text is ignored.
func embeddedReviewObject(text string) string {
	for i := strings.IndexByte(text, '{'); i >= 0; {
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var probe map[string]json.RawMessage
		if dec.Decode(&probe) == nil && (probe["findings"] != nil || probe["summary"] != nil) {
			return text[i : i+int(dec.InputOffset())]
		}
		next := strings.IndexByte(text[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return ""
}
