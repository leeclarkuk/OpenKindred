package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
)

func TestListToolsFollowsPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res := map[string]any{}
		switch {
		case req.Method == "initialize":
			res = map[string]any{"protocolVersion": "2025-06-18"}
		case req.Method == "tools/list" && req.Params.Cursor == "":
			res = map[string]any{"tools": []map[string]string{{"name": "a"}}, "nextCursor": "p2"}
		case req.Method == "tools/list":
			res = map[string]any{"tools": []map[string]string{{"name": "b"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": res})
	}))
	defer srv.Close()
	c, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := c.ListTools(context.Background())
	if err != nil || len(tl) != 2 || tl[1].Name != "b" {
		t.Fatalf("tools=%v err=%v", tl, err)
	}
}

func TestHTTPFailureIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := mcp.Connect(context.Background(), srv.URL, nil)
	if errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("got %v", err)
	}
}
