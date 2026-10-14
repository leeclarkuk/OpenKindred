# Agent composition

`platform/crossplane/agent/` turns an `Agent` into a running runtime. Design decisions are in
[ADR 0001](adr/0001-agent-lifecycle.md). The runtime it deploys is described in
[runtime.md](runtime.md).

| File | Purpose |
|---|---|
| `xrd.yaml` | `Agent` as a namespaced Crossplane v2 composite, with admission rules |
| `composition.yaml` | Pipeline with one `function-go-templating` step |
| `rbac.yaml` | Lets Crossplane manage the generated resources and read `Model` and `McpServer` |
| `tests/` | Inputs for the render tests |

**These files are inert.** No ArgoCD application syncs `platform/crossplane/agent`, because the
XRD defines `agents.openkindred.io`, which the plain CRD in `api/v1alpha1/agent.yaml` already
serves. Wiring this up is the CRD handover in the ADR and needs a cluster rehearsal first.

## What it generates

For an Agent named `helper` in namespace `demo`:

| Resource | Notes |
|---|---|
| `ConfigMap helper-config-<hash>` | Immutable. The runtime configuration, named by the first 12 hex characters of its SHA-256, which is also the hash the runtime reports at `/readyz`. |
| `Deployment helper` | One replica, `Recreate`. Non-root, read-only root filesystem, all capabilities dropped, no service account token, probes on `/readyz` and `/healthz`. |
| `Service helper` | ClusterIP, port 80 to the runtime. |
| `ServiceAccount helper` | One per Agent, no token mounted. |
| `NetworkPolicy helper` | Ingress from pods in the same namespace only. Egress is open except link-local (cloud metadata). Narrowing egress to the model and tool endpoints is future work. |

`Model` and `McpServer` are read from the Agent's own namespace only.

## Behaviour

- **Reference changes.** The template looks up the `Model` and each `McpServer` on every
  reconcile. Any change produces a new ConfigMap name and so a new pod template, which rolls
  the Deployment. Propagation time is Crossplane's poll interval plus rollout time.
- **Fail closed.** If a reference is missing, or an `McpServer` has only an `image` (not yet
  supported), the resources are still rendered and the problem is written into the
  configuration's `unresolved` list. The runtime then refuses to execute and says why. Nothing
  is deleted.
- **Conditions.** `ReferencesResolved` and `WorkloadAvailable` are set on the Agent. `Ready` is
  Crossplane's, and is true only when the Deployment's ready annotation is true, which needs all
  of: references resolved; the observed Deployment's pod template carries the hash of the
  template just rendered; its `observedGeneration` equals its `generation`; and updated,
  total and available replicas are all 1 with none unavailable. An available replica of an
  older template therefore never makes the Agent Ready.
- **Allow lists.** Omitted and empty stay distinct all the way into the configuration.
- **Credentials.** `Model.credentialsRef` becomes `secretKeyRef` on the environment variable
  `OPENKINDRED_MODEL_API_KEY` (key defaults to `api-key`). The Secret name never enters the
  configuration. Rotating by pointing at a new Secret changes the pod template and rolls the
  Agent. Editing a Secret in place does not (ADR).
- **Status.** `status.configHash` and `status.endpoint`.

The runtime image is a single constant at the top of the template
(`ghcr.io/leeclarkuk/openkindred-runtime:0.1.0`). **No such image exists yet.**

## Tests

```
python3 hack/test-composition.py       # renders the composition with the real engine
(cd hack/xrdcheck && go test ./...)    # evaluates the XRD admission rules with CEL
```

The first renders each scenario with the Crossplane render engine and checks the output,
including feeding the rendered configuration to the runtime's own parser
(`runtime/cmd/configcheck`). By default it uses docker. Without docker, build the tools with
`hack/build-render-tools.sh` and set the environment variables it prints.

What these tests do not cover is in the PR description and the ADR: a real API server, the
Crossplane controllers, RBAC, the function package being installable, admission of the XRD, and
the image.
