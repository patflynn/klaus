package valley

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// KnotTimeout bounds each `valley knot` call.
const KnotTimeout = 30 * time.Second

// KnotSchemaV1 is the schema of what `valley knot show --json` prints.
const KnotSchemaV1 = "valley-knot/v1"

// knotRef is a knot ID, outcome, bug, idea or question, optionally behind
// the name of the project whose plan graph holds it: oc-897917f or
// the-valley:oc-897917f.
var knotRef = regexp.MustCompile(`^(?:([A-Za-z0-9][A-Za-z0-9._-]*):)?((?:oc|bd|ida|qn)-[0-9a-f]{7})$`)

// KnotRef names a knot in a valley project's plan graph. Project is "" for
// the graph of the project valley runs in.
type KnotRef struct {
	Project string
	ID      string
}

// ParseKnotRef reads an --issue value as a knot: a knot ID (oc-, bd-, ida- or
// qn- and 7 hex digits), optionally as <project>:<id>. ok is false for
// anything else, such as a GitHub issue number.
func ParseKnotRef(s string) (KnotRef, bool) {
	m := knotRef.FindStringSubmatch(s)
	if m == nil {
		return KnotRef{}, false
	}
	return KnotRef{Project: m[1], ID: m[2]}, true
}

func (r KnotRef) String() string {
	if r.Project == "" {
		return r.ID
	}
	return r.Project + ":" + r.ID
}

// Knot is a knot as `valley knot show --json` prints it.
type Knot struct {
	ID        string        `json:"id"`
	Type      string        `json:"type"`
	Status    string        `json:"status"`
	Active    bool          `json:"active"`
	Title     string        `json:"title"`
	Created   string        `json:"created"`
	CreatedBy string        `json:"created_by"`
	Source    string        `json:"source"`
	Parent    string        `json:"parent"`
	Owner     string        `json:"owner"`
	Target    string        `json:"target"`
	ClaimedBy string        `json:"claimed_by"`
	ClaimedAt string        `json:"claimed_at"`
	Changes   []string      `json:"changes"`
	Body      string        `json:"body"`
	Comments  []KnotComment `json:"comments"`
	Children  []string      `json:"children"`
}

// KnotComment is a comment on a knot.
type KnotComment struct {
	Author string `json:"author"`
	At     string `json:"at"`
	Body   string `json:"body"`
}

// KnotView is what `valley knot show --json` prints: the knot and the
// project whose graph holds it.
type KnotView struct {
	Schema  string `json:"schema"`
	Project string `json:"project"`
	Knot    Knot   `json:"knot"`
}

// ShowKnot runs `valley knot show <id> --json` in dir, in project's graph
// ("" for the graph of the project valley runs in).
func ShowKnot(ctx context.Context, dir, project, id string) (*KnotView, error) {
	out, err := command(ctx, dir, KnotTimeout, knotArgs(project, "show", id, "--json")...)
	if err != nil {
		return nil, err
	}
	var v KnotView
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, &Error{Verb: "knot show", Reason: "unexpected output: " + shortReason(out)}
	}
	if v.Schema != KnotSchemaV1 {
		return nil, &Error{Verb: "knot show", Reason: fmt.Sprintf("schema %q, want %q", v.Schema, KnotSchemaV1)}
	}
	return &v, nil
}

// ClaimKnot runs `valley knot claim <id> --run <runID>`. A knot claimed
// already is a conflict (Error.Conflict).
func ClaimKnot(ctx context.Context, dir, project, id, runID string) error {
	_, err := command(ctx, dir, KnotTimeout, knotArgs(project, "claim", id, "--run", runID)...)
	return err
}

// UnclaimKnot runs `valley knot unclaim <id> --if claimed_by=<runID>`,
// releasing runID's claim. A claim that isn't runID's is a conflict
// (Error.Conflict), and is left alone.
func UnclaimKnot(ctx context.Context, dir, project, id, runID string) error {
	_, err := command(ctx, dir, KnotTimeout, knotArgs(project, "unclaim", id, "--if", "claimed_by="+runID)...)
	return err
}

// LinkKnot runs `valley knot link <id> --change <change>`. Linking a change
// the knot holds already changes nothing.
func LinkKnot(ctx context.Context, dir, project, id, change string) error {
	_, err := command(ctx, dir, KnotTimeout, knotArgs(project, "link", id, "--change", change)...)
	return err
}

// CommentKnot runs `valley knot comment <id> --body -` with body on stdin.
func CommentKnot(ctx context.Context, dir, project, id, body string) error {
	_, err := commandInput(ctx, dir, KnotTimeout, []byte(body), knotArgs(project, "comment", id, "--body", "-")...)
	return err
}

// SetKnotStatus runs `valley knot set <id> status=<status>` with an --if for
// each of conditions, <field>=<value>: valley checks them against the knot
// it writes, and a field that holds another value is a conflict. A list
// field's value is its entries joined by commas, as show lists them.
func SetKnotStatus(ctx context.Context, dir, project, id, status string, conditions ...string) error {
	args := []string{id, "status=" + status}
	for _, c := range conditions {
		args = append(args, "--if", c)
	}
	_, err := command(ctx, dir, KnotTimeout, knotArgs(project, "set", args...)...)
	return err
}

// ChangeLink is how a knot links a valley change: valley:<repo>/<change ID>.
func ChangeLink(repo, changeID string) string {
	return "valley:" + repo + "/" + changeID
}

// changeLink matches a valley change link.
var changeLink = regexp.MustCompile(`^valley:([^/\s]+)/(\S+)$`)

// ParseChangeLink reads a knot's link to a valley change; ok is false for
// any other link, such as a GitHub pull request URL.
func ParseChangeLink(link string) (repo, changeID string, ok bool) {
	m := changeLink.FindStringSubmatch(link)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// knotArgs is `knot <verb> <args>`, with --project when project is set.
func knotArgs(project, verb string, args ...string) []string {
	out := append([]string{"knot", verb}, args...)
	if project != "" {
		out = append(out, "--project", project)
	}
	return out
}

// DetectKnots reports whether the repo in dir is a valley project whose
// plan graph klaus can work: Detect's checks against head, the commit a
// launch starts from, and a valley on PATH that also lists knot.
func DetectKnots(ctx context.Context, dir, head string) bool {
	return detect(ctx, dir, "", head, append(changeVerbs, "knot")...)
}
