#!/usr/bin/env bash
# Create a local hub cluster with kind and hand it to ArgoCD.
# Requires docker, kind and kubectl. After this, Git is the source of truth.
set -euo pipefail

ARGOCD_VERSION="${ARGOCD_VERSION:-v3.5.5}"
cd "$(dirname "$0")/.."

kind get clusters | grep -qx openkindred-hub \
  || kind create cluster --config platform/hub/kind-config.yaml

kubectl config use-context kind-openkindred-hub
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n argocd --server-side \
  -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
kubectl -n argocd rollout status deploy/argocd-server --timeout=300s
kubectl apply -f platform/hub/root-app.yaml

echo "Hub is up. Initial admin password:"
echo "  kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
