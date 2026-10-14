# Local hub

`hack/bootstrap-hub.sh` creates a kind cluster, installs ArgoCD and applies
`platform/hub/root-app.yaml`. The root application syncs `platform/hub/apps`, which
deploys the CRDs from `api/v1alpha1`, Crossplane and KubeVela.

Requires docker, kind and kubectl. ArgoCD is pinned to a version in the script and can be
overridden with `ARGOCD_VERSION`.

The applications track `main` on GitHub, so changes only reach the hub once merged.

## Versions

| Component | Source | Version |
|---|---|---|
| ArgoCD | install manifest | v3.5.5 |
| Crossplane | `crossplane` chart | 2.4.2 |
| KubeVela | `vela-core` chart | 1.10.9 |

Crossplane 2.4.2 is the latest release tag. KubeVela is pinned one minor behind the newest
tag (1.11.0). I could not reach the chart repositories to confirm that these chart versions
are published, so check with `helm search repo` if a sync fails.

Both are installed with server-side apply because their CRDs are large.

## Not yet done

Crossplane providers and compositions, and KubeVela definitions. Nothing uses the two
controllers yet.
