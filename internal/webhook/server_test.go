package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseCheckRun(t *testing.T) {
	payload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "success",
			"pull_requests": [{"number": 42}, {"number": 43}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	events := parseEvent("check_run", json.RawMessage(payload))
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].PRNumber != "42" || events[0].CI != "passing" || events[0].Repo != "owner/repo" {
		t.Errorf("unexpected event[0]: %+v", events[0])
	}
	if events[1].PRNumber != "43" {
		t.Errorf("unexpected event[1] PRNumber: %s", events[1].PRNumber)
	}
}

func TestParseCheckRunFailure(t *testing.T) {
	payload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "failure",
			"pull_requests": [{"number": 10}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	events := parseEvent("check_run", json.RawMessage(payload))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].CI != "failing" {
		t.Errorf("expected CI=failing, got %s", events[0].CI)
	}
}

func TestParseCheckRunIgnoresNonCompleted(t *testing.T) {
	payload := `{
		"action": "created",
		"check_run": {
			"conclusion": "success",
			"pull_requests": [{"number": 1}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	events := parseEvent("check_run", json.RawMessage(payload))
	if len(events) != 0 {
		t.Errorf("expected 0 events for non-completed action, got %d", len(events))
	}
}

func TestParseCheckSuite(t *testing.T) {
	payload := `{
		"action": "completed",
		"check_suite": {
			"conclusion": "failure",
			"pull_requests": [{"number": 99}]
		},
		"repository": {"full_name": "org/project"}
	}`

	events := parseEvent("check_suite", json.RawMessage(payload))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].CI != "failing" || events[0].PRNumber != "99" || events[0].Repo != "org/project" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestParsePullRequest(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantLen  int
		wantState string
		wantConflicts string
	}{
		{
			name: "opened",
			payload: `{
				"action": "opened",
				"pull_request": {"number": 5, "state": "open", "merged": false, "mergeable": true},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1, wantState: "OPEN", wantConflicts: "none",
		},
		{
			name: "closed with merge",
			payload: `{
				"action": "closed",
				"pull_request": {"number": 5, "state": "closed", "merged": true},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1, wantState: "MERGED",
		},
		{
			name: "has conflicts",
			payload: `{
				"action": "synchronize",
				"pull_request": {"number": 5, "state": "open", "merged": false, "mergeable": false, "mergeable_state": "dirty"},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1, wantState: "OPEN", wantConflicts: "yes",
		},
		{
			name: "ignored action",
			payload: `{
				"action": "labeled",
				"pull_request": {"number": 5, "state": "open", "merged": false},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := parseEvent("pull_request", json.RawMessage(tt.payload))
			if len(events) != tt.wantLen {
				t.Fatalf("expected %d events, got %d", tt.wantLen, len(events))
			}
			if tt.wantLen > 0 {
				if events[0].State != tt.wantState {
					t.Errorf("expected State=%s, got %s", tt.wantState, events[0].State)
				}
				if tt.wantConflicts != "" && events[0].Conflicts != tt.wantConflicts {
					t.Errorf("expected Conflicts=%s, got %s", tt.wantConflicts, events[0].Conflicts)
				}
			}
		})
	}
}

func TestParsePullRequestReview(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantLen    int
		wantDecision string
	}{
		{
			name: "approved",
			payload: `{
				"action": "submitted",
				"review": {"state": "approved"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1, wantDecision: "APPROVED",
		},
		{
			name: "changes requested",
			payload: `{
				"action": "submitted",
				"review": {"state": "changes_requested"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1, wantDecision: "CHANGES_REQUESTED",
		},
		{
			name: "commented (ignored)",
			payload: `{
				"action": "submitted",
				"review": {"state": "commented"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := parseEvent("pull_request_review", json.RawMessage(tt.payload))
			if len(events) != tt.wantLen {
				t.Fatalf("expected %d events, got %d", tt.wantLen, len(events))
			}
			if tt.wantLen > 0 && events[0].ReviewDecision != tt.wantDecision {
				t.Errorf("expected ReviewDecision=%s, got %s", tt.wantDecision, events[0].ReviewDecision)
			}
		})
	}
}

func TestParsePush(t *testing.T) {
	t.Run("default branch push", func(t *testing.T) {
		payload := `{
			"ref": "refs/heads/main",
			"repository": {"full_name": "owner/repo", "default_branch": "main"}
		}`
		events := parseEvent("push", json.RawMessage(payload))
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0].Repo != "owner/repo" || events[0].Conflicts != "unknown" || events[0].PRNumber != "" {
			t.Errorf("unexpected event: %+v", events[0])
		}
	})

	t.Run("non-default branch push ignored", func(t *testing.T) {
		payload := `{
			"ref": "refs/heads/feature",
			"repository": {"full_name": "owner/repo", "default_branch": "main"}
		}`
		events := parseEvent("push", json.RawMessage(payload))
		if len(events) != 0 {
			t.Errorf("expected 0 events, got %d", len(events))
		}
	})
}

func TestServerHandleWebhook(t *testing.T) {
	ch := make(chan Event, 10)
	srv := NewServer(0, "/webhook/github", ch)

	// Build the handler directly for testing
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", srv.handleWebhook)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	payload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "success",
			"pull_requests": [{"number": 55}]
		},
		"repository": {"full_name": "test/repo"}
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/github", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "check_run")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	select {
	case ev := <-ch:
		if ev.PRNumber != "55" || ev.CI != "passing" || ev.Repo != "test/repo" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestServerRejectsGet(t *testing.T) {
	ch := make(chan Event, 10)
	srv := NewServer(0, "/webhook/github", ch)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", srv.handleWebhook)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/webhook/github")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestServerRejectsMissingEventHeader(t *testing.T) {
	ch := make(chan Event, 10)
	srv := NewServer(0, "/webhook/github", ch)

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", srv.handleWebhook)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/github", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	// No X-GitHub-Event header

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestPartialStateMerge(t *testing.T) {
	existing := map[string]*Event{
		"42": {PRNumber: "42", CI: "failing", State: "OPEN", Conflicts: "none", ReviewDecision: ""},
	}

	// A check_run event only updates CI
	update := Event{PRNumber: "42", Repo: "owner/repo", CI: "passing"}
	MergeEvent(existing, update)

	got := existing["42"]
	if got.CI != "passing" {
		t.Errorf("expected CI=passing after merge, got %s", got.CI)
	}
	if got.State != "OPEN" {
		t.Errorf("expected State=OPEN preserved, got %s", got.State)
	}
	if got.Conflicts != "none" {
		t.Errorf("expected Conflicts=none preserved, got %s", got.Conflicts)
	}
}

func TestPartialStateMergeNewPR(t *testing.T) {
	existing := map[string]*Event{}

	update := Event{PRNumber: "99", Repo: "owner/repo", CI: "passing"}
	MergeEvent(existing, update)

	got, ok := existing["99"]
	if !ok {
		t.Fatal("expected PR 99 to be added")
	}
	if got.CI != "passing" {
		t.Errorf("expected CI=passing, got %s", got.CI)
	}
}

func TestMapConclusion(t *testing.T) {
	tests := []struct {
		conclusion string
		want       string
	}{
		{"success", "passing"},
		{"neutral", "passing"},
		{"skipped", "passing"},
		{"failure", "failing"},
		{"timed_out", "failing"},
		{"cancelled", "failing"},
		{"action_required", "failing"},
		{"", "pending"},
		{"stale", "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.conclusion, func(t *testing.T) {
			if got := mapConclusion(tt.conclusion); got != tt.want {
				t.Errorf("mapConclusion(%q) = %s, want %s", tt.conclusion, got, tt.want)
			}
		})
	}
}
