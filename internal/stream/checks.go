package stream

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/patflynn/klaus/internal/event"
)

// Check is a build, test or lint command an agent ran, as its stream shows
// it.
type Check struct {
	// Kind is event.CheckBuild, event.CheckTest or event.CheckLint, or
	// event.CheckPreReview for a sandbox run's _pre-review timed from the
	// stream.
	Kind    string
	Command string // the command's first line
	Start   time.Time
	// IncludesPreReview says the command line also runs klaus _pre-review,
	// which reports its own check: that part of the check's time is the
	// pre-review's.
	IncludesPreReview bool
}

// ReportPrefix starts a line a klaus command prints to report a check of the
// run it runs in when it can't reach the run's event log, as on a sandbox
// host: the prefix, then the check:completed event's data as JSON. The line
// reaches the pane's _format-stream in the tool result of the agent's call.
const ReportPrefix = "klaus-check-completed: "

// CheckWatcher finds the checks in an agent's stream as it arrives, and times
// each from the line that calls its command to the line with its result.
// Claude's calls and results carry IDs, so parallel calls pair up; Codex's
// and agy's don't, and pair in order. Lines go to Observe as NormalizeLine
// makes them.
type CheckWatcher struct {
	OnStart func(c Check)
	// OnEnd gets success when the result says, nil when it doesn't.
	OnEnd func(c Check, end time.Time, success *bool)
	// OnReport gets the check:completed data of each check reported in the
	// result of a call that runs klaus _pre-review (ReportPrefix lines).
	OnReport func(data map[string]interface{})
	// Sandbox says the agent runs on a sandbox host, where _pre-review
	// reports in its output. A call that runs it and no other check, whose
	// result reports nothing (the output was cut, or the host's klaus
	// predates reports), is timed from the stream as a pre-review check
	// instead. A line that runs another check first may have stopped before
	// _pre-review, so it gets none.
	Sandbox bool

	byID  map[string]*Check
	queue []*Check // calls without IDs, in order; nil for a call the watcher doesn't time
}

