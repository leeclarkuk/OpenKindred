// Package app assembles configuration, provider, tools and agent, tracks readiness
// and serves the HTTP API.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/agent"
	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/providers"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
	"github.com/leeclarkuk/openkindred/runtime/internal/tools"
)

const maxBodyBytes = 1 << 20

type Options struct {
	ConfigPath string
	Getenv     func(string) string
	HTTPClient *http.Client
	Logger     *slog.Logger
	// RetryInterval is the wait between attempts to reach MCP servers at start up.
	RetryInterval time.Duration
}

// App is safe for concurrent use.
type App struct {
	opts Options
	log  *slog.Logger

	mu       sync.RWMutex
	agent    *agent.Agent
	reason   string // why the app is not ready; empty when ready
	kind     errs.Kind
	hash     string
	draining bool
}

// Start loads configuration and begins initialising in the background. The returned
// App answers requests immediately, reporting not ready until initialisation ends.
func Start(ctx context.Context, opts Options) *App {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.RetryInterval == 0 {
		opts.RetryInterval = 2 * time.Second
	}
	a := &App{opts: opts, log: opts.Logger, reason: "starting", kind: errs.Unavailable}
	go a.init(ctx)
	return a
}

func (a *App) setReady(ag *agent.Agent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agent, a.reason, a.kind = ag, "", ""
}

func (a *App) setNotReady(kind errs.Kind, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agent, a.reason, a.kind = nil, reason, kind
}

// permanent records a failure that a retry cannot fix.
func (a *App) permanent(err error) {
	reason := err.Error()
	var e *errs.Error
	if errors.As(err, &e) {
		reason = e.Detail()
	}
	a.log.Error("runtime cannot start", "error", reason)
	a.setNotReady(errs.KindOf(err), reason)
}

func (a *App) init(ctx context.Context) {
	cfg, hash, err := config.Load(a.opts.ConfigPath)
	a.mu.Lock()
	a.hash = hash
	a.mu.Unlock()
	if err != nil {
		a.permanent(err)
		return
	}
	provider, err := providers.New(cfg.Model, a.opts.Getenv, a.opts.HTTPClient)
	if err != nil {
		a.permanent(err)
		return
	}
	for {
		set, err := a.connectTools(ctx, cfg)
		if err == nil {
			ag, err := agent.New(agent.Config{
				Provider: provider, Tools: set, Instructions: cfg.Instructions,
				MaxOutput: cfg.Model.MaxOutputTokens, MaxToolCalls: cfg.Limits.MaxToolCalls,
				RequestTimeout: cfg.Limits.RequestTimeout.D(), ToolTimeout: cfg.Limits.ToolTimeout.D(),
			})
			if err != nil {
				a.permanent(err)
				return
			}
			a.log.Info("runtime ready", "configHash", hash, "tools", len(set.Names()))
			a.setReady(ag)
			return
		}
		if k := errs.KindOf(err); k == errs.InvalidConfig {
			a.permanent(err)
			return
		}
		a.log.Warn("waiting for tool servers", "error", err.Error())
		a.setNotReady(errs.Unavailable, "waiting for tool servers: "+err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.opts.RetryInterval):
		}
	}
}

func (a *App) connectTools(ctx context.Context, cfg *config.Config) (*tools.Set, error) {
	callers := map[string]tools.Caller{}
	for _, s := range cfg.MCPServers {
		cctx, cancel := context.WithTimeout(ctx, cfg.Limits.ToolTimeout.D())
		c, err := mcp.Connect(cctx, s.URL, a.opts.HTTPClient)
		cancel()
		if err != nil {
			return nil, errors.Join(errs.New(errs.Unavailable, "mcp server %q unreachable", s.Name), err)
		}
		callers[s.Name] = c
	}
	cctx, cancel := context.WithTimeout(ctx, cfg.Limits.ToolTimeout.D())
	defer cancel()
	return tools.Build(cctx, cfg.MCPServers, callers)
}

// Drain marks the app not ready so that traffic moves away before shutdown.
func (a *App) Drain() {
	a.mu.Lock()
	a.draining = true
	a.mu.Unlock()
}

func (a *App) state() (ag *agent.Agent, kind errs.Kind, reason, hash string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.draining {
		return nil, errs.Unavailable, "shutting down", a.hash
	}
	return a.agent, a.kind, a.reason, a.hash
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("POST /v1/invoke", a.invoke)
	return mux
}

func (a *App) readyz(w http.ResponseWriter, _ *http.Request) {
	ag, _, reason, hash := a.state()
	if ag == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": reason, "configHash": hash})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "configHash": hash})
}

type invokeRequest struct {
	Input string `json:"input"`
}

func (a *App) invoke(w http.ResponseWriter, r *http.Request) {
	ag, kind, reason, _ := a.state()
	if ag == nil {
		writeError(w, errs.New(kind, "%s", reason), http.StatusServiceUnavailable)
		return
	}
	var req invokeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, errs.Wrap(errs.BadRequest, err, "invalid request body"), 0)
		return
	}
	res, err := ag.Invoke(r.Context(), req.Input)
	if err != nil {
		a.log.Info("invocation failed", "kind", string(errs.KindOf(err)))
		writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var statusByKind = map[errs.Kind]int{
	errs.BadRequest:    http.StatusBadRequest,
	errs.Forbidden:     http.StatusForbidden,
	errs.RateLimited:   http.StatusTooManyRequests,
	errs.LimitExceeded: http.StatusUnprocessableEntity,
	errs.Timeout:       http.StatusGatewayTimeout,
	errs.Canceled:      499,
	errs.Unsupported:   http.StatusNotImplemented,
	errs.Auth:          http.StatusBadGateway,
	errs.Unavailable:   http.StatusBadGateway,
	errs.InvalidConfig: http.StatusServiceUnavailable,
	errs.Internal:      http.StatusInternalServerError,
}

func writeError(w http.ResponseWriter, err error, status int) {
	kind := errs.KindOf(err)
	if status == 0 {
		status = statusByKind[kind]
		if status == 0 {
			status = http.StatusInternalServerError
		}
	}
	msg := err.Error()
	var e *errs.Error
	if errors.As(err, &e) {
		msg = e.Message
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"kind": string(kind), "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
