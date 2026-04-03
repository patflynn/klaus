package github

import (
	"fmt"
	"testing"
)

func TestFetchReviewThreads(t *testing.T) {
	responseJSON := `{
		"data": {
			"repository": {
				"pullRequest": {
					"reviewThreads": {
						"nodes": [
							{"id": "RT_abc", "isResolved": false},
							{"id": "RT_def", "isResolved": true},
							{"id": "RT_ghi", "isResolved": false}
						]
					}
				}
			}
		}
	}`

	runner := func(query string) ([]byte, error) {
		return []byte(responseJSON), nil
	}

	threads, err := fetchReviewThreadsWithRunner("ignored", runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 3 {
		t.Fatalf("expected 3 threads, got %d", len(threads))
	}
	if threads[0].ID != "RT_abc" || threads[0].IsResolved {
		t.Errorf("thread 0: got %+v", threads[0])
	}
	if threads[1].ID != "RT_def" || !threads[1].IsResolved {
		t.Errorf("thread 1: got %+v", threads[1])
	}
	if threads[2].ID != "RT_ghi" || threads[2].IsResolved {
		t.Errorf("thread 2: got %+v", threads[2])
	}
}

func TestFetchReviewThreadsEmpty(t *testing.T) {
	responseJSON := `{
		"data": {
			"repository": {
				"pullRequest": {
					"reviewThreads": {
						"nodes": []
					}
				}
			}
		}
	}`

	runner := func(query string) ([]byte, error) {
		return []byte(responseJSON), nil
	}

	threads, err := fetchReviewThreadsWithRunner("ignored", runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 0 {
		t.Fatalf("expected 0 threads, got %d", len(threads))
	}
}

func TestFetchReviewThreadsAPIError(t *testing.T) {
	runner := func(query string) ([]byte, error) {
		return nil, fmt.Errorf("gh api graphql: exit status 1: not found")
	}

	_, err := fetchReviewThreadsWithRunner("ignored", runner)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveReviewThread(t *testing.T) {
	responseJSON := `{
		"data": {
			"resolveReviewThread": {
				"thread": {
					"isResolved": true
				}
			}
		}
	}`

	var calledQuery string
	runner := func(query string) ([]byte, error) {
		calledQuery = query
		return []byte(responseJSON), nil
	}

	err := resolveReviewThreadWithRunner("RT_abc", runner)
	if err != nil {
		t.Fatal(err)
	}
	if calledQuery == "" {
		t.Error("expected query to be called")
	}
}

func TestResolveReviewThreadGraphQLError(t *testing.T) {
	responseJSON := `{
		"data": null,
		"errors": [{"message": "Could not resolve thread"}]
	}`

	runner := func(query string) ([]byte, error) {
		return []byte(responseJSON), nil
	}

	err := resolveReviewThreadWithRunner("RT_abc", runner)
	if err == nil {
		t.Fatal("expected error from GraphQL error response")
	}
	if err.Error() != "GraphQL error: Could not resolve thread" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveReviewThreadAPIError(t *testing.T) {
	runner := func(query string) ([]byte, error) {
		return nil, fmt.Errorf("gh api graphql: exit status 1: forbidden")
	}

	err := resolveReviewThreadWithRunner("RT_abc", runner)
	if err == nil {
		t.Fatal("expected error")
	}
}
