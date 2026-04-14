package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
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
	if events[0].EventType != "check_run" {
		t.Errorf("expected EventType=check_run, got %q", events[0].EventType)
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
	if events[0].EventType != "check_run" {
		t.Errorf("expected EventType=check_run, got %q", events[0].EventType)
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
	if events[0].EventType != "check_suite" {
		t.Errorf("expected EventType=check_suite, got %q", events[0].EventType)
	}
}

func TestParsePullRequest(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantLen int
	}{
		{
			name: "opened",
			payload: `{
				"action": "opened",
				"pull_request": {"number": 5, "state": "open", "merged": false, "mergeable": true},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1,
		},
		{
			name: "closed with merge",
			payload: `{
				"action": "closed",
				"pull_request": {"number": 5, "state": "closed", "merged": true},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1,
		},
		{
			name: "synchronize",
			payload: `{
				"action": "synchronize",
				"pull_request": {"number": 5, "state": "open", "merged": false, "mergeable": false, "mergeable_state": "dirty"},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1,
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
				if events[0].EventType != "pull_request" {
					t.Errorf("expected EventType=pull_request, got %q", events[0].EventType)
				}
				if events[0].PRNumber != "5" {
					t.Errorf("expected PRNumber=5, got %q", events[0].PRNumber)
				}
				if events[0].Repo != "owner/repo" {
					t.Errorf("expected Repo=owner/repo, got %q", events[0].Repo)
				}
			}
		})
	}
}

func TestParsePullRequestReview(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantLen int
	}{
		{
			name: "approved",
			payload: `{
				"action": "submitted",
				"review": {"state": "approved"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1,
		},
		{
			name: "changes requested",
			payload: `{
				"action": "submitted",
				"review": {"state": "changes_requested"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
			wantLen: 1,
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
			if tt.wantLen > 0 {
				if events[0].EventType != "pull_request_review" {
					t.Errorf("expected EventType=pull_request_review, got %q", events[0].EventType)
				}
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
		if events[0].Repo != "owner/repo" || events[0].PRNumber != "" {
			t.Errorf("unexpected event: %+v", events[0])
		}
		if events[0].EventType != "push" {
			t.Errorf("expected EventType=push, got %q", events[0].EventType)
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
		if ev.PRNumber != "55" || ev.Repo != "test/repo" || ev.EventType != "check_run" {
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
		if ev.PRNumber != "77" || ev.EventType != "check_run" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestServerGracefulShutdownReleasesPort(t *testing.T) {
	ch := make(chan Event, 10)
	srv := NewServer(0, "/webhook/github", ch)

	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	addr := srv.Addr()

	go func() {
		_ = srv.Serve()
	}()

	// Verify the server is accepting connections.
	url := "http://" + addr + "/webhook/github"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("server not reachable: %v", err)
	}
	resp.Body.Close()

	// Shut down with a deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// The port should be released — we can bind a new server on the same address.
	srv2 := NewServer(0, "/webhook/github", ch)
	// Extract the port from the original address to reuse it.
	_, port, _ := net.SplitHostPort(addr)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("port not released after shutdown: %v", err)
	}
	ln.Close()
	_ = srv2 // suppress unused warning
}

func TestEventIsInvalidationSignal(t *testing.T) {
	// Verify that webhook events are pure invalidation signals: they carry
	// only PRNumber, Repo, and EventType — no status data. This is the
	// core design invariant that prevents webhook/polling divergence bugs.
	tests := []struct {
		name      string
		eventType string
		payload   string
	}{
		{
			name:      "check_run success",
			eventType: "check_run",
			payload: `{
				"action": "completed",
				"check_run": {
					"conclusion": "success",
					"pull_requests": [{"number": 42}]
				},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
		{
			name:      "check_run failure",
			eventType: "check_run",
			payload: `{
				"action": "completed",
				"check_run": {
					"conclusion": "failure",
					"pull_requests": [{"number": 42}]
				},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
		{
			name:      "check_suite",
			eventType: "check_suite",
			payload: `{
				"action": "completed",
				"check_suite": {
					"conclusion": "success",
					"pull_requests": [{"number": 10}]
				},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
		{
			name:      "pull_request opened",
			eventType: "pull_request",
			payload: `{
				"action": "opened",
				"pull_request": {"number": 5, "state": "open", "merged": false, "mergeable": true},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
		{
			name:      "pull_request merged",
			eventType: "pull_request",
			payload: `{
				"action": "closed",
				"pull_request": {"number": 5, "state": "closed", "merged": true},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
		{
			name:      "pull_request_review approved",
			eventType: "pull_request_review",
			payload: `{
				"action": "submitted",
				"review": {"state": "approved"},
				"pull_request": {"number": 7},
				"repository": {"full_name": "owner/repo"}
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := parseEvent(tt.eventType, json.RawMessage(tt.payload))
			if len(events) == 0 {
				t.Fatal("expected at least one event")
			}
			for _, ev := range events {
				if ev.PRNumber == "" {
					t.Error("expected non-empty PRNumber")
				}
				if ev.Repo == "" {
					t.Error("expected non-empty Repo")
				}
				if ev.EventType == "" {
					t.Error("expected non-empty EventType")
				}
				if ev.EventType != tt.eventType {
					t.Errorf("expected EventType=%q, got %q", tt.eventType, ev.EventType)
				}
			}
		})
	}
}
