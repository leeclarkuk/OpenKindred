# API

Group `openkindred.io`, version `v1alpha1`. The CRDs live in `api/v1alpha1/`.

| Kind | Purpose |
|---|---|
| `Model` | A model endpoint an agent can use. Provider, model name, endpoint, credentials, limits. |
| `McpServer` | A tool server, either an existing URL or an image the platform runs. |
| `Agent` | Instructions plus references to a model, tools, memory, browser, code interpreter and A2A peers. |

References are by name within the namespace. Peers may name a spoke cluster.

## Not yet defined

Identity, tracing configuration, sandbox policy and the controllers that reconcile these
resources. The schemas describe intent only. Nothing acts on them yet.

## Validating the examples

```
pip install pyyaml jsonschema
python3 hack/validate-examples.py
```

This checks examples against the CRD schemas. It is not a substitute for applying them to a
real API server.
