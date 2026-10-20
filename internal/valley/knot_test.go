package valley

import "testing"

// TestParseKnotRef: --issue takes a knot only in the knot ID form, alone or
// behind a project; anything else, a GitHub issue number included, is left
// to GitHub.
func TestParseKnotRef(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want KnotRef
		ok   bool
	}{
		{"oc-897917f", KnotRef{ID: "oc-897917f"}, true},
		{"bd-086ef7b", KnotRef{ID: "bd-086ef7b"}, true},
		{"ida-0123abc", KnotRef{ID: "ida-0123abc"}, true},
		{"qn-fedcba9", KnotRef{ID: "qn-fedcba9"}, true},
		{"the-valley:oc-897917f", KnotRef{Project: "the-valley", ID: "oc-897917f"}, true},
		{"42", KnotRef{}, false},
		{"oc-897917", KnotRef{}, false},   // six digits
		{"oc-897917fa", KnotRef{}, false}, // eight
		{"oc-897917F", KnotRef{}, false},  // not lowercase hex
		{"oc-89791zz", KnotRef{}, false},
		{"xx-897917f", KnotRef{}, false},
		{":oc-897917f", KnotRef{}, false},
		{"a b:oc-897917f", KnotRef{}, false},
	} {
		got, ok := ParseKnotRef(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseKnotRef(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
		if ok && got.String() != tc.in {
			t.Errorf("%+v.String() = %q, want %q", got, got.String(), tc.in)
		}
	}
}
