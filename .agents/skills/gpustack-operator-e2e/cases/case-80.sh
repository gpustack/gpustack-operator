#!/usr/bin/env bash
#
# CASE 80 — The node advertises exactly the RDMA endpoints its inventory says it has, sized per mode
#   (MUTATING, self-recovering; AUTO-SKIPS without a node carrying an RDMA endpoint)
#
#   case-80.sh <NS>
#
# <NS> is the operator's own namespace. This case reads cluster-scoped objects and the node, and its
# one mutation is a read-only host probe Pod it deletes again.
#
# Goal:        The four things that make the RDMA extended resources a contract rather than a number
#              on a node:
#                - every RDMA device the HOST's own subsystem lists is in the published inventory.
#                  The host side is read from the kernel's device model through a probe Pod, never
#                  from the inventory itself: a check that reads both sides from our own record
#                  compares a value with itself and passes on a node whose inventory names nothing
#                  real. A non-empty interface list is NOT the reading;
#                - the per-mode counts the node advertises are the endpoint counts times the mode's
#                  token size, with endpoints whose link verdict is `failed` excluded. Presence of
#                  the keys is not the reading either: a key at zero on a node that has endpoints is
#                  a failure, not a pass;
#                - a `failed` endpoint's tokens are ADVERTISED AND UNHEALTHY — still in capacity,
#                  out of allocatable. Advertised-and-unhealthy and never-advertised are
#                  indistinguishable in allocatable alone, so both sides are read;
#                - where the host has SR-IOV virtual functions configured, the partitioned key
#                  counts them and the whole-function keys count that physical function ZERO times.
# Environment: A node running a device manager and carrying at least one RDMA endpoint. AUTO-SKIPS
#              (exit 0, printing NOTHING WAS VERIFIED) otherwise, naming what every candidate node
#              lacked. Individual checks skip with their own reason: the whole-function counts on a
#              node whose every interface is a partitioned physical function, the virtual-function
#              count on a node with none configured, and the unhealthy-token check on a node where
#              no link reports `failed` — this case never induces one, because driving a link down
#              is a host mutation with no reliable restore.
#              The probe Pod mounts the host's /sys read-only, which a `restricted` PodSecurity
#              namespace refuses. There the case does NOT skip whole: the count checks need no host
#              reading, so they still run, and the two checks that do need one are recorded as skips
#              naming the missing instrument. What is never done is falling back to the ledger for
#              the host side — that would compare a value with itself.
# Inputs:      All real, nothing mocked. One unprivileged Pod that mounts the host's /sys read-only
#              and runs no RDMA userspace at all.
# Expected:    - every device the host's RDMA subsystem lists is some interface's or virtual
#                function's rdmaDevice;
#              - allocatable of the whole-function key equals the number of whole-function endpoints
#                whose link verdict is not `failed`;
#              - allocatable of the shared key equals that number times the shared pool size;
#              - allocatable of the partitioned key equals the number of virtual-function endpoints
#                whose link verdict is not `failed`, and with virtual functions configured the host's
#                sriov_numvfs total equals the ledger's virtual functions under partitioned functions;
#              - the retired sliced key is either absent or zero — an extended resource that has
#                entered a node's status is not removed when the plugin stops serving it, so a node
#                that once ran the older build keeps the key forever, and only a NON-ZERO value is a
#                finding;
#              - with a `failed` endpoint present, capacity counts it and allocatable does not;
#              - with virtual functions configured, the physical function contributes nothing to the
#                whole-function keys.
# Cleanup:     Trap deletes the probe Pod. Nothing else was created and no baseline was changed, so
#              there is nothing else to restore; the trap runs on pass AND fail and is safe to re-run.
set -uo pipefail

NS="${1:?usage: case-80.sh <NS>}"
CASE_ID=80
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

trap rdma_cleanup EXIT

rdma_select devices endpoint

# The probe is tried rather than required. The count checks below read the ledger and the node
# object and need no host reading at all, so losing the probe must not lose them — it loses exactly
# the two checks that depend on an instrument outside the thing under test, and those say so.
rdma_probe_try || true

# ---------------------------------------------------------------------------------------------------
# 1. The correspondence. The host's own list against the published inventory.
# ---------------------------------------------------------------------------------------------------
HOST_DEVICES=$(rdma_host_rdma_devices)
# Every rdmaDevice the inventory carries, including a partitioned physical function's own, which is
# not an endpoint but is still a device the host lists.
LEDGER_DEVICES=$(printf '%s' "$F_RDMA_NAMES" | tr ',' '\n' | grep -v '^$' | LC_ALL=C sort -u)

