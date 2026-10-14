// Package config defines the resolved configuration the runtime consumes. It is
// produced elsewhere (by the composition) and contains no Kubernetes types.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
)

type Config struct {
	Model        Model        `json:"model"`
	Instructions string       `json:"instructions"`
	MCPServers   []MCPServer  `json:"mcpServers"`
	Limits       Limits       `json:"limits"`
	Unresolved   []Unresolved `json:"unresolved"`
}

// Model comes from Model.spec and nothing else. Provider-specific behaviour lives in
// the adapter selected by Provider.
type Model struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	// APIKeyEnv names the environment variable holding the credential. The value is
	// never written to configuration.
	APIKeyEnv       string `json:"apiKeyEnv"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

// MCPServer is one resolved tool server. The two allow lists are tri-state:
// omitted means no restriction from that side, an empty list means no tools.
type MCPServer struct {
	Name              string    `json:"name"`
	URL               string    `json:"url"`
	AllowedTools      *[]string `json:"allowedTools"`
	AgentAllowedTools *[]string `json:"agentAllowedTools"`
}

type Limits struct {
	RequestTimeout Duration `json:"requestTimeout"`
	ToolTimeout    Duration `json:"toolTimeout"`
	MaxToolCalls   int      `json:"maxToolCalls"`
}

// Unresolved records a reference the composition could not resolve. Any entry makes
// the configuration unusable, and the reason is surfaced to callers.
type Unresolved struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,32}$`)

// Parse decodes and validates configuration. Unknown fields are rejected so that a
// typo cannot silently widen permissions. It returns the content hash of data.
func Parse(data []byte) (*Config, string, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, "", errs.Wrap(errs.InvalidConfig, err, "cannot parse configuration")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, hash, err
	}
	return &c, hash, nil
}

// Load reads and parses a configuration file.
func Load(path string) (*Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", errs.Wrap(errs.InvalidConfig, err, "cannot read configuration")
	}
	return Parse(data)
}

func (c *Config) applyDefaults() {
	if c.Limits.RequestTimeout == 0 {
		c.Limits.RequestTimeout = Duration(60 * time.Second)
	}
	if c.Limits.ToolTimeout == 0 {
		c.Limits.ToolTimeout = Duration(30 * time.Second)
	}
	if c.Limits.MaxToolCalls == 0 {
		c.Limits.MaxToolCalls = 10
	}
	if c.Model.MaxOutputTokens == 0 {
		c.Model.MaxOutputTokens = 1024
	}
}

func (c *Config) Validate() error {
	if len(c.Unresolved) > 0 {
		var parts []string
		for _, u := range c.Unresolved {
			parts = append(parts, fmt.Sprintf("%s %q: %s", u.Kind, u.Name, u.Reason))
		}
		return errs.New(errs.InvalidConfig, "unresolved references: %s", strings.Join(parts, "; "))
	}
	if c.Model.Provider == "" {
		return errs.New(errs.InvalidConfig, "model.provider is required")
	}
	if c.Model.Name == "" {
		return errs.New(errs.InvalidConfig, "model.name is required")
	}
	if strings.TrimSpace(c.Instructions) == "" {
		return errs.New(errs.InvalidConfig, "instructions are required")
	}
	if c.Model.MaxOutputTokens < 0 || c.Limits.MaxToolCalls < 0 || c.Limits.RequestTimeout < 0 || c.Limits.ToolTimeout < 0 {
		return errs.New(errs.InvalidConfig, "limits must not be negative")
	}
	seen := map[string]bool{}
	for _, s := range c.MCPServers {
		if !nameRe.MatchString(s.Name) {
			return errs.New(errs.InvalidConfig, "mcp server name %q must match %s", s.Name, nameRe)
		}
		if seen[s.Name] {
			return errs.New(errs.InvalidConfig, "duplicate mcp server %q", s.Name)
		}
		seen[s.Name] = true
		if s.URL == "" {
			return errs.New(errs.InvalidConfig, "mcp server %q has no url", s.Name)
		}
	}
	return nil
}
