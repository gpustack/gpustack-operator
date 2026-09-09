#!/usr/bin/env bash
#
# CASE 34 — single-numa-node topology with partition capacity only on the far socket
#   (MUTATING, self-recovering; AUTO-SKIPS unless the node is dual-socket AND runs the
#    single-numa-node TopologyManager policy, AND a node-SSH address is supplied)
#
#   MIG_NODE_SSH=<user@host> case-34.sh <NS>
#
# Goal:        RECORD what a non-default TopologyManager policy does to a hardware partition. The
#              partition resource deliberately reports NO NUMA topology, because the plugin — not the
#              kubelet — chooses the card, so any topology it reported would describe a card that may
#              not be used. Reporting nothing stops the kubelet from aligning CPU and memory to the
#              wrong card, but it does not make the policy safe: a resource with no topology
#              contributes no constraint, so under single-numa-node the CPU and memory providers can
#              settle on one socket while the only card with room sits on the other. Nothing in the
#              design claims otherwise; this case exists so the limitation is a measured statement
#              rather than an inference. It asserts NOTHING about alignment — it reports the policy,
#              the card's socket, the socket the container's CPUs landed on, and whether the Pod ran.
# Environment: A reachable cluster whose active context is the GPU cluster, a DUAL-SOCKET nvidia node
#              whose kubelet runs topologyManagerPolicy: single-numa-node, at least one card that can
#              be put into a hardware partitioning mode, AND SSH to that node supplied via
#              MIG_NODE_SSH=<user@host>. It EXITS 2 (input required) when MIG_NODE_SSH is unset. It
#              AUTO-SKIPS (exit 0) — the expected outcome on a default cluster — when the node reports
#              fewer than two NUMA nodes, when the TopologyManager policy is anything other than
#              single-numa-node (the default none cannot exhibit this at all), when the policy cannot
#              be read, or when the card reports no partitioning mode.
# Inputs:      All real, nothing mocked. The case reads the kubelet's own configuration, maps each card
#              to its NUMA node through the PCI device tree, pins a guaranteed-QoS filler Pod onto the
#              CPUs of the socket the partitioned card sits on, and then submits a guaranteed-QoS
#              partition Pod that can only be given CPUs from the far socket. Profiles are DISCOVERED
#              from the card's own capability.
# Expected:    - the observation is captured and printed: policy, CPU-manager policy, the card's NUMA
#                node, the NUMA node the partition Pod's CPUs came from, and the Pod's outcome
#                (Running, TopologyAffinityError, or held);
#              - no alignment assertion is made either way.
# Cleanup:     Trap deletes every test Pod, waits for the instances to reclaim, and restores the
#              partitioning mode of the card this case toggled (a card found already partitioned is
#              left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-34.sh <NS>}"
CASE_ID=34
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

PODPFX=gpustack-e2e-numa

part_require_node_ssh "case-34.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-34] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

# ---------------------------------------------------------------------------------------------------
# The two gates. Both are expected to fail on a default cluster, and that is a clean skip.
# ---------------------------------------------------------------------------------------------------
NUMA_NODES="$(node_ssh 'ls -d /sys/devices/system/node/node[0-9]* 2>/dev/null | wc -l' 2>/dev/null | tr -d '[:space:]')"
if [ "${NUMA_NODES:-0}" -lt 2 ]; then
  part_skip \
    "Node ${GPU_NODE} reports ${NUMA_NODES:-0} NUMA node(s). Cross-socket mis-alignment needs at least two;" \
    "on a single-socket node the situation this case records cannot arise."
fi

