package review

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// LintConfig configures which lint commands to run.
type LintConfig struct {
	Commands []string // e.g. ["go vet ./...", "golangci-lint run"]
}

// LintResult captures the output of a single lint command.
type LintResult struct {
	Command string
	Passed  bool
	Output  string
}

// RunLinters executes each lint command in the given directory and returns results.
func RunLinters(dir string, commands []string) ([]LintResult, error) {
	if len(commands) == 0 {
		return nil, nil
	}

	var results []LintResult
	for _, cmdStr := range commands {
		result, err := runLintCommand(dir, cmdStr)
		if err != nil {
			return nil, fmt.Errorf("running %q: %w", cmdStr, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func runLintCommand(dir, cmdStr string) (LintResult, error) {
	if strings.TrimSpace(cmdStr) == "" {
		return LintResult{Command: cmdStr, Passed: true}, nil
	}

	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}
	output = strings.TrimSpace(output)

	passed := err == nil
	// exec.ExitError means the command ran but returned non-zero — that's a lint failure, not an error.
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return LintResult{}, fmt.Errorf("executing %q: %w", cmdStr, err)
		}
	}

	return LintResult{
		Command: cmdStr,
		Passed:  passed,
		Output:  output,
	}, nil
}
