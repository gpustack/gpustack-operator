#!/usr/bin/env bash
#
# Provision a verification cluster from testing/infra/clusters/<MODALITY>.
# MUTATING and, for a cloud modality, BILLABLE PER HOUR — the skill confirms
# TWICE before running this: once that a cluster should be provisioned at all,
# and once on the modality after being told its cost. Run from the repo root.
#
#   provision.sh <eks|k3s|nebius|rke2> [extra terraform -var/... args...]
#
# Prefer a cluster the user already has: this script is only for the opt-in
# path. Whatever it creates, THIS RUN OWNS — destroy.sh <MODALITY> at teardown
# is mandatory, and a cluster the user brought is never destroyed.
#
# The extra args are passed to BOTH plan and apply, e.g.:
#   provision.sh nebius -var='project_id=<id>' -var='release=1.31'
#   provision.sh k3s    -var='server=["<addr>"]' -var='ssh_user=<user>'
#   provision.sh rke2   -var='server=["<addr>"]' -var='agent=["<addr>"]' \
#                       -var='node_internal_ip={"<addr>"="<ip>"}' -var='ssh_jumper=<addr>'
#   provision.sh eks    -var='region=<region>'
# Pass secrets/ids/addresses inline at run time; never write them into a file.
#
# Each module merges its cluster into ~/.kube/config as a new context and leaves
# it current. That is the one legitimate context change in a run — the "never
# switch context" rule is about never moving among the USER's own contexts, not
# about the context this run just created.
#
# Pass -var='switch_kube_context=false' (every modality takes it) to merge the
# cluster in WITHOUT making it current, when the user has something running on
# their active context. The "active context" line printed below is then still
# theirs, by design; reach the new cluster with kube-context.sh instead.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

MOD="${1:?usage: provision.sh <eks|k3s|nebius|rke2> [terraform args...]}"
shift
DIR="testing/infra/clusters/${MOD}"
if [ ! -d "$DIR" ]; then
  echo "unknown modality '${MOD}' — no ${DIR} (run from the repo root)"
  exit 1
fi

# A cluster is merged into the kubeconfig kubectl/aws/nebius resolve, and an isolated copy
# (kube-context.sh) is a throwaway snapshot of ONE context — the last place a brand-new
# cluster should be recorded, since nothing would ever read it back. Drop such a pin so the
# new context lands where the user can reach it, mirroring destroy.sh's cleanup. Any OTHER
# KUBECONFIG is the caller's deliberate choice of target and is left alone.
case "${KUBECONFIG:-}" in
*e2e-kubeconfig-*)
  echo "[provision] unsetting KUBECONFIG=${KUBECONFIG} so the new context is merged into the real kubeconfig"
  unset KUBECONFIG
  ;;
esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "== credential precheck =="
if ! bash "${HERE}/cluster-auth.sh" "$MOD"; then
  echo
  echo "[provision] REFUSING to provision: the credential that creates this cluster is also"
  echo "[provision] the credential that destroys it. Fix it first or paid hardware strands."
  exit 1
fi

echo
echo "== terraform init (${DIR}) =="
terraform -chdir="$DIR" init -input=false || exit 1

echo
echo "== terraform plan (${DIR}) ${*} =="
terraform -chdir="$DIR" plan -input=false "$@" || exit 1

echo
echo "== terraform apply (${DIR}) ${*} =="
terraform -chdir="$DIR" apply -input=false -auto-approve "$@" || exit 1

echo
echo "== resulting cluster =="
case "$MOD" in
eks)
  region=$(terraform -chdir="$DIR" output -raw region 2>/dev/null)
  name=$(terraform -chdir="$DIR" output -raw cluster_name 2>/dev/null)
  echo "cluster ${name} in ${region}; kubeconfig already refreshed by the module"
  echo "re-fetch with: aws eks --region ${region} update-kubeconfig --name ${name}"
  ;;
*)
  echo "context: $(terraform -chdir="$DIR" output -raw context_name 2>/dev/null)"
  ;;
esac
echo "active context: $(kubectl config current-context 2>/dev/null)"

echo
echo "[provision] done — THIS RUN PROVISIONED THIS CLUSTER."
echo "[provision] Teardown obligation: bash .claude/skills/_e2e-lib/scripts/destroy.sh ${MOD}"
echo "[provision] Record the modality and the -var inputs in the report (no addresses/ids)."
