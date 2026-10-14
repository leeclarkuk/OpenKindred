# 0001. Agent lifecycle is owned by Crossplane

Status: proposed

## Context

`Agent` exists as a plain CRD with no controller behind it. Crossplane and KubeVela are
installed on the hub, but neither supplies the code that calls a model, executes MCP tools
or holds a conversation. We need one Agent working end to end before building further.

## Requirements

### Provider independence

OpenKindred must not be coupled to any model provider. This is a requirement on the design,
not a feature of one adapter.

- The runtime has its own contract: its own messages, tool calls, tool results, usage and
  errors (`runtime/internal/llm`, `runtime/internal/errs`). Nothing above the adapters refers
  to a provider.
- Provider protocols, authentication and SDK types live behind adapters, selected by
  `Model.spec.provider`. The Anthropic Messages API and the OpenAI-compatible chat
  completions protocol are two adapters. Neither is the platform's internal contract.
- Model selection lives in the `Model` resource only. The `Agent` has no provider-specific
  fields, and adding one is a design error.
- Adapters declare capabilities. A requirement the selected provider cannot meet is rejected
  with a clear error before the first request, not discovered mid-conversation.
- The same execution tests run against every adapter.
- The runtime consumes resolved configuration and has no Kubernetes client.
- The public interface is OpenKindred's own `POST /v1/invoke`.

## Decision

1. **Crossplane owns the Agent.** `Agent` becomes a namespaced Crossplane composite
   (Crossplane v2). The XRD is the authoritative schema and replaces
   `api/v1alpha1/agent.yaml`. `Model` and `McpServer` stay plain CRDs that the composition
   reads. KubeVela is not in this execution path.
2. **One runtime.** A platform-controlled, versioned image written in Go. It reads a
   resolved configuration file and serves `/v1/invoke`, `/healthz` and `/readyz`.
3. **Generated resources:** Deployment, Service (cluster internal), immutable ConfigMap,
   ServiceAccount (one per Agent) and NetworkPolicy. No cloud provider is needed.
4. **Ownership.** ArgoCD owns the declarations (XRD, Composition, Agent, Model, McpServer).
   Crossplane owns everything it generates. Nothing is owned twice.
5. **Placement.** One local cluster. Hub to spoke delivery is a later decision.

## Behaviours

### Configuration and reference changes

The composition resolves the `Model` and each `McpServer` on every reconcile and renders
the runtime configuration into a **ConfigMap that is immutable and named by the hash of its
content** (for example `helper-config-3f9a1c2b7d41`). A change produces a new ConfigMap and
a new pod template, never an edit in place. The previous ConfigMap is garbage collected once
nothing references it. This removes the window in which a pod could read half-updated
configuration, and makes "which configuration is this pod running" answerable from its spec.

The runtime reports the content hash of the file it loaded in `/readyz`.

Propagation time is the Crossplane poll interval (target: 1 minute, set explicitly in the hub
install) plus rollout time.

### Rollout and revocation

The Deployment uses the **`Recreate`** strategy initially. A rolling update is unsafe for a
permission model: if replacement pods fail readiness, old pods keep serving with the
previous tool permissions, so a revocation silently does not take effect. `Recreate`
terminates old pods before starting new ones. The cost is downtime on every configuration
change, which we accept for now.

Revocation takes effect after, in order:

1. The change is merged and ArgoCD syncs it (ArgoCD sync interval or webhook).
2. Crossplane next reconciles the Agent (up to the poll interval).
3. Old pods receive SIGTERM, report not ready immediately and keep serving in-flight
   requests for the shutdown delay (default 5s) before the listener closes. In-flight
   requests can complete with the old permissions during this time, bounded by the shutdown
   delay and the request timeout.

The slowest of these dominates. This is documented behaviour, not a guarantee of
immediacy. Anyone who needs an immediate stop deletes the Agent.

### Readiness

`/readyz` is wired to the pod readiness probe. It passes only when the configuration is
valid, the provider adapter was built, and every referenced MCP server was reached and its
tools listed. `/healthz` (liveness) stays up when configuration is invalid, so a bad
configuration produces a not-ready pod and a clear reason rather than a restart loop.

The Agent's `Ready` condition is computed from the Deployment's status for its **current
generation**, not from the existence of an available pod:

- `observedGeneration` equals `metadata.generation`, and
- `updatedReplicas`, `replicas` and `availableReplicas` all equal the desired replica count,
  and
- no replica from a previous ReplicaSet exists.

An old available replica therefore cannot make an updated Agent look ready. With `Recreate`
this is also true by construction, but the check stays so that changing strategy later
cannot silently weaken it.

Conditions on the Agent:

| Condition | True when |
|---|---|
| `ReferencesResolved` | The Model and every McpServer were found. Reasons name the missing one. |
| `WorkloadAvailable` | The Deployment rollout for the current generation is complete and available. |
| `Ready` | Both above are true. |

### Failure when a reference is missing

