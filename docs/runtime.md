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
  "limits": { "requestTimeout": "60s", "toolTimeout": "30s", "maxToolCalls": 10 },
  "unresolved": []
}
```

- `model` is the Model resource. There are no provider-specific fields on the Agent.
- `apiKeyEnv` names an environment variable. The credential value is never in the file.
- `allowedTools` and `agentAllowedTools`: omitted means no restriction from that side, an
  empty list means no tools.
- A non-empty `unresolved` list stops execution and its reasons are returned to callers.
- The tool a model sees is named `<server>__<tool>`.

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
- No limit on concurrent requests.
- Streaming is not supported.
- Only text results from tools are passed to the model.
