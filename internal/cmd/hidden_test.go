package cmd

import "testing"

func TestExtractPRNumberFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "standard PR URL",
			url:  "https://github.com/owner/repo/pull/123",
			want: "123",
		},
		{
			name: "PR URL with trailing slash",
			url:  "https://github.com/owner/repo/pull/456/",
			want: "456",
		},
		{
			name: "PR URL with query params",
			url:  "https://github.com/owner/repo/pull/789?diff=split",
			want: "789",
		},
		{
			name: "PR URL with fragment",
			url:  "https://github.com/owner/repo/pull/42#discussion_r123",
			want: "42",
		},
		{
			name: "PR URL with files path",
			url:  "https://github.com/owner/repo/pull/99/files",
			want: "99",
		},
		{
			name: "not a PR URL",
			url:  "https://github.com/owner/repo/issues/123",
			want: "",
		},
		{
			name: "empty string",
			url:  "",
			want: "",
		},
		{
			name: "no number after pull",
			url:  "https://github.com/owner/repo/pull/",
			want: "",
		},
		{
			name: "large PR number",
			url:  "https://github.com/owner/repo/pull/12345",
			want: "12345",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRNumberFromURL(tt.url)
			if got != tt.want {
				t.Errorf("extractPRNumberFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestExtractPRURL(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "URL in sentence",
			text: "I created a PR at https://github.com/owner/repo/pull/42 for review.",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "URL with trailing period",
			text: "See https://github.com/owner/repo/pull/99.",
			want: "https://github.com/owner/repo/pull/99",
		},
		{
			name: "no PR URL",
			text: "This is just some text without a PR link.",
			want: "",
		},
		{
			name: "issue URL not matched",
			text: "Check https://github.com/owner/repo/issues/10 for details.",
			want: "",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "URL only",
			text: "https://github.com/owner/repo/pull/1",
			want: "https://github.com/owner/repo/pull/1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRURL(tt.text)
			if got != tt.want {
				t.Errorf("extractPRURL(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}