// Observe takes one normalized stream line, seen at now.
func (w *CheckWatcher) Observe(line []byte, now time.Time) {
	var ev struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Message *struct {
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   *bool           `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			if b.Type != "tool_use" {
				continue
			}
			c := callCheck(b.Input, now)
			if b.ID == "" {
				w.queue = append(w.queue, c)
			} else if c != nil {
				if w.byID == nil {
					w.byID = map[string]*Check{}
				}
				w.byID[b.ID] = c
			}
			if c != nil && c.Kind != "" && w.OnStart != nil {
				w.OnStart(*c)
			}
		}
	case "user":
		if ev.Message == nil {
			return
		}
		for _, b := range ev.Message.Content {
			c, ok := w.byID[b.ToolUseID]
			if b.Type != "tool_result" || !ok {
				continue
			}
			delete(w.byID, b.ToolUseID)
			var success *bool
			if b.IsError != nil {
				passed := !*b.IsError
				success = &passed
			}
			w.end(c, now, success, b.Content)
		}
	case "tool_result":
		if len(w.queue) == 0 {
			return
		}
		c := w.queue[0]
		w.queue = w.queue[1:]
		if c != nil {
			w.end(c, now, nil, ev.Content)
		}
	}
}

// callCheck returns what the watcher times of a tool call: the check its
// command runs, with Kind "" for one that runs only klaus _pre-review. It is
// nil for a call that runs neither, or runs in the background, where its
// result comes back before it ends.
func callCheck(input json.RawMessage, now time.Time) *Check {
	cmd, background := toolCommand(input)
	if cmd == "" || background {
		return nil
	}
	kind, preReview := CheckKind(cmd), RunsPreReview(cmd)
	if kind == "" && !preReview {
		return nil
	}
	return &Check{Kind: kind, Command: firstLine(cmd), Start: now, IncludesPreReview: preReview}
}

// end ends a call at its result: its check, then the pre-review checks its
// output reports.
func (w *CheckWatcher) end(c *Check, now time.Time, success *bool, output json.RawMessage) {
	if c.Kind != "" && w.OnEnd != nil {
		w.OnEnd(*c, now, success)
	}
	if !c.IncludesPreReview {
		return
	}
	reports := checkReports(resultText(output))
	for _, r := range reports {
		if w.OnReport != nil {
			w.OnReport(r)
		}
	}
	if len(reports) == 0 && w.Sandbox && c.Kind == "" {
		p := Check{Kind: event.CheckPreReview, Command: c.Command, Start: c.Start}
		if w.OnStart != nil {
			w.OnStart(p)
		}
		if w.OnEnd != nil {
			w.OnEnd(p, now, success)
		}
	}
}

// resultText is a tool result's output: a string, or Claude's text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// checkReports returns the data of the checks output reports (ReportPrefix).
func checkReports(output string) []map[string]interface{} {
	var reports []map[string]interface{}
	for _, line := range strings.Split(output, "\n") {
		i := strings.Index(line, ReportPrefix)
		if i < 0 {
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(line[i+len(ReportPrefix):])), &data) != nil {
			continue
		}
		if kind, _ := data["kind"].(string); kind != "" {
			reports = append(reports, data)
		}
	}
	return reports
}

// toolCommand returns the shell command a tool call runs, "" for a call that
// runs none, and whether it runs in the background, where its result comes
// back before it ends. Claude and Codex name it command; agy's names vary.
func toolCommand(input json.RawMessage) (string, bool) {
	var in struct {
		Command     string `json:"command"`
		CommandLine string `json:"CommandLine"`
		Cmd         string `json:"cmd"`
		Background  bool   `json:"run_in_background"`
	}
	if len(input) == 0 || json.Unmarshal(input, &in) != nil {
		return "", false
	}
	for _, c := range []string{in.Command, in.CommandLine, in.Cmd} {
		if c != "" {
			return c, in.Background
		}
	}
	return "", false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// checkCommands are the command lines CheckKind knows, by the words they
// start with. make is decided by its target.
var checkCommands = []struct {
	kind  string
	words []string
}{
	{event.CheckTest, []string{"go", "test"}},
	{event.CheckTest, []string{"cargo", "test"}},
	{event.CheckTest, []string{"cargo", "nextest"}},
	{event.CheckTest, []string{"npm", "test"}},
	{event.CheckTest, []string{"npm", "run", "test"}},
	{event.CheckTest, []string{"yarn", "test"}},
	{event.CheckTest, []string{"pnpm", "test"}},
	{event.CheckTest, []string{"bun", "test"}},
	{event.CheckTest, []string{"deno", "test"}},
	{event.CheckTest, []string{"pytest"}},
	{event.CheckTest, []string{"python", "-m", "pytest"}},
	{event.CheckTest, []string{"python3", "-m", "pytest"}},
	{event.CheckTest, []string{"npx", "jest"}},
	{event.CheckTest, []string{"npx", "vitest"}},
	{event.CheckTest, []string{"jest"}},
	{event.CheckTest, []string{"vitest"}},
	{event.CheckTest, []string{"nix", "flake", "check"}},
	{event.CheckTest, []string{"mvn", "test"}},
	{event.CheckTest, []string{"gradle", "test"}},
	{event.CheckTest, []string{"./gradlew", "test"}},
	{event.CheckTest, []string{"bazel", "test"}},
	{event.CheckTest, []string{"dotnet", "test"}},
	{event.CheckTest, []string{"mix", "test"}},
	{event.CheckTest, []string{"ctest"}},
	{event.CheckTest, []string{"tox"}},
	{event.CheckTest, []string{"rspec"}},
	{event.CheckLint, []string{"go", "vet"}},
	{event.CheckLint, []string{"golangci-lint"}},
	{event.CheckLint, []string{"staticcheck"}},
	{event.CheckLint, []string{"cargo", "clippy"}},
	{event.CheckLint, []string{"npm", "run", "lint"}},
	{event.CheckLint, []string{"yarn", "lint"}},
	{event.CheckLint, []string{"pnpm", "lint"}},
	{event.CheckLint, []string{"eslint"}},
	{event.CheckLint, []string{"npx", "eslint"}},
	{event.CheckLint, []string{"ruff"}},
	{event.CheckLint, []string{"flake8"}},
	{event.CheckLint, []string{"mypy"}},
	{event.CheckLint, []string{"pylint"}},
	{event.CheckLint, []string{"shellcheck"}},
	{event.CheckBuild, []string{"go", "build"}},
	{event.CheckBuild, []string{"cargo", "build"}},
	{event.CheckBuild, []string{"cargo", "check"}},
	{event.CheckBuild, []string{"npm", "run", "build"}},
	{event.CheckBuild, []string{"yarn", "build"}},
	{event.CheckBuild, []string{"pnpm", "build"}},
	{event.CheckBuild, []string{"tsc"}},
	{event.CheckBuild, []string{"npx", "tsc"}},
	{event.CheckBuild, []string{"nix", "build"}},
	{event.CheckBuild, []string{"mvn", "package"}},
	{event.CheckBuild, []string{"mvn", "compile"}},
	{event.CheckBuild, []string{"gradle", "build"}},
	{event.CheckBuild, []string{"./gradlew", "build"}},
	{event.CheckBuild, []string{"bazel", "build"}},
	{event.CheckBuild, []string{"dotnet", "build"}},
	{event.CheckBuild, []string{"docker", "build"}},
	{event.CheckBuild, []string{"cmake", "--build"}},
}

// checkRank orders kinds for a command line that runs several: a test run
// usually takes longest, so it names the line.
var checkRank = map[string]int{event.CheckBuild: 1, event.CheckLint: 2, event.CheckTest: 3}

var segmentSep = regexp.MustCompile(`&&|\|\||[;|\n]`)

// CheckKind returns the kind of check a shell command line runs, or "" when
// it runs none CheckKind knows. Each command in the line counts (split at
// &&, ||, ;, | and newlines), after any leading VAR=value assignments and
// wrappers such as time, timeout or nix develop -c; the line takes the
// highest kind among them, test over lint over build. klaus _pre-review is no
// kind here: it reports itself (see RunsPreReview).
func CheckKind(line string) string {
	best := ""
	for _, f := range lineCommands(line) {
		if k := commandKind(f); checkRank[k] > checkRank[best] {
			best = k
		}
	}
	return best
}

// RunsPreReview reports whether a shell command line runs klaus _pre-review,
// alone or among other commands.
func RunsPreReview(line string) bool {
	for _, f := range lineCommands(line) {
		if len(f) > 1 && (f[0] == "klaus" || strings.HasSuffix(f[0], "/klaus")) && f[1] == "_pre-review" {
			return true
		}
	}
	return false
}

// lineCommands splits a command line into its commands' words, without the
// wrappers before each.
func lineCommands(line string) [][]string {
	var cmds [][]string
	for _, seg := range segmentSep.Split(line, -1) {
		f := strings.Fields(seg)
		for i := range f {
			f[i] = strings.TrimRight(f[i], ")}")
		}
		if f = stripWrappers(f); len(f) > 0 {
			cmds = append(cmds, f)
		}
	}
	return cmds
}

// stripWrappers drops what runs before the command proper.
func stripWrappers(f []string) []string {
	for len(f) > 0 {
		w := strings.TrimLeft(f[0], "({")
		switch {
		case w == "":
			f = f[1:]
		case w != f[0]:
			f = append([]string{w}, f[1:]...)
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "-"):
			f = f[1:] // VAR=value
		case w == "time" || w == "env" || w == "command" || w == "exec" || w == "nice" || w == "sudo":
			f = f[1:]
		case w == "timeout" && len(f) > 1:
			f = f[2:]
		case w == "nix" && len(f) > 2 && f[1] == "develop" && (f[2] == "-c" || f[2] == "--command"):
			f = f[3:]
		default:
			return f
		}
	}
	return f
}

func commandKind(f []string) string {
	if len(f) == 0 {
		return ""
	}
	if f[0] == "make" || f[0] == "gmake" {
		return makeKind(f[1:])
	}
	for _, c := range checkCommands {
		if hasWords(f, c.words) {
			return c.kind
		}
	}
	return ""
}

func hasWords(f, words []string) bool {
	if len(f) < len(words) {
		return false
	}
	for i, w := range words {
		if f[i] != w {
			return false
		}
	}
	return true
}

// makeKind decides a make run by its first target: test… and check… are
// tests, lint… and vet… lint, anything else (the default target included)
// a build.
func makeKind(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-f" || a == "-j" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			i++
			continue
		case strings.HasPrefix(a, "-") || strings.Contains(a, "="):
			continue
		case strings.HasPrefix(a, "test") || strings.HasPrefix(a, "check"):
			return event.CheckTest
		case strings.HasPrefix(a, "lint") || strings.HasPrefix(a, "vet"):
			return event.CheckLint
		}
		return event.CheckBuild
	}
	return event.CheckBuild
}
