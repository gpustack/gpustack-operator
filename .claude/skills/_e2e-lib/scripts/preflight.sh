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
kubectl config current-context
echo
kubectl cluster-info
echo
kubectl get nodes -o wide
