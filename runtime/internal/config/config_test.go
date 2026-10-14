package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
)

const valid = `{"model":{"provider":"anthropic","name":"m"},"instructions":"hi"}`

func TestDefaultsAndHash(t *testing.T) {
	c, h, err := config.Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxToolCalls != 10 || c.Limits.RequestTimeout.D() != 60*time.Second || c.Model.MaxOutputTokens != 1024 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	_, h2, _ := config.Parse([]byte(valid))
	_, h3, _ := config.Parse([]byte(valid + " "))
	if h == "" || h != h2 || h == h3 {
		t.Fatal("hash must be stable and content dependent")
	}
}

func TestAllowListsDistinguishOmittedFromEmpty(t *testing.T) {
	c, _, err := config.Parse([]byte(`{"model":{"provider":"p","name":"m"},"instructions":"x",
	 "mcpServers":[{"name":"a","url":"http://a","allowedTools":[]},{"name":"b","url":"http://b"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MCPServers[0].AllowedTools == nil || len(*c.MCPServers[0].AllowedTools) != 0 {
		t.Fatal("empty list must stay a non-nil empty list")
	}
	if c.MCPServers[1].AllowedTools != nil {
		t.Fatal("omitted list must stay nil")
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"not json":           `{`,
		"unknown field":      `{"model":{"provider":"p","name":"m"},"instructions":"x","allowAll":true}`,
		"missing provider":   `{"model":{"name":"m"},"instructions":"x"}`,
		"missing model name": `{"model":{"provider":"p"},"instructions":"x"}`,
		"missing prompt":     `{"model":{"provider":"p","name":"m"}}`,
		"bad duration":       `{"model":{"provider":"p","name":"m"},"instructions":"x","limits":{"requestTimeout":5}}`,
		"negative limit":     `{"model":{"provider":"p","name":"m"},"instructions":"x","limits":{"maxToolCalls":-1}}`,
		"duplicate server":   `{"model":{"provider":"p","name":"m"},"instructions":"x","mcpServers":[{"name":"a","url":"u"},{"name":"a","url":"u"}]}`,
		"bad server name":    `{"model":{"provider":"p","name":"m"},"instructions":"x","mcpServers":[{"name":"a b","url":"u"}]}`,
		"server without url": `{"model":{"provider":"p","name":"m"},"instructions":"x","mcpServers":[{"name":"a"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := config.Parse([]byte(in))
			if errs.KindOf(err) != errs.InvalidConfig {
				t.Fatalf("want invalid_config, got %v", err)
			}
		})
	}
}

func TestUnresolvedReferencesBlockConfig(t *testing.T) {
	_, _, err := config.Parse([]byte(`{"model":{"provider":"p","name":"m"},"instructions":"x",
	 "unresolved":[{"kind":"McpServer","name":"docs","reason":"not found"}]}`))
	if err == nil || !strings.Contains(err.Error(), `McpServer "docs": not found`) {
		t.Fatalf("reason not surfaced: %v", err)
	}
}
