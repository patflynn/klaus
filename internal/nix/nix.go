package nix

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SetupDevEnvironment checks if flake.nix exists in the given directory
// and runs 'nix develop --command true' to set up the dev environment.
// If the nix command fails, a warning is printed to stderr with the
// combined output.
func SetupDevEnvironment(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "flake.nix")); err != nil {
		return
	}
	fmt.Println("  nix project detected, setting up dev environment...")
	nixCmd := exec.Command("nix", "develop", "--command", "true")
	nixCmd.Dir = dir
	output, err := nixCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: nix develop failed: %v\n%s", err, string(output))
	}
}
