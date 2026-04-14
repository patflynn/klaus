package git

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunGitRespectsContextTimeout(t *testing.T) {
	// Use an already-expired context to verify the command fails fast.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	_, err := runGit(ctx, "", "sleep", "60")
	if err == nil {
		t.Fatal("expected error from timed-out context")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected timeout-related error, got: %v", err)
	}
}

func TestRunGitNetworkRespectsContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	_, err := runGitNetwork(ctx, "", "sleep", "60")
	if err == nil {
		t.Fatal("expected error from timed-out context")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected timeout-related error, got: %v", err)
	}
}

func TestEnsureTimeoutNoOverride(t *testing.T) {
	// If the context already has a shorter deadline, ensureTimeout should not extend it.
	shortTimeout := 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	newCtx, newCancel := ensureTimeout(ctx, 5*time.Minute)
	defer newCancel()

	deadline, ok := newCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining > 1*time.Second {
		t.Errorf("deadline should be close to original short timeout, but remaining = %v", remaining)
	}
}

func TestEnsureTimeoutAppliesDefault(t *testing.T) {
	ctx := context.Background()
	timeout := 42 * time.Second

	newCtx, cancel := ensureTimeout(ctx, timeout)
	defer cancel()

	deadline, ok := newCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline to be set")
	}
	remaining := time.Until(deadline)
	if remaining < 40*time.Second || remaining > 43*time.Second {
		t.Errorf("expected deadline ~42s from now, got %v", remaining)
	}
}
