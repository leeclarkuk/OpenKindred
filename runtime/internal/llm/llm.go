// Package llm is OpenKindred's own model contract. Nothing here refers to a
// provider. Adapters in sub-packages translate to and from provider protocols.
package llm

import (
	"context"
	"encoding/json"
)

type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Message is one turn. A user turn carries text and tool results. An assistant
// turn carries text and tool calls.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage // a JSON object
}

type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage // a JSON Schema object
}

type StopReason string

const (
	StopEnd       StopReason = "end"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopOther     StopReason = "other"
)

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Request struct {
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
}

type Response struct {
	Message Message
	Stop    StopReason
	Usage   Usage
}

// Capabilities are declared by each provider. The runtime rejects an agent whose
// requirements a provider does not meet instead of discovering it mid-request.
type Capabilities struct {
	ToolCalling bool
}

// Provider is the only way the execution loop talks to a model.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	Complete(ctx context.Context, req Request) (Response, error)
}
