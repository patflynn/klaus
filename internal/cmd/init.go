package cmd

import (
	"fmt"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/git"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize .klaus/ config in the current repo",
	Long:  "Scaffolds a .klaus/ directory with default config.json and prompt.md template.",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := git.RepoRoot()
		if err != nil {
			return fmt.Errorf("not inside a git repository")
		}

		if err := config.Init(root); err != nil {
			return err
		}

		fmt.Println("Initialized .klaus/ directory:")
		fmt.Println("  .klaus/config.json  — configuration")
		fmt.Println("  .klaus/prompt.md    — system prompt template")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
