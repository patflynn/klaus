package config

import (
	"encoding/json"
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

func TestProseDefaults(t *testing.T) {
	cfg := Defaults()
	p := cfg.Prose()
	if p.TimeoutSeconds != 600 || p.MaxInputBytes != 40000 || !*p.Enabled {
		t.Errorf("Prose() = timeout %d, max input %d, enabled %v; want 600, 40000, true", p.TimeoutSeconds, p.MaxInputBytes, *p.Enabled)
	}
}

// The writer's model is pre_review.prose.writer's, else klaus's prose model
// for the backend (agy's alone), else the backend's general default; its
// effort skips the middle step. The checker's skips klaus's prose model.
func TestProseModels(t *testing.T) {
	for _, tc := range []struct {
		name, config, kind string
		writer, checker    [2]string // model, effort
	}{
		{"agy with no settings", `{}`, "agy",
			[2]string{"gemini-4-hydrogen-high", ""}, [2]string{"", ""}},
		{"agy over its general default", `{"backends": {"agy": {"model": "gemini-general", "effort": "low"}}}`, "agy",
			[2]string{"gemini-4-hydrogen-high", "low"}, [2]string{"gemini-general", "low"}},
		{"agy prose settings", `{"backends": {"agy": {"model": "gemini-general", "effort": "low"}},
			"pre_review": {"prose": {"writer": {"agy": {"model": "gemini-writer", "effort": "high"}}, "checker": {"agy": {"model": "gemini-checker"}}}}}`, "agy",
			[2]string{"gemini-writer", "high"}, [2]string{"gemini-checker", "low"}},
		{"codex has no prose model", `{"backends": {"codex": {"model": "gpt-general", "effort": "medium"}}}`, "codex",
			[2]string{"gpt-general", "medium"}, [2]string{"gpt-general", "medium"}},
		{"codex prose effort only", `{"backends": {"codex": {"model": "gpt-general"}},
			"pre_review": {"prose": {"writer": {"codex": {"effort": "high"}}}}}`, "codex",
			[2]string{"gpt-general", "high"}, [2]string{"gpt-general", ""}},
		{"another backend's prose settings", `{"pre_review": {"prose": {"writer": {"agy": {"model": "gemini-writer"}}}}, "default_agent_model": "sonnet"}`, "claude",
			[2]string{"sonnet", ""}, [2]string{"sonnet", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.config), &cfg); err != nil {
				t.Fatal(err)
			}
			if m, e := cfg.ProseWriter(tc.kind); [2]string{m, e} != tc.writer {
				t.Errorf("ProseWriter(%s) = %q, %q; want %q", tc.kind, m, e, tc.writer)
			}
			if m, e := cfg.ProseChecker(tc.kind); [2]string{m, e} != tc.checker {
				t.Errorf("ProseChecker(%s) = %q, %q; want %q", tc.kind, m, e, tc.checker)
			}
		})
	}
}
