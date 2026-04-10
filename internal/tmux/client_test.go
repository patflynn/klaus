package tmux

import "testing"

// Compile-time check that ExecClient satisfies the Client interface.
var _ Client = (*ExecClient)(nil)

func TestNewExecClient(t *testing.T) {
	c := NewExecClient()
	if c == nil {
		t.Fatal("NewExecClient returned nil")
	}
}
