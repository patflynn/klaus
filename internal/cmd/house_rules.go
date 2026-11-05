package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/houserules"
	"github.com/patflynn/klaus/internal/project"
)

// loadHouseRules reads the house rules for work on the repo at repoRoot: the
// repo's own .klaus/config.json may name other rules than the host config's
// fallback. A missing file warns once and the work goes ahead without it.
func loadHouseRules(ctx context.Context, repoRoot string, fallback config.Config, reg *project.Registry, info io.Writer) houserules.Rules {
	cfg := fallback
	if repoRoot != "" {
		if c, err := config.Load(repoRoot); err == nil {
			cfg = c
		}
	}
	rules := houserules.Load(ctx, cfg.HouseRules, reg)
	if w := rules.Warning(); w != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	} else if rules.Digest != "" {
		fmt.Fprintf(info, "House rules: %s (sha256 %.12s)\n", rules.Source, rules.Digest)
	}
	return rules
}