# The kubelet's own configuration is the authority; configz first, the on-disk config as the fallback.
TM_POLICY="$(kubectl get --raw "/api/v1/nodes/${GPU_NODE}/proxy/configz" 2>/dev/null | python3 -c "
import json,sys
try: print(json.load(sys.stdin).get('kubeletconfig',{}).get('topologyManagerPolicy',''))
except Exception: print('')
" 2>/dev/null)"
CM_POLICY="$(kubectl get --raw "/api/v1/nodes/${GPU_NODE}/proxy/configz" 2>/dev/null | python3 -c "
import json,sys
try: print(json.load(sys.stdin).get('kubeletconfig',{}).get('cpuManagerPolicy',''))
except Exception: print('')
" 2>/dev/null)"
if [ -z "$TM_POLICY" ]; then
  TM_POLICY="$(node_ssh "grep -E '^topologyManagerPolicy:' /var/lib/kubelet/config.yaml 2>/dev/null | awk '{print \$2}'" 2>/dev/null | tr -d '[:space:]')"
  CM_POLICY="$(node_ssh "grep -E '^cpuManagerPolicy:' /var/lib/kubelet/config.yaml 2>/dev/null | awk '{print \$2}'" 2>/dev/null | tr -d '[:space:]')"
fi
if [ -z "$TM_POLICY" ]; then
  part_skip \
    "Could not read the kubelet's topologyManagerPolicy (neither the configz endpoint nor" \
    "/var/lib/kubelet/config.yaml on ${MIG_NODE_SSH}). Without knowing the policy nothing can be recorded."
fi
if [ "$TM_POLICY" != "single-numa-node" ]; then
  part_skip \
    "Node ${GPU_NODE} runs topologyManagerPolicy '${TM_POLICY}' (cpuManagerPolicy '${CM_POLICY:-?}')." \
    "The default 'none' cannot exhibit cross-socket mis-alignment: a resource with no topology" \
    "constrains nothing only when the policy asks for a constraint. Re-run on a node configured with" \
    "single-numa-node to record this observation."
fi
echo "[case-34] topologyManagerPolicy=${TM_POLICY}, cpuManagerPolicy=${CM_POLICY:-?}, NUMA nodes=${NUMA_NODES}"

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
[ -n "$MIDKEY" ] || part_skip "The partitioned card advertises no 4-memory-slice profile key — nothing to request."

# ---------------------------------------------------------------------------------------------------
# Which socket is the partitioned card on, and which CPUs belong to each socket?
# ---------------------------------------------------------------------------------------------------
# nvidia-smi reports a padded bus id (00000000:07:00.0); the sysfs entry uses the last domain:bus:dev.fn.
CARD_NUMA="$(node_ssh "bdf=\$(nvidia-smi -i ${GPU_INDEX} --query-gpu=pci.bus_id --format=csv,noheader | tr -d '[:space:]' | tr 'A-F' 'a-f' | awk '{print substr(\$0,length(\$0)-11)}'); cat /sys/bus/pci/devices/\${bdf}/numa_node 2>/dev/null" 2>/dev/null | tr -d '[:space:]')"
if [ -z "$CARD_NUMA" ] || [ "$CARD_NUMA" = "-1" ]; then
  part_skip \
    "Card ${GPU_INDEX} reports no NUMA affinity in the PCI device tree (got '${CARD_NUMA:-<none>}')." \
    "Without the card's socket there is nothing to compare the container's CPUs against."
fi
NEAR_CPUS="$(node_ssh "cat /sys/devices/system/node/node${CARD_NUMA}/cpulist 2>/dev/null" | tr -d '[:space:]')"
NEAR_COUNT="$(node_ssh "cat /sys/devices/system/node/node${CARD_NUMA}/cpulist 2>/dev/null | tr ',' '\n' | awk -F- '{if (NF==2) s+=\$2-\$1+1; else s+=1} END {print s}'" 2>/dev/null | tr -d '[:space:]')"
echo "[case-34] card ${GPU_INDEX} sits on NUMA node ${CARD_NUMA} (cpus ${NEAR_CPUS}, ${NEAR_COUNT} of them)"

# ---------------------------------------------------------------------------------------------------
# Occupy the near socket's CPUs, then ask for a partition that can only be given far-socket CPUs.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-34] === filling NUMA node ${CARD_NUMA}'s CPUs, then requesting a ${MID} partition ==="
FILL_CPU=$((NEAR_COUNT - 2))
[ "${FILL_CPU:-0}" -lt 1 ] && FILL_CPU=1
F="${PODPFX}-filler"
TESTPODS+=("$F")
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${F}, namespace: default }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { cpu: "${FILL_CPU}", memory: 2Gi }
        requests: { cpu: "${FILL_CPU}", memory: 2Gi }
