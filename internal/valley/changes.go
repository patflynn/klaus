package valley

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	State    string    `json:"state"` // StateReady or StateWithdrawn
	Record   string    `json:"record"`
	Seq      int       `json:"seq"`
	Active   int       `json:"active"` // n of the active version
	Versions []Version `json:"versions"`
	// Includes names the changes whose active head is a strict ancestor of
	// this one's active head, nearest only.
	Includes []string `json:"includes"`
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
