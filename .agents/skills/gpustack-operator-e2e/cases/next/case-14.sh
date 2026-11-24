#!/usr/bin/env bash
#
# CASE 14 — Unified NIC/RDMA inventory, extended resource sizing, container device-cgroup isolation and host-fabric member DaemonSet   (MUTATING, self-recovering; RDMA-hardware legs auto-skip cleanly without RDMA NICs)
#
#   case-14.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_MOONCAKE_IMAGE,E2E_RDMA_IMAGE,E2E_RDMA_WORKLOAD_IMAGE
# Goal:        Verifies the full NIC/RDMA stack across four legs in one self-recovering run:
#              1. Host-fabric member DaemonSet rendering (runs on any cluster): a KVCacheBackend
#                 whose transport is RDMA renders a member DaemonSet limiting
#                 device.gpustack.ai/rdma.shared=1 with zero /dev/infiniband hostPath mount,
#                 hostNetwork=true + ClusterFirstWithHostNet, and IPC_LOCK/SYS_RESOURCE without
#                 privileged;
#              2. NIC/RDMA inventory, rdma.* labels, stability & restart (runs when any Devices
#                 object exists): Devices.spec.interfaces is non-empty, sorted by name, marks
#                 virtual interfaces, sets pciRootId to the outermost pciSwitches bridge, agrees
#                 with node rdma.* labels in both directions (negative when no RDMA; link states,
#                 Failed reason/firstSeenTime, and existential rdma.capable when RDMA is present),
#                 stays generation-stable and byte-identical across quiet detect passes, and keeps
#                 hardware-bearing rows intact across a device-manager Pod restart;
#              3. RDMA extended resource sizing (runs when any node carries an RDMA endpoint):
#                 host /sys/class/infiniband correspondence via an independent read-only probe Pod,
#                 per-mode allocatable counts (whole, shared = N x pool size, partitioned = usable
#                 VFs), retired .sliced absent or 0, Failed endpoint capacity vs allocatable, and
#                 SR-IOV sriov_numvfs vs ledger VF/PF separation;
#              4. Container device-cgroup open()/verbs isolation (runs when any node carries a
#                 whole-function RDMA endpoint): an unprivileged Pod requesting one shared endpoint
#                 receives NCCL_IB_HCA matching the ledger, per-endpoint /dev/infiniband/uverbs*,
#                 succeeds on open(O_RDWR), and optionally passes ibv_devinfo (E2E_RDMA_IMAGE); the
#                 identical unprivileged control Pod without the request is refused open().
# Environment: Any cluster with the operator installed. Leg 1 always runs. Legs 2, 3 and 4
#              auto-skip individually when no Devices object, no RDMA endpoint, or no whole-function
#              RDMA endpoint is present, recording explicit SKIP rows rather than aborting earlier
#              verdicts.
# Inputs:      All real, nothing mocked — one render-only KVCacheBackend (deleted after Leg 1),
#              one device-manager Pod restart (Leg 2), one read-only /sys probe Pod (Legs 3-4),
#              and two unprivileged workload Pods (Leg 4).
# Expected:    Every applicable leg records PASS; absent hardware legs record SKIP with the reason.
# Cleanup:     Trap deletes the KVCacheBackend and calls rdma_cleanup to delete all test and probe
#              Pods. Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: case-14.sh <NS>}"
# shellcheck disable=SC2034 # Read by _rdma-lib.sh.
CASE_ID=14
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

QUIET_SECONDS="${CASE14_QUIET_SECONDS:-70}"
RESTART_SECONDS="${CASE14_RESTART_SECONDS:-300}"
SETTLE_SECONDS="${CASE14_SETTLE_SECONDS:-40}"
WORK_IMAGE="${E2E_RDMA_WORKLOAD_IMAGE:-$RDMA_PROBE_IMAGE}"
MOONCAKE_IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

RENDER_ONLY_LABEL="gpustack.ai/e2e-c14-render-only"
BACKEND="e2e-c14-fabric-grant-$$"
DEVICE_TREE="/dev/infiniband"

cleanup_all() {
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rdma_cleanup
}
trap cleanup_all EXIT

# ===================================================================================================
# Leg 1 — Host-fabric member DaemonSet rendering (runs on any cluster, no RDMA hardware needed).
# ===================================================================================================
echo
echo "[case-14] === leg 1: host-fabric RDMA KVCacheBackend member DaemonSet rendering ==="

if ! kubectl apply -f - >/dev/null 2>&1 <<YAML; then
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${MOONCAKE_IMAGE}
  transport:
    protocol: RDMA
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector: {${RENDER_ONLY_LABEL}: "true"}
          medium: DRAM
          capacityPerMember: 1Gi
YAML
  record FAIL "leg 1: operator accepts RDMA KVCacheBackend" "apply was refused"
