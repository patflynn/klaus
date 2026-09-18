package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
)

func TestChooseReviewer(t *testing.T) {
	off := false
	tests := []struct {
		name      string
		onPath    []string
		author    backend.Kind
		cfg       config.Config
		wantKind  backend.Kind
		wantModel string
	}{
		{
			name:     "explicit review_backend wins even when missing from PATH",
			onPath:   []string{"claude"},
			author:   backend.Claude,
			cfg:      config.Config{BackendDefaults: map[string]config.BackendDefaults{"claude": {ReviewBackend: "codex"}, "codex": {ReviewModel: "m1"}}},
			wantKind: backend.Codex, wantModel: "m1",
		},
		{
			name:     "order skips the author",
			onPath:   []string{"claude", "codex", "agy"},
			author:   backend.Codex,
			wantKind: backend.Claude, wantModel: "haiku",
		},
		{
			name:     "order skips CLIs missing from PATH",
			onPath:   []string{"claude", "agy"},
			author:   backend.Agy,
			wantKind: backend.Claude, wantModel: "haiku",
		},
		{
			name:     "order selects agy with its configured model",
			onPath:   []string{"claude", "codex", "agy"},
			author:   backend.Claude,
			cfg:      config.Config{CrossReview: &config.CrossReviewConfig{Order: []string{"agy", "codex"}}, BackendDefaults: map[string]config.BackendDefaults{"agy": {ReviewModel: "m"}}},
			wantKind: backend.Agy, wantModel: "m",
		},
		{
			name:     "falls back to the author's family when nothing else is installed",
			onPath:   []string{"claude"},
			author:   backend.Claude,
			wantKind: backend.Claude, wantModel: "haiku",
		},
		{
			name:     "disabled keeps the author's family",
			onPath:   []string{"claude", "codex", "agy"},
			author:   backend.Codex,
			cfg:      config.Config{CrossReview: &config.CrossReviewConfig{Enabled: &off}},
			wantKind: backend.Codex,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, bin := range tt.onPath {
				if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			kind, model, err := ChooseReviewer(tt.author, tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if kind != tt.wantKind || model != tt.wantModel {
				t.Errorf("ChooseReviewer(%s) = %s %q, want %s %q", tt.author, kind, model, tt.wantKind, tt.wantModel)
			}
		})
	}
}

func TestChooseReviewerAllowsAgy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for name, tt := range map[string]struct {
		author backend.Kind
		cfg    config.Config
	}{
		"explicit review_backend":  {backend.Claude, config.Config{BackendDefaults: map[string]config.BackendDefaults{"claude": {ReviewBackend: "agy"}}}},
		"agy author, no other CLI": {backend.Agy, config.Config{}},
	} {
		if kind, _, err := ChooseReviewer(tt.author, tt.cfg); err != nil || kind != backend.Agy {
			t.Errorf("%s: kind = %s, err = %v, want agy", name, kind, err)
		}
	}
}

func TestChooseReviewerRejectsUnknownBackend(t *testing.T) {
	cfg := config.Config{CrossReview: &config.CrossReviewConfig{Order: []string{"gemini"}}}
	if _, _, err := ChooseReviewer(backend.Claude, cfg); err == nil {
		t.Error("unknown backend in cross_review.order must be an error, not silently skipped")
	}
}
