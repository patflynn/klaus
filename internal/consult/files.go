package consult

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const MaxFileBytes = 200 * 1024

func InlineFiles(prompt string, paths []string) (string, error) {
	var out strings.Builder
	out.WriteString(prompt)
	remaining := MaxFileBytes
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("reading attachment %s: %w", path, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
		f.Close()
		if err != nil {
			return "", fmt.Errorf("reading attachment %s: %w", path, err)
		}
		remaining -= len(data)
		if remaining < 0 {
			return "", fmt.Errorf("inlined files exceed the total 200KB limit")
		}
		fmt.Fprintf(&out, "\n\n### %s\n%s", path, data)
	}
	return out.String(), nil
}
