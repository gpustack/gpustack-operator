#!/usr/bin/env bash
#
# Destroy the verification cluster THIS RUN provisioned from
# testing/infra/clusters/<MODALITY>. MUTATING and irreversible — the skill
# confirms before running this. Run from the repo root.
#
#   destroy.sh <eks|k3s|nebius|rke2> [extra terraform destroy args...]
#
# ONLY for a cluster provision.sh created in this run. A cluster the user
# brought is NEVER destroyed — for that one, the in-cluster teardown.sh is the
# whole teardown.
#
# No -var is normally needed: each module reuses the last successful apply's
# inputs (an auto-loaded *.auto.tfvars.json snapshot).
#
# The credential precheck is not ceremony: a module may resolve inputs from a
# live cloud API call at DESTROY time too, so an expired token here leaves paid
# hardware running. After the destroy, the state must be empty — if it is not,
# this exits non-zero and the run is NOT finished.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

MOD="${1:?usage: destroy.sh <eks|k3s|nebius|rke2> [terraform args...]}"
shift
DIR="testing/infra/clusters/${MOD}"
if [ ! -d "$DIR" ]; then
  echo "unknown modality '${MOD}' — no ${DIR} (run from the repo root)"
  exit 1
fi

# The module's destroy provisioner strips its context/cluster/user from the kubeconfig
# kubectl resolves, which has to be the user's real one — that is where the apply put
# them. A run pinned to an isolated copy (kube-context.sh) would otherwise have the
# entries deleted from the copy and left behind in ~/.kube/config. Matched as a
# substring, so a colon-separated KUBECONFIG that merely includes such a copy is
# caught too, not only a lone one.
case "${KUBECONFIG:-}" in
*e2e-kubeconfig-*)
  echo "[destroy] unsetting KUBECONFIG=${KUBECONFIG} so the kubeconfig cleanup reaches the real one"
  unset KUBECONFIG
  ;;
esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "== credential precheck (an expired token here strands the cluster) =="
if ! bash "${HERE}/cluster-auth.sh" "$MOD"; then
  echo
  echo "[destroy] STOP: cannot destroy with this credential. Re-authenticate the cloud CLI"
  echo "[destroy] and re-run — the cluster is still RUNNING and still BILLING."
  exit 1
fi

echo
echo "== terraform destroy (${DIR}) ${*} =="
terraform -chdir="$DIR" destroy -input=false -auto-approve "$@"
rc=$?

echo
echo "== leftover state =="
left=$(terraform -chdir="$DIR" state list 2>/dev/null | wc -l | tr -d ' ')
terraform -chdir="$DIR" state list 2>/dev/null

if [ "$rc" -ne 0 ] || [ "${left:-0}" != "0" ]; then
  echo
  echo "[destroy] NOT CLEAN — destroy rc=${rc}, ${left} resource(s) still in state."
  echo "[destroy] The cluster may still be RUNNING and BILLING. Re-run destroy.sh ${MOD}"
  echo "[destroy] after fixing the cause; do not close the run until state is empty."
  exit 1
fi

echo
echo "[destroy] done — ${MOD} state is empty, the provisioned cluster is gone."
echo "[destroy] The module also removed its kubeconfig context/cluster/user entries."