else
  record PASS "leg 1: operator accepts RDMA KVCacheBackend" "${BACKEND} applied (render-only selector)"
  DS="${BACKEND}-member-0"
  TEMPLATE=""
  for _ in $(seq 1 30); do
    TEMPLATE=$(kubectl -n "$NS" get daemonset "$DS" -o json 2>/dev/null)
    [ -n "$TEMPLATE" ] && break
    sleep 2
  done
  if [ -z "$TEMPLATE" ]; then
    record FAIL "leg 1: member DaemonSet is rendered" "${DS} did not appear in ${NS} within 60s"
  else
    record PASS "leg 1: member DaemonSet is rendered" "${DS} in ${NS}"
    POD_SPEC=$(printf '%s' "$TEMPLATE" | jq -c '.spec.template.spec')

    GRANT=$(printf '%s' "$POD_SPEC" | jq -r --arg k "$RDMA_KEY_SHARED" '.containers[0].resources.limits[$k] // "ABSENT"')
    if [ "$GRANT" = "1" ]; then
      record PASS "leg 1: member requests one shared RDMA endpoint" "${RDMA_KEY_SHARED}=1"
    else
      record FAIL "leg 1: member requests one shared RDMA endpoint" "${RDMA_KEY_SHARED}=${GRANT}"
    fi

    VOLS=$(printf '%s' "$POD_SPEC" | jq -r --arg p "$DEVICE_TREE" '[.volumes // [] | .[] | select(.hostPath.path == $p) | .name] | join(",")')
    MOUNTS=$(printf '%s' "$POD_SPEC" | jq -r --arg p "$DEVICE_TREE" '[.containers[0].volumeMounts // [] | .[] | select(.mountPath == $p) | .name] | join(",")')
    if [ -z "$VOLS" ] && [ -z "$MOUNTS" ]; then
      record PASS "leg 1: member mounts no device tree" "no volume and no volumeMount on ${DEVICE_TREE}"
    else
      record FAIL "leg 1: member mounts no device tree" "volume(s)='${VOLS}' mount(s)='${MOUNTS}' on ${DEVICE_TREE}"
    fi

    HOSTNET=$(printf '%s' "$POD_SPEC" | jq -r '.hostNetwork // false')
    DNSPOL=$(printf '%s' "$POD_SPEC" | jq -r '.dnsPolicy // ""')
    if [ "$HOSTNET" = "true" ] && [ "$DNSPOL" = "ClusterFirstWithHostNet" ]; then
      record PASS "leg 1: member keeps host-fabric network base" "hostNetwork=true dnsPolicy=ClusterFirstWithHostNet"
    else
      record FAIL "leg 1: member keeps host-fabric network base" "hostNetwork=${HOSTNET} dnsPolicy=${DNSPOL}"
    fi

    CAPS=$(printf '%s' "$POD_SPEC" | jq -r '[.containers[0].securityContext.capabilities.add // [] | .[]] | sort | join(",")')
    PRIV=$(printf '%s' "$POD_SPEC" | jq -r '.containers[0].securityContext.privileged // "unset"')
    if [ "$CAPS" = "IPC_LOCK,SYS_RESOURCE" ] && [ "$PRIV" = "unset" ]; then
      record PASS "leg 1: member gets IPC_LOCK,SYS_RESOURCE without privileged" "add=[${CAPS}] privileged=${PRIV}"
    else
      record FAIL "leg 1: member gets IPC_LOCK,SYS_RESOURCE without privileged" "add=[${CAPS}] privileged=${PRIV}"
    fi
  fi
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
fi

# ===================================================================================================
# Leg 2 — NIC/RDMA inventory, rdma.* labels, stability & restart (requires Devices objects).
# ===================================================================================================
echo
echo "[case-14] === leg 2: NIC/RDMA inventory, rdma.* labels, stability and restart ==="

