package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp/mcptest"
)

func tool(name string) mcptest.Tool {
	return mcptest.Tool{Name: name, Handler: func(json.RawMessage) (string, bool) { return "done", false }}
}

func TestSessionIsEstablishedAndEnded(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Close(context.Background())
	if srv.Deleted() != 1 {
		t.Fatal("session was not ended on close")
	}
}

func TestUnsupportedProtocolVersionIsRejected(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	srv.Version = "2024-11-05"
	_, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("got %v", err)
	}
}

func TestStatelessServerWorks(t *testing.T) {
	// No Mcp-Session-Id is ever issued.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var res any = map[string]any{}
		switch req.Method {
		case "initialize":
			res = map[string]any{"protocolVersion": "2025-03-26"}
		case "tools/list":
			res = map[string]any{"tools": []map[string]string{{"name": "x"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": res})
	}))
	defer srv.Close()
	c, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tl, err := c.ListTools(context.Background()); err != nil || len(tl) != 1 {
		t.Fatalf("%v %v", tl, err)
	}
}

func TestExpiredSessionIsRecoveredForListing(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	srv.Expire()
	tl, err := c.ListTools(context.Background())
	if err != nil || len(tl) != 1 {
		t.Fatalf("listing should re-initialise and retry: %v %v", tl, err)
	}
	if srv.Inits() != 2 {
		t.Fatalf("expected one re-initialisation, got %d inits", srv.Inits())
	}
}

func TestExpiredSessionDuringCallIsNotRetried(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	srv.Expire()
	_, err := c.CallTool(context.Background(), "a", nil)
	if errs.KindOf(err) != errs.Unavailable || !strings.Contains(err.Error(), "the call was not made") {
		t.Fatalf("got %v", err)
	}
	if srv.Calls("a") != 0 {
		t.Fatal("the rejected call must not have run, and must not have been retried")
	}
	// The next call uses the new session.
	if _, err := c.CallTool(context.Background(), "a", nil); err != nil {
		t.Fatalf("session was not re-established: %v", err)
	}
	if srv.Calls("a") != 1 {
		t.Fatalf("tool ran %d times", srv.Calls("a"))
	}
}

func TestConcurrentCallsShareOneReinitialisation(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	srv.Expire()
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() { _, _ = c.CallTool(context.Background(), "a", nil); done <- struct{}{} }()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	if srv.Inits() != 2 {
		t.Fatalf("expected one shared re-initialisation, got %d inits", srv.Inits())
	}
}

func TestLostConnectionAfterRunIsOutcomeUnknown(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	srv.DropAfterRun = true
	_, err := c.CallTool(context.Background(), "a", nil)
	if errs.KindOf(err) != errs.OutcomeUnknown {
		t.Fatalf("want outcome_unknown, got %v", err)
	}
	if srv.Calls("a") != 1 {
		t.Fatalf("the client must not retry: tool ran %d times", srv.Calls("a"))
	}
}

func TestDeadlineDuringCallIsOutcomeUnknownNotTimeoutOfTheToolOnly(t *testing.T) {
	srv := mcptest.New(t, false, mcptest.Tool{Name: "slow", Delay: 3 * time.Second, Handler: func(json.RawMessage) (string, bool) { return "x", false }})
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.CallTool(ctx, "slow", nil)
	if k := errs.KindOf(err); k != errs.Timeout && k != errs.OutcomeUnknown {
		t.Fatalf("got %v", err)
	}
}

func TestRefusedConnectionIsDefinitelyNotRun(t *testing.T) {
	srv := mcptest.New(t, false, tool("a"))
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	srv.Close()
	_, err := c.CallTool(context.Background(), "a", nil)
	if errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("a connection that was never made is a plain failure, got %v", err)
	}
}

func TestClientErrorStatusIsNotUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "initialize" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{"protocolVersion": "2025-06-18"}})
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := mcp.Connect(context.Background(), srv.URL, nil)
	if _, err := c.CallTool(context.Background(), "a", nil); errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedResponsesAreReportedNotTruncated(t *testing.T) {
	for _, sse := range []bool{false, true} {
		big := strings.Repeat("a", 9<<20)
		srv := mcptest.New(t, sse, mcptest.Tool{Name: "big", Handler: func(json.RawMessage) (string, bool) { return big, false }})
		c, err := mcp.Connect(context.Background(), srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CallTool(context.Background(), "big", nil)
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("sse=%v: want a clear size error, got %v", sse, err)
		}
		if errs.KindOf(err) != errs.OutcomeUnknown {
			t.Fatalf("sse=%v: the tool ran, so the outcome is unknown, got %v", sse, errs.KindOf(err))
		}
	}
}

func TestToolListSizeIsCapped(t *testing.T) {
	var many []mcptest.Tool
	for i := 0; i < 1001; i++ {
		many = append(many, tool("t"+string(rune('a'+i%26))+strings.Repeat("x", i/26)))
	}
	srv := mcptest.New(t, false, many...)
	c, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("got %v", err)
	}
}

func TestTransportErrorsDoNotCarryURLCredentials(t *testing.T) {
	_, err := mcp.Connect(context.Background(), "http://user:hunter2pass@127.0.0.1:1/mcp?token=abcdef123456", nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "hunter2pass") || strings.Contains(err.Error(), "abcdef123456") {
		t.Fatalf("URL credentials leaked: %v", err)
	}
}
