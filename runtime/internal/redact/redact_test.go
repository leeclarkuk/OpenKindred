package redact_test

import (
	"strings"
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/redact"
)

func TestRedact(t *testing.T) {
	var r redact.Redactor
	r.Add("topsecretvalue")
	r.Add("ab") // too short to be safe to replace
	cases := map[string]string{
		"key topsecretvalue failed":           "key [REDACTED] failed",
		"Authorization: Bearer abc.DEF-123_x": "Authorization: Bearer [REDACTED]",
		"Post http://u:p4ss@host/x failed":    "Post http://[REDACTED]@host/x failed",
		"GET /mcp?token=abc123&x=1":           "GET /mcp?token=[REDACTED]&x=1",
		"GET /mcp?api_key=abc123":             "GET /mcp?api_key=[REDACTED]",
		"nothing sensitive, ab is fine":       "nothing sensitive, ab is fine",
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("%q\n got  %q\n want %q", in, got, want)
		}
	}
	var nilR *redact.Redactor
	if nilR.String("x") != "x" || strings.Contains(r.String("topsecretvalue"), "secret") {
		t.Fatal("nil receiver or exact match misbehaved")
	}
}
