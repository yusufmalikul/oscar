// Package redact masks secrets before text is stored, logged or shown.
package redact

import "regexp"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`),                        // Slack tokens
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`),                            // Anthropic keys
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                           // GitHub tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                         // GitHub fine-grained tokens
	regexp.MustCompile(`ATATT[A-Za-z0-9_=-]{20,}`),                             // Atlassian API tokens
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                     // AWS access key ids
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // JWTs
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// key=value style secrets: keep the key, mask the value.
var kv = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY)[A-Z0-9_]*)("?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s"',;]+)`)

// user:pass@host in URLs
var urlCreds = regexp.MustCompile(`(://[^:/@\s]+):([^@\s/]+)@`)

// Bearer / Basic auth headers
var authHeader = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token)\s+)[A-Za-z0-9._~+/=-]+`)

// String returns s with secrets replaced by [redacted].
func String(s string) string {
	for _, p := range patterns {
		s = p.ReplaceAllString(s, "[redacted]")
	}
	s = kv.ReplaceAllString(s, "$1$2[redacted]")
	s = urlCreds.ReplaceAllString(s, "$1:[redacted]@")
	s = authHeader.ReplaceAllString(s, "$1[redacted]")
	return s
}
