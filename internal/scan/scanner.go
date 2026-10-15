package scan

import (
	"bufio"
	"io"
	"regexp"
	"strings"
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
		category: privateIPCategory,
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
	return CheckSensitivityAllowing(r, nil)
}

// CheckSensitivityAllowing is CheckSensitivity with an allow-list of literal
// hosts (typically the run's own git remotes). A private-IP match counts only
// when it is not one of allowedHosts; any other private address still flags.
// Lines of any length are scanned. If the content cannot be read to the end,
// a finding is reported so the caller keeps it private.
func CheckSensitivityAllowing(r io.Reader, allowedHosts []string) []Finding {
	allowed := make(map[string]bool, len(allowedHosts))
	for _, h := range allowedHosts {
		if h != "" {
			allowed[h] = true
		}
	}
	found := make(map[string]bool)
	var findings []Finding
	add := func(cat string) {
		if !found[cat] {
			found[cat] = true
			findings = append(findings, Finding{Category: cat})
		}
	}

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			for _, p := range patterns {
				if found[p.category] {
					continue
				}
				if p.category == privateIPCategory {
					if hasDisallowedPrivateIP(line, allowed) {
						add(p.category)
					}
				} else if p.re.MatchString(line) {
					add(p.category)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			add(incompleteCategory)
			break
		}
	}
	return findings
}

const (
	privateIPCategory  = "private IP addresses"
	incompleteCategory = "could not scan the whole file"
)

var privateIPRe = regexp.MustCompile(`(?:^|[^0-9])(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2[0-9]|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})(?:[^0-9]|$)`)

// hasDisallowedPrivateIP reports whether line holds a private address that is
// not in allowed. Matches are found one at a time from just past the previous
// address, since the regexp consumes the delimiter after each one.
func hasDisallowedPrivateIP(line string, allowed map[string]bool) bool {
	for len(line) > 0 {
		loc := privateIPRe.FindStringSubmatchIndex(line)
		if loc == nil {
			return false
		}
		if !allowed[line[loc[2]:loc[3]]] {
			return true
		}
		line = line[loc[3]:]
	}
	return false
}

// RemoteHost extracts the host from a git remote URL, in scp-style
// (git@10.100.0.1:repo.git) or URL-style (ssh://git@10.100.0.1:22/repo.git).
// It returns "" when no host can be found (e.g. a local path).
func RemoteHost(remote string) string {
	remote = strings.TrimSpace(remote)
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		if j := strings.LastIndex(rest, "@"); j >= 0 {
			rest = rest[j+1:]
		}
		if strings.HasPrefix(rest, "[") {
			if j := strings.Index(rest, "]"); j >= 0 {
				return rest[1:j]
			}
		}
		if j := strings.Index(rest, ":"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	// scp-style: [user@]host:path, where no slash precedes the colon.
	colon := strings.Index(remote, ":")
	if colon < 0 || strings.Contains(remote[:colon], "/") {
		return ""
	}
	host := remote[:colon]
	if j := strings.LastIndex(host, "@"); j >= 0 {
		host = host[j+1:]
	}
	return strings.Trim(host, "[]")
}
