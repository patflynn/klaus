package valley

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaV1 is the schema of the machine view `valley changes --json` prints.
const SchemaV1 = "valley-changes/v1"

// Landing values: what a valley knows about whether a version landed.
const (
	Landed         = "landed"          // the version's head is an ancestor of main
	LandedRecorded = "landed-recorded" // proven by the integrator's signed landing statement
	Uncertain      = "uncertain"       // a request was consumed or failed, with no landing evidence
	NotLanded      = "not-landed"
)

// Change states.
const (
	StateReady     = "ready"
	StateWithdrawn = "withdrawn"
	// StateDraft is a change its submitter returned from ready (`valley
	// unready`), as launch does when a follow-up run continues it. approve
	// refuses it until a head is marked ready again.
	StateDraft = "draft"
)

// Changes is a valley's view of every change record it serves, in every
// state (valley-changes/v1).
type Changes struct {
	Schema string `json:"schema"`
	Host   string `json:"host"` // the serving host, as reached
	Repos  []Repo `json:"repos"`
}

// Repo is one repository the valley serves.
type Repo struct {
	Repo string `json:"repo"`
	URL  string `json:"url"`
	// Error is set (non-null) when the valley couldn't read the repo.
	Error    json.RawMessage `json:"error"`
	Changes  []Change        `json:"changes"`
	Branches []Branch        `json:"branches"`
}

// Err returns why the valley couldn't read the repo, or "" when it could.
func (r Repo) Err() string {
	raw := bytes.TrimSpace(r.Error)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// Change is a change record. Changes are keyed by (repo, target, change ID).
type Change struct {
	ChangeID string    `json:"change_id"`
	Handle   string    `json:"handle"` // shortest unique prefix of the ID, at least 4 characters
	Target   string    `json:"target"`
	Owner    string    `json:"owner"`
	State    string    `json:"state"` // StateReady, StateDraft or StateWithdrawn
	Record   string    `json:"record"`
	Seq      int       `json:"seq"`
	Active   int       `json:"active"` // n of the active version
	Versions []Version `json:"versions"`
	// Includes names the changes whose active head is a strict ancestor of
	// this one's active head, nearest only.
	Includes []string `json:"includes"`
	// Knots are the knots in the repo's plan graph that link the change
	// (valley:<repo>/<change ID>), active first; a valley from before plan
	// graphs lists none.
	Knots []ChangeKnot `json:"knots"`
	// Restack is the latest report the host's restacker wrote about the
	// change, or nil for none; a valley from before restack reports lists
	// none.
	Restack *Restack `json:"restack"`
}

// Restack is a change's latest restack report as the valley lists it: its
// ref and commit on origin, whether it is current (every ref it observed
// still holds what it observed), and what it says.
type Restack struct {
	Ref     string         `json:"ref"`
	Commit  string         `json:"commit"`
	Current bool           `json:"current"`
	Report  *RestackReport `json:"report"`
}

// RestackSchema is the schema of a restack report.
const RestackSchema = "valley-restack-report/v1"

// What came of a restack attempt.
const (
	RestackConflict   = "conflict"   // a layer's rebase conflicted; nothing was published
	RestackBlocked    = "blocked"    // integration requests held it back
	RestackPartial    = "partial"    // branches moved, and not every version was added
	RestackComplete   = "complete"   // every layer was rebased and versioned
	RestackSuperseded = "superseded" // its inputs moved while it ran
)

// RestackReport is what the restacker observed when it tried to rebase a
// ready change onto its target (valley-restack-report/v1). It is an
// observation, not evidence: klaus compares its inputs with what origin
// holds before acting on it, and never runs or relays its free text.
// Heads and commits carry the "git-sha1:" prefix.
type RestackReport struct {
	Schema    string         `json:"schema"`
	ChangeID  string         `json:"change_id"`
	Target    string         `json:"target"`
	TargetTip string         `json:"target_tip"`
	Operation string         `json:"operation"`
	Result    string         `json:"result"`
	Layers    []RestackLayer `json:"layers"` // the changes rebased together, bottom first
	Failing   *string        `json:"failing_layer"`
	Conflicts []string       `json:"conflict_paths"`
	Refusal   *string        `json:"refusal"`
}

// RestackLayer is one change a restack attempt rebased, as it read it.
// BranchTip is nil when origin had no such branch.
type RestackLayer struct {
	ChangeID  string            `json:"change_id"`
	Record    string            `json:"record"`
	Version   int               `json:"version"`
	Head      string            `json:"head"`
	Branch    string            `json:"branch"`
	BranchTip *string           `json:"branch_tip"`
	Published *RestackPublished `json:"published"`
}

// RestackPublished is what an attempt published for a layer.
type RestackPublished struct {
	Head    string  `json:"head"`
	Version *int    `json:"version"`
	Record  *string `json:"record"`
}

// Layer returns the report's layer for the change id.
func (r *RestackReport) Layer(id string) (RestackLayer, bool) {
	for _, l := range r.Layers {
		if l.ChangeID == id {
			return l, true
		}
	}
	return RestackLayer{}, false
}

// headPrefix opens every head and commit a restack report names.
const headPrefix = "git-sha1:"

// ReportOID returns the object ID a report names, without its "git-sha1:"
// prefix, or "" when s isn't one: the prefix and 40 lowercase hex digits.
func ReportOID(s string) string {
	oid, ok := strings.CutPrefix(s, headPrefix)
	if !ok || !IsOID(oid) {
		return ""
	}
	return oid
}

// IsOID reports whether s is a full SHA-1 object ID in lowercase hex.
func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// ChangeKnot is a knot that links a change.
type ChangeKnot struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

// Version is one head marked ready as a version of a change.
type Version struct {
	N            int      `json:"n"`
	Head         string   `json:"head"`
	Title        string   `json:"title"`
	Branch       string   `json:"branch"`
	Uploader     string   `json:"uploader"`
	Landing      string   `json:"landing"`
	LandedCommit *string  `json:"landed_commit"`
	Request      *Request `json:"request"` // the integration request filed for it, if any
	Outcome      *Outcome `json:"outcome"` // the integrator's decision on that request, if any
}

// Request is an integration request.
type Request struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// Outcome is the integrator's decision on a request.
type Outcome struct {
	Decision string `json:"decision"`
	Head     string `json:"head"`
	Tip      string `json:"tip"`
}

// Branch is a branch on the repo's origin.
type Branch struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// Decode parses the output of `valley changes --json`. It refuses any schema
// but SchemaV1, since a later schema may change what the fields mean, and
// ignores fields it doesn't know.
func Decode(data []byte) (*Changes, error) {
	var c Changes
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decoding valley changes: %w", err)
	}
	if c.Schema != SchemaV1 {
		return nil, fmt.Errorf("valley changes: schema %q, want %q", c.Schema, SchemaV1)
	}
	return &c, nil
}
