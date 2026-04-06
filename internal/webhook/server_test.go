package webhook

import (
	"bytes"
	"context"
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
	if events[0].PRNumber != "42" || events[0].Repo != "owner/repo" {
		t.Errorf("unexpected event[0]: %+v", events[0])
	}
	if !events[0].CheckRunCompleted {
		t.Error("expected CheckRunCompleted=true for check_run event")
	}
	if events[0].CI != "" {
		t.Errorf("check_run should not set CI directly, got %q", events[0].CI)
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
	if !events[0].CheckRunCompleted {
		t.Error("expected CheckRunCompleted=true")
	}
	if events[0].CI != "" {
		t.Errorf("check_run should not set CI directly, got %q", events[0].CI)
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
	if events[0].PRNumber != "99" || events[0].Repo != "org/project" {
		t.Errorf("unexpected event: %+v", events[0])
	}
	if !events[0].CheckRunCompleted {
		t.Error("expected CheckRunCompleted=true for check_suite event")
	}
	if events[0].CI != "" {
		t.Errorf("check_suite should not set CI directly, got %q", events[0].CI)
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
		if ev.PRNumber != "55" || ev.Repo != "test/repo" || !ev.CheckRunCompleted {
			t.Errorf("unexpected event: %+v", ev)
		}
		if ev.CI != "" {
			t.Errorf("check_run should not set CI directly, got %q", ev.CI)
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

	// A pull_request event updates State but preserves other fields.
	update := Event{PRNumber: "42", Repo: "owner/repo", State: "CLOSED"}
	MergeEvent(existing, update)

	got := existing["42"]
	if got.State != "CLOSED" {
		t.Errorf("expected State=CLOSED after merge, got %s", got.State)
	}
	if got.CI != "failing" {
		t.Errorf("expected CI=failing preserved, got %s", got.CI)
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

func TestServerListenThenServe(t *testing.T) {
	ch := make(chan Event, 10)
	srv := NewServer(0, "/webhook/github", ch)

	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	// Addr is available immediately after Listen, no race.
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("expected non-empty address after Listen")
	}

	go func() {
		_ = srv.Serve()
	}()
	defer srv.Shutdown(context.Background())

	// Send a webhook to the live server.
	payload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "success",
			"pull_requests": [{"number": 77}]
		},
		"repository": {"full_name": "test/repo"}
	}`
	url := "http://" + addr + "/webhook/github"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(payload))
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
		if ev.PRNumber != "77" || !ev.CheckRunCompleted {
			t.Errorf("unexpected event: %+v", ev)
		}
		if ev.CI != "" {
			t.Errorf("check_run should not set CI directly, got %q", ev.CI)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestCheckRunDoesNotOverwriteCI(t *testing.T) {
	// Simulate the scenario from issue #172: a PR has multiple checks.
	// A failing check completes first, then a passing check completes.
	// The dashboard should NOT show "passing" — it should re-poll for
	// aggregate CI status.
	//
	// Since check_run events no longer carry CI status, we verify that
	// multiple check_run events with different conclusions all produce
	// events with CheckRunCompleted=true and empty CI.

	failPayload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "failure",
			"pull_requests": [{"number": 42}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	passPayload := `{
		"action": "completed",
		"check_run": {
			"conclusion": "success",
			"pull_requests": [{"number": 42}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	failEvents := parseEvent("check_run", json.RawMessage(failPayload))
	passEvents := parseEvent("check_run", json.RawMessage(passPayload))

	// Both events should signal a re-poll, not carry CI status.
	for _, ev := range append(failEvents, passEvents...) {
		if ev.CI != "" {
			t.Errorf("check_run event should not set CI, got %q", ev.CI)
		}
		if !ev.CheckRunCompleted {
			t.Error("check_run event should set CheckRunCompleted=true")
		}
		if ev.PRNumber != "42" {
			t.Errorf("expected PRNumber=42, got %s", ev.PRNumber)
		}
	}

	// Verify that applying these events via MergeEvent does not change
	// an existing CI status (since CI is empty on the events).
	existing := map[string]*Event{
		"42": {PRNumber: "42", CI: "failing", State: "OPEN"},
	}
	for _, ev := range append(failEvents, passEvents...) {
		MergeEvent(existing, ev)
	}
	if existing["42"].CI != "failing" {
		t.Errorf("CI should remain 'failing' after check_run merges, got %q", existing["42"].CI)
	}
}

func TestCheckSuiteDoesNotOverwriteCI(t *testing.T) {
	payload := `{
		"action": "completed",
		"check_suite": {
			"conclusion": "success",
			"pull_requests": [{"number": 10}]
		},
		"repository": {"full_name": "owner/repo"}
	}`

	events := parseEvent("check_suite", json.RawMessage(payload))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].CI != "" {
		t.Errorf("check_suite should not set CI directly, got %q", events[0].CI)
	}
	if !events[0].CheckRunCompleted {
		t.Error("expected CheckRunCompleted=true")
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
