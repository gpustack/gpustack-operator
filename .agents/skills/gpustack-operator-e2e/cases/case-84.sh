#!/usr/bin/env bash
#
# CASE 84 — On classic InfiniBand, the injected set carries a real transport, not just verbs
#   (MUTATING, self-recovering; AUTO-SKIPS without an endpoint whose link layer is InfiniBand)
#
#   case-84.sh <NS>
#
# <NS> is the operator's own namespace. The test Pods run in `default` (or RDMA_TEST_NS).
#
# Goal:        Whether what an allocation injects is everything an InfiniBand transport needs.
#              WHAT DOES NOT COUNT, and is the reason this case gates so hard:
#                - a pass on a RoCE or EFA adapter. Those run the verbs layer over Ethernet and open
#                  the same character device, so the whole question — what InfiniBand ADDITIONALLY
#                  needs — is not being asked. The link layer is read from the kernel's own port
#                  attribute in a separate probe Pod, and an Ethernet reading skips the case;
#                - `ibv_devinfo` succeeding. That exercises verbs and says nothing about the rest of
#                  the set, which is why this case runs a transport that establishes a connection
#                  and moves bytes instead.
#              Also recorded rather than asserted: the connection-manager path, which needs an
#              IPoIB address the container's network namespace does not have. That is a deployment
#              shape, not a product defect, and the case separates it from "the device was never
#              injected" so a failure there is not read as the latter.
# Environment: A node running a device manager with at least one WHOLE-FUNCTION RDMA endpoint whose
#              port link layer reads `InfiniBand`, plus E2E_RDMA_PERFTEST_IMAGE naming an image that
#              ships `ib_write_bw`. AUTO-SKIPS (exit 0, printing NOTHING WAS VERIFIED) otherwise,
#              and the RoCE skip names the fact that it is a host-shape answer rather than a verdict
#              — a RoCE host reporting a pass against this row is the one outcome that would retire
#              the question wrongly.
#              The probe Pod that reads the link layer mounts the host's /sys read-only, which a
#              `restricted` PodSecurity namespace refuses.
#              The image is PROBED once its Pod is running, never trusted: naming an image is not
#              the same as that image carrying the tool, and a plain base image does not. Without it
#              the case skips naming the executable and the packages, because an absent tool is a
#              fact about the image and not a reading of the operator.
# Inputs:      All real, nothing mocked. One unprivileged Pod holding one shared endpoint, running a
#              server and a client of the transport against its own loopback. Loopback rather than a
#              second Pod on purpose: a second Pod adds a second grant and a second failure mode,
#              and the question is what ONE injected set carries.
# Expected:    - the granted endpoint's port link layer is InfiniBand;
#              - the transport establishes and reports a non-zero bandwidth from the injected set
#                alone, with no privilege and no host mount;
#              - the connection-manager variant's outcome is printed as an observation, with a
#                failure inside address resolution named as the absent IPoIB interface rather than
#                as a missing device.
# Cleanup:     Trap deletes the test Pod and the probe Pod. No baseline was changed; the trap runs
#              on pass AND fail and is safe to re-run.
#
# IF THIS CASE IS EVER MOVED OFF perftest AND ONTO A COLLECTIVE COMMUNICATION LIBRARY, one thing
# must move with it, and it is the reason the swap looks safer than it is. A container image lacking
# libibverbs and the matching provider library does not fail: the collective library SILENTLY FALLS
# BACK TO TCP and the job completes successfully, over Ethernet, with no error anywhere. That shape
# has been measured on real hardware. So a version of this case built on such a library must assert
# WHICH TRANSPORT WAS SELECTED, never that the job succeeded — the success is exactly what both
# outcomes have in common. perftest is used here precisely because it has no such fallback: it fails
# outright without a usable device, so its completing IS the reading.
set -uo pipefail

NS="${1:?usage: case-84.sh <NS>}"
CASE_ID=84
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

trap rdma_cleanup EXIT

rdma_select devices endpoint endpoint-whole
rdma_probe_start

# The link layer, per host device, from the kernel's own port attribute. Read in the probe Pod and
# not in the workload Pod: the workload Pod carries no host mount by design, and the whole point of
# this reading is that it comes from somewhere the thing under test did not write.
IB_DEVICES=""
ETH_DEVICES=""
for dev in $(rdma_host_rdma_devices); do
  case "$(rdma_host_link_layer "$dev")" in
  InfiniBand) IB_DEVICES="${IB_DEVICES}${dev} " ;;
  Ethernet) ETH_DEVICES="${ETH_DEVICES}${dev} " ;;
  esac
done

if [ -z "$IB_DEVICES" ]; then
  rdma_skip "No endpoint on ${RDMA_NODE} reports link_layer=InfiniBand." \
    "Ethernet-attached devices found: ${ETH_DEVICES:-none}." \
    "A RoCE or EFA adapter runs the verbs layer over Ethernet and opens the same character device," \
    "so running this case there would report a pass on the one host shape that cannot answer it." \
    "What this reading needs is stated as a reading and not as a part number: some endpoint whose" \
    "/sys/class/infiniband/<dev>/ports/1/link_layer says InfiniBand. See" \
    "references/rdma-host-shapes.md for the shapes that carry one."
fi
echo "[case-84] InfiniBand endpoint(s) on ${RDMA_NODE}: ${IB_DEVICES}"

# The image gate comes AFTER the host-shape gate on purpose. Both end the case, but only one of the
# two skips is actionable: a missing image is something the operator supplies, and reporting it on a
# RoCE host would send them to find an image for a reading that host still could not answer.
rdma_require_image E2E_RDMA_PERFTEST_IMAGE "ib_write_bw (the perftest suite) and an RDMA userspace"
PERF_IMAGE="$RDMA_IMAGE"

