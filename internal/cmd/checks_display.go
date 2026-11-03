package cmd

import (
	"fmt"
	"strings"

	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// maxFailedChecksW bounds the failed check names a dashboard row shows
// after its CI label.
const maxFailedChecksW = 40

// checksCI is what a valley change's checks read in the CI column, as a
// PR's CI does: passing, failing or pending; "" when the valley lists none.
func checksCI(c *valley.Checks) string {
	if c == nil {
		return ""
	}
	switch c.State {
	case valley.ChecksPassed:
		return "passing"
	case valley.ChecksFailed:
		return "failing"
	case valley.ChecksPending:
		return "pending"
	}
	return ""
}

// failingChecks names, once per change of states, the changes whose active
// version's checks failed, and the checks that did.
func failingChecks(states []*run.State, changes map[integration.ChangeKey]integration.Result) []string {
	var out []string
	for _, k := range followedChanges(states) {
		c := changes[k].Change
		if c == nil || checksCI(c.Checks) != "failing" {
			continue
		}
		name := c.Handle
		if name == "" {
			name = c.ChangeID
		}
		out = append(out, fmt.Sprintf("%s %s v%d: %s", k.Repo, name, c.Version, strings.Join(c.Checks.FailedNames(), ", ")))
	}
	return out
}
