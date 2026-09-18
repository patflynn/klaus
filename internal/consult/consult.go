package consult

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

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

type Result struct {
	Answer, Stderr, DiagnosticLog, SessionID, SessionIDSource string
}

func Ask(ctx context.Context, t *Thread, prompt string, threaded bool, sessionID string, out, errOut io.Writer) (Result, error) {
	binary, err := LookupBinary(string(t.Backend))
	if err != nil {
		return Result{}, err
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
			return Result{}, err
		}
		defer cleanup()
		if threaded {
			f, err := os.CreateTemp("", "klaus-consult-agy-*.log")
			if err != nil {
				return Result{}, err
			}
			diagnosticLog = f.Name()
			f.Close()
			defer os.Remove(diagnosticLog)
		}
	}
	argv, err := backend.OneShot(t.Backend, backend.OneShotOptions{Prompt: agyPrompt, SystemPrompt: system, Model: t.Model, Effort: t.Effort, ResumeID: t.BackendSessionID, SessionID: sessionID, Threaded: threaded, AgyAgent: agyAgent, DiagnosticLog: diagnosticLog})
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	cmd.Dir = t.Dir
	if t.Backend != backend.Agy {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	var outputMu sync.Mutex
	cmd.Stdout = io.MultiWriter(&stdout, lockedWriter{&outputMu, out})
	cmd.Stderr = io.MultiWriter(&stderr, lockedWriter{&outputMu, errOut})
	runErr := cmd.Run()
	result := Result{Answer: stdout.String(), Stderr: stderr.String()}
	var extractionErr error
	if threaded && cmd.Process != nil {
		data, _ := os.ReadFile(diagnosticLog)
		result.DiagnosticLog = string(data)
		result.SessionID, result.SessionIDSource, extractionErr = extractSessionID(t.Backend, t.BackendSessionID, sessionID, result.Answer, result.Stderr, string(data))
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return result, errors.Join(fmt.Errorf("consult %s timed out: %w", t.Backend, ctx.Err()), extractionErr)
		}
		return result, errors.Join(fmt.Errorf("consult %s: %w", t.Backend, ctx.Err()), extractionErr)
	}
	if runErr != nil {
		return result, errors.Join(fmt.Errorf("consult %s: %w", t.Backend, runErr), extractionErr)
	}
	if extractionErr != nil {
		return result, extractionErr
	}
	if strings.TrimSpace(result.Answer) == "" {
		return result, fmt.Errorf("consult %s returned no answer", t.Backend)
	}
	return result, nil
}

func extractSessionID(kind backend.Kind, resumeID, assignedID, stdout, stderr, diagnostic string) (string, string, error) {
	if kind == backend.Claude && assignedID != "" {
		return assignedID, "assigned", nil
	}
	var tried []string
	if kind == backend.Agy {
		tried = append(tried, "diagnostic-log")
		if m := agySessionHeader.FindStringSubmatch(diagnostic); len(m) > 1 {
			return m[1], "diagnostic-log", nil
		}
	}
	tried = append(tried, "stderr")
	if id := sessionIDFromText(stderr); id != "" {
		return id, "stderr", nil
	}
	if resumeID != "" {
		return resumeID, "resume", nil
	}
	tried = append(tried, "stdout-json")
	if id := sessionIDFromJSON(stdout); id != "" {
		return id, "stdout-json", nil
	}
	tried = append(tried, "text")
	if id := sessionIDHeader(stdout); id != "" {
		return id, "text", nil
	}
	return "", "", fmt.Errorf("%s did not report a session ID; tried %s", kind, strings.Join(tried, ", "))
}

var agySessionHeader = regexp.MustCompile(`Sending user message to conversation ([a-zA-Z0-9_-]+) \(`)

var sessionHeader = regexp.MustCompile(`(?mi)^session id:\s*([a-zA-Z0-9_-]+)\s*$`)

func sessionIDFromText(text string) string {
	if id := sessionIDHeader(text); id != "" {
		return id
	}
	return sessionIDFromJSON(text)
}

func sessionIDHeader(text string) string {
	if m := sessionHeader.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	return ""
}

func sessionIDFromJSON(text string) string {
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

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}