if [ "$RDMA_PROBE_READY" -ne 1 ]; then
  record SKIP "the published inventory names the host's RDMA devices" \
    "NO INDEPENDENT INSTRUMENT: the host probe Pod could not run, so the host's own device list is unavailable. ${RDMA_PROBE_SKIP_REASON} Reading both sides of this correspondence from the ledger would compare a value with itself and pass on a node whose inventory names nothing real"
elif [ -z "$HOST_DEVICES" ]; then
  # The probe ran and the kernel's RDMA class directory is empty or unreadable, while the ledger
  # says this node has endpoints. Those two cannot both be right, and the disagreement is the
  # correspondence failing rather than a reason to skip.
  record FAIL "the published inventory names the host's RDMA devices" \
    "the host lists no RDMA device under /sys/class/infiniband while the ledger publishes ${F_EP_TOTAL} endpoint(s)"
else
  MISSING=""
  for dev in $HOST_DEVICES; do
    printf '%s\n' "$LEDGER_DEVICES" | grep -qxF "$dev" || MISSING="${MISSING}${dev} "
  done
  # The reverse direction is reported, not asserted: a virtual function whose parent was reconfigured
  # between the detect pass and this reading legitimately lingers, and the criterion is one-way.
  EXTRA=""
  for dev in $LEDGER_DEVICES; do
    printf '%s\n' "$HOST_DEVICES" | grep -qxF "$dev" || EXTRA="${EXTRA}${dev} "
  done
  if [ -z "$MISSING" ]; then
    record PASS "the published inventory names the host's RDMA devices" \
      "$(printf '%s' "$HOST_DEVICES" | grep -c .) host device(s) all present$([ -n "$EXTRA" ] && echo "; ledger also names ${EXTRA}the host did not list")"
  else
    record FAIL "the published inventory names the host's RDMA devices" \
      "the host lists ${MISSING}which no interface or virtual function carries as rdmaDevice"
  fi
fi

# ---------------------------------------------------------------------------------------------------
# 2. The per-mode counts. Each branch asserted only where the node exercises it.
#
#    `absent` and `0` are different answers throughout and are never folded: a key at zero is a
#    plugin serving nothing, a key that is not there is a plugin that never served it. The counts
#    below read allocatable, which is healthy tokens only — so the endpoint population they are
#    compared against is the usable one, and a node carrying one `failed` verdict makes the
#    unconditioned formula wrong.
# ---------------------------------------------------------------------------------------------------
WANT_WHOLE="$F_EP_WHOLE_OK"
WANT_SHARED=$((F_EP_WHOLE_OK * RDMA_SHARED_POOL_SIZE))
WANT_PART="$F_EP_VF_OK"

if [ "$F_EP_WHOLE" -eq 0 ]; then
  record SKIP "the whole-function key counts one token per usable endpoint" \
    "NOT EXERCISED: every interface on ${RDMA_NODE} is an SR-IOV physical function with virtual functions, so it serves the partitioned family only. The key reading zero here equals what a node with no RDMA at all reports"
  record SKIP "the shared key counts the pool size per usable endpoint" \
    "NOT EXERCISED: same reason — no whole-function endpoint on this node"
else
  if [ "$RDMA_ALLOC_WHOLE" = "$WANT_WHOLE" ]; then
    record PASS "the whole-function key counts one token per usable endpoint" \
      "${RDMA_KEY_WHOLE}=${RDMA_ALLOC_WHOLE} over ${F_EP_WHOLE_OK} usable of ${F_EP_WHOLE} whole-function endpoint(s)"
  else
    record FAIL "the whole-function key counts one token per usable endpoint" \
      "${RDMA_KEY_WHOLE}=${RDMA_ALLOC_WHOLE:-<absent>}, expected ${WANT_WHOLE} (one per usable whole-function endpoint)"
  fi
  if [ "$RDMA_ALLOC_SHARED" = "$WANT_SHARED" ]; then
    record PASS "the shared key counts the pool size per usable endpoint" \
      "${RDMA_KEY_SHARED}=${RDMA_ALLOC_SHARED} = ${F_EP_WHOLE_OK} x ${RDMA_SHARED_POOL_SIZE}"
  else
    record FAIL "the shared key counts the pool size per usable endpoint" \
      "${RDMA_KEY_SHARED}=${RDMA_ALLOC_SHARED:-<absent>}, expected ${WANT_SHARED} = ${F_EP_WHOLE_OK} x ${RDMA_SHARED_POOL_SIZE}; a different ceiling here is the running code identifying itself, not a configuration difference"
  fi
fi

if [ "$F_EP_VF" -eq 0 ]; then
  record SKIP "the partitioned key counts one token per usable virtual function" \
    "NOT EXERCISED: no SR-IOV virtual function is configured on ${RDMA_NODE}. The key reading zero or being absent here says nothing about how the count is derived — zero equals zero"