Fail closed, without deleting anything. If a reference cannot be resolved, the composition
still renders the workload, but the configuration records the unresolved reference
(`unresolved` in the config). The runtime refuses to start executing: `/readyz` fails with
the reason and `/v1/invoke` answers 503 with the same reason. Omitting resources instead
would make Crossplane delete them, which is the wrong response to a missing dependency.

### Tool permissions

Effective tools for a server are the intersection of the `McpServer` `allowedTools` and the
Agent's `allowedTools` for that server, enforced by the runtime at both tool listing and
tool call.

- Omitted `allowedTools`: no restriction from that side.
- Empty list: no tools from that side.
- An Agent has no access to a server it does not list in `tools`.

A tool call outside the effective set is refused without contacting the server, whatever the
model was told. The `McpServer` schema text was corrected to match (previously "empty means
all").

### Unsupported capabilities

The first runtime supports a model, instructions and MCP tools. The XRD rejects, with
validation rules, `memory.enabled`, `browser.enabled` and `codeInterpreter.enabled` set to
true, and `peers`. Each is enabled by relaxing the rule in the release that implements it.
The hello example drops `memory` until then.

### Credentials and rotation

`Model.credentialsRef` is passed to the workload as a Secret reference (environment from
Secret). Credential values never appear in the ConfigMap, the config hash or status. The
runtime reads the credential once at start.

The non-secret configuration hash does not change when a Secret's contents change, so an
in-place edit of a Secret does **not** restart the runtime. First implementation:

- Credentials are rotated by creating a new Secret and pointing `Model.credentialsRef` at it.
  The name is part of the resolved configuration, so the hash changes and the Agent rolls.
- Editing a Secret in place is unsupported. Operators who do it must restart the Agent by
  deleting its pods. Doing this through a Deployment annotation would conflict with
  Crossplane's ownership of the Deployment.
- Later options, if this proves painful: a reloader controller, or the composition including
  a hash of the Secret in the pod template. The second needs the composition function to
  read Secret data, which widens its permissions, so it is deferred on purpose.

## Handover from the existing CRD

`api/v1alpha1/agent.yaml` is managed by the ArgoCD `api` application, which has pruning
enabled. Deleting the file in Git would make ArgoCD delete the CRD, and deleting a CRD deletes
every Agent. A single commit that removes the CRD and adds the XRD is therefore not a safe
transition. The order is:

1. **Protect.** In its own change, annotate the CRD with
   `argocd.argoproj.io/sync-options: Prune=false` and let ArgoCD sync it. Verify the
   annotation is live on the cluster before continuing.
2. **Confirm nothing depends on it.** List Agents in every namespace. The expected answer
   today is none.
3. **Release.** Remove the file from the `api` path. With `Prune=false` ArgoCD leaves the CRD
   in place and reports it as orphaned.
4. **Adopt.** Apply the XRD for `agents.openkindred.io`. Crossplane generates the CRD from it.
   Remove ArgoCD's tracking annotation from the old CRD if it remains.
5. **Verify** that the served schema is the XRD's and that ArgoCD no longer reports the CRD
   as out of sync.

Steps 3 to 5 depend on how Crossplane behaves when the CRD it generates already exists.
That has not been tested and **must be rehearsed on a throwaway cluster** before it is done
anywhere that holds Agents. Until then, the safe state is no Agents anywhere and the hub
not yet bootstrapped from this repository.

ArgoCD ordering for the new resources: Crossplane and its functions first, then the XRD and
Composition, then Agents (sync waves).

## Composition functions

`function-go-templating` can request existing resources itself: a template emits an
`ExtraResources` document naming what it needs by name or label, and Crossplane returns them
to the template in `extraResources`. That is enough to read the `Model` and `McpServer`
resources, so **no separate fetching function is needed**. Its template functions include
`sha256sum`, so a content-addressed ConfigMap name is possible in the template.

Not yet verified, and needed before writing the composition:

- Whether lookups of namespaced resources can be scoped to the Agent's namespace. The
  documented requirement fields are `apiVersion`, `kind`, `matchName` and `matchLabels`, and
  I found no namespace field.
- Whether Crossplane v2 renames this mechanism ("required resources") and what the function
  expects on the version we install.
- How the composite reports per-reference failures as conditions from a template.

If namespace scoping is not possible, the fallback is a small function written in Go.

## Rejected

- **Rolling updates for the first implementation.** See rollout and revocation.
- **Hot reload of configuration.** Two paths to the same state; harder to reason about
  permissions.
- **KubeVela in the execution path.** It would not reconcile the standalone Agent resource
  without a bridge.

## First milestone

Apply an Agent and send a request. See a model call and an allowed MCP tool call, then a
response. Change the instructions and the referenced endpoint and see both take effect.
Remove a dependency and get a clear failure. Delete the Agent and see its workloads go
while shared Model and McpServer resources remain.

The runtime's part of this (`runtime/`) is proven locally without a cluster. Deployment,
reconciliation, readiness and the CRD handover still need cluster testing.
