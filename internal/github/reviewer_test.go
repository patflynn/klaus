package github

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeGH answers the two gh calls the resolver makes and records every call.
type fakeGH struct {
	user          string
	userErr       error
	collaborators string // raw `gh api --paginate` output
	collabErr     error
	calls         [][]string
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch {
	case reflect.DeepEqual(args, []string{"api", "user", "-q", ".login"}):
		return []byte(f.user + "\n"), f.userErr
	case len(args) == 3 && args[0] == "api" && args[1] == "--paginate":
		return []byte(f.collaborators), f.collabErr
	}
	return nil, fmt.Errorf("unexpected gh call: %v", args)
}

func TestReviewerResolverResolve(t *testing.T) {
	tests := []struct {
		name         string
		repo         string
		configured   string
		gh           fakeGH
		want         string
		wantWarning  []string // substrings; nil means no warning
		wantNoGHCall bool
	}{
		{
			name:         "configured pr_reviewer wins without gh",
			repo:         "patflynn/klaus",
			configured:   "carol",
			want:         "carol",
			wantNoGHCall: true,
		},
		{
			name: "owner picked over other writers",
			repo: "patflynn/klaus",
			gh: fakeGH{user: "patflynn-agent", collaborators: `[
				{"login":"alice","type":"User"},
				{"login":"patflynn-agent","type":"User"},
				{"login":"PatFlynn","type":"User"},
				{"login":"bob","type":"User"}]`},
			want: "PatFlynn",
		},
		{
			// Two pages, as --paginate prints them: arrays back to back.
			name: "single non-author writer when owner is an org",
			repo: "acme/widget",
			gh: fakeGH{user: "acme-agent", collaborators: `[{"login":"acme-agent","type":"User"},{"login":"renovate","type":"Bot"}]
[{"login":"alice","type":"User"}]`},
			want: "alice",
		},
		{
			name: "author and bots are excluded even when they own the repo",
			repo: "acme/widget",
			gh: fakeGH{user: "Acme", collaborators: `[
				{"login":"acme","type":"User"},
				{"login":"acme-bot","type":"Bot"},
				{"login":"alice","type":"User"}]`},
			want: "alice",
		},
		{
			name: "several candidates and no owner",
			repo: "acme/widget",
			gh: fakeGH{user: "acme-agent", collaborators: `[
				{"login":"alice","type":"User"},
				{"login":"acme-agent","type":"User"},
				{"login":"bob","type":"User"}]`},
			wantWarning: []string{"alice, bob", "pr_reviewer"},
		},
		{
			name:        "nobody but the author can approve",
			repo:        "acme/widget",
			gh:          fakeGH{user: "acme-agent", collaborators: `[{"login":"acme-agent","type":"User"},{"login":"ci","type":"Bot"}]`},
			wantWarning: []string{"acme-agent", "pr_reviewer"},
		},
		{
			name:        "gh user lookup fails",
			repo:        "acme/widget",
			gh:          fakeGH{userErr: errors.New("exit status 4: gh auth login\nto authenticate")},
			wantWarning: []string{"gh auth login to authenticate", "pr_reviewer"},
		},
		{
			name:        "collaborators call fails",
			repo:        "acme/widget",
			gh:          fakeGH{user: "acme-agent", collabErr: errors.New("HTTP 403: Must have push access")},
			wantWarning: []string{"HTTP 403", "pr_reviewer"},
		},
		{
			name:        "unknown repo",
			repo:        "",
			wantWarning: []string{"origin", "pr_reviewer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewerResolver{Run: tt.gh.run}
			got, warning := r.Resolve(context.Background(), tt.repo, tt.configured)
			if got != tt.want {
				t.Errorf("reviewer = %q, want %q (warning %q)", got, tt.want, warning)
			}
			if tt.wantWarning == nil && warning != "" {
				t.Errorf("unexpected warning %q", warning)
			}
			for _, sub := range tt.wantWarning {
				if !strings.Contains(warning, sub) {
					t.Errorf("warning %q does not mention %q", warning, sub)
				}
			}
			if strings.Contains(warning, "\n") {
				t.Errorf("warning spans several lines: %q", warning)
			}
			if tt.wantNoGHCall && len(tt.gh.calls) > 0 {
				t.Errorf("gh called %v, want no calls", tt.gh.calls)
			}
		})
	}
}

// The collaborators query is the one contract with GitHub that the fake can't
// check for itself: push access, direct and outside collaborators alike, every
// page.
func TestReviewerResolverQueriesPushCollaborators(t *testing.T) {
	f := &fakeGH{user: "acme-agent", collaborators: `[{"login":"alice","type":"User"}]`}
	r := &ReviewerResolver{Run: f.run}
	r.Resolve(context.Background(), "acme/widget", "")
	r.Resolve(context.Background(), "acme/gadget", "")

	want := [][]string{
		{"api", "user", "-q", ".login"},
		{"api", "--paginate", "repos/acme/widget/collaborators?permission=push&affiliation=all"},
		{"api", "--paginate", "repos/acme/gadget/collaborators?permission=push&affiliation=all"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("gh calls = %v, want %v (the gh user is looked up once)", f.calls, want)
	}
}
