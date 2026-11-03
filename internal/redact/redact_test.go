package redact

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	cases := map[string]string{
		"token xoxp-1234567890-abcdefghij here":           "xoxp-",
		"PGPASSWORD=hunter2 psql -h db":                   "hunter2",
		"postgres://app:s3cr3t@db.internal:5432/app":       "s3cr3t",
		"-H 'Authorization: Bearer abc.def.ghi123456789'":  "abc.def",
		`{"api_key": "abcd1234"}`:                          "abcd1234",
		"key AKIAABCDEFGHIJKLMNOP":                         "AKIAABCDEFGHIJKLMNOP",
	}
	for in, secret := range cases {
		out := String(in)
		if strings.Contains(out, secret) {
			t.Errorf("secret %q still in %q", secret, out)
		}
	}
	if got := String("go test ./... passed"); got != "go test ./... passed" {
		t.Errorf("plain text changed: %q", got)
	}
}
