// Package httpx holds the small amount of HTTP plumbing shared by provider adapters.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
)

const maxResponseBytes = 8 << 20

// PostJSON sends body as JSON and returns the response body. Non-2xx statuses and
// transport failures come back as typed errors. providerMsg extracts a human message
// from a provider error body, and may return "".
func PostJSON(ctx context.Context, c *http.Client, url string, headers map[string]string, body any, providerMsg func([]byte) string) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, errs.Wrap(errs.InvalidConfig, err, "build request")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, ce
		}
		return nil, errs.Wrap(errs.Unavailable, err, "model request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return nil, ce
		}
		return nil, errs.Wrap(errs.Unavailable, err, "read model response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}
	msg := ""
	if providerMsg != nil {
		msg = providerMsg(data)
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, StatusError(resp.StatusCode, msg)
}

// StatusError maps an HTTP status to the shared taxonomy.
func StatusError(status int, msg string) *errs.Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errs.New(errs.Auth, "model provider refused credentials (%d): %s", status, msg)
	case status == http.StatusTooManyRequests:
		return errs.New(errs.RateLimited, "model provider rate limited the request: %s", msg)
	case status >= 400 && status < 500:
		return errs.New(errs.BadRequest, "model provider rejected the request (%d): %s", status, msg)
	default:
		return errs.New(errs.Unavailable, "model provider failed (%d): %s", status, msg)
	}
}
