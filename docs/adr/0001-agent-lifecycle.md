# 0001. Agent lifecycle is owned by Crossplane

Status: proposed

## Context

`Agent` exists as a plain CRD with no controller behind it. Crossplane and KubeVela are
installed on the hub, but neither supplies the code that calls a model, executes MCP tools
or holds a conversation. We need one Agent working end to end before building further.

## Decision

1. **Crossplane owns the Agent.** `Agent` becomes a namespaced Crossplane composite
   (Crossplane v2). The XRD is the authoritative schema and replaces
   `api/v1alpha1/agent.yaml`. `Model` and `McpServer` stay plain CRDs that the composition
   reads. KubeVela is not in this execution path.
2. **One runtime.** A platform-controlled, versioned image that reads a documented config
   file and serves a request endpoint. Written in Go.
3. **Generated resources:** Deployment, Service (cluster internal), ConfigMap,
   ServiceAccount (one per Agent) and NetworkPolicy. No cloud provider is needed.
4. **Ownership.** ArgoCD owns the declarations (XRD, Composition, Agent, Model, McpServer).
   Crossplane owns everything it generates. Nothing is owned twice.
5. **Placement.** One local cluster. Hub to spoke delivery is a later decision.

## Behaviours

### Reference changes

The composition resolves the `Model` and each `McpServer` on every reconcile and renders
the runtime config into the ConfigMap. The Deployment pod template carries a hash of that
config, so any change to instructions, model or tool endpoints causes a rolling update.
There is no hot reload.

Propagation time is the Crossplane poll interval, set explicitly in the hub install
(target: 1 minute). A change to a referenced resource is therefore live within the poll
interval plus rollout time.

### Failure and readiness

Fail closed, without deleting anything. If a reference cannot be resolved, the composition
still renders the workload, but the config records the unresolved reference. The runtime
then reports not ready and answers requests with 503 and the reason. Omitting resources
instead would make Crossplane delete them, which is the wrong response to a missing
dependency.

Conditions on the Agent:

| Condition | True when |
|---|---|
| `ReferencesResolved` | The Model and every McpServer were found. Reasons name the missing one. |
| `WorkloadAvailable` | The Deployment has an available replica. |
| `Ready` | Both above are true and the runtime's `/readyz` passes. |

`/readyz` passes only when the config is valid and the runtime has loaded the model
settings and listed tools from each MCP server. A running pod alone is not ready.

### Tool permissions

Effective tools for a server are the intersection of the `McpServer` `allowedTools` and the
Agent's `allowedTools` for that server, enforced by the runtime at both tool listing and
tool call.

- Omitted `allowedTools`: no restriction from that side.
- Empty list: no tools from that side.
- An Agent has no access to a server it does not list in `tools`.

This changes the current `McpServer` schema text, which says empty means all. It will be
corrected to match.

### Unsupported capabilities

The first runtime supports a model, instructions and MCP tools. The XRD rejects, with
validation rules, `memory.enabled`, `browser.enabled` and `codeInterpreter.enabled` set to
true, and `peers`. Each is enabled by relaxing the rule in the release that implements it.
The hello example drops `memory` until then.

### Credentials

`Model.credentialsRef` is passed to the workload as a Secret reference (env from secret).
Credential values never appear in the generated ConfigMap or in status. `McpServer` has
no auth fields yet, so the first runtime calls MCP servers without credentials.

## Transition from the existing CRD

1. Remove `api/v1alpha1/agent.yaml` and add the XRD in the same change, so only one
   definition of `agents.openkindred.io` exists.
2. Order the ArgoCD sync: Crossplane first, then the XRD and Composition, then Agents.
3. No Agents exist yet, so no data migration.

## Open questions

- Which composition functions to use for reading `Model` and `McpServer`. The likely set is
  a templating function plus a function that fetches existing resources, but I have not
  checked what Crossplane v2 recommends today. This needs confirming against its docs
  before the composition is written.
- Whether the runtime should expose an OpenAI-compatible endpoint or a simpler custom one.

## First milestone

Apply an Agent and send a request. See a model call and an allowed MCP tool call, then a
response. Change the instructions and the referenced endpoint and see both take effect.
Remove a dependency and get a clear failure. Delete the Agent and see its workloads go
while shared Model and McpServer resources remain.
