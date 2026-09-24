#!/usr/bin/env bash
#
# CASE 9 — Instance lifecycle survives an InstanceType unit-spec change   (MUTATING, self-recovering)
#
#   case-9.sh <NS>
#
# Goal:        Two admin scenarios on the general pool —
#              Q1: a RUNNING Instance whose unit spec shrank can still be STOPPED and DELETED. That
#                  state cannot be entered: an InstanceType's spec is immutable apart from
#                  displayName/description/inactive, and a running Instance's spec.type is immutable,
#                  so nothing can put a smaller unit spec behind an Instance while it runs. Q1 is
#                  therefore a SKIP, and the SKIP is ASKED FOR rather than declared: both refusals are
#                  submitted as server-side dry runs and quoted, and either one being admitted turns
#                  the row into a FAIL naming the route, because Q1 is then reachable and unmeasured.
#              Q2: (re)starting a STOPPED Instance against a SMALLER unit spec follows the
#                  instance-general-resources-overcommit setting — ON (default) re-derives CPU/RAM
#                  from the unit spec it starts against and resizes-and-starts; OFF keeps the retained
#                  resources and the start-time cap rejects them. The smaller unit spec is reached the
#                  one way the product allows: the stopped Instance is moved to a sibling InstanceType
#                  on the same pool with a smaller unit RAM (spec.type is editable while stopped).
#                  (Whether resize-on-start is the desired semantics is a separate product question;
#                  this case pins the behavior as it stands.)
# Environment: Any cluster with a materialized general pool whose unit RAM is at least 2Gi (a unit RAM
#              is a whole number of Gi, so 1Gi leaves no smaller sibling and Q2 SKIPS). No GPU. Reads
#              the overcommit setting from the settings Secret and asserts the matching branch.
# Inputs:      All real, nothing mocked — a case-owned sibling InstanceType on the general pool with
#              unit RAM 1Gi; INST_A (running, the target of the Q1 dry run) and INST_B (running →
#              stopped → moved to the sibling → started), both alpine. The general InstanceType is
#              only read: its unit spec is the dry run's target and is never changed.
# Expected:    - Q1 — SKIP quoting both refusals; FAIL if either dry run is admitted;
#              - Q2 precondition — moving the stopped INST_B to the sibling type is accepted;
#              - Q2 (overcommit ON) — INST_B starts and its spec.resources.ram re-derives to 1Gi;
#                Q2 (overcommit OFF) — the start is rejected for exceeding the sibling's RAM cap.
# Cleanup:     Trap deletes both test Instances, then the sibling InstanceType, and waits for it to go.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-9.sh <NS>}"
SMALL_IT=gpustack-e2e-case9-small     # the sibling type with the smaller unit RAM, owned by this case
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' \
  | grep -v -x "$SMALL_IT" | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
INST_A=gpustack-e2e-lifecycle-stop    # Q1: the running Instance the dry run targets
INST_B=gpustack-e2e-lifecycle-start   # Q2: stopped → moved to the sibling → (re)started
SMALL_RAM=1Gi

# The Q2 outcome depends on the overcommit setting; read it (seeded in the settings Secret).
OVERCOMMIT=$(kubectl -n "$NS" get secret gpustack-settings -o jsonpath='{.data.instance-general-resources-overcommit}' 2>/dev/null | base64 -d 2>/dev/null)
[ -n "$OVERCOMMIT" ] || OVERCOMMIT=true

restore() {
  echo
  echo "[case-9] cleanup: deleting test Instances and the sibling InstanceType"
  kubectl -n default delete instance "$INST_A" "$INST_B" --ignore-not-found 2>/dev/null || true
  kubectl delete instancetypes.worker.gpustack.ai "$SMALL_IT" --ignore-not-found --wait=false 2>/dev/null || true
  # Waited for, because the next case lists InstanceTypes and one on its way out is a type a
  # deployment cannot name.
  for _ in $(seq 1 30); do
    kubectl get instancetypes.worker.gpustack.ai "$SMALL_IT" >/dev/null 2>&1 || return 0
    sleep 2
  done
  echo "[case-9] WARNING: InstanceType ${SMALL_IT} is still present after 60s"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

wait_phase() { # name phase
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get instance "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$2" ] && return 0
    sleep 3
  done
  return 1
}

mk_instance() { # name
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: $1, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF
}

