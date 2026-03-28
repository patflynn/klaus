package cmd

import (
	"testing"
)

func TestSeveritiesAtOrAbove(t *testing.T) {
	tests := []struct {
		level    string
		included []string
		excluded []string
	}{
		{
			level:    "critical",
			included: []string{"critical"},
			excluded: []string{"high", "medium", "low"},
		},
		{
			level:    "high",
			included: []string{"high", "critical"},
			excluded: []string{"medium", "low"},
		},
		{
			level:    "medium",
			included: []string{"medium", "high", "critical"},
			excluded: []string{"low"},
		},
		{
			level:    "low",
			included: []string{"low", "medium", "high", "critical"},
			excluded: []string{},
		},
		{
			level:    "unknown",
			included: []string{"critical"},
			excluded: []string{"high", "medium", "low"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			result := severitiesAtOrAbove(tt.level)
			for _, s := range tt.included {
				if !result[s] {
					t.Errorf("expected %q to be included for level %q", s, tt.level)
				}
			}
			for _, s := range tt.excluded {
				if result[s] {
					t.Errorf("expected %q to be excluded for level %q", s, tt.level)
				}
			}
		})
	}
}
