// Package llmtest provides local fake servers that speak each supported provider
// protocol, driven by one provider-neutral script. The same execution tests run
// against every adapter by swapping the server.
package llmtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Call is a tool call the fake model will make.
type Call struct {
	ID   string
	Name string
	Args string // JSON object
}

// Step is one model reply.
type Step struct {
	Text  string
	Calls []Call
	// Hang makes the server hold the request open until the client gives up.
	Hang bool
	// Delay waits before replying.
	Delay time.Duration
	// Status, when set, replies with this HTTP status and an error body instead.
	Status int
	// Release, when set, holds the request open until the channel is closed.
	Release <-chan struct{}
	// ErrorMessage is the message in an error body, used with Status.
	ErrorMessage string
	// MaxTokens ends the reply as truncated.
	MaxTokens bool
}

type Server struct {
	*httptest.Server
	mu       sync.Mutex
	steps    []Step
	bodies   [][]byte
	headers  []http.Header
	Protocol string
}

// Bodies returns the raw request bodies received, in order.
func (s *Server) Bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

type encoder func(Step) (any, int)

func newServer(t *testing.T, protocol, path string, enc encoder, errBody func(Step) any, steps []Step) *Server {
	s := &Server{steps: steps, Protocol: protocol}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		s.headers = append(s.headers, r.Header.Clone())
		var step Step
		ok := len(s.steps) > 0
		if ok {
			step = s.steps[0]
			s.steps = s.steps[1:]
		}
		s.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"script exhausted"}}`, http.StatusInternalServerError)
			return
		}
		if step.Hang {
			<-r.Context().Done()
			return
		}
		if step.Release != nil {
			select {
			case <-step.Release:
			case <-r.Context().Done():
				return
			}
		}
		if step.Delay > 0 {
			select {
			case <-time.After(step.Delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if step.Status != 0 {
			w.WriteHeader(step.Status)
			_ = json.NewEncoder(w).Encode(errBody(step))
			return
		}
		resp, _ := enc(step)
		_ = json.NewEncoder(w).Encode(resp)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func errMsg(st Step) string {
	if st.ErrorMessage != "" {
		return st.ErrorMessage
	}
	return "provider says no"
}

func raw(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(s)
}

// NewAnthropic serves the Anthropic Messages protocol.
func NewAnthropic(t *testing.T, steps ...Step) *Server {
	enc := func(st Step) (any, int) {
		var content []map[string]any
		if st.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": st.Text})
		}
		for _, c := range st.Calls {
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": raw(c.Args)})
		}
		stop := "end_turn"
		switch {
		case len(st.Calls) > 0:
			stop = "tool_use"
		case st.MaxTokens:
			stop = "max_tokens"
		}
		return map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "content": content,
			"stop_reason": stop, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		}, 200
	}
	errBody := func(st Step) any {
		return map[string]any{"type": "error", "error": map[string]string{"type": "x", "message": errMsg(st)}}
	}
	return newServer(t, "anthropic", "/v1/messages", enc, errBody, steps)
}

// NewOpenAI serves the OpenAI-compatible chat completions protocol.
func NewOpenAI(t *testing.T, steps ...Step) *Server {
	enc := func(st Step) (any, int) {
		msg := map[string]any{"role": "assistant", "content": st.Text}
		if st.Text == "" {
			msg["content"] = nil
		}
		var calls []map[string]any
		for _, c := range st.Calls {
			calls = append(calls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]string{"name": c.Name, "arguments": string(raw(c.Args))},
			})
		}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		finish := "stop"
		switch {
		case len(calls) > 0:
			finish = "tool_calls"
		case st.MaxTokens:
			finish = "length"
		}
		return map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		}, 200
	}
	errBody := func(st Step) any {
		return map[string]any{"error": map[string]string{"message": errMsg(st)}}
	}
	return newServer(t, "openai", "/v1/chat/completions", enc, errBody, steps)
}
