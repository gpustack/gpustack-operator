#!/usr/bin/env bash
#
# E2E preflight (READ-ONLY). Shared by the gpustack-operator-e2e and
# gpustack-operator-chart-e2e skills.
#
# Shows the required host tools and the ACTIVE kube context so the operator can
# be told to confirm it is the intended LOCAL (k3s / docker-desktop) cluster
# before any mutation. This script NEVER switches context and NEVER mutates.
set -uo pipefail

echo "== required host tools =="
command -v kubectl helm docker || echo "MISSING a required tool (kubectl / helm / docker)"

echo
echo "== active kube context (confirm this is the intended LOCAL cluster) =="
ctx=$(kubectl config current-context 2>/dev/null)
echo "${ctx:-<none set>}"

# A kubeconfig may name a current-context it no longer defines — a cluster removed
# or the file rewritten underneath it. Every later kubectl then fails with the same
# "context was not found" line, which reads like a connectivity problem rather than
# a kubeconfig one, so say which it is before anything else runs.
if [ -n "${ctx}" ] && ! kubectl config get-contexts -o name | grep -qxF "${ctx}"; then
  echo
  echo "WARNING: current-context '${ctx}' is not defined in this kubeconfig — every"
  echo "         kubectl call fails until it is repointed at one that is:"
  kubectl config get-contexts -o name | sed 's/^/           /'
fi

echo
kubectl cluster-info
echo
kubectl get nodes -o wide
