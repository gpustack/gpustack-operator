#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
helm_bin="${HELM_BIN:-helm}"

function render() {
  "${helm_bin}" template gpustack-operator "${chart_dir}" "$@"
}

function expect_render() {
  local name="$1"
  shift

  local rendered
  rendered="$(render "$@")"
  if ! grep -Fq 'Source: gpustack-operator/charts/topograph/' <<<"${rendered}"; then
    echo "${name}: Topograph did not render" >&2
    return 1
  fi
  printf '%s' "${rendered}"
}

function expect_failure() {
  local name="$1" expected="$2"
  shift
  shift

  local output
  if output="$(render "$@" 2>&1)"; then
    echo "${name}: expected render failure" >&2
    return 1
  fi
  if ! grep -Fq "${expected}" <<<"${output}"; then
    echo "${name}: unexpected render failure: ${output}" >&2
    return 1
  fi
}

disabled="$(render --kube-version 1.26.0)"
if grep -Fq 'Source: gpustack-operator/charts/topograph/' <<<"${disabled}"; then
  echo "disabled: Topograph rendered" >&2
  exit 1
fi

generic="$(expect_render generic --kube-version 1.27.0 \
  --set topograph.enabled=true \
  --set topograph.provider.name=dra \
  --set-string 'topograph.nodeDataBroker.nodeSelector.topology\.gpustack\.ai/topograph=enabled')"
if ! grep -Fq 'topology.gpustack.ai/topograph: enabled' <<<"${generic}"; then
  echo 'generic: expected Topograph node selector is missing' >&2
  exit 1
fi

nvidia="$(expect_render nvidia --kube-version 1.27.0 \
  --set topograph.enabled=true \
  --set topograph.provider.name=dra \
  --set-string 'topograph.nodeDataBroker.nodeSelector.feature\.node\.kubernetes\.io/pci-10de\.present=true')"
if ! grep -Fq 'feature.node.kubernetes.io/pci-10de.present: "true"' <<<"${nvidia}"; then
  echo 'nvidia: expected NVIDIA node selector is missing' >&2
  exit 1
fi

aws="$(expect_render aws --kube-version 1.27.0 \
  --set topograph.enabled=true \
  --set topograph.provider.name=aws)"
if ! grep -Fq 'serviceAccountName: gpustack-operator-topograph' <<<"${aws}"; then
  echo 'aws: expected Topograph ServiceAccount name is missing' >&2
  exit 1
fi
if ! grep -Fq 'Source: gpustack-operator/charts/topograph/templates/serviceaccount.yaml' <<<"${aws}"; then
  echo "aws: Topograph API ServiceAccount was not rendered" >&2
  exit 1
fi

expect_failure kubernetes-1.26 'topograph.enabled requires Kubernetes 1.27 or later' \
  --kube-version 1.26.0 --set topograph.enabled=true --set topograph.provider.name=dra
expect_failure test-provider 'topograph.enabled requires a production provider' --kube-version 1.27.0 --set topograph.enabled=true
expect_failure wrong-engine 'topograph.enabled requires engine.name=k8s' --kube-version 1.27.0 --set topograph.enabled=true --set topograph.provider.name=dra --set topograph.engine.name=graph
