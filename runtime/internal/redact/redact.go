// Package redact removes credentials from text before it is logged or returned.
// It is defence in depth: the primary rule is that credentials are never put in
// configuration, URLs or error messages in the first place.
package redact

import (
	"regexp"
	"strings"
	"sync"
)

const mask = "[REDACTED]"

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`), "Bearer " + mask},
	{regexp.MustCompile(`://[^/@\s"]+@`), "://" + mask + "@"},
	{regexp.MustCompile(`(?i)([?&](?:token|key|api_?key|access_token|auth|password|secret|sig|signature)=)[^&\s"]+`), "${1}" + mask},
}

type Redactor struct {
	mu      sync.RWMutex
	secrets []string
}

// Add registers an exact secret value. Very short values are ignored because
// replacing them would mangle ordinary text.
func (r *Redactor) Add(secret string) {
	if len(secret) < 6 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, secret)
}

func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, mask)
	}
	r.mu.RUnlock()
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
