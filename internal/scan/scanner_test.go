package scan

import (
	"strings"
	"testing"
)

func TestCleanContent(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}
{"type":"result","total_cost_usd":1.5}`
	findings := CheckSensitivity(strings.NewReader(input))
	if len(findings) != 0 {
		t.Errorf("clean content produced %d findings, want 0", len(findings))
	}
}

func TestPrivateIPs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"10.x", "connecting to 10.0.1.5 on port 22", true},
		{"172.16.x", "host 172.16.0.1 is up", true},
		{"172.31.x", "addr: 172.31.255.255", true},
		{"192.168.x", "gateway 192.168.1.1", true},
		{"public IP", "server at 8.8.8.8", false},
		{"172.15 not private", "host 172.15.0.1", false},
		{"172.32 not private", "host 172.32.0.1", false},
		{"version number", "v10.0.0.0 release", true}, // looks like IP — acceptable false positive
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := CheckSensitivity(strings.NewReader(tt.input))
			found := hasCategory(findings, "private IP addresses")
			if found != tt.want {
				t.Errorf("input %q: found=%v, want=%v", tt.input, found, tt.want)
			}
		})
	}
}

func TestSSHKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"openssh", "-----BEGIN OPENSSH PRIVATE KEY-----", true},
		{"rsa", "-----BEGIN RSA PRIVATE KEY-----", true},
		{"ec", "-----BEGIN EC PRIVATE KEY-----", true},
		{"dsa", "-----BEGIN DSA PRIVATE KEY-----", true},
		{"public key", "-----BEGIN PUBLIC KEY-----", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := CheckSensitivity(strings.NewReader(tt.input))
			found := hasCategory(findings, "SSH private key material")
			if found != tt.want {
				t.Errorf("input %q: found=%v, want=%v", tt.input, found, tt.want)
			}
		})
	}
}

func TestCredentialPatterns(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"password=", "password=hunter2", true},
		{"PASSWORD:", "PASSWORD: secret", true},
		{"token=", "token=abc123", true},
		{"secret:", "secret: mysecret", true},
		{"api_key=", "api_key=xyz", true},
		{"apikey:", "apikey: foo", true},
		{"api-key=", "api-key=bar", true},
		{"no match", "the password was reset by admin", false},
		{"word boundary", "secretive planning", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := CheckSensitivity(strings.NewReader(tt.input))
			found := hasCategory(findings, "credential patterns (password/token/secret/api_key)")
			if found != tt.want {
				t.Errorf("input %q: found=%v, want=%v", tt.input, found, tt.want)
			}
		})
	}
}

func TestAgeSecrets(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"age ref", "secrets/wifi.age: network-key-here", true},
		{"no content", "secrets/wifi.age", false},
		{"age word", "the age of reason", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := CheckSensitivity(strings.NewReader(tt.input))
			found := hasCategory(findings, ".age secret file references")
			if found != tt.want {
				t.Errorf("input %q: found=%v, want=%v", tt.input, found, tt.want)
			}
		})
	}
}

func TestMultipleFindings(t *testing.T) {
	input := `connecting to 10.0.1.5
-----BEGIN RSA PRIVATE KEY-----
password=hunter2
secrets/wifi.age: key`
	findings := CheckSensitivity(strings.NewReader(input))
	if len(findings) != 4 {
		t.Errorf("got %d findings, want 4", len(findings))
		for _, f := range findings {
			t.Logf("  found: %s", f.Category)
		}
	}
}

func TestDeduplication(t *testing.T) {
	input := `host 10.0.0.1
host 10.0.0.2
host 192.168.1.1`
	findings := CheckSensitivity(strings.NewReader(input))
	count := 0
	for _, f := range findings {
		if f.Category == "private IP addresses" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d 'private IP' findings, want 1 (deduped)", count)
	}
}

func hasCategory(findings []Finding, category string) bool {
	for _, f := range findings {
		if f.Category == category {
			return true
		}
	}
	return false
}
