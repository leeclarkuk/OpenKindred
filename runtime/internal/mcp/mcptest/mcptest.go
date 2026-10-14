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
	mu       sync.Mutex
	calls    map[string]int
	sessions map[string]bool
	inits    int
	deleted  int
	// SSE makes the server answer in event-stream form.
	SSE bool
	// Version is the protocol version returned by initialize. Defaults to 2025-06-18.
	Version string
	// DropAfterRun makes the server run a tool call, then cut the connection without
	// answering, as a crash or network failure would.
	DropAfterRun bool
}

// Expire invalidates every session, as a server restart would.
func (s *Server) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
}

// Inits is the number of initialize requests received.
func (s *Server) Inits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inits
}

// Deleted is the number of sessions the client ended.
func (s *Server) Deleted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted
}

// Calls returns how many times a tool was invoked.
func (s *Server) Calls(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[tool]
}

func New(t *testing.T, sse bool, tools ...Tool) *Server {
	s := &Server{calls: map[string]int{}, sessions: map[string]bool{}, SSE: sse, Version: "2025-06-18"}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.mu.Lock()
			s.deleted++
			delete(s.sessions, r.Header.Get("Mcp-Session-Id"))
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
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
		if req.Method != "initialize" {
			if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
				s.mu.Lock()
				ok := s.sessions[sid]
				s.mu.Unlock()
				if !ok {
					http.Error(w, "unknown session", http.StatusNotFound)
					return
				}
			}
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			s.mu.Lock()
			s.inits++
			sid := fmt.Sprintf("session-%d", s.inits)
			s.sessions[sid] = true
			s.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			result = map[string]any{"protocolVersion": s.Version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fake", "version": "0"}}
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
			if s.DropAfterRun {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						conn.Close()
					}
				}
				return
			}
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
