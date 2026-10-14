// Package mcptest is a fake MCP server for tests.
package mcptest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type Tool struct {
	Name        string
	Description string
	// Handler returns the text result for a call.
	Handler func(args json.RawMessage) (text string, isError bool)
	// Delay slows the call down, to exercise timeouts.
	Delay time.Duration
}

type Server struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
	// SSE makes the server answer in event-stream form.
	SSE bool
}

// Calls returns how many times a tool was invoked.
func (s *Server) Calls(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[tool]
}

func New(t *testing.T, sse bool, tools ...Tool) *Server {
	s := &Server{calls: map[string]int{}, SSE: sse}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fake", "version": "0"}}
		case "tools/list":
			var list []map[string]any
			for _, tl := range tools {
				list = append(list, map[string]any{
					"name": tl.Name, "description": tl.Description,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			s.mu.Lock()
			s.calls[p.Name]++
			s.mu.Unlock()
			tl, ok := byName[p.Name]
			if !ok {
				s.reply(w, *req.ID, nil, map[string]any{"code": -32602, "message": "unknown tool"})
				return
			}
			if tl.Delay > 0 {
				select {
				case <-time.After(tl.Delay):
				case <-r.Context().Done():
					return
				}
			}
			text, isErr := tl.Handler(p.Arguments)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
		default:
			s.reply(w, *req.ID, nil, map[string]any{"code": -32601, "message": "method not found"})
			return
		}
		s.reply(w, *req.ID, result, nil)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) reply(w http.ResponseWriter, id int64, result any, rpcErr any) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	data, _ := json.Marshal(msg)
	if s.SSE {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
