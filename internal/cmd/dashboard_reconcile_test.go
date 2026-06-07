package cmd

import (
	"testing"
	"time"

	"github.com/patflynn/klaus/internal/config"
)

// TestShouldScheduleReconcile verifies the heartbeat scheduling decision:
// the slow reconcile sweep runs only in webhook-only mode (webhook on,
// polling off) with a positive interval. It must NOT run when polling is
// active (polling already re-fetches every 30s) or in plain poll mode.
func TestShouldScheduleReconcile(t *testing.T) {
	const iv = 5 * time.Minute
	tests := []struct {
		name        string
		useWebhook  bool
		pollEnabled bool
		interval    time.Duration
		want        bool
	}{
		{"webhook only", true, false, iv, true},
		{"webhook plus polling (poll_fallback)", true, true, iv, false},
		{"poll only (no webhook)", false, true, iv, false},
		{"neither", false, false, iv, false},
		{"webhook only but disabled interval", true, false, 0, false},
		{"webhook only but negative interval", true, false, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldScheduleReconcile(tt.useWebhook, tt.pollEnabled, tt.interval)
			if got != tt.want {
				t.Errorf("shouldScheduleReconcile(%v, %v, %v) = %v, want %v",
					tt.useWebhook, tt.pollEnabled, tt.interval, got, tt.want)
			}
		})
	}
}

// TestReconcileTickAfterCmd verifies the defensive guard: a non-positive
// interval returns a nil command (heartbeat disabled), so the re-arm path can
// never spin tea.Tick into an immediate-fire infinite loop. A positive interval
// returns a real command.
func TestReconcileTickAfterCmd(t *testing.T) {
	if cmd := reconcileTickAfterCmd(0); cmd != nil {
		t.Errorf("reconcileTickAfterCmd(0) = non-nil, want nil")
	}
	if cmd := reconcileTickAfterCmd(-1 * time.Second); cmd != nil {
		t.Errorf("reconcileTickAfterCmd(-1s) = non-nil, want nil")
	}
	if cmd := reconcileTickAfterCmd(5 * time.Minute); cmd == nil {
		t.Errorf("reconcileTickAfterCmd(5m) = nil, want non-nil")
	}
}

// TestReconcileInterval verifies config resolution: zero/nil → default,
// negative → disabled (0), positive → that many seconds.
func TestReconcileInterval(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.WebhookConfig
		want time.Duration
	}{
		{"nil config uses default", nil, defaultReconcileInterval},
		{"zero uses default", &config.WebhookConfig{ReconcileIntervalSeconds: 0}, defaultReconcileInterval},
		{"negative disables", &config.WebhookConfig{ReconcileIntervalSeconds: -1}, 0},
		{"positive override", &config.WebhookConfig{ReconcileIntervalSeconds: 120}, 120 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcileInterval(tt.cfg); got != tt.want {
				t.Errorf("reconcileInterval(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}
