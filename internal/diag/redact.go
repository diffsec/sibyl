package diag

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Redactor cleans text that leaves the box. Secrets are replaced outright.
// Addresses and the hostname are replaced with a keyed hash, so one address
// gives the same token in every file of one device, and the token cannot be
// turned back into the address.
type Redactor struct {
	Salt     []byte
	Hostname string
}

type rule struct {
	re   *regexp.Regexp
	repl string
}

// secretRules run in order: PEM blocks and bearer tokens before the generic patterns.
var secretRules = []rule{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "<private-key>"},
	{regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`), "${1}<token>"},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`), "Bearer <token>"},
	{regexp.MustCompile(`(?i)\b(x-api-key:\s*)\S+`), "${1}<token>"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), "<api-key>"},
	{regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}\b`), "<hf-token>"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), "<github-token>"},
	{regexp.MustCompile(`\b[A-Za-z]+-[0-9a-f]{40,}\b`), "<api-key>"},
	{regexp.MustCompile(`(?i)\b((?:api[_-]?key|secret|password|passwd|token)["']?\s*[=:]\s*)["']?[^\s"',}]+`), "${1}<redacted>"},
	{regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), "<email>"},
}

var (
	ipv4Re = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)
	macRe  = regexp.MustCompile(`\b(?:[0-9a-fA-F]{2}[:-]){5}[0-9a-fA-F]{2}\b`)
	ipv6Re = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b`)
	// Addresses that say nothing about the owner's network: loopback, the
	// unspecified and broadcast addresses, and Podman's container networks.
	keepIPv4 = regexp.MustCompile(`^(127\.|0\.0\.0\.0$|255\.255\.255\.255$|10\.88\.|10\.89\.)`)
)

func (r Redactor) token(kind, v string) string {
	m := hmac.New(sha256.New, r.Salt)
	m.Write([]byte(v))
	return "<" + kind + "-" + hex.EncodeToString(m.Sum(nil))[:8] + ">"
}

// Text returns s with secrets, addresses and the hostname replaced.
func (r Redactor) Text(s string) string {
	for _, ru := range secretRules {
		s = ru.re.ReplaceAllString(s, ru.repl)
	}
	s = macRe.ReplaceAllStringFunc(s, func(m string) string { return r.token("mac", strings.ToLower(m)) })
	s = ipv6Re.ReplaceAllStringFunc(s, func(m string) string { return r.token("ip6", strings.ToLower(m)) })
	s = ipv4Re.ReplaceAllStringFunc(s, func(m string) string {
		if keepIPv4.MatchString(m) {
			return m
		}
		return r.token("ip", m)
	})
	if h := r.Hostname; h != "" && h != "localhost" && h != "sibyl" {
		s = regexp.MustCompile(`\b`+regexp.QuoteMeta(h)+`\b`).ReplaceAllString(s, "<hostname>")
	}
	return s
}
