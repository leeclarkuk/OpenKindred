// Package anthropic adapts the Anthropic Messages API to the llm contract.
package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/httpx"
)

const (
	DefaultEndpoint = "https://api.anthropic.com"
	apiVersion      = "2023-06-01"
)

type Config struct {
	Endpoint string
	Model    string
	APIKey   string
	Client   *http.Client
}

type Provider struct {
	cfg Config
}

func New(cfg Config) (*Provider, error) {
	if cfg.Model == "" {
		return nil, errs.New(errs.InvalidConfig, "anthropic: model name is required")
	}
	if cfg.APIKey == "" {
		return nil, errs.New(errs.InvalidConfig, "anthropic: an API key is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &Provider{cfg: cfg}, nil
}

func (p *Provider) Name() string { return "anthropic" }

func (p *Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalling: true}
}

// Wire types.

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
}

type wireResponse struct {
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if req.MaxTokens <= 0 {
		return llm.Response{}, errs.New(errs.InvalidConfig, "anthropic: max output tokens must be set")
	}
	wr := wireRequest{Model: p.cfg.Model, MaxTokens: req.MaxTokens, System: req.System}
	for _, m := range req.Messages {
		wr.Messages = append(wr.Messages, toWire(m))
	}
	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		wr.Tools = append(wr.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	headers := map[string]string{"x-api-key": p.cfg.APIKey, "anthropic-version": apiVersion}
	data, err := httpx.PostJSON(ctx, p.cfg.Client, p.cfg.Endpoint+"/v1/messages", headers, wr, errorMessage)
	if err != nil {
		return llm.Response{}, err
	}
	var resp wireResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return llm.Response{}, errs.Wrap(errs.Unavailable, err, "anthropic: malformed response")
	}
	out := llm.Response{
		Message: llm.Message{Role: llm.Assistant},
		Usage:   llm.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	var text []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, llm.ToolCall{ID: b.ID, Name: b.Name, Args: args})
		}
	}
	out.Message.Text = strings.Join(text, "")
	switch resp.StopReason {
	case "end_turn", "stop_sequence":
		out.Stop = llm.StopEnd
	case "tool_use":
		out.Stop = llm.StopToolUse
	case "max_tokens":
		out.Stop = llm.StopMaxTokens
	default:
		out.Stop = llm.StopOther
	}
	return out, nil
}

func toWire(m llm.Message) wireMessage {
	w := wireMessage{Role: string(m.Role)}
	for _, r := range m.ToolResults {
		w.Content = append(w.Content, block{Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError})
	}
	if m.Text != "" {
		w.Content = append(w.Content, block{Type: "text", Text: m.Text})
	}
	for _, c := range m.ToolCalls {
		args := c.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		w.Content = append(w.Content, block{Type: "tool_use", ID: c.ID, Name: c.Name, Input: args})
	}
	return w
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
