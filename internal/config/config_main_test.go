package config

import (
	"os"
	"testing"
)

// TestMain points HOME and XDG_CONFIG_HOME at an empty temp dir so no test
// reads the host's ~/.klaus/config.json. Tests that need a global config set
// their own HOME.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "klaus-config-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", home+"/.config")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
