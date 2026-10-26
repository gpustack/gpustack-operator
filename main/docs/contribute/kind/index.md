# kind Setup

Use [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) to provide a local Kubernetes
cluster for development and integration tests. Keep its kubeconfig separate from other clusters.

## Contents

- [Prerequisites](#prerequisites)
- [Step 1: Install tools](#step-1-install-tools)
- [Step 2: Create a cluster](#step-2-create-a-cluster)
- [Step 3: Verify access](#step-3-verify-access)
- [Existing clusters](#existing-clusters)
- [Cleanup](#cleanup)

## Prerequisites

- A running container runtime supported by kind, such as Docker.
- Permission to create containers and download node images.

## Step 1: Install tools

On macOS with Homebrew:

```sh
brew install kind kubectl
```

For other platforms, follow the official [kind installation instructions](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
and [kubectl installation instructions](https://kubernetes.io/docs/tasks/tools/#kubectl).

```sh
kind version
kubectl version --client
```

## Step 2: Create a cluster

Choose an unused cluster name and kubeconfig path:

```sh
mkdir -p "$HOME/.kube"
kind create cluster --name gpustack-dev \
  --kubeconfig "$HOME/.kube/gpustack-kind.yaml" --wait 120s
```

This uses kind's default node image. Record the actual server version when testing.

## Step 3: Verify access

```sh
kubectl --kubeconfig "$HOME/.kube/gpustack-kind.yaml" \
  --context kind-gpustack-dev --request-timeout=15s cluster-info
kubectl --kubeconfig "$HOME/.kube/gpustack-kind.yaml" \
  --context kind-gpustack-dev --request-timeout=15s get nodes -o wide
```

Provide the kubeconfig path and context name to the local test runner. Keep the file private and
out of Git.

## Existing clusters

Export an existing cluster's kubeconfig to an unused file:

```sh
kind get kubeconfig --name gpustack-dev > "$HOME/.kube/gpustack-kind.yaml"
```

Then verify access using the preceding commands.

## Cleanup

The cluster owner removes the cluster when testing is complete:

```sh
kind delete cluster --name gpustack-dev \
  --kubeconfig "$HOME/.kube/gpustack-kind.yaml"
```

---

**See also** — [Development](/gpustack-operator/main/docs/contribute/development/index.md) (build and test commands) ·
[Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md) (operator deployment).

**Next** → [Development](/gpustack-operator/main/docs/contribute/development/index.md) — run the project checks against the selected cluster.
