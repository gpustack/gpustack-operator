#!/usr/bin/env bash
# Reconciles the Calico multi-NIC fix against the cluster's real node identities. Runs on the
# WORKSTATION, after the control plane is up, with kubectl on PATH.
#
#   calico-multi-nic-fix.sh --kubeconfig <path> --enabled <yes|no> --expect-nodes <n>
#
# What it fixes, measured on a multi-homed node: the route to a local workload interface picks its
# source address by the kernel's usual rules, which on a host with several subnets is not the
# node's cluster address. The kubelet's probe to a Pod then leaves with a source the Pod's reply
# cannot get back to, so CoreDNS never becomes Ready while everything else looks healthy. Calico's
# per-node FelixConfiguration.deviceRouteSourceAddress is what pins that source.
#
#   before: ip route get <podIP> -> dev caliXXXX src <a VLAN address>   (probe times out)
#   after:  ip route get <podIP> -> dev caliXXXX src <the node address>  (probe 200 OK)
#
# The second half is a DaemonSet that keeps a SNAT rule for pod egress at the TOP of the nat
# POSTROUTING chain. Position, not mere presence: kube-proxy re-prepends KUBE-POSTROUTING on every
# restart, and the MASQUERADE inside it terminates nat processing, so a rule that has been pushed
# down is never reached again.
#
# Everything it creates is labelled, so an object whose node has left the cluster -- or every
# object, when the fix is turned off -- can be pruned without guessing at ownership. Nothing here
# is cleaned on destroy: the uninstall removes the cluster along with it.
set -euo pipefail

PROG="$(basename "$0")"
readonly PROG
readonly OWNER_KEY="gpustack.ai/managed-by"
readonly OWNER_VALUE="testing-infra-rke2"
readonly DS_NAME="calico-multi-nic-fix"
readonly DS_NAMESPACE="kube-system"

log() { echo "[$PROG] $*"; }
die() {
  echo "[$PROG] $*" >&2
  exit 1
}

kubeconfig=""
enabled=""
image=""
expect_nodes=""
while [ "$#" -gt 0 ]; do
  case "$1" in
  --kubeconfig)
    kubeconfig="${2:-}"
    [ -n "$kubeconfig" ] || die "--kubeconfig needs a value"
    shift 2
    ;;
  --expect-nodes)
    expect_nodes="${2:-}"
    [ -n "$expect_nodes" ] || die "--expect-nodes needs a value"
    shift 2
    ;;
  --enabled)
    enabled="${2:-}"
    [ -n "$enabled" ] || die "--enabled needs a value"
    shift 2
    ;;
  *)
    echo "usage: $PROG --kubeconfig <path> --enabled <yes|no> --expect-nodes <n>" >&2
    exit 2
    ;;
  esac
done
[ -n "$kubeconfig" ] || die "--kubeconfig is required"
case "$enabled" in
yes | no) ;;
*) die "--enabled must be yes or no, got '${enabled}'" ;;
esac
case "$expect_nodes" in
'' | *[!0-9]*) die "--expect-nodes must be a whole number, got '${expect_nodes}'" ;;
esac

kc() { KUBECONFIG="$kubeconfig" kubectl "$@"; }

# A read that fails is never taken for an answer: against a control plane that has only just
# started, a transport failure is not information about the cluster. Only a real reply decides.
retry() {
  local tries="$1" what="$2"
  shift 2
  local i
  for i in $(seq 1 "$tries"); do
    if "$@"; then return 0; fi
    if [ "$i" = "$tries" ]; then
      die "${what} did not succeed after ${tries} attempts"
    fi
    sleep 5
  done
}

# --- prune -------------------------------------------------------------------

