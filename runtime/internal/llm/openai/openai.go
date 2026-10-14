// Package openai adapts the OpenAI-compatible chat completions protocol to the llm
// contract. It is one supported protocol among several. Servers such as vLLM speak it.
package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/httpx"
)

type Config struct {
	// Endpoint is the base URL, for example http://vllm:8000/v1.
	Endpoint string
	Model    string
	// APIKey is optional. Many self-hosted servers need none.
	APIKey string
	Client *http.Client
	// ToolCalling declares whether the served model supports tool calls.
	// Defaults to true. Set false for models that do not.
	ToolCalling *bool
}

type Provider struct {
	cfg  Config
	caps llm.Capabilities
}

func New(cfg Config) (*Provider, error) {
	if cfg.Model == "" {
		return nil, errs.New(errs.InvalidConfig, "openai-compatible: model name is required")
	}
	if cfg.Endpoint == "" {
		return nil, errs.New(errs.InvalidConfig, "openai-compatible: an endpoint is required")
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	tools := true
	if cfg.ToolCalling != nil {
		tools = *cfg.ToolCalling
	}
	return &Provider{cfg: cfg, caps: llm.Capabilities{ToolCalling: tools}}, nil
}

func (p *Provider) Name() string                   { return "openai-compatible" }
func (p *Provider) Capabilities() llm.Capabilities { return p.caps }

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func str(s string) *string { return &s }

func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if len(req.Tools) > 0 && !p.caps.ToolCalling {
		return llm.Response{}, errs.New(errs.Unsupported, "openai-compatible: model %q is not declared as supporting tool calls", p.cfg.Model)
	}
	wr := wireRequest{Model: p.cfg.Model, MaxTokens: req.MaxTokens}
	if req.System != "" {
		wr.Messages = append(wr.Messages, wireMessage{Role: "system", Content: str(req.System)})
	}
	for _, m := range req.Messages {
		wr.Messages = append(wr.Messages, toWire(m)...)
	}
	for _, t := range req.Tools {
		var w wireTool
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.InputSchema
		if len(w.Function.Parameters) == 0 {
			w.Function.Parameters = json.RawMessage(`{"type":"object"}`)
		}
		wr.Tools = append(wr.Tools, w)
	}
	headers := map[string]string{}
	if p.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.cfg.APIKey
	}
	data, err := httpx.PostJSON(ctx, p.cfg.Client, p.cfg.Endpoint+"/chat/completions", headers, wr, errorMessage)
	if err != nil {
		return llm.Response{}, err
	}
	var resp wireResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return llm.Response{}, errs.Wrap(errs.Unavailable, err, "openai-compatible: malformed response")
	}
	if len(resp.Choices) == 0 {
		return llm.Response{}, errs.New(errs.Unavailable, "openai-compatible: response had no choices")
	}
	ch := resp.Choices[0]
	out := llm.Response{
		Message: llm.Message{Role: llm.Assistant},
		Usage:   llm.Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens},
	}
	if ch.Message.Content != nil {
		out.Message.Text = *ch.Message.Content
	}
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if strings.TrimSpace(tc.Function.Arguments) == "" {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return llm.Response{}, errs.New(errs.Unavailable, "openai-compatible: tool call %q had invalid JSON arguments", tc.Function.Name)
		}
		out.Message.ToolCalls = append(out.Message.ToolCalls, llm.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	switch ch.FinishReason {
	case "stop":
		out.Stop = llm.StopEnd
	case "tool_calls", "function_call":
		out.Stop = llm.StopToolUse
	case "length":
		out.Stop = llm.StopMaxTokens
	default:
		out.Stop = llm.StopOther
	}
	return out, nil
}

func toWire(m llm.Message) []wireMessage {
	var out []wireMessage
	// Tool results are separate messages in this protocol.
	for _, r := range m.ToolResults {
		out = append(out, wireMessage{Role: "tool", ToolCallID: r.CallID, Content: str(r.Content)})
	}
	if m.Text != "" || len(m.ToolCalls) > 0 {
		w := wireMessage{Role: string(m.Role)}
		if m.Text != "" {
			w.Content = str(m.Text)
		}
		for _, c := range m.ToolCalls {
			var tc wireToolCall
			tc.ID = c.ID
			tc.Type = "function"
			tc.Function.Name = c.Name
			tc.Function.Arguments = string(c.Args)
			if tc.Function.Arguments == "" {
				tc.Function.Arguments = "{}"
			}
			w.ToolCalls = append(w.ToolCalls, tc)
		}
		out = append(out, w)
	}
	return out
}

func errorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		return e.Error.Message
	}
	return ""
}