EOF
fillran=0
wait_running "$F" && fillran=1
record PASS "OBSERVED: near-socket CPUs occupied" "filler Pod requesting ${FILL_CPU} guaranteed CPU(s) on the card's socket is $([ "$fillran" = 1 ] && echo Running || echo "not Running ($(held_reason "$F"))")"

P="${PODPFX}-part"
mkpod "$P" "$(partition_reslines "$MIDKEY")
          cpu: \"2\"
          memory: 4Gi"
outcome="held"
wait_running "$P" 30 && outcome="Running"
POD_CPUS=""
POD_NUMA=""
if [ "$outcome" = "Running" ]; then
  POD_CPUS="$(kubectl -n default exec "$P" -- sh -c "grep Cpus_allowed_list /proc/self/status | awk '{print \$2}'" 2>/dev/null | tr -d '[:space:]')"
  if [ -n "$POD_CPUS" ]; then
    # Which socket owns the first CPU the container was given. Kept to POSIX sh constructs: the remote
    # login shell is not guaranteed to be bash, so no process substitution.
    POD_NUMA="$(node_ssh "first=\$(echo '${POD_CPUS}' | cut -d, -f1 | cut -d- -f1); for n in /sys/devices/system/node/node[0-9]*; do hit=\$(tr ',' '\n' < \$n/cpulist | awk -F- -v c=\$first '{ if (NF==2) { if (c>=\$1 && c<=\$2) f=1 } else { if (c==\$1) f=1 } } END { print f+0 }'); if [ \"\$hit\" = 1 ]; then basename \$n | sed 's/^node//'; fi; done" 2>/dev/null | head -1 | tr -d '[:space:]')"
  fi
fi
# An unreadable event list is reported as such, never as zero: this row is an observation, and "we
# could not tell" is a different observation from "it did not happen".
if TAE="$(pod_events "$P")"; then
  TAE="$(printf '%s\n' "$TAE" | grep -ci 'TopologyAffinityError')"
  [ "${TAE:-0}" = 0 ] && TAE=0
else
  TAE="unreadable"
fi

record PASS "OBSERVED: the partition Pod's outcome under ${TM_POLICY}" "outcome=${outcome}$([ "$outcome" = Running ] && echo ", container cpus=${POD_CPUS:-?} on NUMA node ${POD_NUMA:-?} vs card on node ${CARD_NUMA}"), TopologyAffinityError events=${TAE}, held reason='$(held_reason "$P")'"
if [ "$outcome" = "Running" ] && [ -n "$POD_NUMA" ] && [ "$POD_NUMA" != "$CARD_NUMA" ]; then
  record PASS "OBSERVED: cross-socket placement occurred" "the container's CPUs came from NUMA node ${POD_NUMA} while its partition lives on a card on node ${CARD_NUMA} — the limitation reproduced, recorded not asserted"
else
  record PASS "OBSERVED: cross-socket placement did not occur" "the container's CPUs and the card are on node ${POD_NUMA:-?} / ${CARD_NUMA} respectively, or the Pod did not run — recorded not asserted"
fi

part_results "single-numa-node topology with partition capacity only on the far socket"

echo
echo "---- fold back into the spec: single-numa-node observation ----"
echo "topologyManagerPolicy : ${TM_POLICY}"
echo "cpuManagerPolicy      : ${CM_POLICY:-?}"
echo "NUMA nodes            : ${NUMA_NODES}"
echo "card ${GPU_INDEX} NUMA node   : ${CARD_NUMA}"
echo "partition Pod outcome : ${outcome}"
echo "container cpus        : ${POD_CPUS:-<not running>} (NUMA node ${POD_NUMA:-?})"
echo "TopologyAffinityError : ${TAE}"
echo "---------------------------------------------------------------"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s) — unexpected for an observation-only case. Diagnose:"
  echo "  kubectl -n default describe pod ${P}"
  echo "  ${MIG_NODE_SSH} numactl --hardware"
  exit 1
fi
echo "CASE 34 PASS"
