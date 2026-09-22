#!/usr/bin/env bash
#
# CASE 85 — A host-fabric member is granted its device by protocol, and mounts no device tree
#   (MUTATING, self-recovering; AUTO-SKIPS without a node carrying a whole-function RDMA endpoint)
#
#   case-85.sh <NS>
#
# <NS> is the operator's own namespace, where the rendered DaemonSet lands. The KVCacheBackend is
# cluster-scoped.
#
# Goal:        A KVCacheBackend whose transport is RDMA renders a member that REQUESTS
#              device.gpustack.ai/rdma.shared and mounts nothing from the host's device tree. The
#              request is the permission: a bind mount of /dev/infiniband carries the device node in
#              and leaves the device cgroup refusing open(), which the store reports as a host with
#              no fabric while installing TCP under an object that still reads RDMA.
#
#              WHAT THIS CASE DELIBERATELY DOES NOT DO is start the store and read its transport.
#              That half is case 81's, from the other end: it proves a granted endpoint opens inside
#              an ordinary container and an ungranted one does not. Splitting them is what keeps
#              either from needing the other's preconditions -- case 81 needs no store image, and
#              this one needs no working fabric, only an operator that renders the request. Together
#              they cover the chain; neither covers it alone, and a reader taking this one for an
#              end-to-end reading has over-read it.
#
# Environment: A node running a device manager with at least one WHOLE-FUNCTION RDMA endpoint, since
#              the shared key is served from those. AUTO-SKIPS (exit 0, printing NOTHING WAS
#              VERIFIED) otherwise. No store image is pulled and no member Pod has to reach Running:
#              every assertion is read from the rendered DaemonSet's pod template.
#
# Cleanup:     Deletes the KVCacheBackend it created, on every exit path.

set -uo pipefail

NS="${1:?usage: case-85.sh <operator-namespace>}"
CASE_ID=85

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

rdma_select endpoint endpoint-whole

BACKEND="gpustack-e2e-fabric-grant-$$"
RDMA_KEY="device.gpustack.ai/rdma.shared"
DEVICE_TREE="/dev/infiniband"

cleanup() {
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

# The image is never pulled: nothing here waits for a Pod. It is named because the schema requires
# one, and the leader block is spelled out for the same reason -- an omitted leader is refused by
# the API server, which a case that never reaches its own checks reports as nothing at all.
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

if ! kubectl apply -f - >/dev/null 2>&1 <<YAML; then
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: RDMA
  connection:
    managed:
      leader: {}
      members:
        - nodeSelector: {kubernetes.io/hostname: ${RDMA_NODE}}
          medium: DRAM
          capacityPerMember: 1Gi
YAML
  record FAIL "the operator accepts a backend whose transport is RDMA" \
    "the apply was refused; nothing below was exercised"
  rdma_results
  exit 1
fi
record PASS "the operator accepts a backend whose transport is RDMA" \
  "${BACKEND} applied, member group selecting ${RDMA_NODE}"

DS="${BACKEND}-member-0"
TEMPLATE=""
for _ in $(seq 1 30); do
  TEMPLATE=$(kubectl -n "$NS" get daemonset "$DS" -o json 2>/dev/null)
  [ -n "$TEMPLATE" ] && break
  sleep 2
done

if [ -z "$TEMPLATE" ]; then
  record FAIL "the member DaemonSet is rendered" \
    "${DS} did not appear in ${NS} within 60s, so no rendering could be read"
  rdma_results
  exit 1
fi
record PASS "the member DaemonSet is rendered" "${DS} in ${NS}"

POD=$(printf '%s' "$TEMPLATE" | jq -c '.spec.template.spec')

# The grant. Read from limits, which is where an extended resource is asked for: request and limit
# may not differ for one, and this operator renders the limit alone so the Pod defaulter fills the
# request in.
GRANT=$(printf '%s' "$POD" | jq -r --arg k "$RDMA_KEY" '.containers[0].resources.limits[$k] // "ABSENT"')
if [ "$GRANT" = "1" ]; then
  record PASS "the member requests one endpoint under the key the Device Manager advertises" \
    "${RDMA_KEY}=1, derived from the protocol and declared nowhere"
else
  record FAIL "the member requests one endpoint under the key the Device Manager advertises" \
    "${RDMA_KEY}=${GRANT}; without the allocation the device cgroup refuses open() and the store installs TCP while the object reads RDMA"
fi

# The absence of the mount, read on BOTH halves. A volume nothing mounts and a mount referencing no
# volume are different defects, and either one alone would leave half the old rendering in place.
VOLS=$(printf '%s' "$POD" | jq -r --arg p "$DEVICE_TREE" '[.volumes // [] | .[] | select(.hostPath.path == $p) | .name] | join(",")')
MOUNTS=$(printf '%s' "$POD" | jq -r --arg p "$DEVICE_TREE" '[.containers[0].volumeMounts // [] | .[] | select(.mountPath == $p) | .name] | join(",")')
if [ -z "$VOLS" ] && [ -z "$MOUNTS" ]; then
  record PASS "the member mounts no device tree" \
    "no volume and no volumeMount on ${DEVICE_TREE}: the grant carries the verbs node of the endpoint it allocated, and the tree would carry every adapter it was not granted"
else
  record FAIL "the member mounts no device tree" \
    "volume(s)='${VOLS}' mount(s)='${MOUNTS}' on ${DEVICE_TREE}; a container granted one endpoint would see the device nodes of every other adapter on the node"
fi

# The rest of the host-fabric base is unchanged, and is asserted here so that a future change
# removing it reports as this case failing rather than as this case passing on a member that lost
# the network and the capabilities it still needs.
HOSTNET=$(printf '%s' "$POD" | jq -r '.hostNetwork // false')
DNSPOL=$(printf '%s' "$POD" | jq -r '.dnsPolicy // ""')
if [ "$HOSTNET" = "true" ] && [ "$DNSPOL" = "ClusterFirstWithHostNet" ]; then
  record PASS "the member keeps the rest of the host-fabric base" \
    "hostNetwork=true with dnsPolicy=ClusterFirstWithHostNet, which is what lets it still resolve the leader's Service name"
else
  record FAIL "the member keeps the rest of the host-fabric base" \
    "hostNetwork=${HOSTNET} dnsPolicy=${DNSPOL}"
fi

CAPS=$(printf '%s' "$POD" | jq -r '[.containers[0].securityContext.capabilities.add // [] | .[]] | sort | join(",")')
PRIV=$(printf '%s' "$POD" | jq -r '.containers[0].securityContext.privileged // "unset"')
if [ "$CAPS" = "IPC_LOCK,SYS_RESOURCE" ] && [ "$PRIV" = "unset" ]; then
  record PASS "the member gets the two capabilities the fabric needs and never privileged" \
    "add=[${CAPS}], privileged unset -- pinning registered memory and raising the locked-memory limit, and nothing wider"
else
  record FAIL "the member gets the two capabilities the fabric needs and never privileged" \
    "add=[${CAPS}] privileged=${PRIV}"
fi

rdma_results