PERF_POD="gpustack-e2e-rdma-transport-${RANDOM}"
TESTPODS+=("$PERF_POD")
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${PERF_POD}, namespace: ${RDMA_TEST_NS} }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: main
      image: ${PERF_IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${RDMA_KEY_SHARED}: "1" }
        requests: { ${RDMA_KEY_SHARED}: "1" }
EOF

PHASE=$(rdma_wait_pod "$PERF_POD" Running)
if [ "$PHASE" != Running ]; then
  record FAIL "a Pod holding one InfiniBand endpoint runs" \
    "phase ${PHASE:-<timeout>}; kubelet says: $(rdma_pod_reason "$PERF_POD")"
  rdma_results
fi

# The tool is probed before anything is measured or recorded. An image that was named but does not
# carry the transport is a fact about the image, not a reading of the operator — and recording a row
# here first would leave a PASS behind and a green exit code on a run that never measured a
# transport, which is the shape this whole family refuses.
if ! rdma_has_tool "$PERF_POD" ib_write_bw; then
  rdma_skip "E2E_RDMA_PERFTEST_IMAGE=${PERF_IMAGE} carries no ib_write_bw." \
    "Name an image that ships ${RDMA_PERFTEST_PACKAGES}; a plain base image does not, and this" \
    "case has no reading to take without it. The absence of a tool is not a reading of the" \
    "operator, so it is a skip rather than a failure."
fi

GRANTED=$(kubectl -n "$RDMA_TEST_NS" exec "$PERF_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]' | cut -d, -f1)
if [ -z "$GRANTED" ]; then
  record FAIL "the granted endpoint is the InfiniBand one" \
    "the container was told no device name, so which endpoint it holds cannot be established"
  rdma_results
fi
if printf '%s ' "$IB_DEVICES" | grep -qw "$GRANTED"; then
  record PASS "the granted endpoint is the InfiniBand one" \
    "granted ${GRANTED}, whose port reports link_layer=InfiniBand from the kernel's own attribute"
else
  # A node can carry both, and the allocator picked the Ethernet one. The transport would still
  # run — over RoCE — and the pass would be the one this row's last column excludes.
  record SKIP "the granted endpoint is the InfiniBand one" \
    "NOT THE HOST SHAPE UNDER TEST: the allocator granted ${GRANTED}, which is not among the InfiniBand endpoint(s) ${IB_DEVICES}. Running the transport on it would measure RoCE, which this row explicitly cannot be answered by"
  rdma_results
fi

# ---------------------------------------------------------------------------------------------------
# The transport. A server and a client in one container, over loopback: the question is what ONE
# injected set carries, and a second Pod would add a second grant and a second failure mode.
# ---------------------------------------------------------------------------------------------------
BW_OUT=$(kubectl -n "$RDMA_TEST_NS" exec "$PERF_POD" -- sh -c \
  "ib_write_bw -d ${GRANTED} >/tmp/server.log 2>&1 & sleep 3; ib_write_bw -d ${GRANTED} 127.0.0.1 2>&1; echo ---SERVER---; cat /tmp/server.log" 2>&1)
BW=$(printf '%s\n' "$BW_OUT" | awk '$1 ~ /^[0-9]+$/ && NF >= 4 {print $4; exit}')
if [ -n "$BW" ] && printf '%s' "$BW" | grep -qE '^[0-9]+([.][0-9]+)?$' && [ "${BW%%.*}" -gt 0 ]; then
  record PASS "the transport installs and runs from the injected set alone" \
    "ib_write_bw over ${GRANTED} reported ${BW} (BW average, the tool's own units), in an unprivileged container mounting nothing"
else
  record FAIL "the transport installs and runs from the injected set alone" \
    "no bandwidth was reported over ${GRANTED}: $(printf '%s' "$BW_OUT" | head -6 | tr '\n' ' ')"
fi

# The connection-manager variant, recorded rather than asserted. Its failure mode is a deployment
# shape — the container's network namespace carries no IPoIB interface for the address resolution to
# bind to — and separating it from "the device was never injected" is the reason it gets a row at
# all: the two look alike from an exit code and mean opposite things.
CM_OUT=$(kubectl -n "$RDMA_TEST_NS" exec "$PERF_POD" -- sh -c \
  "ib_write_bw -R -d ${GRANTED} >/tmp/cmserver.log 2>&1 & sleep 3; ib_write_bw -R -d ${GRANTED} 127.0.0.1 2>&1" 2>&1)
if printf '%s' "$CM_OUT" | grep -qi 'rdma_resolve_addr'; then
  record SKIP "the connection-manager path over the same endpoint" \
    "OBSERVATION: address resolution failed (rdma_resolve_addr). The container's network namespace has no IPoIB interface to resolve against, which is a deployment shape rather than a defect in what was injected — the same endpoint moved bytes on the line above"
elif printf '%s\n' "$CM_OUT" | awk '$1 ~ /^[0-9]+$/ && NF >= 4 {found=1} END {exit !found}'; then
  record SKIP "the connection-manager path over the same endpoint" \
    "OBSERVATION: it ran. This cluster's network namespace does carry an address the connection manager can resolve, which is more than the injected set is claimed to provide"
else
  record SKIP "the connection-manager path over the same endpoint" \
    "OBSERVATION: neither a resolution failure nor a completed run: $(printf '%s' "$CM_OUT" | head -4 | tr '\n' ' ')"
fi

rdma_results
