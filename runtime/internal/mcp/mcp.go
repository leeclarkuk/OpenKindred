// Package mcp is a minimal client for MCP servers over streamable HTTP. It supports
// initialisation, tool listing and tool calls, which is all the runtime needs.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
)

const (
	protocolVersion  = "2025-06-18"
	maxResponseBytes = 8 << 20
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Client struct {
	url     string
	http    *http.Client
	session string
	nextID  atomic.Int64
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Connect performs the MCP initialisation handshake.
func Connect(ctx context.Context, url string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	c := &Client{url: url, http: hc}
	_, hdr, err := c.do(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "openkindred-runtime", "version": "0"},
	}, true)
	if err != nil {
		return nil, err
	}
	c.session = hdr.Get("Mcp-Session-Id")
	if _, _, err := c.do(ctx, "notifications/initialized", nil, false); err != nil {
		return nil, err
	}
	return c, nil
}

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, _, err := c.do(ctx, "tools/list", params, true)
		if err != nil {
			return nil, err
		}
		var out struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return nil, errs.Wrap(errs.Unavailable, err, "mcp: malformed tools/list result")
		}
		all = append(all, out.Tools...)
		if out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
	return nil, errs.New(errs.Unavailable, "mcp: tools/list did not terminate")
}

// CallResult is the outcome of a tool call.
type CallResult struct {
	Text    string
	IsError bool
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, _, err := c.do(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, true)
	if err != nil {
		return CallResult{}, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return CallResult{}, errs.Wrap(errs.Unavailable, err, "mcp: malformed tools/call result")
	}
	var parts []string
	for _, p := range out.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
		} else {
			parts = append(parts, "[non-text content omitted]")
		}
	}
	return CallResult{Text: strings.Join(parts, "\n"), IsError: out.IsError}, nil
}

func (c *Client) do(ctx context.Context, method string, params any, wantResponse bool) (json.RawMessage, http.Header, error) {
	rr := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	var id int64
	if wantResponse {
		id = c.nextID.Add(1)
		rr.ID = &id
	}
	body, err := json.Marshal(rr)
	if err != nil {
		return nil, nil, errs.Wrap(errs.Internal, err, "mcp: encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errs.Wrap(errs.InvalidConfig, err, "mcp: bad server URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, nil, ce
		}
		return nil, nil, errs.Wrap(errs.Unavailable, err, "mcp: request to %s failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, errs.New(errs.Unavailable, "mcp: %s returned HTTP %d", method, resp.StatusCode)
	}
	if !wantResponse {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, resp.Header, nil
	}
	var msg rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, err = readSSE(resp.Body, id)
	} else {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err == nil {
			err = json.Unmarshal(data, &msg)
		}
	}
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, nil, ce
		}
		return nil, nil, errs.Wrap(errs.Unavailable, err, "mcp: reading %s response", method)
	}
	if msg.Error != nil {
		return nil, nil, errs.New(errs.Unavailable, "mcp: %s failed: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
	}
	return msg.Result, resp.Header, nil
}

// readSSE returns the JSON-RPC response with the given id from an event stream.
func readSSE(r io.Reader, id int64) (rpcResponse, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxResponseBytes))
	sc.Buffer(make([]byte, 64*1024), maxResponseBytes)
	var data []string
	flush := func() (rpcResponse, bool) {
		if len(data) == 0 {
			return rpcResponse{}, false
		}
		payload := strings.Join(data, "\n")
		data = nil
		var m rpcResponse
		if json.Unmarshal([]byte(payload), &m) == nil && m.ID != nil && *m.ID == id {
			return m, true
		}
		return rpcResponse{}, false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m, ok := flush(); ok {
				return m, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if m, ok := flush(); ok {
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return rpcResponse{}, err
	}
	return rpcResponse{}, fmt.Errorf("no response with id %d in event stream", id)
}
