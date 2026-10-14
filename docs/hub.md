# Local hub

`hack/bootstrap-hub.sh` creates a kind cluster, installs ArgoCD and applies
`platform/hub/root-app.yaml`. The root application syncs `platform/hub/apps`, which
currently deploys the CRDs from `api/v1alpha1`.

Requires docker, kind and kubectl. ArgoCD is pinned to a version in the script and can be
overridden with `ARGOCD_VERSION`.

The applications track `main` on GitHub, so changes only reach the hub once merged.

## Not yet on the hub

Crossplane and KubeVela. They will be added as further applications under
`platform/hub/apps`.
