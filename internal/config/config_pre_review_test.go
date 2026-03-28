package config

import (
	"testing"
)

func TestPreReviewEnabled_defaults(t *testing.T) {
	cfg := Defaults()
	if !cfg.PreReviewEnabled() {
		t.Error("expected pre-review to be enabled by default")
	}
}

func TestPreReviewEnabled_explicit(t *testing.T) {
	enabled := true
	cfg := Config{PreReview: &PreReviewConfig{Enabled: &enabled}}
	if !cfg.PreReviewEnabled() {
		t.Error("expected pre-review to be enabled")
	}

	disabled := false
	cfg = Config{PreReview: &PreReviewConfig{Enabled: &disabled}}
	if cfg.PreReviewEnabled() {
		t.Error("expected pre-review to be disabled")
	}
}

func TestPreReviewModel_defaults(t *testing.T) {
	cfg := Defaults()
	if got := cfg.PreReviewModel(); got != "haiku" {
		t.Errorf("PreReviewModel() = %q, want %q", got, "haiku")
	}
}

func TestPreReviewModel_configured(t *testing.T) {
	cfg := Config{PreReview: &PreReviewConfig{ReviewModel: "sonnet"}}
	if got := cfg.PreReviewModel(); got != "sonnet" {
		t.Errorf("PreReviewModel() = %q, want %q", got, "sonnet")
	}
}

func TestPreReviewLinters_defaults(t *testing.T) {
	cfg := Defaults()
	if linters := cfg.PreReviewLinters(); linters != nil {
		t.Errorf("expected nil linters by default, got %v", linters)
	}
}

func TestPreReviewLinters_configured(t *testing.T) {
	cfg := Config{PreReview: &PreReviewConfig{Linters: []string{"go vet ./..."}}}
	linters := cfg.PreReviewLinters()
	if len(linters) != 1 || linters[0] != "go vet ./..." {
		t.Errorf("unexpected linters: %v", linters)
	}
}

func TestPreReviewBlockOn_defaults(t *testing.T) {
	cfg := Defaults()
	if got := cfg.PreReviewBlockOn(); got != "high" {
		t.Errorf("PreReviewBlockOn() = %q, want %q", got, "high")
	}
}

func TestPreReviewBlockOn_configured(t *testing.T) {
	cfg := Config{PreReview: &PreReviewConfig{BlockOn: "critical"}}
	if got := cfg.PreReviewBlockOn(); got != "critical" {
		t.Errorf("PreReviewBlockOn() = %q, want %q", got, "critical")
	}
}

func TestPreReviewMaxFixRounds_defaults(t *testing.T) {
	cfg := Defaults()
	if got := cfg.PreReviewMaxFixRounds(); got != 2 {
		t.Errorf("PreReviewMaxFixRounds() = %d, want %d", got, 2)
	}
}

func TestPreReviewMaxFixRounds_configured(t *testing.T) {
	cfg := Config{PreReview: &PreReviewConfig{MaxFixRounds: 5}}
	if got := cfg.PreReviewMaxFixRounds(); got != 5 {
		t.Errorf("PreReviewMaxFixRounds() = %d, want %d", got, 5)
	}
}
