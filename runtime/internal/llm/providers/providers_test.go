package providers_test

import (
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/providers"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSelectionByProvider(t *testing.T) {
	cases := []struct {
		name string
		m    config.Model
		env  map[string]string
		want string
		kind errs.Kind
	}{
		{"anthropic", config.Model{Provider: "anthropic", Name: "m", APIKeyEnv: "K"}, map[string]string{"K": "x"}, "anthropic", ""},
		{"openai-compatible", config.Model{Provider: "openai-compatible", Name: "m", Endpoint: "http://x/v1"}, nil, "openai-compatible", ""},
		{"vllm uses the openai-compatible protocol", config.Model{Provider: "vllm", Name: "m", Endpoint: "http://x/v1"}, nil, "openai-compatible", ""},
		{"anthropic needs a key", config.Model{Provider: "anthropic", Name: "m"}, nil, "", errs.InvalidConfig},
		{"named key must be set", config.Model{Provider: "anthropic", Name: "m", APIKeyEnv: "K"}, nil, "", errs.InvalidConfig},
		{"openai-compatible needs endpoint", config.Model{Provider: "openai-compatible", Name: "m"}, nil, "", errs.InvalidConfig},
		{"bedrock not implemented", config.Model{Provider: "bedrock", Name: "m"}, nil, "", errs.Unsupported},
		{"unknown", config.Model{Provider: "nope", Name: "m"}, nil, "", errs.InvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := providers.New(tc.m, env(tc.env), nil)
			if tc.kind != "" {
				if errs.KindOf(err) != tc.kind {
					t.Fatalf("want %s, got %v", tc.kind, err)
				}
				return
			}
			if err != nil || p.Name() != tc.want {
				t.Fatalf("p=%v err=%v", p, err)
			}
		})
	}
}
