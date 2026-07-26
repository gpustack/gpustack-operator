#!/usr/bin/env bash
#
# Verify the toolchain + cloud credential a cluster modality's Terraform module
# needs. READ-ONLY — it never provisions, destroys, or mutates a cluster.
#
#   cluster-auth.sh <eks|k3s|nebius>
#
# Run it TWICE in a run: once BEFORE provisioning, and again BEFORE the teardown
# destroy. A module can resolve inputs from a LIVE cloud API call at plan, apply
# AND destroy time, so a credential that expired mid-run fails `terraform
# destroy` and strands paid hardware. provision.sh and destroy.sh both call this
# and refuse to run when it fails.
#
# Exit 0 = ready, 1 = not ready (missing tool / unauthenticated / unknown modality).
set -uo pipefail

MOD="${1:?usage: cluster-auth.sh <eks|k3s|nebius>}"
FAILS=0
need() {
  if command -v "$1" >/dev/null 2>&1; then
    echo "ok      tool ${1}"
  else
    echo "MISSING tool ${1}"
    FAILS=$((FAILS + 1))
  fi
}

echo "== cluster modality: ${MOD} =="
need terraform
need kubectl

case "$MOD" in
eks)
  need aws
  echo "== aws credential (aws sts get-caller-identity) =="
  if aws sts get-caller-identity >/dev/null 2>&1; then
    echo "ok      aws credential valid"
  else
    echo "FAILED  aws sts get-caller-identity — credential missing or expired"
    FAILS=$((FAILS + 1))
  fi
  ;;
nebius)
  need nebius
  need jq
  # The module resolves each GPU group's os/drivers_preset through this exact
  # CLI call in a data.external, on plan, apply AND destroy. Probing the same
  # call is the only check that proves a destroy will not fail on it later.
  REL="${NEBIUS_COMPAT_RELEASE:-1.31}"
  PLAT="${NEBIUS_COMPAT_PLATFORM:-gpu-h100-sxm}"
  echo "== nebius credential (the compatibility-matrix call the module depends on) =="
  echo "nebius mk8s node-group get-compatibility-matrix --cluster-kubernetes-version ${REL} --platform ${PLAT} --format json"
  if nebius mk8s node-group get-compatibility-matrix \
    --cluster-kubernetes-version "$REL" --platform "$PLAT" --format json >/dev/null 2>&1; then
    echo "ok      nebius credential valid (matrix resolved)"
  else
    echo "FAILED  compatibility-matrix call — CLI unauthenticated, token expired, or release/platform invalid"
    echo "        override the probe with NEBIUS_COMPAT_RELEASE / NEBIUS_COMPAT_PLATFORM"
    FAILS=$((FAILS + 1))
  fi
  ;;
k3s)
  need ssh
  # No cloud credential: the module installs onto servers the user already owns.
  # Their reachability and passwordless sudo are preconditions this script
  # cannot check without the host addresses, which are never written to a file.
  echo "note    k3s installs onto servers you already own — verify passwordless SSH"
  echo "        and passwordless sudo yourself; addresses stay in the live command only"
  ;;
*)
  echo "unknown modality '${MOD}' (expected eks | k3s | nebius)"
  exit 1
  ;;
esac

echo
if [ "$FAILS" -ne 0 ]; then
  echo "NOT READY — ${FAILS} problem(s). Fix them before provisioning or destroying."
  exit 1
fi
echo "READY — ${MOD} toolchain and credential are usable."