else
  if [ "$RDMA_ALLOC_PART" = "$WANT_PART" ]; then
    record PASS "the partitioned key counts one token per usable virtual function" \
      "${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART} over ${F_EP_VF_OK} usable of ${F_EP_VF} virtual-function endpoint(s)"
  else
    record FAIL "the partitioned key counts one token per usable virtual function" \
      "${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART:-<absent>}, expected ${WANT_PART}"
  fi
fi

# The retired key. Its absence is the expected state on a node that never served it, and its
# presence at zero the expected state on one that did — an extended resource is not withdrawn from a
# node's status when the plugin stops publishing it. Only a non-zero value says something is still
# serving a family that was retired because two keys over one pool set no ceiling at all.
SLICED_ALLOC=$(rdma_node_quantity "$RDMA_NODE" allocatable "$RDMA_KEY_SLICED_RETIRED")
if [ -z "$SLICED_ALLOC" ]; then
  record PASS "the retired sliced key is not served" \
    "${RDMA_KEY_SLICED_RETIRED} is absent from allocatable; this node never served it"
elif [ "$SLICED_ALLOC" = "0" ]; then
  record PASS "the retired sliced key is not served" \
    "${RDMA_KEY_SLICED_RETIRED}=0 — the residue of a node that once ran a build serving it, which a node's status keeps"
else
  record FAIL "the retired sliced key is not served" \
    "${RDMA_KEY_SLICED_RETIRED}=${SLICED_ALLOC}, so something is still advertising a family that was retired"
fi

# ---------------------------------------------------------------------------------------------------
# 3. A failed endpoint is advertised and unhealthy.
#
#    Both halves are needed. Allocatable alone cannot separate "advertised and unhealthy" from "never
#    advertised", which is a detector outcome and a different question; only capacity separates them.
# ---------------------------------------------------------------------------------------------------
if [ "$F_EP_FAILED" -eq 0 ]; then
  record SKIP "a failed link keeps its tokens in capacity and loses them from allocatable" \
    "NOT REACHED: no endpoint on ${RDMA_NODE} reports a failed link. The case does not induce one — driving a link down is a host mutation with no reliable restore, and an endpoint simply ABSENT from the inventory would be a detector outcome rather than this gate"
else
  CAP_WHOLE=$(rdma_node_quantity "$RDMA_NODE" capacity "$RDMA_KEY_WHOLE")
  CAP_PART=$(rdma_node_quantity "$RDMA_NODE" capacity "$RDMA_KEY_PARTITIONED")
  CAP_TOTAL=$(( ${CAP_WHOLE:-0} + ${CAP_PART:-0} ))
  ALLOC_TOTAL=$(( ${RDMA_ALLOC_WHOLE:-0} + ${RDMA_ALLOC_PART:-0} ))
  WANT_CAP=$((F_EP_WHOLE + F_EP_VF))
  WANT_ALLOC=$((F_EP_WHOLE_OK + F_EP_VF_OK))
  if [ "$CAP_TOTAL" -eq "$WANT_CAP" ] && [ "$ALLOC_TOTAL" -eq "$WANT_ALLOC" ] && [ "$CAP_TOTAL" -gt "$ALLOC_TOTAL" ]; then
    record PASS "a failed link keeps its tokens in capacity and loses them from allocatable" \
      "${F_EP_FAILED} failed endpoint(s): capacity ${CAP_TOTAL} counts them, allocatable ${ALLOC_TOTAL} does not"
  else
    record FAIL "a failed link keeps its tokens in capacity and loses them from allocatable" \
      "capacity ${CAP_TOTAL} (expected ${WANT_CAP}), allocatable ${ALLOC_TOTAL} (expected ${WANT_ALLOC}) with ${F_EP_FAILED} failed endpoint(s); equal totals mean the tokens were withdrawn rather than marked unhealthy"
  fi
fi

# ---------------------------------------------------------------------------------------------------
# 4. The SR-IOV branch, gated on the HOST's own virtual-function count rather than on the ledger.
#
#    The ledger is what this case is judging, so a gate taken from it would let a node the detector
#    misread be excused from the check that would have caught the misreading. sriov_numvfs is the
#    kernel's number.
# ---------------------------------------------------------------------------------------------------
HOST_VF_TOTAL=0
HOST_VF_DEVICES=""
HOST_VF_ABSENT=0
HOST_VF_ZERO=0
for dev in $HOST_DEVICES; do
  n=$(rdma_host_sriov_state "$dev")
  case "$n" in
  absent)
    HOST_VF_ABSENT=$((HOST_VF_ABSENT + 1))
    continue
    ;;
  0)
    HOST_VF_ZERO=$((HOST_VF_ZERO + 1))
    continue
    ;;
  '' | unreadable | *[!0-9]*) continue ;;
  esac
  HOST_VF_TOTAL=$((HOST_VF_TOTAL + n))
  HOST_VF_DEVICES="${HOST_VF_DEVICES}${dev}=${n} "
