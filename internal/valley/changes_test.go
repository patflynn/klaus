package valley

import (
	"strings"
	"testing"
)

const sampleChanges = `{
  "schema": "valley-changes/v1",
  "host": "classic-laddie",
  "served_by": "valley 0.9",
  "repos": [
    { "repo": "cosmo", "url": "git@classic-laddie:cosmo.git", "error": null, "mirror": true,
      "changes": [
        { "change_id": "nxvpkqlmzrtwoyus", "handle": "nxvp", "target": "main", "owner": "klaus-env",
          "state": "ready", "record": "1111111111111111111111111111111111111111", "seq": 3, "active": 2,
          "priority": "high",
          "versions": [
            { "n": 1, "head": "aaaa", "title": "Add a health check", "branch": "agent/a", "uploader": "klaus-env",
              "landing": "not-landed", "landed_commit": null, "request": null, "outcome": null, "signed": false },
            { "n": 2, "head": "bbbb", "title": "Add a health check", "branch": "agent/a", "uploader": "klaus-env",
              "landing": "landed", "landed_commit": "cccc",
              "request": { "ref": "refs/valley/requests/nxvp", "commit": "dddd", "lease": 1 },
              "outcome": { "decision": "landed", "head": "bbbb", "tip": "cccc" } }
          ],
          "includes": ["zzzz"] }
      ],
      "branches": [ { "branch": "agent/a", "head": "bbbb" } ] },
    { "repo": "broken", "url": "git@classic-laddie:broken.git", "error": "fetch failed: permission denied", "changes": [] },
    { "repo": "odd", "url": "x", "error": { "code": 7 }, "changes": [] }
  ]
}`

func TestDecodeToleratesUnknownFields(t *testing.T) {
	c, err := Decode([]byte(sampleChanges))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Host != "classic-laddie" || len(c.Repos) != 3 {
		t.Fatalf("decoded %+v", c)
	}
	cosmo := c.Repos[0]
	if cosmo.Err() != "" {
		t.Errorf("cosmo error = %q, want none", cosmo.Err())
	}
	ch := cosmo.Changes[0]
	if ch.Handle != "nxvp" || ch.State != StateReady || ch.Active != 2 || len(ch.Versions) != 2 || ch.Includes[0] != "zzzz" {
		t.Errorf("change = %+v", ch)
	}
	v1, v2 := ch.Versions[0], ch.Versions[1]
	if v1.Request != nil || v1.Outcome != nil || v1.LandedCommit != nil || v1.Landing != NotLanded {
		t.Errorf("v1 = %+v", v1)
	}
	if v2.Landing != Landed || *v2.LandedCommit != "cccc" || v2.Request.Ref != "refs/valley/requests/nxvp" || v2.Outcome.Decision != "landed" {
		t.Errorf("v2 = %+v", v2)
	}
	if got := c.Repos[1].Err(); got != "fetch failed: permission denied" {
		t.Errorf("broken repo error = %q", got)
	}
	if got := c.Repos[2].Err(); got == "" {
		t.Error("a repo whose error is an object should still read as failed")
	}
}

func TestDecodeRefusesOtherSchemas(t *testing.T) {
	for name, schema := range map[string]string{
		"later version": `"valley-changes/v2"`,
		"other view":    `"valley-status/v1"`,
		"missing":       ``,
	} {
		t.Run(name, func(t *testing.T) {
			data := `{"host":"h","repos":[]}`
			if schema != "" {
				data = `{"schema":` + schema + `,"host":"h","repos":[]}`
			}
			_, err := Decode([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "schema") {
				t.Errorf("Decode(%s) error = %v, want a schema error", data, err)
			}
		})
	}
	if _, err := Decode([]byte("valley: no such verb changes")); err == nil {
		t.Error("Decode should refuse output that isn't JSON")
	}
}

func TestParseReady(t *testing.T) {
	r, err := parseReady([]byte("note: fetching origin\ncosmo nxvp v2 ready\n"))
	if err != nil || r != (Readied{Repo: "cosmo", Handle: "nxvp", Version: 2}) {
		t.Errorf("parseReady = %+v, %v", r, err)
	}
	if _, err := parseReady([]byte("ok\n")); err == nil {
		t.Error("parseReady should refuse output without a ready line")
	}
}
