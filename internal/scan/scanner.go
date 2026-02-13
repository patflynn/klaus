package scan

import (
	"bufio"
	"io"
	"regexp"
)

// Finding represents a category of sensitive data found in a log.
type Finding struct {
	Category string
}

var patterns = []struct {
	re       *regexp.Regexp
	category string
}{
	{
		re:       regexp.MustCompile(`(?:^|[^0-9])(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2[0-9]|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})(?:[^0-9]|$)`),
		category: "private IP addresses",
	},
	{
		re:       regexp.MustCompile(`OPENSSH PRIVATE KEY|RSA PRIVATE KEY|EC PRIVATE KEY|DSA PRIVATE KEY`),
		category: "SSH private key material",
	},
	{
		re:       regexp.MustCompile(`(?i)(?:password|token|secret|api_key|apikey|api-key)\s*[=:]`),
		category: "credential patterns (password/token/secret/api_key)",
	},
	{
		re:       regexp.MustCompile(`\.age\b.*:\s*\S`),
		category: ".age secret file references",
	},
}

// CheckSensitivity scans the content from r for common sensitive data patterns.
// Returns a list of findings. An empty list means the content is clean.
func CheckSensitivity(r io.Reader) []Finding {
	// Track which categories we've already found to avoid duplicates
	found := make(map[string]bool)
	var findings []Finding

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		for _, p := range patterns {
			if !found[p.category] && p.re.MatchString(line) {
				found[p.category] = true
				findings = append(findings, Finding{Category: p.category})
			}
		}
		// Early exit if all patterns matched
		if len(found) == len(patterns) {
			break
		}
	}

	return findings
}
