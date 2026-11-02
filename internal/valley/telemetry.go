package valley

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// FollowTimeout bounds one `valley telemetry follow`. The host ends a follow
// after its follow_seconds, which is at most 300.
const FollowTimeout = 6 * time.Minute

// maxTelemetryLine caps one line of a follow's answer; the host caps a whole
// answer at its response_bytes.
const maxTelemetryLine = 8 << 20

// wakeTypes are the telemetry record types after which a change can stand
// elsewhere: the integrator's decision on a request, and the check worker's
// stored evidence and the end of its batch.
var wakeTypes = map[string]bool{"decision": true, "evidence.stored": true, "batch.end": true}

// telemetryLine is the part of a valley-telemetry/v1 answer line klaus
// reads: a record's type, and the cursor the end line carries.
type telemetryLine struct {
	Kind   string `json:"kind"`
	Type   string `json:"type"`
	Cursor string `json:"cursor"`
}

// Follow runs `valley telemetry --json follow --source <source>` in dir,
// after since when it isn't "", until the host ends the follow. It calls
// wake as each record arrives whose type can move a change: a decision,
// stored evidence or a batch's end. A record says only that something
// happened, never where a change stands. Follow returns the cursor the
// follow ended with, to resume from; an answer that doesn't end with one is
// an error, as is a valley without telemetry or a key without the host's
// observe grant.
func Follow(ctx context.Context, dir, source, since string, wake func()) (string, error) {
	args := []string{"telemetry", "--json", "follow", "--source", source}
	if since != "" {
		args = append(args, "--since", since)
	}
	ctx, cancel := context.WithTimeout(ctx, FollowTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "valley", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	isolate(cmd)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", &Error{Verb: "telemetry", Reason: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return "", &Error{Verb: "telemetry", Reason: err.Error()}
	}
	cursor := ""
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), maxTelemetryLine)
	for sc.Scan() {
		var line telemetryLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Kind {
		case "record":
			if wakeTypes[line.Type] {
				wake()
			}
		case "end":
			cursor = line.Cursor
		}
	}
	// A line too long to read leaves the rest unread; valley must not block
	// writing it.
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	e := &Error{Verb: "telemetry"}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		e.Reason = fmt.Sprintf("timed out after %s", FollowTimeout)
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.As(err, &exitErr):
		e.exited = true
		e.Reason = shortReason(errOut.Bytes())
		if e.Reason == "" {
			e.Reason = err.Error()
		}
	case err != nil:
		e.Reason = err.Error()
	case sc.Err() != nil:
		e.Reason = "reading the follow: " + sc.Err().Error()
	case cursor == "":
		e.Reason = "the follow ended without a cursor"
	default:
		return cursor, nil
	}
	return "", e
}
