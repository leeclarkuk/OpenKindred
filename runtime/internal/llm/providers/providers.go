// Package providers selects an adapter from Model.spec.provider. It is the only place
// that knows which provider names exist.
package providers

import (
	"net/http"

	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/anthropic"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/openai"
)

// New builds the adapter for m. getenv resolves the credential named by m.APIKeyEnv.
func New(m config.Model, getenv func(string) string, hc *http.Client) (llm.Provider, error) {
	key := ""
	if m.APIKeyEnv != "" {
		key = getenv(m.APIKeyEnv)
		if key == "" {
			return nil, errs.New(errs.InvalidConfig, "credential environment variable %s is empty or unset", m.APIKeyEnv)
		}
	}
	switch m.Provider {
	case "anthropic":
		return anthropic.New(anthropic.Config{Endpoint: m.Endpoint, Model: m.Name, APIKey: key, Client: hc})
	case "openai-compatible", "vllm":
		return openai.New(openai.Config{Endpoint: m.Endpoint, Model: m.Name, APIKey: key, Client: hc})
	case "bedrock", "vertex":
		return nil, errs.New(errs.Unsupported, "provider %q is not implemented by this runtime", m.Provider)
	default:
		return nil, errs.New(errs.InvalidConfig, "unknown provider %q", m.Provider)
	}
}