# Objects this module owns whose node is no longer in the cluster, or all of them when the fix is
# off. An object outliving its reason is worse than no object: it pins a source address for a node
# that may have been re-addressed, and nothing in the cluster says why.
prune() {
  local wanted="$1" ours name why
  # The reason differs by caller, and a message that names the wrong one sends the reader looking
  # for a node that never left.
  if [ -n "$wanted" ]; then why="its node is no longer in the cluster"; else why="the fix is turned off"; fi
  # A read that fails is not "nothing to prune". A missing CRD IS the normal case with a non-Calico
  # CNI, so the two are told apart by a read that must succeed either way: if the apiserver answers,
  # the failed list means the CRD is absent; if it does not, the apply fails rather than reporting a
  # prune that never happened.
  if ! ours=$(kc get felixconfigurations.crd.projectcalico.org -l "${OWNER_KEY}=${OWNER_VALUE}" -o name 2>/dev/null); then
    kc get --raw=/readyz --request-timeout=10s >/dev/null 2>&1 ||
      die "cannot reach the cluster to list this module's FelixConfigurations; refusing to report a prune that did not happen"
    log "this cluster has no FelixConfiguration CRD; nothing of ours to prune"
    return 0
  fi
  for name in $(printf '%s\n' "$ours" | sed 's|^.*/||'); do
    [ -n "$name" ] || continue
    if ! printf '%s\n' "$wanted" | grep -qx "$name"; then
      # --ignore-not-found makes an already-absent object a success, so a non-zero here is a real
      # failure and must not be logged as a prune.
      kc delete felixconfigurations.crd.projectcalico.org "$name" --ignore-not-found >/dev/null ||
        die "cannot delete FelixConfiguration ${name}, which ${why}"
      log "pruned FelixConfiguration ${name}: ${why}"
    fi
  done
}

if [ "$enabled" = no ]; then
  prune ""
  kc delete daemonset -n "$DS_NAMESPACE" "$DS_NAME" --ignore-not-found >/dev/null ||
    die "cannot delete the ${DS_NAME} DaemonSet; this module's objects are NOT removed"
  log "calico_multi_nic_fix is off; this module's objects are removed"
  exit 0
fi

# --- node identities ---------------------------------------------------------

# Read from the cluster, never guessed from SSH: a FelixConfiguration is matched by exact node
# name, and RKE2's node name is only BY DEFAULT the hostname -- node-name, with-node-id, or cloud
# metadata naming all change it, which would leave an object nothing consumes.
# And waited for, not merely read once. An install resource completing means the installer ran and the
# service was started without blocking -- the node has NOT registered yet. Reading the list at that
# moment returns a cluster that is missing nodes, and each missing node then never gets its
# FelixConfiguration, which surfaces only as "CoreDNS on that one node stays 0/1".
nodes=""
read_nodes() {
  nodes=$(kc get nodes -o json 2>/dev/null |
    jq -r '.items[] | select(any(.status.addresses[]; .type == "InternalIP")) | "\(.metadata.name) \(first(.status.addresses[] | select(.type == "InternalIP") | .address))"') &&
    [ "$(printf '%s\n' "$nodes" | grep -c .)" -ge "$expect_nodes" ]
}
retry 60 "waiting for all ${expect_nodes} node(s) to register" read_nodes
log "all ${expect_nodes} node(s) have registered with an internal address"

# --- image -------------------------------------------------------------------

# The fix needs nothing but iptables and a shell, and must be an image the node already has --
# otherwise the Pod that repairs the network is itself unschedulable on a network that needs
# repairing. calico-node's own image is the one certainty here: it is running on every node this
# fix targets, so IfNotPresent can never pull. Read from the cluster rather than pinned or
# overridable: the tag moves with every RKE2 release, and any other ref may need pulling on exactly
# the node whose network is broken.
read_image() {
  image=$(kc get daemonset -n calico-system calico-node \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null) && [ -n "$image" ]
}
# Generous, because this is also the wait for Calico itself: on a cold cluster the helm job, the
# tigera operator and the image import between them take minutes before calico-node exists at all.
# Measured at roughly four minutes from the server starting on the lab pair.
retry 96 "waiting for calico-node (Calico must be deployed before this fix can be applied)" read_image
log "using calico-node's own image: ${image}"

# --- apply -------------------------------------------------------------------

wanted=""
while read -r name address; do
  [ -n "$name" ] || continue
  wanted="${wanted}${wanted:+
}node.${name}"
  apply_felix() {
    printf '%s\n' \
      "apiVersion: crd.projectcalico.org/v1" \
      "kind: FelixConfiguration" \
      "metadata:" \
      "  name: node.${name}" \
      "  labels:" \
      "    ${OWNER_KEY}: ${OWNER_VALUE}" \
      "spec:" \
      "  deviceRouteSourceAddress: ${address}" | kc apply -f - >/dev/null
  }
  retry 24 "applying FelixConfiguration node.${name}" apply_felix
  log "FelixConfiguration node.${name} pins the route source to ${address}"
done <<EOF
$nodes
EOF