inventory() { kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{.spec.interfaces}' 2>/dev/null; }
interfaces() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{range .spec.interfaces[*]}{.name}{"\t"}{.bus}{"\t"}{.virtual}{"\t"}{.pciBusId}{"\t"}{.pciRootId}{"\t"}{.pciSwitches}{"\t"}{.rdma}{"\t"}{.rdmaDevice}{"\t"}{.link.state}{"\t"}{.link.reason}{"\t"}{.link.firstSeenTime}{"\n"}{end}' 2>/dev/null
}
vf_rows() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{range .spec.interfaces[*]}{range .virtualFunctions[*]}{.pciBusId}{"\t"}{.rdma}{"\t"}{.link.state}{"\t"}{.link.reason}{"\t"}{.link.firstSeenTime}{"\n"}{end}{end}' 2>/dev/null
}
significant_interfaces() {
  interfaces "$1" | awk -F'\t' '$3 != "true" || $7 == "true" || $9 != ""'
}
generation() { kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{.metadata.generation}' 2>/dev/null; }
rdma_labels() {
  local labels
  labels=$(kubectl get node "$1" -o jsonpath='{.metadata.labels}' 2>/dev/null) || return
  printf '%s' "$labels" | tr ',' '\n' | grep -o 'feature\.gpustack\.ai/rdma\.[a-z]*' | sort -u || true
}

NODES_RAW=$(kubectl get devices.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
read_failed $? "the list of Devices objects"
NODES=$(printf '%s' "$NODES_RAW" | grep -v '^$' || true)

if [ -z "$NODES" ]; then
  record SKIP "leg 2: NIC/RDMA inventory, labels, stability and restart" "no Devices object in cluster — no node runs a device manager"
  record SKIP "leg 3: RDMA extended resource sizing" "no Devices object in cluster"
  record SKIP "leg 4: RDMA container device-cgroup isolation" "no Devices object in cluster"
  rdma_results
  exit 0
fi

echo "[case-14] profiling nodes: $(echo "$NODES" | tr '\n' ' ')"
for node in $NODES; do
  before=$(interfaces "$node"); read_failed $? "the interface rows of $node"
  vf_before=$(vf_rows "$node"); read_failed $? "the virtual-function rows of $node"
  if [ -z "$before" ]; then
    record FAIL "${node} publishes an interface inventory" "spec.interfaces is empty"
    continue
  fi
  count=$(printf '%s\n' "$before" | grep -c .)
  nameless=$(printf '%s\n' "$before" | awk -F'\t' '$1 == "" || $2 == "" {c++} END {print c+0}')
  if [ "$nameless" -eq 0 ]; then
    record PASS "${node} publishes an interface inventory" "${count} interface(s), all named and bused"
  else
    record FAIL "${node} publishes an interface inventory" "${nameless} of ${count} entries lack a name or bus"
  fi

  names=$(printf '%s\n' "$before" | awk -F'\t' '{print $1}')
  if [ "$names" = "$(printf '%s\n' "$names" | LC_ALL=C sort)" ]; then
    record PASS "${node} publishes the list sorted by name" "$(printf '%s' "$names" | tr '\n' ' ')"
  else
    record FAIL "${node} publishes the list sorted by name" "order is $(printf '%s' "$names" | tr '\n' ' ')"
  fi

  virtual=$(printf '%s\n' "$before" | awk -F'\t' '$3 == "true" {c++} END {print c+0}')
  if [ "$virtual" -gt 0 ]; then
    record PASS "${node} marks virtual interfaces" "${virtual} of ${count}"
  else
    record FAIL "${node} marks virtual interfaces" "none of ${count} is virtual"
  fi

  read -r checked bad <<EOF
$(printf '%s\n' "$before" | awk -F'\t' '
    $2 == "pci" && $4 != "" {
      checked++
      switches = $6
      gsub(/^\[|\]$|"/, "", switches)
      if (switches == "") { want = $4 }
      else { n = split(switches, parts, ","); want = parts[n] }
      if ($5 != want) bad++
    }
    END { print checked+0, bad+0 }')
EOF
  if [ "$checked" -eq 0 ]; then
    record SKIP "${node} pciRootId is the outermost bridge" "no PCI-backed interface on this node"
  elif [ "$bad" -eq 0 ]; then
    record PASS "${node} pciRootId is the outermost bridge" "${checked} PCI interface(s) consistent"
  else
    record FAIL "${node} pciRootId is the outermost bridge" "${bad} of ${checked} disagree with last element of pciSwitches"
  fi

  rdma_count=$(printf '%s\n' "$before" | awk -F'\t' '$7 == "true" {c++} END {print c+0}')
  vf_rdma=$(printf '%s\n' "$vf_before" | awk -F'\t' '$2 == "true" {c++} END {print c+0}')
  stated=$(printf '%s\n' "$before" | awk -F'\t' '$7 != "true" && $9 != "" {c++} END {print c+0}')
  vf_stated=$(printf '%s\n' "$vf_before" | awk -F'\t' '$2 != "true" && $3 != "" {c++} END {print c+0}')
  labels=$(rdma_labels "$node"); read_failed $? "the rdma.* labels of $node"

  if [ "$((rdma_count + vf_rdma + stated + vf_stated))" -eq 0 ]; then
    if [ -z "$labels" ]; then
      record PASS "${node} carries no rdma label without RDMA hardware" "no rdma.* key"
    else
      record FAIL "${node} carries no rdma label without RDMA hardware" "labels present: $(printf '%s' "$labels" | tr '\n' ' ')"
    fi
    continue
  fi

  bound=$((rdma_count + vf_rdma))
  unstated=$(printf '%s\n' "$before" | awk -F'\t' '$7 == "true" && $9 != "OK" && $9 != "Unverified" && $9 != "Failed" {c++} END {print c+0}')
  vf_unstated=$(printf '%s\n' "$vf_before" | awk -F'\t' '$2 == "true" && $3 != "OK" && $3 != "Unverified" && $3 != "Failed" {c++} END {print c+0}')
  if [ "$bound" -eq 0 ]; then
    record SKIP "${node} every RDMA interface carries a link state" "no bound RDMA device; ${stated} iface(s) and ${vf_stated} VF(s) carry verdict without one"
  elif [ "$((unstated + vf_unstated))" -eq 0 ]; then
    record PASS "${node} every RDMA interface carries a link state" "${rdma_count} interface(s) and ${vf_rdma} VF(s)"
  else
    record FAIL "${node} every RDMA interface carries a link state" "$((unstated + vf_unstated)) of ${bound} report no state"
  fi

  failed_count=$(printf '%s\n' "$before" | awk -F'\t' '$9 == "Failed" {c++} END {print c+0}')
  vf_failed=$(printf '%s\n' "$vf_before" | awk -F'\t' '$3 == "Failed" {c++} END {print c+0}')
  if [ "$((failed_count + vf_failed))" -gt 0 ]; then
    incomplete=$(printf '%s\n' "$before" | awk -F'\t' '$9 == "Failed" && ($10 == "" || $11 == "") {c++} END {print c+0}')
    vf_incomplete=$(printf '%s\n' "$vf_before" | awk -F'\t' '$3 == "Failed" && ($4 == "" || $5 == "") {c++} END {print c+0}')
    if [ "$((incomplete + vf_incomplete))" -eq 0 ]; then
      record PASS "${node} failed link carries reason and first-seen time" "${failed_count} iface(s) and ${vf_failed} VF(s)"
    else
      record FAIL "${node} failed link carries reason and first-seen time" "$((incomplete + vf_incomplete)) missing one of the two"
    fi
  else
    record SKIP "${node} failed link carries reason and first-seen time" "no link reports failed"
  fi

  capable_count=$(printf '%s\n' "$before" | awk -F'\t' '($9 != "" && $9 != "Failed") || ($9 == "" && $7 == "true") {c++} END {print c+0}')
  vf_capable=$(printf '%s\n' "$vf_before" | awk -F'\t' '($3 != "" && $3 != "Failed") || ($3 == "" && $2 == "true") {c++} END {print c+0}')
  usable=$((capable_count + vf_capable))
  all_failed=$((failed_count + vf_failed))
  has_capable=$(printf '%s\n' "$labels" | grep -c 'rdma\.capable$')
  if [ "$usable" -eq 0 ]; then
    if [ "$has_capable" -eq 0 ]; then
      record PASS "${node} every link failed withholds rdma.capable" "${all_failed} failed, 0 usable"
    else
      record FAIL "${node} every link failed withholds rdma.capable" "rdma.capable present while all ${all_failed} failed"
    fi
  else
    if [ "$has_capable" -eq 1 ]; then
      record PASS "${node} usable link emits rdma.capable" "${usable} usable endpoint(s)"
    else
      record FAIL "${node} usable link emits rdma.capable" "rdma.capable absent while ${usable} usable"
    fi
    record SKIP "${node} every link failed withholds rdma.capable" "NOT REACHED: ${usable} endpoint(s) usable"
  fi
done

# Stability hold across quiet detect passes.
declare -a GEN_BEFORE=() IFACE_BEFORE=() INV_BEFORE=()
idx=0
for node in $NODES; do
  GEN_BEFORE[$idx]=$(generation "$node"); read_failed $? "baseline generation of $node"
  IFACE_BEFORE[$idx]=$(interfaces "$node"); read_failed $? "baseline interfaces of $node"
  INV_BEFORE[$idx]=$(inventory "$node"); read_failed $? "baseline inventory of $node"
  idx=$((idx + 1))
done
echo "[case-14] holding ${QUIET_SECONDS}s to verify unchanged pass writes nothing"
sleep "$QUIET_SECONDS"
idx=0
for node in $NODES; do
  gen_now=$(generation "$node"); read_failed $? "post-hold generation of $node"
  if [ "$gen_now" = "${GEN_BEFORE[$idx]}" ]; then
    record PASS "${node} unchanged pass writes nothing" "generation stayed ${gen_now}"
  else
    record FAIL "${node} unchanged pass writes nothing" "generation moved ${GEN_BEFORE[$idx]} -> ${gen_now}"
  fi
  if [ "$(inventory "$node")" = "${INV_BEFORE[$idx]}" ]; then
    record PASS "${node} inventory byte-identical across passes" "$(printf '%s\n' "${IFACE_BEFORE[$idx]}" | grep -c .) interface(s)"
  else
    record FAIL "${node} inventory byte-identical across passes" "record differs between reads"
  fi
  idx=$((idx + 1))
done

# Restart one device-manager Pod and verify hardware-bearing rows are not corrupted.
declare -a SIG_BEFORE=()
idx=0
for node in $NODES; do
  SIG_BEFORE[$idx]=$(significant_interfaces "$node"); read_failed $? "pre-restart significant interfaces of $node"
  idx=$((idx + 1))
done
DM_PODS=$(kubectl -n "$NS" get pods \
  -l app.kubernetes.io/part-of=gpustack-operator,app.kubernetes.io/component=device-manager \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\t"}{.metadata.uid}{"\t"}{.status.phase}{"\n"}{end}' 2>/dev/null \
  | awk -F'\t' -v nodes="$NODES" \
      'BEGIN { n = split(nodes, a, "\n"); for (i = 1; i <= n; i++) profiled[a[i]] = 1 }
       $2 != "" && $4 == "Running" && ($2 in profiled)')
if [ -z "$DM_PODS" ]; then
  record SKIP "inventory survives detector restart" "no Running device manager pod in ${NS} on a profiled node"
else
  IFS=$'\t' read -r victim victim_node victim_uid _ <<<"$(printf '%s\n' "$DM_PODS" | head -1)"
  echo "[case-14] deleting ${victim} on ${victim_node} so DaemonSet recreates it"
  kubectl -n "$NS" delete pod "$victim" --wait=false >/dev/null 2>&1
  ready=0; deadline=$((SECONDS + RESTART_SECONDS))
  while :; do
    sleep 5
    ready=$(kubectl -n "$NS" get pods \
      -l app.kubernetes.io/part-of=gpustack-operator,app.kubernetes.io/component=device-manager \
      --field-selector "spec.nodeName=${victim_node}" \
      -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{.status.phase}{"\n"}{end}' 2>/dev/null \
      | awk -v uid="$victim_uid" '$1 != uid && $2 == "Running" {c++} END {print c+0}')
    [ "${ready:-0}" -ge 1 ] && break
    [ "$SECONDS" -lt "$deadline" ] || break
  done
  if [ "${ready:-0}" -lt 1 ]; then
    record FAIL "inventory survives detector restart" "no replacement for ${victim} came back Running within ${RESTART_SECONDS}s"
  else
    sleep "$SETTLE_SECONDS"
    idx=0
    for node in $NODES; do
      if [ "$node" != "$victim_node" ]; then
        record SKIP "${node} inventory survives detector restart" "NOT RESTARTED: only ${victim_node}'s device manager was deleted"
        idx=$((idx + 1))
        continue
      fi
      sig_now=$(significant_interfaces "$node"); read_failed $? "post-restart significant interfaces of $node"
      gen_now=$(generation "$node"); read_failed $? "post-restart generation of $node"
      if [ "$sig_now" != "${SIG_BEFORE[$idx]}" ]; then
        record FAIL "${node} inventory survives detector restart" "hardware-bearing rows changed across restart"
      else
        record SKIP "${node} inventory survives detector restart" "NOT PROVEN: hardware-bearing rows identical (generation ${GEN_BEFORE[$idx]} -> ${gen_now})"
      fi
      idx=$((idx + 1))
    done
  fi
fi

# ===================================================================================================
# Helper to select a node for RDMA legs without exiting early (so earlier PASS rows are kept).
# ===================================================================================================
select_rdma_node_soft() {
  local reqs=("$@") candidates node req unmet
  RDMA_NODE=""
  candidates="${RDMA_NODE_NAME:-$NODES}"
  for node in $candidates; do
    rdma_facts "$node"
    unmet=""
    for req in "${reqs[@]}"; do
      rdma_requirement_met "$req" || unmet="${unmet}${req} "
    done
    if [ -z "$unmet" ]; then
      RDMA_NODE="$node"
      return 0
    fi
  done
  return 1
}

# ===================================================================================================
# Leg 3 — RDMA extended resource sizing (requires a node with >=1 RDMA endpoint).
# ===================================================================================================
echo
echo "[case-14] === leg 3: RDMA extended resource sizing ==="

if ! select_rdma_node_soft devices endpoint; then
  record SKIP "leg 3: RDMA extended resource sizing" "no node in cluster carries an RDMA endpoint"
else
  echo "[case-14] leg 3 node: ${RDMA_NODE} (${F_EP_WHOLE} whole, ${F_EP_VF} VF)"
  rdma_probe_try || true

  HOST_DEVICES=$(rdma_host_rdma_devices)
  LEDGER_DEVICES=$(printf '%s' "$F_RDMA_NAMES" | tr ',' '\n' | grep -v '^$' | LC_ALL=C sort -u)

  if [ "$RDMA_PROBE_READY" -ne 1 ]; then
    record SKIP "leg 3: published inventory names host RDMA devices" "host probe Pod unavailable: ${RDMA_PROBE_SKIP_REASON}"
  elif [ -z "$HOST_DEVICES" ]; then
    record FAIL "leg 3: published inventory names host RDMA devices" "host lists no RDMA device while ledger publishes ${F_EP_TOTAL} endpoint(s)"
  else
    MISSING=""
    for dev in $HOST_DEVICES; do
      printf '%s\n' "$LEDGER_DEVICES" | grep -qxF "$dev" || MISSING="${MISSING}${dev} "
    done
    if [ -z "$MISSING" ]; then
      record PASS "leg 3: published inventory names host RDMA devices" "$(printf '%s' "$HOST_DEVICES" | grep -c .) host device(s) all present"
    else
      record FAIL "leg 3: published inventory names host RDMA devices" "host lists ${MISSING}missing from ledger"
    fi
  fi

  WANT_WHOLE="$F_EP_WHOLE_OK"
  WANT_SHARED=$((F_EP_WHOLE_OK * RDMA_SHARED_POOL_SIZE))
  WANT_PART="$F_EP_VF_OK"

  if [ "$F_EP_WHOLE" -eq 0 ]; then
    record SKIP "leg 3: whole-function key counts usable endpoints" "no whole-function endpoint on ${RDMA_NODE}"
    record SKIP "leg 3: shared key counts pool size per usable endpoint" "no whole-function endpoint on ${RDMA_NODE}"
  else
    if [ "$RDMA_ALLOC_WHOLE" = "$WANT_WHOLE" ]; then
      record PASS "leg 3: whole-function key counts usable endpoints" "${RDMA_KEY_WHOLE}=${RDMA_ALLOC_WHOLE}"
    else
      record FAIL "leg 3: whole-function key counts usable endpoints" "${RDMA_KEY_WHOLE}=${RDMA_ALLOC_WHOLE:-<absent>}, want ${WANT_WHOLE}"
    fi
    if [ "$RDMA_ALLOC_SHARED" = "$WANT_SHARED" ]; then
      record PASS "leg 3: shared key counts pool size per usable endpoint" "${RDMA_KEY_SHARED}=${RDMA_ALLOC_SHARED} = ${F_EP_WHOLE_OK} x ${RDMA_SHARED_POOL_SIZE}"
    else
      record FAIL "leg 3: shared key counts pool size per usable endpoint" "${RDMA_KEY_SHARED}=${RDMA_ALLOC_SHARED:-<absent>}, want ${WANT_SHARED}"
    fi
  fi

  if [ "$F_EP_VF" -eq 0 ]; then
    record SKIP "leg 3: partitioned key counts usable virtual functions" "no SR-IOV VF configured on ${RDMA_NODE}"
  elif [ "$RDMA_ALLOC_PART" = "$WANT_PART" ]; then
    record PASS "leg 3: partitioned key counts usable virtual functions" "${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART}"
  else
    record FAIL "leg 3: partitioned key counts usable virtual functions" "${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART:-<absent>}, want ${WANT_PART}"
  fi

  SLICED_ALLOC=$(rdma_node_quantity "$RDMA_NODE" allocatable "$RDMA_KEY_SLICED_RETIRED")
  if [ -z "$SLICED_ALLOC" ] || [ "$SLICED_ALLOC" = "0" ]; then
    record PASS "leg 3: retired sliced key is not served" "${RDMA_KEY_SLICED_RETIRED}=${SLICED_ALLOC:-<absent>}"
  else
    record FAIL "leg 3: retired sliced key is not served" "${RDMA_KEY_SLICED_RETIRED}=${SLICED_ALLOC}"
  fi

  if [ "$F_EP_FAILED" -eq 0 ]; then
    record SKIP "leg 3: failed link keeps tokens in capacity and loses them from allocatable" "no endpoint reports failed link"
  else
    CAP_WHOLE=$(rdma_node_quantity "$RDMA_NODE" capacity "$RDMA_KEY_WHOLE")
    CAP_PART=$(rdma_node_quantity "$RDMA_NODE" capacity "$RDMA_KEY_PARTITIONED")
    CAP_TOTAL=$(( ${CAP_WHOLE:-0} + ${CAP_PART:-0} ))
    ALLOC_TOTAL=$(( ${RDMA_ALLOC_WHOLE:-0} + ${RDMA_ALLOC_PART:-0} ))
    WANT_CAP=$((F_EP_WHOLE + F_EP_VF))
    WANT_ALLOC=$((F_EP_WHOLE_OK + F_EP_VF_OK))
    if [ "$CAP_TOTAL" -eq "$WANT_CAP" ] && [ "$ALLOC_TOTAL" -eq "$WANT_ALLOC" ] && [ "$CAP_TOTAL" -gt "$ALLOC_TOTAL" ]; then
      record PASS "leg 3: failed link keeps tokens in capacity and loses them from allocatable" "capacity=${CAP_TOTAL} allocatable=${ALLOC_TOTAL}"
    else
      record FAIL "leg 3: failed link keeps tokens in capacity and loses them from allocatable" "capacity=${CAP_TOTAL}(want ${WANT_CAP}) allocatable=${ALLOC_TOTAL}(want ${WANT_ALLOC})"
    fi
  fi

  HOST_VF_TOTAL=0; HOST_VF_DEVICES=""
  for dev in $HOST_DEVICES; do
    n=$(rdma_host_sriov_state "$dev")
    case "$n" in absent|0|''|unreadable|*[!0-9]*) continue ;; esac
    HOST_VF_TOTAL=$((HOST_VF_TOTAL + n))
    HOST_VF_DEVICES="${HOST_VF_DEVICES}${dev}=${n} "
  done
  if [ "$RDMA_PROBE_READY" -ne 1 ]; then
    record SKIP "leg 3: SR-IOV VF count and PF exclusion" "host probe unavailable"
  elif [ "$HOST_VF_TOTAL" -eq 0 ]; then
    record SKIP "leg 3: SR-IOV VF count and PF exclusion" "no host virtual functions configured"
  else
    PF_COUNT=$(printf '%s' "$HOST_VF_DEVICES" | tr ' ' '\n' | grep -c '=')
    if [ "${RDMA_ALLOC_PART:-0}" -eq "$F_EP_VF_OK" ] && [ "$F_EP_VF" -gt 0 ] && [ "$HOST_VF_TOTAL" -eq "$F_VF_TOTAL" ]; then
      record PASS "leg 3: partitioned key counts host VFs" "host=${HOST_VF_TOTAL} ledger=${F_VF_TOTAL} alloc=${RDMA_ALLOC_PART}"
    else
      record FAIL "leg 3: partitioned key counts host VFs" "host=${HOST_VF_TOTAL} ledger=${F_VF_TOTAL} alloc=${RDMA_ALLOC_PART:-<absent>}"
    fi
    if [ "$F_SRIOV_IFACES" -eq "$PF_COUNT" ]; then
      record PASS "leg 3: PF with VFs counts zero times in whole-function keys" "${PF_COUNT} PF(s) in partitioned branch"
    else
      record FAIL "leg 3: PF with VFs counts zero times in whole-function keys" "host=${PF_COUNT} ledger=${F_SRIOV_IFACES}"
    fi
  fi
fi

# ===================================================================================================
# Leg 4 — Container device-cgroup open()/verbs isolation (requires a whole-function RDMA endpoint).
# ===================================================================================================
echo
echo "[case-14] === leg 4: container device-cgroup open() and verbs isolation ==="

if ! select_rdma_node_soft devices endpoint endpoint-whole; then
  record SKIP "leg 4: RDMA container device-cgroup isolation" "no node in cluster carries a whole-function RDMA endpoint"
else
  echo "[case-14] leg 4 node: ${RDMA_NODE}"
  [ "$RDMA_PROBE_READY" -eq 1 ] || rdma_probe_try || true

  try_open() {
    local out
    # shellcheck disable=SC2016 # Expressions execute inside the container shell.
    out=$(kubectl -n "$RDMA_TEST_NS" exec "$1" -- sh -c '
p="$1"; d=$(dirname "$p")
if [ ! -d "$d" ]; then echo "RDMA_NODIR"
elif [ ! -e "$p" ]; then echo "RDMA_NOFILE"
elif ( exec 3<>"$p" ) 2>/dev/null; then echo "RDMA_OPEN"
else echo "RDMA_REFUSED"; ( exec 3<>"$p" ) 2>&1 | head -1
fi' sh "$2" 2>&1)
    case "$out" in
    *RDMA_OPEN*) echo "OPEN" ;;
    *RDMA_REFUSED*) echo "REFUSED:$(printf '%s' "$out" | tr '\n' ' ')" ;;
    *RDMA_NOFILE*) echo "NOFILE" ;;
    *RDMA_NODIR*) echo "NODIR" ;;
    *) echo "OTHER:$(printf '%s' "$out" | tr '\n' ' ')" ;;
    esac
  }

  create_rdma_pod() {
    local name="$1" res="$2" image="${3:-$WORK_IMAGE}" resblock=""
    [ -n "$res" ] && resblock="
      resources:
        limits:   { ${res} }
        requests: { ${res} }"
    TESTPODS+=("$name")
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${name}, namespace: ${RDMA_TEST_NS} }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: main
      image: ${image}
      command: ["sleep", "86400"]${resblock}
EOF
  }

  HOST_DEVICES_L4=$(rdma_host_rdma_devices)
  VERBS_MAP=""
  for dev in $HOST_DEVICES_L4; do
    vnode=$(rdma_probe "ls /host/sys/class/infiniband/${dev}/device/infiniband_verbs 2>/dev/null" | head -1 | tr -d '[:space:]')
    [ -n "$vnode" ] && VERBS_MAP="${VERBS_MAP}${dev} ${vnode}
