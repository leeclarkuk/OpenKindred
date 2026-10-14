package llm_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/anthropic"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/llmtest"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/openai"
)

// A conversation containing every message shape, sent to each adapter.
func conversation() llm.Request {
	return llm.Request{
		System:    "sys",
		MaxTokens: 100,
		Tools:     []llm.ToolSpec{{Name: "docs__search", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []llm.Message{
			{Role: llm.User, Text: "question"},
			{Role: llm.Assistant, Text: "thinking", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "docs__search", Args: json.RawMessage(`{"q":"x"}`)}}},
			{Role: llm.User, ToolResults: []llm.ToolResult{{CallID: "c1", Content: "found", IsError: false}}},
		},
	}
}

func TestAnthropicWireFormat(t *testing.T) {
	srv := llmtest.NewAnthropic(t, llmtest.Step{Text: "ok"})
	p, _ := anthropic.New(anthropic.Config{Endpoint: srv.URL, Model: "m1", APIKey: "secret"})
	if _, err := p.Complete(context.Background(), conversation()); err != nil {
		t.Fatal(err)
	}
	h := srv.Headers()[0]
	if h.Get("x-api-key") != "secret" || h.Get("anthropic-version") == "" {
		t.Fatalf("missing auth headers: %v", h)
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(srv.Bodies()[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "m1" || body.MaxTokens != 100 || body.System != "sys" || len(body.Tools) != 1 || len(body.Messages) != 3 {
		t.Fatalf("bad body: %s", srv.Bodies()[0])
	}
	if got := body.Messages[1].Content; len(got) != 2 || got[0].Type != "text" || got[1].Type != "tool_use" {
		t.Fatalf("assistant blocks wrong: %+v", got)
	}
	if got := body.Messages[2].Content; len(got) != 1 || got[0].Type != "tool_result" || got[0].ToolUseID != "c1" {
		t.Fatalf("tool result blocks wrong: %+v", got)
	}
}

func TestOpenAIWireFormat(t *testing.T) {
	srv := llmtest.NewOpenAI(t, llmtest.Step{Text: "ok"})
	p, _ := openai.New(openai.Config{Endpoint: srv.URL + "/v1", Model: "m1", APIKey: "secret"})
	if _, err := p.Complete(context.Background(), conversation()); err != nil {
		t.Fatal(err)
	}
	if srv.Headers()[0].Get("Authorization") != "Bearer secret" {
		t.Fatal("missing bearer token")
	}
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct{ Type string } `json:"tools"`
	}
	if err := json.Unmarshal(srv.Bodies()[0], &body); err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool"}
	if len(roles) != len(want) {
		t.Fatalf("roles %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles %v want %v", roles, want)
		}
	}
	if body.Messages[2].ToolCalls[0].Function.Arguments != `{"q":"x"}` || body.Messages[3].ToolCallID != "c1" || body.Tools[0].Type != "function" {
		t.Fatalf("bad body: %s", srv.Bodies()[0])
	}
}

func TestOpenAINoAuthHeaderWithoutKey(t *testing.T) {
	srv := llmtest.NewOpenAI(t, llmtest.Step{Text: "ok"})
	p, _ := openai.New(openai.Config{Endpoint: srv.URL + "/v1", Model: "m"})
	if _, err := p.Complete(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.User, Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if srv.Headers()[0].Get("Authorization") != "" {
		t.Fatal("must not send an empty bearer token")
	}
}

func TestOpenAIDeclaredWithoutToolCalling(t *testing.T) {
	no := false
	p, _ := openai.New(openai.Config{Endpoint: "http://x", Model: "m", ToolCalling: &no})
	if p.Capabilities().ToolCalling {
		t.Fatal("capability should be false")
	}
}
