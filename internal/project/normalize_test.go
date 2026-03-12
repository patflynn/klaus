package project

import (
	"testing"
)

func TestNormalizeRepoName(t *testing.T) {
	reg := &Registry{
		Projects: map[string]string{
			"cosmo":  "/home/user/src/cosmo",
			"klaus":  "/home/user/src/klaus",
		},
	}

	tests := []struct {
		name string
		ref  string
		reg  *Registry
		want string
	}{
		{
			name: "project name stays as-is",
			ref:  "cosmo",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "owner/repo matching project returns project name",
			ref:  "patflynn/cosmo",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "owner/repo not matching project stays as owner/repo",
			ref:  "patflynn/other-thing",
			reg:  reg,
			want: "patflynn/other-thing",
		},
		{
			name: "full HTTPS URL matching project returns project name",
			ref:  "https://github.com/patflynn/cosmo",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "full HTTPS URL with .git matching project",
			ref:  "https://github.com/patflynn/cosmo.git",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "SSH URL matching project returns project name",
			ref:  "git@github.com:patflynn/klaus.git",
			reg:  reg,
			want: "klaus",
		},
		{
			name: "full URL not matching project returns owner/repo",
			ref:  "https://github.com/someorg/newrepo",
			reg:  reg,
			want: "someorg/newrepo",
		},
		{
			name: "empty ref returns empty",
			ref:  "",
			reg:  reg,
			want: "",
		},
		{
			name: "nil registry with project-like name",
			ref:  "cosmo",
			reg:  nil,
			want: "cosmo",
		},
		{
			name: "nil registry with owner/repo",
			ref:  "patflynn/cosmo",
			reg:  nil,
			want: "patflynn/cosmo",
		},
		{
			name: "owner/repo with trailing slash",
			ref:  "patflynn/cosmo/",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "HTTP URL (not HTTPS)",
			ref:  "http://github.com/patflynn/cosmo",
			reg:  reg,
			want: "cosmo",
		},
		{
			name: "unknown bare name with no registry match",
			ref:  "unknown-project",
			reg:  reg,
			want: "unknown-project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeRepoName(tt.ref, tt.reg)
			if got != tt.want {
				t.Errorf("NormalizeRepoName(%q) = %q, want %q", tt.ref, got, tt.want)
			}
		})
	}
}
