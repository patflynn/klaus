package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ReviewConfig configures the peer review agent.
type ReviewConfig struct {
	Model        string // e.g. "haiku" — maps to claude-haiku-4-5-20251001
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
}

// modelID maps short model names to full Anthropic model IDs.
func modelID(name string) anthropic.Model {
	switch strings.ToLower(name) {
	case "haiku":
		return "claude-haiku-4-5-20251001"
	case "sonnet":
		return "claude-sonnet-4-5-20250514"
	case "opus":
		return "claude-opus-4-0-20250514"
	default:
		return anthropic.Model(name)
	}
}

// maxDiffBytes is the maximum diff size we send to the review model.
// Haiku has 200k context; we cap the diff well under that.
const maxDiffBytes = 80_000

// ReviewDiff runs a peer review on the diff between the current branch and main.
func ReviewDiff(dir string, cfg ReviewConfig) (*ReviewResult, error) {
	diff, err := getDiff(dir)
	if err != nil {
		return nil, fmt.Errorf("getting diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return &ReviewResult{Summary: "No changes to review."}, nil
	}

	// Truncate large diffs
	if len(diff) > maxDiffBytes {
		diff = diff[:maxDiffBytes] + "\n\n[... diff truncated due to size ...]"
	}

	return callReviewAPI(diff, cfg)
}

func getDiff(dir string) (string, error) {
	cmd := exec.Command("git", "diff", "main...HEAD")
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
	model := cfg.Model
	if model == "" {
		model = "haiku"
	}

	client := anthropic.NewClient()

	prompt := buildReviewPrompt(diff)

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     modelID(model),
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		System: []anthropic.TextBlockParam{
			{Text: reviewSystemPrompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("calling Claude API: %w", err)
	}

	// Extract text from response
	var responseText string
	for _, block := range msg.Content {
		if block.Type == "text" {
			responseText += block.Text
		}
	}

	return parseReviewResponse(responseText)
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

func buildReviewPrompt(diff string) string {
	return fmt.Sprintf(`Review this diff for:
- Correctness bugs (logic errors, quoting issues, off-by-one)
- Unchecked errors and type assertions
- Security issues (injection, path traversal, unchecked input)
- Race conditions
- Edge cases and nil pointer dereferences

Diff:
%s`, diff)
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
		return &ReviewResult{
			Summary: "Failed to parse review response: " + text,
		}, nil
	}

	// Normalize severity values
	for i := range result.Findings {
		result.Findings[i].Severity = strings.ToLower(result.Findings[i].Severity)
	}

	return &result, nil
}