"
  done
  HOST_VERBS_COUNT=$(printf '%s' "$VERBS_MAP" | grep -c . || true)

  GRANTED_POD="e2e-c14-rdma-granted-${RANDOM}"
  UNGRANTED_POD="e2e-c14-rdma-ungranted-${RANDOM}"

  create_rdma_pod "$GRANTED_POD" "${RDMA_KEY_SHARED}: \"1\""
  PHASE=$(rdma_wait_pod "$GRANTED_POD" Running)
  if [ "$PHASE" != Running ]; then
    record FAIL "leg 4: Pod requesting one shared RDMA endpoint runs" "phase=${PHASE:-<timeout>}; $(rdma_pod_reason "$GRANTED_POD")"
  else
    record PASS "leg 4: Pod requesting one shared RDMA endpoint runs" "${GRANTED_POD} Running on ${RDMA_NODE}, unprivileged"

    GRANTED_NAMES=$(kubectl -n "$RDMA_TEST_NS" exec "$GRANTED_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
    if [ -z "$GRANTED_NAMES" ]; then
      record FAIL "leg 4: container receives NCCL_IB_HCA" "NCCL_IB_HCA is empty"
    else
      UNKNOWN=""
      for name in $(printf '%s' "$GRANTED_NAMES" | tr ',' ' '); do
        printf '%s\n' "$F_EP_NAMES" | tr ',' '\n' | grep -qxF "$name" || UNKNOWN="${UNKNOWN}${name} "
      done
      if [ -z "$UNKNOWN" ]; then
        record PASS "leg 4: container receives NCCL_IB_HCA" "NCCL_IB_HCA=${GRANTED_NAMES}"
      else
        record FAIL "leg 4: container receives NCCL_IB_HCA" "NCCL_IB_HCA=${GRANTED_NAMES} names unknown ${UNKNOWN}"
      fi
    fi

    DEV_LIST=$(kubectl -n "$RDMA_TEST_NS" exec "$GRANTED_POD" -- sh -c 'ls /dev/infiniband 2>/dev/null' | grep -v '^$' | LC_ALL=C sort)
    UVERBS_SEEN=$(printf '%s\n' "$DEV_LIST" | grep '^uverbs' || true)
    UVERBS_COUNT=$(printf '%s\n' "$UVERBS_SEEN" | grep -c . || true)
    GRANT_COUNT=$(printf '%s' "$GRANTED_NAMES" | tr ',' '\n' | grep -c . || true)

    if [ "$UVERBS_COUNT" -eq "$GRANT_COUNT" ] && [ "$GRANT_COUNT" -gt 0 ]; then
      if [ "$RDMA_PROBE_READY" -ne 1 ]; then
        record SKIP "leg 4: container gets granted endpoint node and no other" "host probe unavailable"
      elif [ "$HOST_VERBS_COUNT" -gt "$GRANT_COUNT" ]; then
        record PASS "leg 4: container gets granted endpoint node and no other" "${UVERBS_COUNT} of ${HOST_VERBS_COUNT} host verbs node(s)"
      else
        record SKIP "leg 4: container gets granted endpoint node and no other" "single-endpoint host (${HOST_VERBS_COUNT} == ${GRANT_COUNT})"
      fi
    else
      record FAIL "leg 4: container gets granted endpoint node and no other" "${UVERBS_COUNT} uverbs node(s) for ${GRANT_COUNT} grant(s)"
    fi

    OPEN_FAILS=""; OPEN_OK=""
    for vnode in $UVERBS_SEEN; do
      ov=$(try_open "$GRANTED_POD" "/dev/infiniband/${vnode}")
      if [ "$ov" = OPEN ]; then OPEN_OK="${OPEN_OK}${vnode} "; else OPEN_FAILS="${OPEN_FAILS}${vnode}=${ov} "; fi
    done
    if [ -z "$UVERBS_SEEN" ]; then
      record FAIL "leg 4: granted device opens without privilege" "no uverbs node injected"
    elif [ -z "$OPEN_FAILS" ]; then
      record PASS "leg 4: granted device opens without privilege" "open(O_RDWR) succeeded on ${OPEN_OK}"
    else
      record FAIL "leg 4: granted device opens without privilege" "refused: ${OPEN_FAILS}"
    fi

    if [ -z "${E2E_RDMA_IMAGE:-}" ]; then
      record SKIP "leg 4: verbs library reports granted device" "set E2E_RDMA_IMAGE to an image shipping ibv_devinfo"
    else
      VERBS_POD="e2e-c14-rdma-verbs-${RANDOM}"
      create_rdma_pod "$VERBS_POD" "${RDMA_KEY_SHARED}: \"1\"" "$E2E_RDMA_IMAGE"
      VPHASE=$(rdma_wait_pod "$VERBS_POD" Running)
      if [ "$VPHASE" != Running ]; then
        record FAIL "leg 4: verbs library reports granted device" "${VERBS_POD} phase=${VPHASE:-<timeout>}"
      elif ! rdma_has_tool "$VERBS_POD" ibv_devinfo; then
        record SKIP "leg 4: verbs library reports granted device" "${E2E_RDMA_IMAGE} ships no ibv_devinfo"
      else
        VNAMES=$(kubectl -n "$RDMA_TEST_NS" exec "$VERBS_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
        VOUT=$(kubectl -n "$RDMA_TEST_NS" exec "$VERBS_POD" -- sh -c 'ibv_devinfo 2>&1' 2>/dev/null)
        VMISSING=""
        for name in $(printf '%s' "$VNAMES" | tr ',' ' '); do
          printf '%s' "$VOUT" | grep -q "$name" || VMISSING="${VMISSING}${name} "
        done
        if [ -n "$VNAMES" ] && [ -z "$VMISSING" ]; then
          record PASS "leg 4: verbs library reports granted device" "ibv_devinfo names ${VNAMES}"
        else
          record FAIL "leg 4: verbs library reports granted device" "granted ${VNAMES:-<none>}, missing ${VMISSING:-all}"
        fi
      fi
    fi

    TARGET_NODE=$(printf '%s\n' "$UVERBS_SEEN" | head -1)
    [ -n "$TARGET_NODE" ] || TARGET_NODE=$(printf '%s' "$VERBS_MAP" | awk 'NF {print $2; exit}')
    if [ -z "$TARGET_NODE" ]; then
      record SKIP "leg 4: ungranted control Pod is refused same device" "no verbs node name resolved"
    else
      create_rdma_pod "$UNGRANTED_POD" ""
      UPHASE=$(rdma_wait_pod "$UNGRANTED_POD" Running)
      if [ "$UPHASE" != Running ]; then
        record FAIL "leg 4: ungranted control Pod is refused same device" "control Pod phase=${UPHASE:-<timeout>}"
      else
        UVERDICT=$(try_open "$UNGRANTED_POD" "/dev/infiniband/${TARGET_NODE}")
        case "$UVERDICT" in
        REFUSED*) record PASS "leg 4: ungranted control Pod is refused same device" "REFUSED AT DEVICE CGROUP: ${UVERDICT#REFUSED:}" ;;
        NOFILE)   record PASS "leg 4: ungranted control Pod is refused same device" "REFUSED AT MOUNT NAMESPACE (file level): ${TARGET_NODE} absent" ;;
        NODIR)    record PASS "leg 4: ungranted control Pod is refused same device" "REFUSED AT MOUNT NAMESPACE (dir level): /dev/infiniband absent" ;;
        OPEN)     record FAIL "leg 4: ungranted control Pod is refused same device" "open(/dev/infiniband/${TARGET_NODE}) SUCCEEDED without request" ;;
        *)        record FAIL "leg 4: ungranted control Pod is refused same device" "unexpected result: ${UVERDICT}" ;;
        esac
      fi
    fi
  fi
fi

rdma_results
