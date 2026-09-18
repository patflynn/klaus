package consult

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
)

// LookupBinary permits executable substitution without adding fake backend kinds.
var LookupBinary = exec.LookPath

func Installed(order []string, caller string) ([]backend.Kind, error) {
	var kinds []backend.Kind
	seen := map[backend.Kind]bool{}
	for _, name := range order {
		kind, err := backend.Parse(name)
		if err != nil || name == "" {
			return nil, fmt.Errorf("invalid consult.order entry %q", name)
		}
		if seen[kind] || name == caller {
			continue
		}
		seen[kind] = true
		if _, err := LookupBinary(name); err == nil {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}

func Ask(ctx context.Context, t *Thread, prompt string, threaded bool, sessionID string, out, errOut io.Writer) (string, string, error) {
	binary, err := LookupBinary(string(t.Backend))
	if err != nil {
		return "", "", err
	}
	system := SystemPrompt(t.Role) + fmt.Sprintf("\n\nInspect the repository at %q. Resolve referenced code paths against that directory.", t.Dir)
	agyPrompt, agyAgent, diagnosticLog := prompt, "", ""
	if t.Backend == backend.Agy {
		context := ""
		if len(system)+len(prompt) > 120*1024 {
			context = system + "\n\n" + prompt
			system = ""
			agyPrompt = "Answer the consultation request included in your system instructions."
		}
		var cleanup func()
		agyAgent, cleanup, err = backend.PrepareAgyReadOnly(context)
		if err != nil {
			return "", "", err
		}
		defer cleanup()
		if threaded {
			f, err := os.CreateTemp("", "klaus-consult-agy-*.log")
			if err != nil {
				return "", "", err
			}
			diagnosticLog = f.Name()
			f.Close()
			defer os.Remove(diagnosticLog)
		}
	}
	argv := backend.OneShot(t.Backend, backend.OneShotOptions{Prompt: agyPrompt, SystemPrompt: system, Model: t.Model, Effort: t.Effort, ResumeID: t.BackendSessionID, SessionID: sessionID, Threaded: threaded, AgyAgent: agyAgent, DiagnosticLog: diagnosticLog})
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.Dir = t.Dir
	if t.Backend != backend.Agy {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(out, &stdout)
	cmd.Stderr = io.MultiWriter(errOut, &stderr)
	if err := cmd.Run(); err != nil {
		return stdout.String(), "", fmt.Errorf("consult %s: %w", t.Backend, err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		return "", "", fmt.Errorf("consult %s returned no answer", t.Backend)
	}
	id := t.BackendSessionID
	if threaded && id == "" {
		switch t.Backend {
		case backend.Claude:
			id = sessionID
		case backend.Codex:
			id = sessionIDFromText(stderr.String())
		case backend.Agy:
			data, _ := os.ReadFile(diagnosticLog)
			if m := agySessionHeader.FindSubmatch(data); len(m) > 1 {
				id = string(m[1])
			}
		}
		if id == "" {
			id = sessionIDFromText(stdout.String())
		}
		if id == "" {
			return stdout.String(), "", fmt.Errorf("%s did not report a session ID; cannot continue thread", t.Backend)
		}
	}
	return stdout.String(), id, nil
}

var agySessionHeader = regexp.MustCompile(`Sending user message to conversation ([a-zA-Z0-9_-]+) \(`)

var sessionHeader = regexp.MustCompile(`(?mi)^session id:\s*([a-zA-Z0-9_-]+)\s*$`)

func sessionIDFromText(text string) string {
	if m := sessionHeader.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	for _, line := range strings.Split(text, "\n") {
		var ev struct {
			SessionID      string `json:"session_id"`
			ThreadID       string `json:"thread_id"`
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		for _, id := range []string{ev.SessionID, ev.ThreadID, ev.ConversationID} {
			if id != "" {
				return id
			}
		}
	}
	return ""
}
