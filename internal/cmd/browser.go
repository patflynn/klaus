package cmd

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// browserOpenCmd returns the command name and arguments needed to open rawURL
// in the system's default browser for the given GOOS. It is pure (no exec) so
// the OS dispatch can be unit-tested in isolation.
//
// rawURL must be an http or https URL. Validating the scheme rejects dangerous
// schemes (file:, javascript:) and malformed input before it ever reaches a
// system handler. On Windows we use rundll32 with url.dll,FileProtocolHandler
// rather than "cmd /c start", which would otherwise run the URL through cmd.exe
// — exposing it to shell-metacharacter injection (e.g. "&") and mishandling
// URLs that need quoting.
func browserOpenCmd(goos, rawURL string) (string, []string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", nil, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}

	switch goos {
	case "darwin":
		return "open", []string{rawURL}, nil
	case "linux":
		return "xdg-open", []string{rawURL}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, nil
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
