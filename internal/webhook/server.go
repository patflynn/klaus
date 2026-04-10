package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Event is an invalidation signal from a GitHub webhook. It carries just
// enough information to know which PR (or repo) needs a fresh status fetch.
// The dashboard uses this to trigger an immediate re-fetch via the same
// code path that polling uses, rather than constructing status from the
// webhook payload directly.
type Event struct {
	PRNumber  string // PR number, e.g. "42"; empty for repo-wide events (e.g. push)
	Repo      string // owner/repo
	EventType string // "check_run", "check_suite", "pull_request", "pull_request_review", "push"
}

// Server is an HTTP server that receives GitHub webhook payloads from a relay
// and sends parsed events on a channel.
type Server struct {
	port     int
	path     string
	events   chan<- Event
	srv      *http.Server
	listener net.Listener
}

// NewServer creates a webhook server that sends parsed events to the given channel.
func NewServer(port int, path string, events chan<- Event) *Server {
	if path == "" {
		path = "/webhook/github"
	}
	return &Server{
		port:   port,
		path:   path,
		events: events,
	}
}

// Addr returns the listener address once the server is started. Returns ""
// if the server hasn't started yet.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return ""
}

// Listen binds the server to its port. Call this before Serve so the
// listener address is available without a data race.
func (s *Server) Listen() error {
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, s.handleWebhook)

	s.srv = &http.Server{
		Handler: mux,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.port))
	if err != nil {
		return fmt.Errorf("webhook server listen: %w", err)
	}
	s.listener = ln
	return nil
}

// Serve starts accepting connections on the already-bound listener.
// It blocks until the server is shut down. Listen must be called first.
func (s *Server) Serve() error {
	if s.listener == nil {
		return fmt.Errorf("webhook server: Listen must be called before Serve")
	}
	return s.srv.Serve(s.listener)
}

// Start binds the listener and serves. It blocks until the server is shut down.
func (s *Server) Start() error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "" {
		http.Error(w, "missing X-GitHub-Event header", http.StatusBadRequest)
		return
	}

	var payload json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	events := parseEvent(eventType, payload)
	for _, ev := range events {
		select {
		case s.events <- ev:
		default:
			// Drop event if channel is full to avoid blocking the HTTP handler.
		}
	}

	w.WriteHeader(http.StatusOK)
}

// parseEvent dispatches to the appropriate parser based on event type.
func parseEvent(eventType string, payload json.RawMessage) []Event {
	switch eventType {
	case "check_run":
		return parseCheckRun(payload)
	case "check_suite":
		return parseCheckSuite(payload)
	case "pull_request":
		return parsePullRequest(payload)
	case "pull_request_review":
		return parsePullRequestReview(payload)
	case "push":
		return parsePush(payload)
	default:
		return nil
	}
}

// GitHub webhook payload structures (only the fields we need).

type checkRunPayload struct {
	Action   string `json:"action"`
	CheckRun struct {
		Conclusion   string `json:"conclusion"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	Repository repoPayload `json:"repository"`
}

type checkSuitePayload struct {
	Action     string `json:"action"`
	CheckSuite struct {
		Conclusion   string `json:"conclusion"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	Repository repoPayload `json:"repository"`
}

type pullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number        int    `json:"number"`
		State         string `json:"state"`
		Merged        bool   `json:"merged"`
		Mergeable     *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
	} `json:"pull_request"`
	Repository repoPayload `json:"repository"`
}

type pullRequestReviewPayload struct {
	Action      string `json:"action"`
	Review      struct {
		State string `json:"state"`
	} `json:"review"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Repository repoPayload `json:"repository"`
}

type pushPayload struct {
	Ref        string      `json:"ref"`
	Repository repoPayload `json:"repository"`
}

type repoPayload struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

func parseCheckRun(payload json.RawMessage) []Event {
	var p checkRunPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.Action != "completed" {
		return nil
	}

	var events []Event
	for _, pr := range p.CheckRun.PullRequests {
		events = append(events, Event{
			PRNumber:  fmt.Sprintf("%d", pr.Number),
			Repo:      p.Repository.FullName,
			EventType: "check_run",
		})
	}
	return events
}

func parseCheckSuite(payload json.RawMessage) []Event {
	var p checkSuitePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.Action != "completed" {
		return nil
	}

	var events []Event
	for _, pr := range p.CheckSuite.PullRequests {
		events = append(events, Event{
			PRNumber:  fmt.Sprintf("%d", pr.Number),
			Repo:      p.Repository.FullName,
			EventType: "check_suite",
		})
	}
	return events
}

func parsePullRequest(payload json.RawMessage) []Event {
	var p pullRequestPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}

	switch p.Action {
	case "opened", "synchronize", "closed", "reopened", "merged":
		// valid actions
	default:
		return nil
	}

	return []Event{{
		PRNumber:  fmt.Sprintf("%d", p.PullRequest.Number),
		Repo:      p.Repository.FullName,
		EventType: "pull_request",
	}}
}

func parsePullRequestReview(payload json.RawMessage) []Event {
	var p pullRequestReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.Action != "submitted" {
		return nil
	}

	// Only signal for meaningful review states.
	switch strings.ToLower(p.Review.State) {
	case "approved", "changes_requested":
		// valid
	default:
		return nil
	}

	return []Event{{
		PRNumber:  fmt.Sprintf("%d", p.PullRequest.Number),
		Repo:      p.Repository.FullName,
		EventType: "pull_request_review",
	}}
}

func parsePush(payload json.RawMessage) []Event {
	var p pushPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}

	// Only care about pushes to the default branch
	defaultRef := "refs/heads/" + p.Repository.DefaultBranch
	if p.Ref != defaultRef {
		return nil
	}

	// Signal that the base branch changed — PRs may have new conflicts.
	// PRNumber is empty to indicate this is a repo-wide event.
	return []Event{{
		Repo:      p.Repository.FullName,
		EventType: "push",
	}}
}

