package github

import "testing"

func TestParseBehindBy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"zero", "0", 0},
		{"positive", "2", 2},
		{"trailing newline", "5\n", 5},
		{"surrounding whitespace", "  3 \n", 3},
		{"empty", "", 0},
		{"non-numeric", "null", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseBehindBy(tt.in); got != tt.want {
				t.Errorf("parseBehindBy(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRefsLine(t *testing.T) {
	tests := []struct {
		name                        string
		in                          string
		wantBase, wantHead, wantOwn string
		wantOK                      bool
	}{
		{"same-repo", "main\tfeature\t", "main", "feature", "", true},
		{"fork", "main\tfeature\tforkowner", "main", "feature", "forkowner", true},
		{"trailing newline", "main\tfeature\t\n", "main", "feature", "", true},
		{"missing head", "main\t\t", "", "", "", false},
		{"missing base", "\tfeature\t", "", "", "", false},
		{"blank", "", "", "", "", false},
		{"single field", "main", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, head, own, ok := parseRefsLine(tt.in)
			if base != tt.wantBase || head != tt.wantHead || own != tt.wantOwn || ok != tt.wantOK {
				t.Errorf("parseRefsLine(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
					tt.in, base, head, own, ok, tt.wantBase, tt.wantHead, tt.wantOwn, tt.wantOK)
			}
		})
	}
}
