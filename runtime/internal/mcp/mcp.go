// Package mcp is a minimal client for MCP servers over streamable HTTP. It supports
// initialisation, tool listing and tool calls, which is all the runtime needs.
//
// Session handling follows the streamable HTTP transport: the server may assign a
// session id on initialize, the client sends it on every later request, and a 404 for
// a request carrying a session id means the session ended and must be re-established.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/limitio"
)

const (
	maxResponseBytes = 8 << 20
	maxTools         = 1000
	maxListPages     = 100
)

// Protocol versions this client can speak over streamable HTTP, newest first.
var supportedVersions = []string{"2025-06-18", "2025-03-26"}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Client struct {
	url    string
	http   *http.Client
	nextID atomic.Int64

	mu      sync.Mutex // guards session and version
	session string
	version string

	reinitMu sync.Mutex // serialises session re-establishment
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

// errSessionGone means the server no longer knows our session id.
var errSessionGone = errors.New("mcp session no longer exists")

// Connect performs the MCP initialisation handshake.
func Connect(ctx context.Context, url string, hc *http.Client) (*Client, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	c := &Client{url: url, http: hc}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) state() (session, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, c.version
}

func (c *Client) initialize(ctx context.Context) error {
	res, hdr, _, err := c.do(ctx, "", "", "initialize", map[string]any{
		"protocolVersion": supportedVersions[0],
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "openkindred-runtime", "version": "0"},
	}, true)
	if err != nil {
		return err
	}
	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return errs.Wrap(errs.Unavailable, err, "mcp: malformed initialize result")
	}
	ok := false
	for _, v := range supportedVersions {
		if out.ProtocolVersion == v {
			ok = true
		}
	}
	if !ok {
		return errs.New(errs.Unavailable, "mcp: server negotiated unsupported protocol version %q", out.ProtocolVersion)
	}
	session := hdr.Get("Mcp-Session-Id")
	if _, _, _, err := c.do(ctx, session, out.ProtocolVersion, "notifications/initialized", nil, false); err != nil {
		return err
	}
	c.mu.Lock()
	c.session, c.version = session, out.ProtocolVersion
	c.mu.Unlock()
	return nil
}

// reinitialise replaces a dead session. dead is the session id that failed, so that
// concurrent callers do not each create a new one.
func (c *Client) reinitialise(ctx context.Context, dead string) error {
	c.reinitMu.Lock()
	defer c.reinitMu.Unlock()
	if cur, _ := c.state(); cur != dead {
		return nil
	}
	return c.initialize(ctx)
}

// Close ends the session on the server, best effort.
func (c *Client) Close(ctx context.Context) {
	session, version := c.state()
	if session == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	req.Header.Set("MCP-Protocol-Version", version)
	if resp, err := c.http.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

// ListTools returns every tool the server offers, following pagination. Listing is
// read-only, so it is retried once on a new session if the old one has gone.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	tools, err := c.listTools(ctx)
	if errors.Is(err, errSessionGone) {
		session, _ := c.state()
		if rerr := c.reinitialise(ctx, session); rerr != nil {
			return nil, rerr
		}
		tools, err = c.listTools(ctx)
	}
	if errors.Is(err, errSessionGone) {
		return nil, errs.New(errs.Unavailable, "mcp: session was rejected twice")
	}
	return tools, err
}

func (c *Client) listTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		session, version := c.state()
		res, _, _, err := c.do(ctx, session, version, "tools/list", params, true)
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
		if len(all) > maxTools {
			return nil, errs.New(errs.Unavailable, "mcp: server offers more than %d tools", maxTools)
		}
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

// CallTool runs a tool exactly once. It never retries. If the request may have
// reached the tool but no answer arrived, the error has kind OutcomeUnknown. If the
// session had expired, the call was not made, a new session is established for the
// next call, and the error says so.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	session, version := c.state()
	res, _, mayHaveRun, err := c.do(ctx, session, version, "tools/call", map[string]any{"name": name, "arguments": args}, true)
	if err != nil {
		switch {
		case errors.Is(err, errSessionGone):
			// The server rejected the request before running anything.
			if rerr := c.reinitialise(ctx, session); rerr != nil {
				return CallResult{}, errs.Wrap(errs.Unavailable, rerr, "mcp: session expired and could not be re-established; the call was not made")
			}
			return CallResult{}, errs.New(errs.Unavailable, "mcp: session had expired; the call was not made")
		case mayHaveRun:
			return CallResult{}, errs.Wrap(errs.OutcomeUnknown, err, "call to tool %q may have taken effect", name)
		}
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
		return CallResult{}, errs.Wrap(errs.OutcomeUnknown, err, "call to tool %q returned a malformed result", name)
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

// do sends one JSON-RPC message. mayHaveRun is true when the request might have
// reached the server and been acted on, so a failed call cannot be assumed harmless.
func (c *Client) do(ctx context.Context, session, version, method string, params any, wantResponse bool) (result json.RawMessage, hdr http.Header, mayHaveRun bool, err error) {
	rr := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	var id int64
	if wantResponse {
		id = c.nextID.Add(1)
		rr.ID = &id
	}
	body, err := json.Marshal(rr)
	if err != nil {
		return nil, nil, false, errs.Wrap(errs.Internal, err, "mcp: encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, false, errs.Wrap(errs.InvalidConfig, errs.Cause(err), "mcp: bad server URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A refused connection means nothing was sent. Anything else, including a
		// deadline, may have happened after the server received the request.
		var op *net.OpError
		dialFailed := errors.As(err, &op) && op.Op == "dial" && ctx.Err() == nil
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, nil, true, ce
		}
		return nil, nil, !dialFailed, errs.Wrap(errs.Unavailable, errs.Cause(err), "mcp: %s request failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && session != "" {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, false, errSessionGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		// A 4xx is a refusal. A 5xx may have happened after the work started.
		return nil, nil, resp.StatusCode >= 500, errs.New(errs.Unavailable, "mcp: %s returned HTTP %d", method, resp.StatusCode)
	}
	if !wantResponse {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, resp.Header, false, nil
	}
	var msg rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, err = readSSE(limitio.NewReader(resp.Body, maxResponseBytes), id)
	} else {
		var data []byte
		data, err = limitio.ReadAll(resp.Body, maxResponseBytes)
		if err == nil {
			err = json.Unmarshal(data, &msg)
		}
	}
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, nil, true, ce
		}
		if errors.Is(err, limitio.ErrTooLarge) {
			return nil, nil, true, errs.New(errs.Unavailable, "mcp: %s response exceeded %d bytes", method, maxResponseBytes)
		}
		return nil, nil, true, errs.Wrap(errs.Unavailable, errs.Cause(err), "mcp: reading %s response", method)
	}
	if msg.Error != nil {
		// The server answered with a protocol error, so it did not run the tool.
		return nil, nil, false, errs.New(errs.Unavailable, "mcp: %s failed: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
	}
	return msg.Result, resp.Header, false, nil
}

// readSSE returns the JSON-RPC response with the given id from an event stream.
func readSSE(r io.Reader, id int64) (rpcResponse, error) {
	sc := bufio.NewScanner(r)
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
