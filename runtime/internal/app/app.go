// Package app assembles configuration, provider, tools and agent, tracks readiness
// and serves the HTTP API.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/agent"
	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/providers"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
	"github.com/leeclarkuk/openkindred/runtime/internal/redact"
	"github.com/leeclarkuk/openkindred/runtime/internal/tools"
)

const maxBodyBytes = 1 << 20

type Options struct {
	ConfigPath string
	Getenv     func(string) string
	HTTPClient *http.Client
	Logger     *slog.Logger
	// RetryInterval is the first wait between attempts to reach MCP servers at start
	// up. It doubles after each failure, with jitter, up to MaxRetryInterval.
	RetryInterval    time.Duration
	MaxRetryInterval time.Duration
}

// App is safe for concurrent use.
type App struct {
	opts Options
	log  *slog.Logger

	red redact.Redactor

	mu       sync.RWMutex
	clients  []*mcp.Client
	agent    *agent.Agent
	sem      chan struct{} // one slot per concurrent invocation
	reason   string        // why the app is not ready; empty when ready
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
	if opts.MaxRetryInterval == 0 {
		opts.MaxRetryInterval = 30 * time.Second
	}
	a := &App{opts: opts, log: opts.Logger, reason: "starting", kind: errs.Unavailable}
	go a.init(ctx)
	return a
}

func (a *App) setReady(ag *agent.Agent, maxConcurrent int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agent, a.reason, a.kind = ag, "", ""
	a.sem = make(chan struct{}, maxConcurrent)
}

func (a *App) setNotReady(kind errs.Kind, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agent, a.reason, a.kind = nil, a.red.String(reason), kind
}

// permanent records a failure that a retry cannot fix.
func (a *App) permanent(err error) {
	reason := err.Error()
	var e *errs.Error
	if errors.As(err, &e) {
		reason = e.Detail()
	}
	a.log.Error("runtime cannot start", "error", a.red.String(reason))
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
	if cfg.Model.APIKeyEnv != "" {
		a.red.Add(a.opts.Getenv(cfg.Model.APIKeyEnv))
	}
	provider, err := providers.New(cfg.Model, a.opts.Getenv, a.opts.HTTPClient)
	if err != nil {
		a.permanent(err)
		return
	}
	delay := a.opts.RetryInterval
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
			a.setReady(ag, cfg.Limits.MaxConcurrent)
			return
		}
		if k := errs.KindOf(err); k == errs.InvalidConfig {
			a.permanent(err)
			return
		}
		msg := a.red.String(err.Error())
		a.log.Warn("waiting for tool servers", "error", msg, "retryIn", delay.String())
		a.setNotReady(errs.Unavailable, "waiting for tool servers: "+msg)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(delay)):
		}
		delay = min(delay*2, a.opts.MaxRetryInterval)
	}
}

// jitter spreads retries by up to 20% either way so replicas do not retry in step.
func jitter(d time.Duration) time.Duration {
	return d - d/5 + time.Duration(rand.Int64N(int64(d)*2/5+1))
}

func (a *App) connectTools(ctx context.Context, cfg *config.Config) (*tools.Set, error) {
	callers := map[string]tools.Caller{}
	var made []*mcp.Client
	fail := func(err error) (*tools.Set, error) {
		closeAll(made)
		return nil, err
	}
	for _, s := range cfg.MCPServers {
		cctx, cancel := context.WithTimeout(ctx, cfg.Limits.ToolTimeout.D())
		c, err := mcp.Connect(cctx, s.URL, a.opts.HTTPClient)
		cancel()
		if err != nil {
			return fail(errors.Join(errs.New(errs.Unavailable, "mcp server %q unreachable", s.Name), err))
		}
		made = append(made, c)
		callers[s.Name] = c
	}
	cctx, cancel := context.WithTimeout(ctx, cfg.Limits.ToolTimeout.D())
	defer cancel()
	set, err := tools.Build(cctx, cfg.MCPServers, callers)
	if err != nil {
		return fail(err)
	}
	a.mu.Lock()
	a.clients = made
	a.mu.Unlock()
	return set, nil
}

func closeAll(cs []*mcp.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, c := range cs {
		c.Close(ctx)
	}
}

// Close ends MCP sessions. Call it after the HTTP server has stopped.
func (a *App) Close() {
	a.mu.RLock()
	cs := a.clients
	a.mu.RUnlock()
	closeAll(cs)
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": a.red.String(reason), "configHash": hash})
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
		a.writeError(w, errs.New(kind, "%s", reason), http.StatusServiceUnavailable)
		return
	}
	// Take a slot or refuse. Queueing would hide overload and hold connections open.
	a.mu.RLock()
	sem := a.sem
	a.mu.RUnlock()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		w.Header().Set("Retry-After", "1")
		a.writeError(w, errs.New(errs.Overloaded, "too many concurrent invocations (limit %d); retry shortly", cap(sem)), 0)
		return
	}
	var req invokeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		status := 0
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		a.writeError(w, errs.Wrap(errs.BadRequest, err, "invalid request body"), status)
		return
	}
	res, err := ag.Invoke(r.Context(), req.Input)
	if err != nil {
		a.log.Info("invocation failed", "kind", string(errs.KindOf(err)), "error", a.red.String(errMessage(err)))
		a.writeError(w, err, 0)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var statusByKind = map[errs.Kind]int{
	errs.BadRequest:     http.StatusBadRequest,
	errs.Forbidden:      http.StatusForbidden,
	errs.RateLimited:    http.StatusTooManyRequests,
	errs.LimitExceeded:  http.StatusUnprocessableEntity,
	errs.Timeout:        http.StatusGatewayTimeout,
	errs.Canceled:       499,
	errs.Unsupported:    http.StatusNotImplemented,
	errs.Auth:           http.StatusBadGateway,
	errs.Unavailable:    http.StatusBadGateway,
	errs.InvalidConfig:  http.StatusServiceUnavailable,
	errs.Overloaded:     http.StatusServiceUnavailable,
	errs.OutcomeUnknown: http.StatusBadGateway,
	errs.Internal:       http.StatusInternalServerError,
}

func errMessage(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func (a *App) writeError(w http.ResponseWriter, err error, status int) {
	kind := errs.KindOf(err)
	if status == 0 {
		status = statusByKind[kind]
		if status == 0 {
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"kind": string(kind), "message": a.red.String(errMessage(err))}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