done

# Why the node cannot answer, in the terms whoever has to fix it needs. The capability being ABSENT
# and the capability being UNCONFIGURED both leave this reading untaken, and they call for opposite
# actions: configure this machine, or go find a different one.
if [ "$HOST_VF_ABSENT" -gt 0 ] && [ "$HOST_VF_ZERO" -eq 0 ]; then
  VF_WHY="the SR-IOV counting file does not exist for ${HOST_VF_ABSENT} device(s), so the capability is not present on this machine at all — an adapter passed through with SR-IOV consumed on the other side looks exactly like this, and no command run here makes it appear"
elif [ "$HOST_VF_ZERO" -gt 0 ] && [ "$HOST_VF_ABSENT" -eq 0 ]; then
  VF_WHY="the SR-IOV counting file reads 0 for ${HOST_VF_ZERO} device(s), so the capability is present and unconfigured — this machine CAN answer once virtual functions are configured on it"
else
  VF_WHY="${HOST_VF_ABSENT} device(s) expose no SR-IOV counting file at all and ${HOST_VF_ZERO} report it at zero"
fi

if [ "$RDMA_PROBE_READY" -ne 1 ]; then
  # Gating this on the ledger instead would excuse exactly the node whose detector misread the
  # virtual functions from the check that would have caught the misreading.
  record SKIP "the partitioned key counts the host's configured virtual functions" \
    "NO INDEPENDENT INSTRUMENT: sriov_numvfs could not be read from the host. ${RDMA_PROBE_SKIP_REASON}"
  record SKIP "a physical function with virtual functions counts zero times in the whole-function keys" \
    "NO INDEPENDENT INSTRUMENT: same reason"
elif [ "$HOST_VF_TOTAL" -eq 0 ]; then
  record SKIP "a physical function with virtual functions counts zero times in the whole-function keys" \
    "NOT EXERCISED: ${VF_WHY}. A host with no virtual functions exercises the other branch of the mode judgment and says nothing about this one"
else
  # Two readings, because the criterion has two halves and one of them passes vacuously on its own.
  # The partitioned count must match the host's virtual-function total, AND the physical functions
  # that carry those virtual functions must be absent from the whole-function population — which is
  # what makes the mode a read off the node rather than a menu.
  PF_COUNT=$(printf '%s' "$HOST_VF_DEVICES" | tr ' ' '\n' | grep -c '=')
  # The host's total is compared with the ledger's virtual functions (with or without an rdmaDevice):
  # a detector that under-reads them shrinks the ledger and allocatable together, which the
  # allocatable comparison alone would pass.
  if [ "${RDMA_ALLOC_PART:-0}" -eq "$F_EP_VF_OK" ] && [ "$F_EP_VF" -gt 0 ] && [ "$HOST_VF_TOTAL" -eq "$F_VF_TOTAL" ]; then
    record PASS "the partitioned key counts the host's configured virtual functions" \
      "host reports ${HOST_VF_DEVICES}(total ${HOST_VF_TOTAL}); ledger has ${F_VF_TOTAL} virtual function(s), ${F_EP_VF} of them endpoint(s), ${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART}"
  else
    record FAIL "the partitioned key counts the host's configured virtual functions" \
      "host reports ${HOST_VF_DEVICES}(total ${HOST_VF_TOTAL}) but the ledger has ${F_VF_TOTAL} virtual function(s), ${F_EP_VF} virtual-function endpoint(s) (${F_EP_VF_OK} usable), and ${RDMA_KEY_PARTITIONED}=${RDMA_ALLOC_PART:-<absent>}"
  fi
  if [ "$F_SRIOV_IFACES" -eq "$PF_COUNT" ]; then
    record PASS "a physical function with virtual functions counts zero times in the whole-function keys" \
      "${PF_COUNT} physical function(s) with virtual functions, all ${F_SRIOV_IFACES} in the partitioned branch; the whole-function keys count ${F_EP_WHOLE_OK} endpoint(s), none of them a physical function"
  else
    record FAIL "a physical function with virtual functions counts zero times in the whole-function keys" \
      "the host carries ${PF_COUNT} physical function(s) with virtual functions but the ledger puts ${F_SRIOV_IFACES} interface(s) in the partitioned branch, so at least one is being counted in both families"
  fi
fi

rdma_results
