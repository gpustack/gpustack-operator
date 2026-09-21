#!/usr/bin/env bash
#
# CASE 81 — A granted RDMA endpoint opens inside an ordinary container, and an ungranted one does not
#   (MUTATING, self-recovering; AUTO-SKIPS without a node carrying a whole-function RDMA endpoint)
#
#   case-81.sh <NS>
#
# <NS> is the operator's own namespace. The test Pods run in `default` (or RDMA_TEST_NS).
#
# Goal:        The mount is not the permission. A container that asks for an RDMA endpoint and
#              mounts nothing by hand gets the endpoint's character device and can open() it; the
#              same container without the request cannot. That second half is the whole point: the
#              device cgroup, not the file mode, is what admits the open, and only an allocation
#              adds the rule.
#              WHAT DOES NOT COUNT, and is therefore deliberately not how these Pods are built: an
#              open() that succeeds in a PRIVILEGED Pod, or in one that also carries a hostPath
#              mount. Either bypasses the cgroup rule this case exists to prove, so both Pods here
#              are unprivileged and mount nothing. The mapping from a granted device name to its
#              character-device node is taken in a SEPARATE probe Pod for the same reason — that Pod
#              does carry a host mount, and nothing about the grant is read from it.
# Environment: A node running a device manager with at least one WHOLE-FUNCTION RDMA endpoint (an
#              interface that is not an SR-IOV physical function with virtual functions configured),
#              since the shared key is served from those. AUTO-SKIPS (exit 0, printing NOTHING WAS
#              VERIFIED) otherwise. The verbs-library half additionally needs E2E_RDMA_IMAGE naming
#              an image that ships `ibv_devinfo`. The image is PROBED rather than trusted — naming
#              one is not the same as its carrying the tool, and a plain base image does not — and
#              without the tool that one check skips naming it, because an absent tool is a fact
#              about the image and not a reading of the operator. The open() checks still run
#              either way: a shell's own `exec <>` redirection IS an open() and needs no RDMA
#              userspace at all.
#              The probe Pod mounts the host's /sys read-only, which a `restricted` PodSecurity
#              namespace refuses. Without it the grant and the refusal are still read — both happen
#              inside the workload Pods — and only the "and no other adapter" check skips, because
#              telling a grant of one from a grant of everything needs the host's own count.
# Inputs:      All real, nothing mocked. Two workload Pods differing in exactly one field — the
#              resource request — and one read-only probe Pod for the device-node mapping.
# Expected:    - the granted Pod's container carries a character device for each granted endpoint
#                and NOT one per adapter on the node;
#              - the environment names the granted RDMA devices, and every name is an endpoint the
#                ledger publishes;
#              - open() on each injected node succeeds;
#              - with an RDMA userspace image, the verbs library reports the granted device;
#              - the identical Pod WITHOUT the request is refused the same path. THREE refusal
#                shapes are accepted and the case records WHICH, because they prove the isolation at
#                different layers: the node present in the container and the open refused (the
#                device cgroup has no rule — the only shape that exercises what an allocation adds);
#                /dev/infiniband present but this node absent from it; and /dev/infiniband absent
#                from the container altogether. They are classified from `test -d` and `test -e`
#                rather than from the shell's error wording, which differs between shells and spelled
#                the third shape in a way no errno phrase matched. A successful open is the failure.
# Cleanup:     Trap deletes the two workload Pods and the probe Pod. No baseline was changed; the
#              trap runs on pass AND fail and is safe to re-run.
set -uo pipefail

NS="${1:?usage: case-81.sh <NS>}"
CASE_ID=81
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

trap rdma_cleanup EXIT

# The workload image needs a shell and nothing else. `exec 3<>path` is an open(2) with O_RDWR, which
# is the call the criterion names, and the shell reports the errno in words — so the refusal shape
# is read from the kernel's own answer rather than inferred from a tool's exit code.
WORK_IMAGE="${E2E_RDMA_WORKLOAD_IMAGE:-$RDMA_PROBE_IMAGE}"
GRANTED_POD="gpustack-e2e-rdma-granted-${RANDOM}"
UNGRANTED_POD="gpustack-e2e-rdma-ungranted-${RANDOM}"

rdma_select devices endpoint endpoint-whole
# Tried, not required. The probe supplies the device-name -> character-device mapping, which only
# the "and no other adapter" check and the fallback path for the control Pod need; the grant and the
# refusal are read inside the workload Pods themselves.
rdma_probe_try || true

