package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/selfupdate"
	"github.com/spf13/cobra"
)

var selfUpdateCmd = &cobra.Command{
	Use:   "self-update",
	Short: "Install the tip of the configured klaus branch into the nix profile",
	Long: `Brings the installed klaus up to the tip of a configured branch, for a host
whose klaus should follow upstream without a system rebuild. It is off until
the host config, ~/.klaus/config.json, has a self_update block (a repo's
.klaus/config.json can't set it):

  "self_update": {"repo": "https://github.com/patflynn/klaus", "branch": "main"}

repo is any git URL; branch defaults to main. Each run:
  1. takes a lock under $XDG_STATE_HOME/klaus, so runs never overlap: a run
     that finds another in progress exits 0 and says so;
  2. resolves the branch's commit with git ls-remote;
  3. exits 0 if the installed klaus was built from that commit (quietly;
     -v says so), or if that commit already failed to build or test (it is
     retried when the branch moves, or with --retry);
  4. builds exactly that commit, the flake's packages.<system>.default:
     nix build 'git+<repo>?ref=refs/heads/<branch>&rev=<sha>#...';
  5. smoke-tests the build: its klaus --version must report the commit, and
     its klaus status must exit 0. A failure records the commit as failed,
     installs nothing, and exits non-zero;
  6. installs it into the user's default nix profile in place of the profile
     element named klaus, leaving other elements alone. The system klaus
     stays underneath as the fallback;
  7. records the current and previous store paths and commits, the time and
     any failure in $XDG_STATE_HOME/klaus/self-update.json.

Only the configured branch installs, pinned to the commit ls-remote returned,
and only after it builds and passes the smoke test.

--rollback reinstalls the previous store path; a second --rollback returns
to the newer one. --pin leaves the current install alone until --unpin:
runs while pinned exit 0 with a note, so a bad tip isn't reinstalled while
it gets fixed.

A running klaus dashboard notices the new klaus on PATH and restarts onto it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		retry, _ := cmd.Flags().GetBool("retry")
		rollback, _ := cmd.Flags().GetBool("rollback")
		pin, _ := cmd.Flags().GetBool("pin")
		unpin, _ := cmd.Flags().GetBool("unpin")
		verbose, _ := cmd.Flags().GetBool("verbose")

		dir, err := selfupdate.DefaultDir()
		if err != nil {
			return err
		}
		// Host config only: a repo's config must not choose what installs.
		cfg, err := config.Load("")
		if err != nil {
			return err
		}
		u := &selfupdate.Updater{Dir: dir, Version: version, Out: os.Stdout, Verbose: verbose, Branch: "main"}
		if su := cfg.SelfUpdate; su != nil {
			u.Repo, u.Branch = su.Repo, su.BranchName()
		}
		switch {
		case rollback:
			return u.Rollback(cmd.Context())
		case pin || unpin:
			return u.Pin(pin)
		}
		if u.Repo == "" {
			return errors.New(`self-update is off: add a self_update block to ~/.klaus/config.json, e.g.
  "self_update": {"repo": "https://github.com/patflynn/klaus", "branch": "main"}`)
		}
		if err := u.Update(cmd.Context(), retry); err != nil {
			return fmt.Errorf("self-update: %w", err)
		}
		return nil
	},
}

func init() {
	selfUpdateCmd.Flags().BoolP("verbose", "v", false, "also say so when klaus is up to date")
	selfUpdateCmd.Flags().Bool("retry", false, "build the branch's tip even if it failed before")
	selfUpdateCmd.Flags().Bool("rollback", false, "reinstall the previous klaus; again to return to the newer one")
	selfUpdateCmd.Flags().Bool("pin", false, "leave the current install alone until --unpin")
	selfUpdateCmd.Flags().Bool("unpin", false, "let self-update install the branch's tip again")
	selfUpdateCmd.MarkFlagsMutuallyExclusive("retry", "rollback", "pin", "unpin")
	rootCmd.AddCommand(selfUpdateCmd)
}
