package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
)

// browserOpenCmd returns the command name and arguments needed to open url in
// the system's default browser for the given GOOS. It is pure (no exec) so the
// OS dispatch can be unit-tested in isolation.
func browserOpenCmd(goos, url string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{url}, nil
	case "linux":
		return "xdg-open", []string{url}, nil
	case "windows":
		return "cmd", []string{"/c", "start", url}, nil
	default:
		return "", nil, fmt.Errorf("unsupported OS: %s", goos)
	}
}

// openInBrowser launches the system browser pointed at url. It uses Start (not
// Run/Wait) so it never blocks the caller — the dashboard UI loop must stay
// responsive while the browser process starts in the background.
func openInBrowser(url string) error {
	name, args, err := browserOpenCmd(runtime.GOOS, url)
	if err != nil {
		return err
	}
	return exec.Command(name, args...).Start()
}
