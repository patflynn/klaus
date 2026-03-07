package cmd

import (
	"fmt"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize klaus config",
	Long: `Scaffolds a .klaus/ directory with default config.json and prompt.md template.

If run inside a git repository, creates .klaus/ in the repo root.
If run outside a git repository, creates ~/.klaus/config.json for global configuration.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, _ := git.RepoRoot()

		if root != "" {
			if err := config.Init(root); err != nil {
				return err
			}
			fmt.Println("Initialized .klaus/ directory:")
			fmt.Println("  .klaus/config.json  — configuration")
			fmt.Println("  .klaus/prompt.md    — system prompt template")
		} else {
			if err := config.InitGlobal(); err != nil {
				return err
			}
			fmt.Println("Initialized global klaus config:")
			fmt.Println("  ~/.klaus/config.json  — configuration")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
