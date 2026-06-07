package github

import "testing"

func TestOwnerRepoFromPRURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"standard PR URL", "https://github.com/patflynn/klaus/pull/270", "patflynn/klaus"},
		{"http scheme", "http://github.com/owner/repo/pull/1", "owner/repo"},
		{"trailing slash after repo", "https://github.com/owner/repo/", "owner/repo"},
		{"owner/repo only", "https://github.com/owner/repo", "owner/repo"},
		{"bare short name returns empty", "klaus", ""},
		{"malformed URL returns empty", "not-a-url", ""},
		{"owner only returns empty", "https://github.com/owner", ""},
		{"empty string returns empty", "", ""},
		{"empty repo segment returns empty", "https://github.com/owner//pull/1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OwnerRepoFromPRURL(tt.url); got != tt.want {
				t.Errorf("OwnerRepoFromPRURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
