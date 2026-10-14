package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLsRemoteAllowProtocol: with GIT_ALLOW_PROTOCOL restricted, an ext::
// repo runs nothing, even where git config would allow the transport.
func TestLsRemoteAllowProtocol(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.ext.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	repo := "ext::touch " + marker
	if _, err := LsRemote(context.Background(), repo, "refs/heads/main", "GIT_ALLOW_PROTOCOL=https:ssh:git:file"); err == nil {
		t.Error("ls-remote of an ext:: repo succeeded")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran the ext:: command despite GIT_ALLOW_PROTOCOL")
	}
	// Without the restriction the same repo does run: the check above is real.
	_, _ = LsRemote(context.Background(), repo, "refs/heads/main")
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("this git does not run ext:: even when allowed (%v); the restriction is untested here", err)
	}
}