# The general type's own unit spec and pool identity, which the sibling copies except for its RAM.
read -r IT_CPU IT_RAM IT_STG IT_GROUP IT_OS IT_ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$IT" \
  -o jsonpath='{.spec.unitResources.cpu} {.spec.unitResources.ram} {.spec.localStorage} {.spec.generalGroup} {.spec.os} {.spec.arch}' 2>/dev/null)"
IT_RAM_GI="${IT_RAM%Gi}"
case "$IT_RAM_GI" in '' | *[!0-9]*) IT_RAM_GI=0 ;; esac
echo "[case-9] general InstanceType ${IT}: unit cpu=${IT_CPU} ram=${IT_RAM} localStorage=${IT_STG}"

# The sibling type on the same pool, identical except for its unit RAM. Created first because both
# questions use it: Q1's dry run names it as the type a running Instance would move to (a type that
# does not exist is refused by the mutating half for not existing, before the rule under test is
# reached), and Q2 moves the stopped Instance onto it.
sibling_out=$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: InstanceType
metadata:
  name: ${SMALL_IT}
spec:
  displayName: case-9 smaller unit RAM
  acceleratable: false
  generalGroup: ${IT_GROUP}
  os: ${IT_OS}
  arch: ${IT_ARCH}
  localStorage: ${IT_STG}
  unitResources:
    cpu: "${IT_CPU}"
    ram: ${SMALL_RAM}
