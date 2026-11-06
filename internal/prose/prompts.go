package prose

import (
	"encoding/json"
	"fmt"
	"strings"
)

// contentRule is the operator's direction for prose klaus's workers write.
const contentRule = `Say what a reader can't easily infer from the code: why, what's surprising, what to watch for. Drop the details, boundary conditions and machinery the implementation already shows. Prefer short sentences and plain words.`

const writerRole = `You edit the prose of a code change for humans and future agents. You don't change what it means.

Your answer is read by a program: reply with one JSON object and nothing else.`

const checkerRole = `You fact-check rewrites of a code change's prose. You read the original prose, the rewrite and the code, and find each rewrite that changes the meaning.

Your answer is read by a program: reply with one JSON object and nothing else.`

func rewritePrompt(dir string, in *input) string {
	var b strings.Builder
	b.WriteString("Rewrite the prose this change adds.\n\n## Content rule\n" + contentRule + "\n")
	if rules := Rules(dir); rules != "" {
		b.WriteString("\n## The repository's writing rules\n" + rules + "\n")
	}
	b.WriteString(`
## Steps
1. Ledger: before rewriting, list each claim the added prose makes, with the conditions it holds under, and the warnings it gives.
2. Rewrite only lines the change adds (the + lines below; the rest is context). In code files, rewrite only user-facing strings, such as help text and messages, and keep the code valid.
3. Check your rewrite against the ledger: every claim, condition and warning a reader needs is still there, no guarantee is stronger or weaker, and no cause is new. Leave a passage alone when you can't improve it.

## Answer
{
  "ledger": [{"id": "C1", "where": "README.md", "claim": "...", "conditions": ["..."]}],
  "edits": [{"file": "README.md", "old": "exact text from the file, whole lines", "new": "the replacement", "claims": ["C1"]}]
}
"old" must appear exactly once in the file as it is now. For the commit message, use the file "` + CommitFile + `". Return "edits": [] when nothing needs rewriting.
`)
	if in.diff != "" {
		b.WriteString("\n## The change's prose hunks\n```diff\n" + in.diff + "```\n")
	}
	if in.message != "" {
		b.WriteString("\n## The head commit's message (" + CommitFile + ")\n```\n" + in.message + "\n```\n")
	}
	return b.String()
}

// checkPrompt asks for a fact check of r's unskipped edits, or is "" when
// there are none.
func checkPrompt(in *input, r *Result) string {
	type numbered struct {
		N int `json:"edit"`
		Edit
	}
	var edits []numbered
	for i, e := range r.Edits {
		if e.Skipped == "" {
			edits = append(edits, numbered{i + 1, e})
		}
	}
	if len(edits) == 0 {
		return ""
	}
	ledger, _ := json.MarshalIndent(r.Ledger, "", "  ")
	proposed, _ := json.MarshalIndent(edits, "", "  ")
	var b strings.Builder
	fmt.Fprintf(&b, `Fact-check these rewrites of a change's prose. Each edit replaces "old", text the change adds, with "new". Read the code in the repository where a claim depends on it.

A rewrite fails when it drops a condition, makes a guarantee stronger or weaker, invents a cause, loses a warning, or says something the code doesn't do. Style isn't your concern.

## Answer
{"failures": [{"edit": 1, "claim": "C1", "problem": "what the rewrite changed"}]}
Return "failures": [] when every rewrite keeps the meaning.

## Claim ledger
%s

## Rewrites
%s
`, ledger, proposed)
	if in.diff != "" {
		b.WriteString("\n## The change's prose hunks\n```diff\n" + in.diff + "```\n")
	}
	if in.message != "" {
		b.WriteString("\n## The head commit's message (" + CommitFile + ")\n```\n" + in.message + "\n```\n")
	}
	return b.String()
}
