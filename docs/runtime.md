# Runtime

The runtime (`runtime/`) executes one Agent. It reads a resolved configuration file and
serves OpenKindred's own HTTP API. It has no Kubernetes client and no provider SDKs.
Design decisions are in [ADR 0001](adr/0001-agent-lifecycle.md).

```
cd runtime
go test -race ./...
go run ./cmd/runtime --config config.json --addr :8080
```

## API

`POST /v1/invoke`

```json
{ "input": "What does the deployment guide say about rollbacks?" }
```

```json
{
  "output": "...",
  "stopReason": "end",
  "usage": { "InputTokens": 120, "OutputTokens": 45 },
  "modelCalls": 2,
  "toolCalls": [{ "name": "docs__search" }]
}
```

`stopReason` is `end`, `max_tokens` or `other`. A truncated answer is reported, not hidden.
Unknown request fields are rejected.

Errors have one shape. `kind` is stable; `message` is for people.

```json
{ "error": { "kind": "limit_exceeded", "message": "tool call limit of 10 reached" } }
```

| kind | HTTP | meaning |
|---|---|---|
| `bad_request` | 400 | Invalid body or empty input |
| `forbidden` | 403 | A tool call was not permitted |
| `rate_limited` | 429 | The model provider asked us to slow down |
| `limit_exceeded` | 422 | A configured limit was reached |
| `timeout` | 504 | The request deadline passed |
| `canceled` | 499 | The caller went away |
| `unsupported` | 501 | A requirement cannot be met |
| `auth`, `unavailable` | 502 | The model provider or a tool server failed |
| `overloaded` | 503 | At the concurrency limit; carries `Retry-After: 1`. The request was not started |
| `outcome_unknown` | 502 | A tool call may have taken effect (see below) |
| `invalid_config` | 503 | The runtime is not usable; the message says why |

`GET /healthz` is liveness and stays up when configuration is invalid.
`GET /readyz` is readiness. It returns 200 with the configuration hash when ready, and 503
with a reason otherwise. It is not ready until configuration is valid and every MCP server
has been reached.

## Configuration

JSON, produced by the composition from the Agent, Model and McpServer resources. Unknown
fields are rejected.

```json
{
  "model": {
    "provider": "anthropic",
    "name": "model-id",
    "endpoint": "",
    "apiKeyEnv": "MODEL_API_KEY",
    "maxOutputTokens": 1024
  },
  "instructions": "Answer using the docs tool.",
  "mcpServers": [
    { "name": "docs", "url": "http://docs.tools.svc:8080/mcp",
      "allowedTools": ["search"], "agentAllowedTools": ["search"] }
  ],
  "limits": { "requestTimeout": "60s", "toolTimeout": "30s", "maxToolCalls": 10, "maxConcurrent": 8 },
  "unresolved": []
}
```

- `model` is the Model resource. There are no provider-specific fields on the Agent.
- `apiKeyEnv` names an environment variable. The credential value is never in the file.
- `allowedTools` and `agentAllowedTools`: omitted means no restriction from that side, an
  empty list means no tools.
- A non-empty `unresolved` list stops execution and its reasons are returned to callers.
- The tool a model sees is named `<server>__<tool>`.

## Concurrency

`limits.maxConcurrent` (default 8) bounds invocations running at once. A request over the
limit is rejected immediately with `overloaded` and `Retry-After: 1`. It is not queued, because
a queue hides overload and holds connections open. A rejected request never reaches the model.
Slots are released when a request finishes, fails, times out or the caller disconnects.
Overload does not make the pod unready; tune the limit, replicas and the caller's retry policy
instead. Per-request timeouts and tool limits do not bound aggregate use, this does.

## Tool call outcomes

A tool call is made at most once. The runtime never retries one.

A timeout or a lost connection does not prove that the tool stopped or that its action failed.
When a request may have reached the tool but no answer arrived, the outcome is **unknown**:

- the model is told so explicitly, and told not to repeat the call;
- an identical call (same tool, same arguments) later in the same invocation is refused;
- the call is recorded with `"outcome": "unknown"` in `toolCalls`;
- if the whole request ends while a call is in flight, the error says the outcome is unknown.

A call is a definite failure only when nothing was sent (connection refused), the server
refused it (4xx, or a protocol error), or the session had expired. The runtime does not know
which tools are safe to repeat, so it treats every call as potentially consequential. Tool
annotations such as read-only hints are not used yet.

## MCP sessions

On start the runtime sends `initialize`, accepts only protocol versions 2025-06-18 and
2025-03-26, sends `notifications/initialized`, and keeps any `Mcp-Session-Id`. Servers that
issue no session id work. If a server answers 404 to a request carrying a session id, the
session has ended:

- listing tools starts a new session and retries once (listing is read-only);
- a tool call is **not** retried. It fails with "the call was not made", and the next call uses
  the new session.

Sessions are ended with `DELETE` on shutdown, best effort.

If a tool server is unreachable at start, the runtime stays not ready and retries with
exponential backoff and jitter (2s doubling to 30s). If it becomes unreachable after the
runtime is ready, calls fail as tool errors reported to the model and readiness does not change.

## Size limits and credentials

- Request bodies: 1 MiB, answered with 413 beyond that.
- Model responses and MCP responses: 8 MiB each. Exceeding it is an explicit error, never a
  silently truncated body. The tool list is capped at 1000 tools.
- Tool output given to the model: 32 KiB, marked `[truncated]`.
- Credentials are read from the environment, never from configuration, URLs or status.
  Transport errors have their URLs removed, and everything returned to callers or logged passes
  a redactor (the model key, bearer tokens, URL userinfo and common secret query parameters).
  Do not put credentials in MCP URLs; redaction is a safety net, not the control.

## Providers

Selected by `model.provider`.

| provider | adapter | notes |
|---|---|---|
| `anthropic` | Anthropic Messages API | key required |
| `openai-compatible` | OpenAI-compatible chat completions | endpoint required, key optional |
| `vllm` | same adapter as `openai-compatible` | |
| `bedrock`, `vertex` | not implemented | rejected with `unsupported` |

To add a provider: implement `llm.Provider`, declare its capabilities, register it in
`internal/llm/providers`, and add it to the adapter list in `internal/agent/agent_test.go` so
the shared behaviour tests run against it.

## Limits of the current implementation

- One request, one conversation. No memory, browser, code interpreter or peers.
- MCP servers are listed once at start. A server that changes its tools needs a restart.
- MCP calls carry no credentials.
- Rejected, not queued, when over the concurrency limit.
- No tool idempotency information is used, so every call is treated as consequential.
- Streaming is not supported.
- Only text results from tools are passed to the model.
