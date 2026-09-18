package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareAgyReadOnly limits the primary agent to file-reading tools.
func PrepareAgyReadOnly(prompt string) (string, func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(home, ".gemini", "config", "agents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(dir, "klaus-consult-*.md")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	name := strings.TrimSuffix(filepath.Base(f.Name()), ".md")
	body := fmt.Sprintf(`---
name: %s
description: Read-only model consultation
tools:
  - view_file
  - grep_search
mainAgent: true
subagent: false
---
You are a read-only thinking partner. You may only read files and answer questions.
%s
`, name, prompt)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}