# try_open <pod> <path> — attempt an O_RDWR open of the path inside the pod and classify the answer
# as one of OPEN, REFUSED, NOFILE, NODIR, or OTHER with the message.
#
# THE CLASSIFICATION COMES FROM READINGS, NOT FROM THE SHELL'S WORDING. An earlier revision matched
# the error text and was measured getting it wrong: where the whole /dev/infiniband directory is
# absent from the container, a Debian-family `sh` says "can't create ...: nonexistent directory",
# which matches neither "No such file or directory" nor a permission phrase, so a real and complete
# refusal was reported as an unrecognisable answer. Every shell spells errno differently and none of
# them is a contract, while `test -d` and `test -e` answer the same question the same way in all of
# them. The message is still collected, for the row's text, and decides nothing.
#
# The three refusal shapes are kept apart rather than folded, because they place the isolation at
# different layers and only one of them exercises the device cgroup:
#
#   REFUSED  the node is in the container and open() still fails -- the device cgroup has no rule;
#   NOFILE   /dev/infiniband is in the container but this endpoint's node is not -- the directory is
#            mounted from somewhere and the grant did not add this node;
#   NODIR    /dev/infiniband is not in the container at all -- nothing was injected, so the cgroup
#            was never consulted.
#
# OTHER is never read as a refusal: an exec that could not reach the container produces a message
# too, and counting that as a refusal would report a broken connection as proof of isolation.
#
# The open runs in a SUBSHELL. `exec` is a special built-in, so a redirection failure on it is fatal
# to the shell running it -- inside `if` the script would simply end, and the branch below would
# never be taken.
try_open() {
  local out
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

# create_pod <name> <resource line or empty> [image] — the two Pods differ in this one field and in
# nothing else, which is what makes the second one a control rather than a different experiment.
create_pod() {
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

# ---------------------------------------------------------------------------------------------------
# The device-node mapping, taken in the probe Pod. A granted device name is what the container is
# told; the character device it resolves to is the kernel's own answer, and the two have to be
# related by something outside the thing under test.
# ---------------------------------------------------------------------------------------------------
#
# The mapping is a plain two-column list rather than an associative array: this suite's cases are run
# by whatever bash the operator's machine offers, and an associative array is a bash 4 feature that
# fails at parse time on the bash 3.2 a macOS still ships as /bin/bash.
HOST_DEVICES=$(rdma_host_rdma_devices)
VERBS_MAP=""
for dev in $HOST_DEVICES; do
  node=$(rdma_probe "ls /host/sys/class/infiniband/${dev}/device/infiniband_verbs 2>/dev/null" | head -1 | tr -d '[:space:]')
  [ -n "$node" ] && VERBS_MAP="${VERBS_MAP}${dev} ${node}
"
done
HOST_VERBS_COUNT=$(printf '%s' "$VERBS_MAP" | grep -c . || true)

# ---------------------------------------------------------------------------------------------------
# 1. The granted Pod.
# ---------------------------------------------------------------------------------------------------
create_pod "$GRANTED_POD" "${RDMA_KEY_SHARED}: \"1\""
PHASE=$(rdma_wait_pod "$GRANTED_POD" Running)
if [ "$PHASE" != Running ]; then
  record FAIL "a Pod requesting one shared endpoint is admitted and runs" \
    "phase ${PHASE:-<timeout>} after ${RDMA_POD_TIMEOUT}s; kubelet says: $(rdma_pod_reason "$GRANTED_POD")"
  rdma_results
fi
record PASS "a Pod requesting one shared endpoint is admitted and runs" \
  "${GRANTED_POD} Running on ${RDMA_NODE}, unprivileged, mounting nothing"

GRANTED_NAMES=$(kubectl -n "$RDMA_TEST_NS" exec "$GRANTED_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
if [ -z "$GRANTED_NAMES" ]; then
  record FAIL "the container is told which RDMA devices it was granted" \
    "the environment carries no NCCL_IB_HCA; a container granted a device it cannot name has been handed half a resource"
else
  UNKNOWN=""
  for name in $(printf '%s' "$GRANTED_NAMES" | tr ',' ' '); do
    printf '%s\n' "$F_EP_NAMES" | tr ',' '\n' | grep -qxF "$name" || UNKNOWN="${UNKNOWN}${name} "
  done
  if [ -z "$UNKNOWN" ]; then
    record PASS "the container is told which RDMA devices it was granted" \
      "NCCL_IB_HCA=${GRANTED_NAMES}, every name an endpoint the ledger publishes"
  else
    record FAIL "the container is told which RDMA devices it was granted" \
      "NCCL_IB_HCA=${GRANTED_NAMES} names ${UNKNOWN}which the ledger does not publish as an endpoint"
  fi
fi

DEV_LIST=$(kubectl -n "$RDMA_TEST_NS" exec "$GRANTED_POD" -- sh -c 'ls /dev/infiniband 2>/dev/null' | grep -v '^$' | LC_ALL=C sort)
UVERBS_SEEN=$(printf '%s\n' "$DEV_LIST" | grep '^uverbs' || true)
UVERBS_COUNT=$(printf '%s\n' "$UVERBS_SEEN" | grep -c . || true)
GRANT_COUNT=$(printf '%s' "$GRANTED_NAMES" | tr ',' '\n' | grep -c . || true)

# One request, one endpoint — and the node count is what separates a per-endpoint grant from a
# directory handed over wholesale. On a single-adapter host the two are equal for a reason that is
# not the code's, so the check says so rather than claiming a distinction it could not draw.
if [ "$UVERBS_COUNT" -eq "$GRANT_COUNT" ] && [ "$GRANT_COUNT" -gt 0 ]; then
  if [ "$RDMA_PROBE_READY" -ne 1 ]; then
    record SKIP "the container gets the granted endpoint's device node and no other" \
      "NO INDEPENDENT INSTRUMENT: ${UVERBS_COUNT} verbs node(s) for ${GRANT_COUNT} granted endpoint(s), but how many the HOST has could not be read, so a grant of everything cannot be told from a grant of one. ${RDMA_PROBE_SKIP_REASON}"
  elif [ "$HOST_VERBS_COUNT" -gt "$GRANT_COUNT" ]; then
    record PASS "the container gets the granted endpoint's device node and no other" \
      "${UVERBS_COUNT} verbs node(s) for ${GRANT_COUNT} granted endpoint(s), out of ${HOST_VERBS_COUNT} on the host"
  else
    record SKIP "the container gets the granted endpoint's device node and no other" \
      "NOT DISCRIMINATING: the host has ${HOST_VERBS_COUNT} verbs node(s) and the grant is ${GRANT_COUNT}, so handing over the whole directory would produce this same count. The reading needs a host with more endpoints than the request"
  fi
else
  record FAIL "the container gets the granted endpoint's device node and no other" \
    "${UVERBS_COUNT} verbs node(s) in /dev/infiniband for ${GRANT_COUNT} granted endpoint(s); contents: $(printf '%s' "$DEV_LIST" | tr '\n' ' ')"
fi

OPEN_FAILS=""
OPEN_OK=""
for node in $UVERBS_SEEN; do
  verdict=$(try_open "$GRANTED_POD" "/dev/infiniband/${node}")
  if [ "$verdict" = OPEN ]; then
    OPEN_OK="${OPEN_OK}${node} "
  else
    OPEN_FAILS="${OPEN_FAILS}${node}=${verdict} "
  fi
done
if [ -z "$UVERBS_SEEN" ]; then
  record FAIL "the granted device opens without privilege and without a host mount" \
    "no verbs node was injected, so there was nothing to open"
elif [ -z "$OPEN_FAILS" ]; then
  record PASS "the granted device opens without privilege and without a host mount" \
    "open(O_RDWR) succeeded on ${OPEN_OK}"
else
  record FAIL "the granted device opens without privilege and without a host mount" \
    "refused: ${OPEN_FAILS}— the allocation injected the node but the device cgroup has no rule for it"
fi

# The verbs half. A shell's open() proves the cgroup rule; it does not prove the library can use the
# device, and `ibv_devinfo` succeeding would not prove the cgroup rule either. The two are separate
# readings and neither stands in for the other.
if [ -z "${E2E_RDMA_IMAGE:-}" ]; then
  record SKIP "the verbs library reports the granted device" \
    "NOT RUN: set E2E_RDMA_IMAGE to an image shipping ibv_devinfo (rdma-core). There is no default — an image without the tool would report an absent tool as an absent capability"
else
  VERBS_POD="gpustack-e2e-rdma-verbs-${RANDOM}"
  create_pod "$VERBS_POD" "${RDMA_KEY_SHARED}: \"1\"" "$E2E_RDMA_IMAGE"
  VPHASE=$(rdma_wait_pod "$VERBS_POD" Running)
  if [ "$VPHASE" != Running ]; then
    record FAIL "the verbs library reports the granted device" \
      "${VERBS_POD} phase ${VPHASE:-<timeout>}; kubelet says: $(rdma_pod_reason "$VERBS_POD")"
  elif ! rdma_has_tool "$VERBS_POD" ibv_devinfo; then
    # An image was named and it does not carry the tool. That is a fact about the image, and
    # recording the tool's own "command not found" as a FAIL would put a verdict about the operator
    # on a reading that was never taken.
    record SKIP "the verbs library reports the granted device" \
      "NOT RUN: E2E_RDMA_IMAGE=${E2E_RDMA_IMAGE} carries no ibv_devinfo. Name an image that ships ${RDMA_VERBS_PACKAGES}; a plain base image does not"
  else
    VNAMES=$(kubectl -n "$RDMA_TEST_NS" exec "$VERBS_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
    VOUT=$(kubectl -n "$RDMA_TEST_NS" exec "$VERBS_POD" -- sh -c 'ibv_devinfo 2>&1' 2>/dev/null)
    MISSING=""
    for name in $(printf '%s' "$VNAMES" | tr ',' ' '); do
      printf '%s' "$VOUT" | grep -q "$name" || MISSING="${MISSING}${name} "
    done
    if [ -n "$VNAMES" ] && [ -z "$MISSING" ]; then
      record PASS "the verbs library reports the granted device" \
        "ibv_devinfo names ${VNAMES}"
    else
      record FAIL "the verbs library reports the granted device" \
        "granted ${VNAMES:-<none>}, ibv_devinfo did not name ${MISSING:-anything}: $(printf '%s' "$VOUT" | head -3 | tr '\n' ' ')"
    fi
  fi
fi

# ---------------------------------------------------------------------------------------------------
# 2. The paired negative. The same Pod, without the request.
#
#    Required, not optional: a successful open in the granted Pod is also what a node with a
#    permissive device cgroup for everyone produces, and only the control separates the two.
# ---------------------------------------------------------------------------------------------------
TARGET_NODE=$(printf '%s\n' "$UVERBS_SEEN" | head -1)
if [ -z "$TARGET_NODE" ]; then
  TARGET_NODE=$(printf '%s' "$VERBS_MAP" | awk 'NF {print $2; exit}')
fi

if [ -z "$TARGET_NODE" ]; then
  record SKIP "the same Pod without the request is refused the same device" \
    "NOT REACHED: no verbs node name could be resolved, so the control has no path to attempt"
else
  create_pod "$UNGRANTED_POD" ""
  UPHASE=$(rdma_wait_pod "$UNGRANTED_POD" Running)
  if [ "$UPHASE" != Running ]; then
    record FAIL "the same Pod without the request is refused the same device" \
      "the control Pod never ran (phase ${UPHASE:-<timeout>}), so the refusal was never attempted: $(rdma_pod_reason "$UNGRANTED_POD")"
  else
    UVERDICT=$(try_open "$UNGRANTED_POD" "/dev/infiniband/${TARGET_NODE}")
    MODE=$(kubectl -n "$RDMA_TEST_NS" exec "$GRANTED_POD" -- sh -c "ls -l /dev/infiniband/${TARGET_NODE} 2>/dev/null" 2>/dev/null | awk '{print $1}')
    case "$UVERDICT" in
    REFUSED*)
      # The strongest of the three shapes: the node IS in the container's mount namespace, its mode
      # bits permit everyone, and the open is still refused — which places the refusal in the device
      # cgroup and nowhere else. The mode is read in the GRANTED pod because that is where the node
      # exists; it is the same node on the same host.
      record PASS "the same Pod without the request is refused the same device" \
        "REFUSED AT THE DEVICE CGROUP: the node is present in the ungranted container and open(O_RDWR) still failed, with the node's own mode bits reading ${MODE:-<unread>}. Kernel's words: ${UVERDICT#REFUSED:}"
      ;;
    NOFILE)
      # Weaker: /dev/infiniband is in the container but this endpoint's node is not, so the cgroup
      # rule was never consulted for it. Still a complete refusal of THIS endpoint.
      record PASS "the same Pod without the request is refused the same device" \
        "REFUSED AT THE MOUNT NAMESPACE (file level): /dev/infiniband exists in the ungranted container but ${TARGET_NODE} is not in it, so the device cgroup was never reached. This proves the endpoint is not handed out; it does NOT exercise the cgroup rule the granted half relies on"
      ;;
    NODIR)
      # Weakest of the three and the most common: the whole /dev/infiniband directory is absent from
      # the container, so there was nothing to refuse at. It proves the same thing the file-level
      # shape does — the endpoint is not reachable without a grant — at a different layer, and the
      # two are recorded apart because only the cgroup shape tests the rule an allocation adds.
      record PASS "the same Pod without the request is refused the same device" \
        "REFUSED AT THE MOUNT NAMESPACE (directory level): /dev/infiniband is not present in the ungranted container at all, so neither the node nor the device cgroup was ever reached. This proves the endpoint is not handed out; it does NOT exercise the cgroup rule the granted half relies on"
      ;;
    OPEN)
      record FAIL "the same Pod without the request is refused the same device" \
        "open(/dev/infiniband/${TARGET_NODE}) SUCCEEDED without any request, so the grant is adding no isolation; the granted half's pass says nothing on this node"
      ;;
    *)
      record FAIL "the same Pod without the request is refused the same device" \
        "the attempt produced neither an open nor a recognisable refusal: ${UVERDICT}"
      ;;
    esac
  fi
fi

rdma_results
