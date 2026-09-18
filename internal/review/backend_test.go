package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSelectedBackend(t *testing.T) {
	for _, kind := range []string{"codex", "agy"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			stub := `#!/bin/sh
output=
while [ "$#" -gt 0 ]; do
 if [ "$1" = haiku ]; then exit 19; fi
 if [ "$1" = --output-last-message ]; then shift; output=$1; fi
 shift
done
if [ -n "$output" ]; then
 printf '%s\n' '{"findings":[],"summary":"reviewed"}' > "$output"
 printf '%s\n' 'progress chatter, not JSON'
 exit 0
fi
printf '%s\n' '{"findings":[],"summary":"reviewed"}'
`
			if err := os.WriteFile(filepath.Join(dir, kind), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 20\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			result, err := callReviewInDir(dir, "diff", ReviewConfig{Backend: kind})
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "reviewed" {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}