apply_daemonset() {
  kc apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${DS_NAME}
  namespace: ${DS_NAMESPACE}
  labels:
    ${OWNER_KEY}: ${OWNER_VALUE}
spec:
  selector:
    matchLabels:
      app: ${DS_NAME}
  template:
    metadata:
      labels:
        app: ${DS_NAME}
        ${OWNER_KEY}: ${OWNER_VALUE}
    spec:
      hostNetwork: true
      tolerations:
        - operator: Exists
      containers:
        - name: fix
          image: ${image}
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
          env:
            - name: NODE_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.hostIP
          command:
            - /bin/sh
            - -c
            - |
              set -e
              # The rules have to be written where kube-proxy's are: a host can carry both an
              # iptables-legacy and an nft table, and a rule in the other one is never consulted.
              # Pick the backend whose nat POSTROUTING actually jumps to KUBE-POSTROUTING.
              ipt=""
              for candidate in iptables-legacy iptables-nft iptables; do
                command -v "\$candidate" >/dev/null 2>&1 || continue
                if "\$candidate" -t nat -S POSTROUTING 2>/dev/null | grep -q KUBE-POSTROUTING; then
                  ipt="\$candidate"
                  break
                fi
              done
              [ -n "\$ipt" ] && echo "using \$ipt" || { echo "no iptables backend carries KUBE-POSTROUTING" >&2; exit 1; }
              want="-A POSTROUTING -o cali+ -m mark --mark 0x4000/0x4000 -j SNAT --to-source \$NODE_IP"
              while true; do
                # Written straight to /proc/sys: this image ships no sysctl binary, and reverse-path
                # filtering on a multi-homed node drops the very replies this fix exists to deliver.
                for f in /proc/sys/net/ipv4/conf/all/rp_filter /proc/sys/net/ipv4/conf/default/rp_filter /proc/sys/net/ipv4/conf/cali*/rp_filter; do
                  [ -f "\$f" ] && echo 0 > "\$f" 2>/dev/null || true
                done
                # Enforce the POSITION, not just the presence: kube-proxy re-prepends
                # KUBE-POSTROUTING on every restart and the MASQUERADE inside it terminates nat
                # processing, so a rule that has been pushed below it is never reached.
                count=\$("\$ipt" -t nat -S POSTROUTING | grep -c -- "--to-source \$NODE_IP" || true)
                first=\$("\$ipt" -t nat -S POSTROUTING | sed -n '2p')
                if [ "\$count" != "1" ] || [ "\$first" != "\$want" ]; then
                  while "\$ipt" -t nat -D POSTROUTING -o cali+ -m mark --mark 0x4000/0x4000 \
                          -j SNAT --to-source "\$NODE_IP" 2>/dev/null; do :; done
                  "\$ipt" -t nat -I POSTROUTING 1 -o cali+ -m mark --mark 0x4000/0x4000 \
                    -j SNAT --to-source "\$NODE_IP"
                  echo "restored the SNAT rule at POSTROUTING 1"
                fi
                sleep 60
              done
YAML
}
retry 12 "applying the ${DS_NAME} DaemonSet" apply_daemonset
log "DaemonSet ${DS_NAMESPACE}/${DS_NAME} keeps the pod-egress SNAT rule at the top of nat POSTROUTING"

prune "$wanted"

# --- assert the tunnel addresses ---------------------------------------------

# The other half of the fix lives in a HelmChartConfig written before Calico was deployed, so it
# cannot be applied from here -- but it CAN be checked from here, and its failure mode is silent:
# every node stays Ready while no pod can reach a pod on another node. Calico records the address it
# chose on the node itself, so compare it with the address the kubelet reports.
mismatched=""
while read -r name address; do
  [ -n "$name" ] || continue
  # Calico writes this annotation when calico-node starts on the node, which can be after this runs.
  # An absent annotation is "not yet", never "fine".
  annotated() {
    chosen=$(kc get node "$name" -o jsonpath='{.metadata.annotations.projectcalico\.org/IPv4Address}' 2>/dev/null || true)
    [ -n "$chosen" ]
  }
  retry 60 "waiting for Calico to record an address for ${name}" annotated
  case "$chosen" in
  "${address}/"* | "$address") ;;
  *) mismatched="${mismatched}${mismatched:+, }${name} advertises ${chosen} but the kubelet reports ${address}" ;;
  esac
done <<EOF
$nodes
EOF
if [ -n "$mismatched" ]; then
  die "Calico is advertising an address the node does not use for cluster traffic (${mismatched}); pod-to-pod across nodes will not work. Check that server/manifests/rke2-calico-config.yaml was in place before the first start."
fi
log "every node advertises its own cluster address to Calico"
