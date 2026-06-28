package cmd

import (
	"reflect"
	"testing"
)

func TestBrowserOpenCmd(t *testing.T) {
	const url = "https://github.com/o/r/pull/42"
	cases := []struct {
		goos     string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{"linux", "xdg-open", []string{url}, false},
		{"darwin", "open", []string{url}, false},
		{"windows", "cmd", []string{"/c", "start", url}, false},
		{"plan9", "", nil, true},
	}
	for _, c := range cases {
		name, args, err := browserOpenCmd(c.goos, url)
		if c.wantErr {
			if err == nil {
				t.Errorf("browserOpenCmd(%q): expected error, got nil", c.goos)
			}
			continue
		}
		if err != nil {
			t.Errorf("browserOpenCmd(%q): unexpected error: %v", c.goos, err)
			continue
		}
		if name != c.wantName {
			t.Errorf("browserOpenCmd(%q) name = %q, want %q", c.goos, name, c.wantName)
		}
		if !reflect.DeepEqual(args, c.wantArgs) {
			t.Errorf("browserOpenCmd(%q) args = %v, want %v", c.goos, args, c.wantArgs)
		}
	}
}