EOF
)
small_phase=""
for _ in $(seq 1 40); do
  small_phase=$(kubectl get instancetypes.worker.gpustack.ai "$SMALL_IT" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$small_phase" = Active ] && break
  sleep 3
done
[ "$small_phase" = Active ] || { echo "sibling ${SMALL_IT} never became Active (phase=${small_phase:-<none>}); apply said: $(echo "$sibling_out" | tr '\n' ' ' | cut -c1-200)"; exit 1; }

# A running INST_A (Q1) and a running→stopped INST_B (Q2), both sized by the general type.
mk_instance "$INST_A"
mk_instance "$INST_B"
wait_phase "$INST_A" Ready || { echo "instance ${INST_A} did not reach Ready"; exit 1; }
wait_phase "$INST_B" Ready || { echo "instance ${INST_B} did not reach Ready"; exit 1; }
kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
wait_phase "$INST_B" Stopped || { echo "instance ${INST_B} did not reach Stopped"; exit 1; }
ramB=$(kubectl -n default get instance "$INST_B" -o jsonpath='{.spec.resources.ram}' 2>/dev/null)
echo "[case-9] ${INST_A} Ready, ${INST_B} Stopped (ram=${ramB}); sibling ${SMALL_IT} Active (unit ram ${SMALL_RAM})"

# Q1 — asked, not declared. Each route that could put a smaller unit spec behind a RUNNING Instance
# is submitted as a server-side dry run, which runs the webhooks and persists nothing.
#
# Against the v1alpha1 CRD, never the unversioned resource. `instancetypes` and `instances` resolve
# to worker.gpustack.ai/v1 -- the aggregated API -- which answers a server-side dry run with
# "patched" without running the CRD's admission (measured: the same patch refused on v1alpha1 and
# refused for real through v1). Through v1 both routes would read as open, and this row would report
# Q1 reachable for a reason that is not about Q1.
q1_unit=$(kubectl patch instancetypes.v1alpha1.worker.gpustack.ai "$IT" --dry-run=server --type=merge \
  -p "{\"spec\":{\"unitResources\":{\"ram\":\"${SMALL_RAM}\"}}}" 2>&1)
q1_unit_rc=$?
q1_type=$(kubectl -n default patch instances.v1alpha1.worker.gpustack.ai "$INST_A" --dry-run=server --type=merge \
  -p "{\"spec\":{\"type\":\"${SMALL_IT}\"}}" 2>&1)
q1_type_rc=$?
q1_unit=$(echo "$q1_unit" | tr '\n' ' ')
q1_type=$(echo "$q1_type" | tr '\n' ' ')
if [ "$q1_unit_rc" -ne 0 ] && echo "$q1_unit" | grep -qF 'is immutable except displayName, description and inactive' \
  && [ "$q1_type_rc" -ne 0 ] && echo "$q1_type" | grep -qF 'type is immutable'; then
  record SKIP "Q1 running instance stops and deletes after a unit-spec shrink" \
    "unreachable: the unit spec is refused in place (${q1_unit:0:140}) and a running Instance cannot change type (${q1_type:0:140})"
else
  record FAIL "Q1 running instance stops and deletes after a unit-spec shrink" \
    "a route to Q1's state was not refused as expected, so Q1 is reachable and unmeasured: unit-spec edit rc=${q1_unit_rc} '${q1_unit:0:140}'; running retype rc=${q1_type_rc} '${q1_type:0:140}'"
fi

# Q2 — start the stopped Instance against the sibling's smaller unit spec.
if [ "$IT_RAM_GI" -lt 2 ]; then
  record SKIP "Q2 start against a smaller unit spec" \
    "the general type's unit RAM is ${IT_RAM:-<unset>}, and a unit RAM is a whole number of Gi, so the ${SMALL_RAM} sibling is not smaller"
else
  retype_out=$(kubectl -n default patch instance "$INST_B" --type=merge -p "{\"spec\":{\"type\":\"${SMALL_IT}\"}}" 2>&1)
  retype_rc=$?
  if [ "$retype_rc" -ne 0 ]; then
    record FAIL "Q2 precondition: the stopped instance moves to the sibling type" \
      "moving stopped ${INST_B} to ${SMALL_IT} was refused: $(echo "$retype_out" | tr '\n' ' ' | cut -c1-160)"
  else
    record PASS "Q2 precondition: the stopped instance moves to the sibling type" \
      "${INST_B} now names ${SMALL_IT} (unit ram ${SMALL_RAM}, was ${IT_RAM})"

    echo "[case-9] Q2: starting ${INST_B} against ${SMALL_IT} (instance-general-resources-overcommit=${OVERCOMMIT})"
    err=$(kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":false}}' 2>&1 >/dev/null)
    rc=$?
    if [ "$OVERCOMMIT" = "true" ]; then
      # Overcommit re-derives CPU/RAM from the unit spec the Instance starts against → resized down.
      newram=""
      wait_phase "$INST_B" Ready && newram=$(kubectl -n default get instance "$INST_B" -o jsonpath='{.spec.resources.ram}' 2>/dev/null)
      if [ "$rc" -eq 0 ] && [ "$newram" = "$SMALL_RAM" ]; then
        record PASS "Q2 start re-derives to the smaller unit spec (overcommit)" "${INST_B} started; ram ${ramB} → ${newram}"
      else
        record FAIL "Q2 start re-derives to the smaller unit spec (overcommit)" \
          "rc=${rc}, ram='${newram:-?}' (want started with ram=${SMALL_RAM}): $(echo "$err" | tr '\n' ' ' | cut -c1-160)"
      fi
    else
      # No overcommit: the retained resources are rejected by the start-time cap of the new type.
      if [ "$rc" -ne 0 ] && echo "$err" | grep -qF "exceeds the maximum RAM ${SMALL_RAM} of instance type ${SMALL_IT}"; then
        record PASS "Q2 start rejected against the smaller unit spec (no overcommit)" \
          "start rejected: $(echo "$err" | grep -oF "exceeds the maximum RAM ${SMALL_RAM} of instance type ${SMALL_IT}")"
      else
        kubectl -n default patch instance "$INST_B" --type=merge -p '{"spec":{"stop":true}}' >/dev/null 2>&1 || true
        record FAIL "Q2 start rejected against the smaller unit spec (no overcommit)" \
          "rc=${rc} — expected a rejection naming the ${SMALL_RAM} RAM cap (ram ${ramB}): $(echo "$err" | tr '\n' ' ' | cut -c1-160)"
      fi
    fi
  fi
fi

echo
echo "== CASE 9 — Instance lifecycle survives an InstanceType unit-spec change =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Q1's state must stay unreachable (both routes refused); Start's"
  echo "outcome against a smaller unit spec follows instance-general-resources-overcommit"
  echo "(resize-and-start vs cap-and-reject)."
  exit 1
fi
# A skipped row verified NOTHING, and Q1 skips on every cluster today, so the footer counts them
# rather than reading like a run that verified everything.
SKIPS=$(printf '%s\n' "${ROWS[@]}" | grep -c '^SKIP|')
if [ "$SKIPS" -gt 0 ]; then
  echo "CASE 9 PASS with ${SKIPS} SKIPPED row(s) — each verified NOTHING; read its OBJECT column"
else
  echo "CASE 9 PASS"
fi
