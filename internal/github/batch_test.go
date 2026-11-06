package github

import "testing"

// TestParseSnapshots: a PR GraphQL reports NOT_FOUND reads NOACCESS, one
// with another error on it or a field is left out to keep its last status,
// and a bot's
// login gets back the "[bot]" suffix trusted_reviewers names it by.
func TestParseSnapshots(t *testing.T) {
	body := `{"data": {"viewer": {"login": "op"}, "repository": {
  "pr1": {"state": "OPEN", "mergeable": "CONFLICTING", "headRefOid": "h1", "baseRef": {"target": {"oid": "b"}},
    "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "FAILURE"}}}]},
    "author": {"__typename": "User", "login": "op"},
    "reviews": {"nodes": [{"databaseId": 7, "author": {"__typename": "Bot", "login": "gemini-code-assist"}, "state": "COMMENTED"}]}},
  "pr2": null, "pr3": null, "pr4": {"state": "OPEN", "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}}}},
 "errors": [{"type": "NOT_FOUND", "path": ["repository", "pr2"]}, {"type": "INTERNAL", "path": ["repository", "pr3"]},
  {"type": "INTERNAL", "path": ["repository", "pr4", "commits", "nodes", 0, "commit", "statusCheckRollup"]}]}`
	got, err := parseSnapshots([]byte(body), []int{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if s := got[1]; s == nil || s.CI != "failing" || s.Conflicts != "yes" || s.BaseSHA != "b" ||
		s.Activity == nil || s.Activity.Operator != "op" || s.Activity.Reviews[0].Author != "gemini-code-assist[bot]" {
		t.Errorf("PR 1 = %+v", got[1])
	}
	if s := got[2]; s == nil || s.State != StateNoAccess {
		t.Errorf("NOT_FOUND PR 2 = %+v, want NOACCESS", got[2])
	}
	if s, ok := got[3]; ok {
		t.Errorf("PR 3, null for an internal error, = %+v, want left out", s)
	}
	if s, ok := got[4]; ok {
		t.Errorf("PR 4, whose check rollup failed, = %+v, want left out", s)
	}
}
