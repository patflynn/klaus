package consult

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/patflynn/klaus/internal/backend"
)

const MaxFileBytes = 200 * 1024

// Attachment is a file given with --file, read whole.
type Attachment struct {
	Path string
	Data []byte
}

// ReadFiles reads the attachments, refusing them past MaxFileBytes in total.
func ReadFiles(paths []string) ([]Attachment, error) {
	files := make([]Attachment, 0, len(paths))
	remaining := MaxFileBytes
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("reading attachment %s: %w", path, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("reading attachment %s: %w", path, err)
		}
		remaining -= len(data)
		if remaining < 0 {
			return nil, fmt.Errorf("--file attachments exceed the 200KB total: %s takes them past %d bytes; attach fewer or smaller files", path, MaxFileBytes)
		}
		files = append(files, Attachment{Path: path, Data: data})
	}
	return files, nil
}

// InlineFiles appends the attachments to the prompt, each under a header
// naming its path. Headless agy can't open a path outside its workspace, and
// a denied read ends its turn with no answer, so for agy each file sits
// between BEGIN and END lines, behind a note that it is there in full.
func InlineFiles(kind backend.Kind, prompt string, files []Attachment) string {
	var out strings.Builder
	out.WriteString(prompt)
	if kind != backend.Agy {
		for _, f := range files {
			fmt.Fprintf(&out, "\n\n### %s\n%s", f.Path, f.Data)
		}
		return out.String()
	}
	if len(files) == 0 {
		return prompt
	}
	count := fmt.Sprintf("%d files are", len(files))
	if len(files) == 1 {
		count = "1 file is"
	}
	fmt.Fprintf(&out, "\n\n%s attached below, each in full between its BEGIN and END lines. Read attachments here, not from disk: their paths can be outside your workspace, where you cannot open them.", count)
	for i, f := range files {
		fmt.Fprintf(&out, "\n\n===== BEGIN ATTACHED FILE %d of %d: %s =====\n%s", i+1, len(files), f.Path, f.Data)
		if !bytes.HasSuffix(f.Data, []byte("\n")) {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "===== END ATTACHED FILE %d of %d: %s =====", i+1, len(files), f.Path)
	}
	return out.String()
}
