# OpenKindred

OpenKindred turns a set of agnostic clusters into an agent platform: a place where a team can onboard a model, declare an agent as a Kubernetes resource, give it tools (MCP), memory, a browser, and a code interpreter, wire multiple agents together (A2A), secure every hop with real identity, and watch it all through end-to-end traces, entirely via GitOps.

It ships with two flagship patterns:

- **Financial services**: a multi-agent system for financial workflows.
- **Nightforge**: an autonomous coding pipeline running in hardware-isolated micro-VM sandboxes.

Everything is declarative and GitOps-driven (ArgoCD, Crossplane and KubeVela). You describe intent; the platform reconciles it across a hub and any number of spoke clusters.

> **Status:** early design. The layout below describes where things will live. Nothing here is production ready.

## Concepts

| Capability | Mechanism |
|---|---|
| Model onboarding | Declarative model resources, reconciled per cluster |
| Agents | Kubernetes custom resources, delivered by GitOps |
| Tools | MCP servers, declared and bound to agents |
| Memory, browser, code interpreter | Platform services agents can request by reference |
| Agent to agent | A2A |
| Identity | Workload identity on every hop |
| Observability | End-to-end traces across agents, tools and models |
| Isolation | Micro-VM sandboxes for untrusted code execution |

## Topology

A **hub** cluster runs the control plane: ArgoCD, Crossplane and KubeVela. **Spoke** clusters run the workloads. The hub reconciles desired state from Git onto each spoke.

## Repository layout

```
platform/
  hub/          Hub control plane (ArgoCD, Crossplane, KubeVela)
  spoke/        Spoke cluster baseline
  crossplane/   Compositions and XRDs
  kubevela/     Application definitions and traits
agents/
  finserv/      Multi-agent financial-services pattern
  nightforge/   Autonomous coding pipeline
docs/           Architecture and design notes
examples/       Minimal worked examples
```

## Contributing

Open an issue before large changes. Keep changes declarative and reviewable.
