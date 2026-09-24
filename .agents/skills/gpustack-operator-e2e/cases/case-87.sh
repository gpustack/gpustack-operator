#!/usr/bin/env bash
#
# CASE 87 — Topology sources drive TAS placement and joint ModelDeployment admission
#           (MUTATING, self-recovering)
#
#   case-87.sh <NS>
#
# Goal:        Prove the complete topology path on real Nodes: cloud labels, a ConfigMap snapshot,
#              and an authenticated HTTPS webhook each become an observed profile and Kueue
#              Topology; the operator-managed flavors and identity-stable ClusterQueue then turn a
#              ModelDeployment role's required level into an actual TAS assignment. Hand-built
#              fixtures retain an independent Kueue-semantic control. The proofs run in order:
#
#              1. FITTING SAME-ZONE PLACEMENT. A two-Pod group annotated
#                 `kueue.x-k8s.io/podset-required-topology: topology.kubernetes.io/zone` is admitted
#                 into a hand-built TAS ClusterQueue, its Workload carries a topologyAssignment, and
#                 both Pods bind on Nodes whose OBSERVED zone labels agree. The per-Pod request is
#                 sized at sixty percent of the smallest counted Node, so two Pods can never share
#                 one Node and same-zone placement is a decision about the zone, not bin-packing.
#
#              2. CROSS-ZONE AGGREGATE-CAPACITY REFUSAL. A three-Pod group with the same annotation
#                 stays unadmitted on a cluster where every zone holds fewer than three counted
#                 Nodes while the cluster holds at least three and the queue's declared quota covers
#                 the whole group. The refusal is only meaningful against that proven-sufficient
#                 aggregate, so the shape and quota are asserted in a row of their own before the
#                 Pending rows, and the Pending readings are taken across TWO intervals.
#
#              3. OPERATOR SOURCE-TO-PLACEMENT. A size-two zone-required ModelDeployment is admitted
#                 and both of its Pods bind in one observed zone; a size-three request stays
#                 unadmitted although the same group without a required level is admitted on the
#                 same queue first, and one deliberately hostname-only Node is excluded. Omission
#                 remains admitted without an explicit required level.
#
#              4. LIVE PROFILE TRANSITION. A zone-aware ModelDeployment first reserves the
#                 operator-managed queue on its old topology. Replacing that profile with rack
#                 holds and drains the same ClusterQueue, retains its old flavors while reservations
#                 remain, switches only after the observed count reaches zero, restores admission,
#                 and allows Kueue to readmit the Workload on the replacement topology.
#
#              5. MODELDEPLOYMENT MULTI-ROLE JOINT ADMISSION. Through the operator's own queue (the
#                 only queue a ModelDeployment can name -- the operator routes it by the
#                 InstanceType's published entrance, which is why this half does not use the case's
#                 hand-built one): a two-role deployment's Workloads all reserve quota and the joint
#                 admission check reads Ready; then, with the pool occupied to its last unit, a
#                 second two-role deployment has every Workload held joint-Pending with no
#                 admission, no quota reservation and no role Pod bound to a Node, across two
#                 intervals. That shortage is SYMMETRIC: every live hostname domain is filled, so
#                 no role fits and "unreserved" is the expected reading. An ASYMMETRIC shortage,
#                 where one role fits, reads differently and is not asserted here: the fitting
#                 role holds QuotaReserved=True and a topology assignment while the joint check
#                 stays Pending and no role Pod binds, until the set admits, the deployment is
#                 deleted, or the joint check parks it. Every Workload of a deployment is
#                 attributed by following the Workload's owner Pod references back to the
#                 deployment's Pods -- Kueue's pod
#                 integration writes no ModelDeployment owner reference of any kind, so a lookup
#                 that searched for one would find nothing on a correct cluster and pass vacuously.
#
# Environment: A cluster with the operator chart's Kueue (TopologyAwareScheduling enabled by the
#              chart) and a materialized scheduling chain (run case-1 first) for the third proof.
#              The namespace must carry the pool's entrance LocalQueue. No GPU or engine image is
#              required: the TAS Pods are pause placeholders and the ModelDeployment replicas are placeholders
#              that never serve; every asserted value is on Kueue objects and Node labels.
#
#              The TAS proofs need a zone shape this case cannot create: at least three schedulable
#              Linux Nodes carrying topology.kubernetes.io/zone, no zone holding three of them, and
#              a zone holding at least two. A cluster without that shape makes the refusal provably
#              unprovable, so those rows SKIP naming the measured shape rather than failing on it.
#              EXITS 2 (input required) when the ModelDeployment half has no InstanceType or no
#              reachable entrance LocalQueue.
#
# Inputs:      All real, nothing mocked. Source fixtures and test Node selector labels are
#              case-prefixed and restored. The Kueue
#              Topology/ResourceFlavor/ClusterQueue/LocalQueue set is created case-prefixed and
#              deleted by name; the ClusterQueue references no AdmissionCheck, so no operator
#              barrier gates the hand-built queue. The ModelDeployments name an explicit image
#              because a CPU-only InstanceType synthesizes none; override with E2E_MD_IMAGE, the
#              InstanceType with E2E_MD_INSTANCE_TYPE.
#
# Expected:    Every source reaches its documented Ready/stale/expired/recovered states and produces
#              the exact generated Topology levels; each writing source's snapshot values appear on
#              the Nodes through its own NodeFeatures, and retiring it removes those NodeFeatures and
#              labels while the cloud region/zone and a label no source owns remain; both the control
#              Job and the size-two deployment
#              carry topologyAssignment and bind in one observed zone; both three-Pod groups hold no
#              admission, quota reservation or bound Pod across two intervals while aggregate Nodes
#              suffice; the queue transition preserves name/UID, is observed held with the old flavors
#              while a reservation is still counted, and never drops an old flavor before that
#              reservation drains; a fitting two-role deployment has both Workloads
#              admitted with the joint check Ready;
#              with every hostname domain occupied (symmetric shortage), a second two-role
#              deployment has every Workload joint Pending, unadmitted, unreserved and unbound
#              across two intervals.
#
# Cleanup:     A trap force-releases every Workload owning each deployment's Pods BEFORE deleting
#              the ModelDeployments (a serving group's finalizer is released by nothing but its
#              Workload being deleted), releases again after, deletes the two Jobs and then the
#              case-prefixed fixtures and temporary Node selector labels. The operator's generated
#              Topologies and ResourceFlavors retire once their profiles lose all references; the
#              managed ClusterQueue keeps its identity. Idempotent and runs on pass AND fail.
#
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-87.sh <NS>" >&2
  exit 2
fi
SYSTEM_NS="${E2E_SYSTEM_NAMESPACE:-gpustack-system}"

PREFIX="case87-$(date +%s)-$$"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
BINDING="${E2E_MD_BINDING:-${PREFIX}-no-such-binding}"
JOINT_CHECK="gpustack-model-deployment-joint"

# The gap between the two observation intervals of every negative probe: long enough for several
# Kueue reconciliation passes, so an admission that was merely late reads as late and not as
# refused, and short enough that the case's own bound still bounds it.
INTERVAL="${E2E_TAS_INTERVAL:-20}"

# A topology.gpustack.ai/* label no source owns, set straight on one Node by this case. Sources publish
# only through their own NodeFeatures, so retiring a source must leave this label where it is.
FOREIGN_LABEL="topology.gpustack.ai/e2e-foreign"

FAILS=0
ROWS=()
CQ_WATCH_PID=""
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
print_rows() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in "${ROWS[@]}"; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
}
# An input-required exit still owes the rows already recorded: a FAIL measured before the missing
# input was noticed is a real failure, and exit 2 alone would report it as "input required".
exit_input_required() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-87] ${FAILS} check(s) FAILED before the input check"; exit 1; }
  exit 2
}

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_topology-tas-lib.sh"

cleanup() {
  local md instance_workload
  if [ -n "$CQ_WATCH_PID" ]; then
    kill "$CQ_WATCH_PID" 2>/dev/null || true
    wait "$CQ_WATCH_PID" 2>/dev/null || true
  fi
  for md in "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved"; do
    tas_md_force_release "$NS" "$md"
  done
  instance_workload="$(kubectl -n "$NS" get pod "${PREFIX}-instance" \
    -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
  [ -n "$instance_workload" ] && kubectl -n "$NS" delete workload "$instance_workload" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete instance.worker.gpustack.ai "${PREFIX}-instance" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  for md in "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved"; do
    tas_md_force_release "$NS" "$md"
  done
  kubectl -n "$NS" delete job.batch "${PREFIX}-fit" "${PREFIX}-refuse" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete topologysources.worker.gpustack.ai \
    "${PREFIX}-native" "${PREFIX}-rack" "${PREFIX}-webhook" \
    --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete deploy "${PREFIX}-webhook" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete service "${PREFIX}-webhook" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete configmap "${PREFIX}-rack" "${PREFIX}-webhook-snapshot" "${PREFIX}-webhook-ca" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete secret "${PREFIX}-webhook-token" "${PREFIX}-webhook-tls" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl label nodes -l topology.gpustack.ai/e2e-native=enabled topology.gpustack.ai/e2e-native- \
    >/dev/null 2>&1 || true
  kubectl label nodes -l topology.gpustack.ai/e2e-rack=enabled topology.gpustack.ai/e2e-rack- \
    >/dev/null 2>&1 || true
  kubectl label nodes -l "$FOREIGN_LABEL" "${FOREIGN_LABEL}-" >/dev/null 2>&1 || true
  tas_delete_fixtures "$PREFIX" "$NS"
}
trap cleanup EXIT

source_condition() {
  kubectl get topologysources.worker.gpustack.ai "$1" \
    -o jsonpath="{range .status.conditions[?(@.type=='$2')]}{.status}|{.reason}{end}" 2>/dev/null
}

wait_source() {
  local name="$1" expected="$2"
  for _ in $(seq 1 60); do
    [ "$(source_condition "$name" Ready)" = "$expected" ] && return 0
    sleep 2
  done
  return 1
}

wait_profile_levels() {
  local expected="$1" profiles profile topology levels matches
  for _ in $(seq 1 60); do
    profiles="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.gpustack\.ai/profile}{"\n"}{end}' 2>/dev/null | sort -u | grep -v '^$' || true)"
    matches=0
    for profile in $profiles; do
      topology="gpustack-${profile}"
      levels="$(kubectl get topology.kueue.x-k8s.io "$topology" -o jsonpath='{range .spec.levels[*]}{.nodeLabel}{" "}{end}' 2>/dev/null)"
      [ "$levels" = "$expected" ] && { SOURCE_TOPOLOGY="$topology"; matches=$((matches + 1)); }
    done
    [ "$matches" = 1 ] && return 0
    sleep 2
  done
  SOURCE_TOPOLOGY=""
  return 1
}

wait_cq_topology_plan() {
  local cq="$1" topology="$2" mode="${3:-only}" absent="${4:-}" ready uid topologies
  for _ in $(seq 1 90); do
    ready="$(kubectl get clusterqueue.kueue.x-k8s.io "$cq" \
      -o jsonpath='{range .status.conditions[?(@.type=="TopologyReady")]}{.status}{end}' 2>/dev/null)"
    uid="$(kubectl get clusterqueue.kueue.x-k8s.io "$cq" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    topologies="$(kubectl get clusterqueue.kueue.x-k8s.io "$cq" -o json 2>/dev/null \
      | jq -r '(.spec.resourceGroups // [])[]?.flavors[]?.name' \
      | while read -r flavor; do
          kubectl get resourceflavor.kueue.x-k8s.io "$flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null
          printf '\n'
        done | sort -u | grep -v '^$' || true)"
    if [ "$ready" = True ] && [ -n "$uid" ] \
      && printf '%s\n' "$topologies" | grep -Fxq "$topology"; then
      if { [ "$mode" != only ] || [ "$(printf '%s\n' "$topologies" | grep -c . || true)" = 1 ]; } \
        && { [ -z "$absent" ] || ! printf '%s\n' "$topologies" | grep -Fxq "$absent"; }; then
        CQ_PLAN_UID="$uid"
        CQ_PLAN_TOPOLOGIES="$topologies"
        return 0
      fi
    fi
    sleep 2
  done
  CQ_PLAN_UID="$uid"
  CQ_PLAN_TOPOLOGIES="$topologies"
  return 1
}

cq_plan_topologies_inline() {
  printf '%s\n' "${CQ_PLAN_TOPOLOGIES:-missing}" | paste -sd, -
}

# "node region/zone" for every Node a writing source selects, sorted.
native_region_zone_pairs() {
  kubectl get nodes -l topology.gpustack.ai/e2e-rack=enabled -o json 2>/dev/null | jq -r '
    .items[] | .metadata.name + " " + (.metadata.labels["topology.kubernetes.io/region"] // "") + "/" +
    (.metadata.labels["topology.kubernetes.io/zone"] // "")' | sort
}

delete_source() {
  kubectl delete topologysources.worker.gpustack.ai "$1" --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
}

apply_native_source() {
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${PREFIX}-native
spec:
  nodeSelector:
    matchLabels:
      topology.gpustack.ai/e2e-native: enabled
  levels:
  - topology.kubernetes.io/region
  - topology.kubernetes.io/zone
  nodeLabels: {}
YAML
}

snapshot_yaml() {
  local revision="$1" label="$2" mode="$3"
  kubectl get nodes -o json | jq -r --arg revision "$revision" --arg label "$label" --arg mode "$mode" '
    "apiVersion: topology.gpustack.ai/v1alpha1\nrevision: " + $revision + "\nnodes:\n" +
    ([.items | map(select(.metadata.labels["topology.gpustack.ai/e2e-rack"] == "enabled"))
      | sort_by(.metadata.name) | to_entries[] |
      "  " + .value.metadata.name + ":\n" +
      (if $mode == "rack" then
         "    topology.kubernetes.io/region: " + .value.metadata.labels["topology.kubernetes.io/region"] + "\n" +
         "    topology.kubernetes.io/zone: " + .value.metadata.labels["topology.kubernetes.io/zone"] + "\n" +
         "    " + $label + ": rack-" + (.value.metadata.labels["topology.kubernetes.io/zone"] | gsub("[^A-Za-z0-9_.-]"; "-")) + "-" + ((.key + 1) | tostring)
       else
         "    " + $label + ": domain-" + (.value.metadata.labels["topology.kubernetes.io/zone"] | gsub("[^A-Za-z0-9_.-]"; "-"))
       end)] | join("\n")) + "\n"'
}

apply_rack_source() {
  snapshot_yaml rack-1 topology.gpustack.ai/rack rack > "${TMP_CASE87}/rack.yaml"
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-rack" --from-file=snapshot.yaml="${TMP_CASE87}/rack.yaml" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${PREFIX}-native
spec:
  nodeSelector:
    matchLabels:
      topology.gpustack.ai/e2e-rack: enabled
  levels:
  - topology.kubernetes.io/region
  - topology.kubernetes.io/zone
  - topology.gpustack.ai/rack
  configMap:
    configMapRef:
      namespace: ${SYSTEM_NS}
      name: ${PREFIX}-rack
    key: snapshot.yaml
    maxStaleness: 30s
YAML
}

apply_webhook_server() {
  local host="${PREFIX}-webhook.${SYSTEM_NS}.svc"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${host}" \
    -addext "subjectAltName=DNS:${host}" -keyout "${TMP_CASE87}/tls.key" \
    -out "${TMP_CASE87}/tls.crt" >/dev/null 2>&1
  snapshot_yaml webhook-1 topology.gpustack.ai/webhook-domain domain > "${TMP_CASE87}/snapshot.json"
  kubectl -n "$SYSTEM_NS" create secret tls "${PREFIX}-webhook-tls" \
    --cert="${TMP_CASE87}/tls.crt" --key="${TMP_CASE87}/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create secret generic "${PREFIX}-webhook-token" --from-literal=token=token-one \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-ca" --from-file=ca.crt="${TMP_CASE87}/tls.crt" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-snapshot" --from-file=snapshot.json="${TMP_CASE87}/snapshot.json" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: ${PREFIX}-webhook
  namespace: ${SYSTEM_NS}
spec:
  selector:
    app.kubernetes.io/name: ${PREFIX}-webhook
  ports:
  - port: 443
    targetPort: 8443
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${PREFIX}-webhook
  namespace: ${SYSTEM_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${PREFIX}-webhook
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${PREFIX}-webhook
    spec:
      containers:
      - name: server
        image: python:3.13-alpine
        command: [python, -c]
        args:
        - |
          import http.server, ssl
          class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
              token = open('/token/token').read().strip()
              if self.headers.get('Authorization') != 'Bearer ' + token:
                self.send_response(401); self.end_headers(); return
              body = open('/snapshot/snapshot.json', 'rb').read()
              self.send_response(200); self.send_header('Content-Type', 'application/json')
              self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
            def log_message(self, *args): pass
          server = http.server.HTTPServer(('0.0.0.0', 8443), Handler)
          context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
          context.load_cert_chain('/tls/tls.crt', '/tls/tls.key')
          server.socket = context.wrap_socket(server.socket, server_side=True)
          server.serve_forever()
        ports:
        - containerPort: 8443
        readinessProbe:
          tcpSocket: {port: 8443}
        volumeMounts:
        - {name: tls, mountPath: /tls, readOnly: true}
        - {name: token, mountPath: /token, readOnly: true}
        - {name: snapshot, mountPath: /snapshot, readOnly: true}
      volumes:
      - name: tls
        secret: {secretName: ${PREFIX}-webhook-tls}
      - name: token
        secret: {secretName: ${PREFIX}-webhook-token}
      - name: snapshot
        configMap: {name: ${PREFIX}-webhook-snapshot}
YAML
  kubectl -n "$SYSTEM_NS" rollout status deploy/"${PREFIX}-webhook" --timeout=180s >/dev/null
}

apply_webhook_source() {
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${PREFIX}-webhook
spec:
  nodeSelector:
    matchLabels:
      topology.gpustack.ai/e2e-rack: enabled
  levels:
  - topology.gpustack.ai/webhook-domain
  webhook:
    url: https://${PREFIX}-webhook.${SYSTEM_NS}.svc/snapshot
    pollInterval: 3s
    timeout: 2s
    maxStaleness: 12s
    caBundleConfigMapRef: {namespace: ${SYSTEM_NS}, name: ${PREFIX}-webhook-ca}
    bearerTokenSecretRef: {namespace: ${SYSTEM_NS}, name: ${PREFIX}-webhook-token}
YAML
}

TMP_CASE87="$(mktemp -d "${TMPDIR:-/tmp}/gpustack-case87.XXXXXX")"
trap 'cleanup; rm -rf "$TMP_CASE87"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- the zone shape the TAS proofs need ----------------------------------------------------------

tas_zone_shape

# The Nodes this case labels and places on are the ones tas_zone_shape counts: Linux, zoned,
# schedulable, and carrying no NoSchedule or NoExecute taint. Selecting on unschedulable alone let a
# tainted control-plane with no zone label in, and the writing sources then fed its null zone to jq
# -- the case died there and then waited out every timeout it had.
TAS_NODE_JQ='select(.metadata.labels["kubernetes.io/os"] == "linux")
  | select((.metadata.labels["topology.kubernetes.io/zone"] // "") != "")
  | select(.spec.unschedulable != true)
  | select([(.spec.taints // [])[] | select(.effect == "NoSchedule" or .effect == "NoExecute")] | length == 0)'
CPU_NODES="$(kubectl get nodes -o json | jq -r ".items[] | ${TAS_NODE_JQ} | .metadata.name")"
while read -r node; do
  [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-rack=enabled --overwrite >/dev/null
done <<EOF
$CPU_NODES
EOF
FOREIGN_NODE="$(printf '%s\n' "$CPU_NODES" | sed -n '1p')"
[ -n "$FOREIGN_NODE" ] && kubectl label node "$FOREIGN_NODE" "${FOREIGN_LABEL}=kept" --overwrite >/dev/null
# The cloud's own region/zone on every Node a writing source selects, read before any source runs:
# retiring a source must leave exactly these values behind.
NATIVE_PAIRS_BEFORE="$(native_region_zone_pairs)"

# Leave one real Node outside the native source. It remains hostname-only while the other three
# retain their cloud region/zone labels, making exclusion from an explicit zone/rack request an
# observable capacity fact rather than an inferred one.
NATIVE_NODES="$(kubectl get nodes -o json | jq -r "
  [.items
   | map(${TAS_NODE_JQ})
   | group_by(.metadata.labels[\"topology.kubernetes.io/zone\"])[]] as \$zones |
  (\$zones[0][] | .metadata.name), (\$zones[1][0] | .metadata.name)" | sort -u)"
while read -r node; do
  [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-native=enabled --overwrite >/dev/null
done <<EOF
$NATIVE_NODES
EOF

# --- source-to-profile proofs -------------------------------------------------------------------

apply_native_source
if wait_source "${PREFIX}-native" 'True|Observed' \
  && wait_profile_levels 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname '; then
  ZONE_PROFILE_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-native=enabled --no-headers | wc -l | tr -d ' ')"
  HOST_ONLY_NODES="$(kubectl get nodes -l '!topology.gpustack.ai/e2e-native' --no-headers | wc -l | tr -d ' ')"
  record PASS "native EKS region and zone labels form the active topology profile" \
    "${SOURCE_TOPOLOGY}: ${ZONE_PROFILE_NODES} region/zone Nodes; ${HOST_ONLY_NODES} Node(s) outside the selector"
else
  record FAIL "native EKS region and zone labels form the active topology profile" \
    "source=$(source_condition "${PREFIX}-native" Ready), topology=${SOURCE_TOPOLOGY:-missing}"
fi

NATIVE_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-native" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
NATIVE_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$NATIVE_SOURCE_UID")"
if [ -n "$NATIVE_SOURCE_UID" ] && [ "$NATIVE_FEATURES" = 0 ]; then
  record PASS "the read-only nodeLabels source owns no NodeFeature" "${PREFIX}-native owns 0 NodeFeatures"
else
  record FAIL "the read-only nodeLabels source owns no NodeFeature" \
    "uid=${NATIVE_SOURCE_UID:-missing}, owned NodeFeatures=${NATIVE_FEATURES:-unreadable}"
fi

# The schedulable Node left outside the selector must read hostname-only, not merely be counted.
EXCLUDED_ROWS=""
EXCLUDED_OK=no
while read -r node; do
  [ -n "$node" ] || continue
  [ "$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/e2e-native}' 2>/dev/null)" = enabled ] && continue
  profile="$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
  levels="$(kubectl get topology.kueue.x-k8s.io "gpustack-${profile}" -o jsonpath='{range .spec.levels[*]}{.nodeLabel}{" "}{end}' 2>/dev/null)"
  EXCLUDED_ROWS="${EXCLUDED_ROWS}${node}=${levels:-missing};"
  if [ "$levels" = 'kubernetes.io/hostname ' ]; then EXCLUDED_OK=yes; else EXCLUDED_OK=bad; break; fi
done <<EOF
$CPU_NODES
EOF
if [ "$EXCLUDED_OK" = yes ]; then
  record PASS "the schedulable Node outside the native selector is hostname-only" "$EXCLUDED_ROWS"
else
  record FAIL "the schedulable Node outside the native selector is hostname-only" "${EXCLUDED_ROWS:-no excluded schedulable Node}"
fi

if [ -z "$IT" ]; then
  IT="$(tas_md_usable_instance_type)"
fi
CQ="$(tas_md_cq_of_it "$NS" "$IT")"
STABLE_CQ_UID=""
if [ -n "$CQ" ] && wait_cq_topology_plan "$CQ" "$SOURCE_TOPOLOGY" contains; then
  STABLE_CQ_UID="$CQ_PLAN_UID"
  record PASS "the operator queue consumes the native topology profile" \
    "${CQ} uid=${STABLE_CQ_UID}, topologies=$(cq_plan_topologies_inline)"
else
  record FAIL "the operator queue consumes the native topology profile" \
    "instanceType=${IT:-missing}, queue=${CQ:-missing}, uid=${CQ_PLAN_UID:-missing}, topologies=$(cq_plan_topologies_inline)"
fi

# Hold a real reservation on the old plan before changing the source. The migration readings below
# come from the ClusterQueue and Workload themselves: they do not infer a drain from controller logs
# or from the desired profile.
NATIVE_TOPOLOGY="$SOURCE_TOPOLOGY"
OLD_FLAVORS="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json 2>/dev/null \
  | jq -r '(.spec.resourceGroups // [])[]?.flavors[]?.name' \
  | while read -r flavor; do
      [ "$(kubectl get resourceflavor "$flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null)" = "$NATIVE_TOPOLOGY" ] \
        && printf '%s\n' "$flavor"
    done | sort -u)"
OLD_FLAVORS_JSON="$(printf '%s\n' "$OLD_FLAVORS" | jq -Rsc 'split("\n") | map(select(length > 0))')"
kubectl get clusterqueue.kueue.x-k8s.io "$CQ" --watch --output-watch-events -o json \
  > "${TMP_CASE87}/cq-watch.json" 2>/dev/null &
CQ_WATCH_PID=$!
CQ_WATCH_READY=no
for _ in $(seq 1 20); do
  if [ -s "${TMP_CASE87}/cq-watch.json" ]; then CQ_WATCH_READY=yes; break; fi
  sleep 1
done
PRE_TRANSITION_STOP="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.spec.stopPolicy}' 2>/dev/null)"
tas_md_apply "${PREFIX}-transition" "$NS" "$BINDING" \
  "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 1 topology.kubernetes.io/zone)"
TRANSITION_WL=""
TRANSITION_RESERVED=no
if tas_md_wait_pods "$NS" "${PREFIX}-transition" 1 \
  && tas_md_wait_workloads "$NS" "${PREFIX}-transition" 1; then
  TRANSITION_WL="$(tas_md_workloads "$NS" "${PREFIX}-transition" | sed -n '1p')"
fi
# The reservation must be THIS queue's and sit on one of the old flavors: a Workload admitted
# elsewhere, or already on a newer flavor, would let the drain rows below pass without ever holding
# an old-plan reservation.
for _ in $(seq 1 60); do
  transition_cq="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.status.admission.clusterQueue}' 2>/dev/null)"
  transition_flavor="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o json 2>/dev/null \
    | jq -r '[.status.admission.podSetAssignments[0].flavors[]?][0] // empty')"
  if [ -n "$TRANSITION_WL" ] \
    && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved)" = True ] \
    && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted)" = True ] \
    && [ "$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.spec.podSets[0].topologyRequest.required}' 2>/dev/null)" = topology.kubernetes.io/zone ] \
    && [ "$transition_cq" = "$CQ" ] && [ -n "$transition_flavor" ] \
    && printf '%s\n' "$OLD_FLAVORS" | grep -Fxq "$transition_flavor"; then
    TRANSITION_RESERVED=yes
    break
  fi
  sleep 2
done
if [ "$TRANSITION_RESERVED" = yes ]; then
  record PASS "a Workload reserves the old topology before migration" \
    "${TRANSITION_WL} is admitted by ${transition_cq} through old flavor ${transition_flavor} on ${NATIVE_TOPOLOGY}; ClusterQueue reservingWorkloads=$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.reservingWorkloads}')"
else
  record FAIL "a Workload reserves the old topology before migration" \
    "workload=${TRANSITION_WL:-missing}, queue=${transition_cq:-missing} (want ${CQ}), flavor=${transition_flavor:-missing} (want one of $(printf '%s' "$OLD_FLAVORS" | paste -sd, -)), quotaReserved=$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved), admitted=$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted)"
fi

# Update the same source so the reserved queue transitions directly from region/zone to
# region/zone/rack, without an intervening hostname-only profile.
apply_rack_source
RACK_TOPOLOGY=""

# Observe the queue transition as a sequence. The watch starts before the source changes, so a
# fast drain cannot be missed by polling. A post-switch reservation may be Kueue's readmission
# on the new flavor; only observations before the first switch describe the old reservation.
TRANSITION_SWITCHED=no
for _ in $(seq 1 180); do
  if [ -z "$RACK_TOPOLOGY" ] && [ "$(source_condition "${PREFIX}-native" Ready)" = 'True|Observed' ]; then
    RACK_TOPOLOGY="$(kubectl get topology.kueue.x-k8s.io -o json 2>/dev/null | jq -r '
      [.items[]
       | select([.spec.levels[]?.nodeLabel] == ["topology.kubernetes.io/region", "topology.kubernetes.io/zone", "topology.gpustack.ai/rack", "kubernetes.io/hostname"])
       | .metadata.name] | sort | .[0] // ""')"
  fi
  stop="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.spec.stopPolicy}' 2>/dev/null)"
  uid_now="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  topologies_now="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json 2>/dev/null \
    | jq -r '(.spec.resourceGroups // [])[]?.flavors[]?.name' \
    | while read -r flavor; do
        kubectl get resourceflavor.kueue.x-k8s.io "$flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null
        printf '\n'
      done | sort -u | grep -v '^$' || true)"
  if [ -n "$RACK_TOPOLOGY" ] && [ "$uid_now" = "$STABLE_CQ_UID" ] \
    && [ "$stop" = "$PRE_TRANSITION_STOP" ] \
    && ! printf '%s\n' "$topologies_now" | grep -Fxq "$NATIVE_TOPOLOGY" \
    && printf '%s\n' "$topologies_now" | grep -Fxq "$RACK_TOPOLOGY"; then
    TRANSITION_SWITCHED=yes
    CQ_PLAN_UID="$uid_now"
    CQ_PLAN_TOPOLOGIES="$topologies_now"
    break
  fi
  sleep 1
done
kill "$CQ_WATCH_PID" 2>/dev/null || true
wait "$CQ_WATCH_PID" 2>/dev/null || true
CQ_WATCH_PID=""
jq -r --argjson olds "$OLD_FLAVORS_JSON" '
  select(.object != null) | .object as $cq |
  [$cq.metadata.uid,
   ($cq.spec.stopPolicy // "None"),
   (($cq.status.reservingWorkloads // 0) | tostring),
   ([$cq.spec.resourceGroups[]?.flavors[]?.name] as $present |
     ($olds | all(. as $old | $present | index($old) != null)) | tostring),
   (([$cq.status.conditions[]? | select(.type == "Active" and .status == "False" and
       .reason == "Stopped" and .observedGeneration >= $cq.metadata.generation)] | length > 0)
     | tostring)] | @tsv' "${TMP_CASE87}/cq-watch.json" > "${TMP_CASE87}/cq-watch.tsv"
read -r TRANSITION_HELD TRANSITION_HELD_RESERVED TRANSITION_DRAINED TRANSITION_DROPPED_OLD \
  <<<"$(tas_cq_transition_verdict "${TMP_CASE87}/cq-watch.tsv")"

RACK_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-native" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
RACK_FEATURES="$(kubectl -n "$SYSTEM_NS" get nodefeatures -l "topology.gpustack.ai/source-uid=${RACK_SOURCE_UID}" -o json 2>/dev/null)"
RACK_FEATURE_COUNT="$(printf '%s' "$RACK_FEATURES" | jq '[.items[]? | select(.spec.labels["topology.gpustack.ai/rack"] != null) | select(.spec.labels["topology.kubernetes.io/region"] == null and .spec.labels["topology.kubernetes.io/zone"] == null)] | length' 2>/dev/null)"
RACK_NODE_COUNT="$(kubectl get nodes -l topology.gpustack.ai/e2e-rack=enabled -o json 2>/dev/null | jq '.items | length')"
if [ "$(source_condition "${PREFIX}-native" Ready)" = 'True|Observed' ] \
  && [ -n "$RACK_TOPOLOGY" ] && [ "$RACK_NODE_COUNT" -gt 0 ] \
  && [ "$RACK_FEATURE_COUNT" = "$RACK_NODE_COUNT" ]; then
  record PASS "the ConfigMap snapshot publishes rack through NodeFeatures" \
    "${RACK_FEATURE_COUNT}/${RACK_NODE_COUNT} source-owned NodeFeatures have rack but no standard region/zone labels"
else
  record FAIL "the ConfigMap snapshot publishes rack through NodeFeatures" \
    "source=$(source_condition "${PREFIX}-native" Ready), topology=${RACK_TOPOLOGY:-missing}, features=${RACK_FEATURE_COUNT:-missing}/${RACK_NODE_COUNT:-missing}"
fi

# The NodeFeature is the source's half; the Node label is NFD's. Each Node must carry the exact rack
# value its snapshot entry names, read off the Node itself.
RACK_EXPECTED="$(tas_snapshot_pairs "${TMP_CASE87}/rack.yaml" topology.gpustack.ai/rack)"
RACK_ACTUAL=""
for _ in $(seq 1 60); do
  RACK_ACTUAL="$(tas_node_label_pairs topology.gpustack.ai/e2e-rack=enabled topology.gpustack.ai/rack)"
  [ -n "$RACK_EXPECTED" ] && [ "$RACK_ACTUAL" = "$RACK_EXPECTED" ] && break
  sleep 2
done
if [ -n "$RACK_EXPECTED" ] && [ "$RACK_ACTUAL" = "$RACK_EXPECTED" ]; then
  record PASS "NFD projects every snapshot rack value onto its Node" \
    "$(printf '%s' "$RACK_ACTUAL" | paste -sd, -)"
else
  record FAIL "NFD projects every snapshot rack value onto its Node" \
    "snapshot=$(printf '%s' "$RACK_EXPECTED" | paste -sd, -); nodes=$(printf '%s' "$RACK_ACTUAL" | paste -sd, -)"
fi

if [ "$CQ_WATCH_READY" = yes ] && [ -n "$OLD_FLAVORS" ] && [ "$TRANSITION_RESERVED" = yes ] && [ "$TRANSITION_HELD" = yes ] \
  && [ "$TRANSITION_HELD_RESERVED" = yes ] && [ "$TRANSITION_DRAINED" = yes ] && [ "$TRANSITION_DROPPED_OLD" = no ] \
  && [ "$TRANSITION_SWITCHED" = yes ]; then
  record PASS "a reserved profile transition drains before switching the same ClusterQueue" \
    "${CQ} kept uid=${CQ_PLAN_UID}; HoldAndDrain observed with the old flavors and a counted reservation, then reservingWorkloads=0 before any old flavor left; stopPolicy restored to ${PRE_TRANSITION_STOP}"
else
  record FAIL "a reserved profile transition drains before switching the same ClusterQueue" \
    "watch=${CQ_WATCH_READY}, oldFlavors=${OLD_FLAVORS:-missing}, reserved=${TRANSITION_RESERVED}, held=${TRANSITION_HELD}, heldWhileReserved=${TRANSITION_HELD_RESERVED}, drained=${TRANSITION_DRAINED}, droppedOld=${TRANSITION_DROPPED_OLD}, switched=${TRANSITION_SWITCHED}, beforeUID=${STABLE_CQ_UID:-missing}, afterUID=${uid_now:-missing}"
fi

TRANSITION_READMITTED=no
for _ in $(seq 1 90); do
  TRANSITION_WL="$(tas_md_workloads "$NS" "${PREFIX}-transition" | sed -n '1p')"
  transition_flavor="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o json 2>/dev/null \
    | jq -r '.status.admission.podSetAssignments[0].flavors.cpu // empty')"
  transition_topology="$(kubectl get resourceflavor "$transition_flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null)"
  if [ -n "$TRANSITION_WL" ] \
    && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved)" = True ] \
    && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted)" = True ] \
    && [ "$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.spec.podSets[0].topologyRequest.required}' 2>/dev/null)" = topology.kubernetes.io/zone ] \
    && [ "$transition_topology" = "$RACK_TOPOLOGY" ]; then
    TRANSITION_READMITTED=yes
    break
  fi
  sleep 2
done
if [ "$TRANSITION_READMITTED" = yes ]; then
  record PASS "Kueue readmits the drained Workload on the replacement topology" \
    "${TRANSITION_WL} is admitted through ${transition_flavor} on ${transition_topology}"
else
  record FAIL "Kueue readmits the drained Workload on the replacement topology" \
    "workload=${TRANSITION_WL:-missing}, reserved=$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved), admitted=$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted), flavor=${transition_flavor:-missing}, topology=${transition_topology:-missing}"
fi
tas_md_force_release "$NS" "${PREFIX}-transition"
kubectl -n "$NS" delete modeldeployment "${PREFIX}-transition" --wait=false >/dev/null 2>&1
for _ in $(seq 1 60); do
  [ "$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)" = 0 ] && break
  sleep 2
done

OLD_PLAN_RETIRED=no
for _ in $(seq 1 90); do
  remaining_old=0
  while read -r old_flavor; do
    [ -z "$old_flavor" ] && continue
    kubectl get resourceflavor.kueue.x-k8s.io "$old_flavor" >/dev/null 2>&1 \
      && remaining_old=$((remaining_old + 1))
  done <<EOF
$OLD_FLAVORS
EOF
  if [ "$remaining_old" = 0 ] \
    && ! kubectl get topology.kueue.x-k8s.io "$NATIVE_TOPOLOGY" >/dev/null 2>&1; then
    OLD_PLAN_RETIRED=yes
    break
  fi
  sleep 2
done
if [ "$OLD_PLAN_RETIRED" = yes ]; then
  record PASS "the unreferenced old flavors and Topology retire after the switch" \
    "all old flavors and ${NATIVE_TOPOLOGY} are absent after references cleared"
else
  record FAIL "the unreferenced old flavors and Topology retire after the switch" \
    "remainingFlavors=${remaining_old:-unknown}, topologyPresent=$(kubectl get topology "$NATIVE_TOPOLOGY" >/dev/null 2>&1 && echo yes || echo no)"
fi

if [ -n "${RACK_TOPOLOGY:-}" ]; then
  kubectl delete topology.kueue.x-k8s.io "$RACK_TOPOLOGY" --wait=false >/dev/null 2>&1
  RACK_DELETING=""
  for _ in $(seq 1 30); do
    RACK_DELETING="$(kubectl get topology.kueue.x-k8s.io "$RACK_TOPOLOGY" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)"
    [ -n "$RACK_DELETING" ] && break
    sleep 2
  done
  if [ -n "$RACK_DELETING" ]; then
    record PASS "a live generated Topology is protected while ResourceFlavors use it" \
      "$RACK_TOPOLOGY is terminating behind Kueue's resource-in-use finalizer"
  else
    record FAIL "a live generated Topology is protected while ResourceFlavors use it" \
      "$RACK_TOPOLOGY disappeared or never entered terminating state"
  fi
fi
delete_source "${PREFIX}-native"
if [ -n "${RACK_TOPOLOGY:-}" ]; then
  RACK_RELEASED=no
  for _ in $(seq 1 60); do
    if ! kubectl get topology.kueue.x-k8s.io "$RACK_TOPOLOGY" >/dev/null 2>&1; then
      RACK_RELEASED=yes
      break
    fi
    sleep 2
  done
  if [ "$RACK_RELEASED" = yes ]; then
    record PASS "the protected Topology is released after its source and flavors retire" \
      "$RACK_TOPOLOGY deleted after the rack profile stopped being live"
  else
    record FAIL "the protected Topology is released after its source and flavors retire" \
      "$RACK_TOPOLOGY is still present after the rack source was deleted"
  fi
fi

# Retiring the writing source must remove exactly what it published: its NodeFeatures, and through
# NFD the private rack labels. The cloud's region/zone and a label no source owns stay as they were.
RETIRED_OK=no
for _ in $(seq 1 60); do
  RETIRED_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$RACK_SOURCE_UID")"
  RETIRED_LEFT="$(tas_node_label_pairs topology.gpustack.ai/e2e-rack=enabled topology.gpustack.ai/rack \
    | awk 'NF > 1' | grep -c . || true)"
  NATIVE_PAIRS_AFTER="$(native_region_zone_pairs)"
  FOREIGN_AFTER="$(kubectl get node "$FOREIGN_NODE" -o json 2>/dev/null \
    | jq -r --arg key "$FOREIGN_LABEL" '.metadata.labels[$key] // ""')"
  if [ -n "$RACK_SOURCE_UID" ] && [ "$RETIRED_FEATURES" = 0 ] && [ "$RETIRED_LEFT" = 0 ] \
    && [ -n "$NATIVE_PAIRS_BEFORE" ] && [ "$NATIVE_PAIRS_AFTER" = "$NATIVE_PAIRS_BEFORE" ] \
    && [ "$FOREIGN_AFTER" = kept ]; then
    RETIRED_OK=yes
    break
  fi
  sleep 2
done
if [ "$RETIRED_OK" = yes ]; then
  record PASS "deleting the snapshot source removes only what it published" \
    "0 NodeFeatures and 0 rack labels remain; region/zone unchanged on every Node; ${FOREIGN_LABEL} kept on ${FOREIGN_NODE}"
else
  record FAIL "deleting the snapshot source removes only what it published" \
    "uid=${RACK_SOURCE_UID:-missing}, NodeFeatures=${RETIRED_FEATURES:-unreadable}, rack labels=${RETIRED_LEFT:-unknown}, region/zone before=$(printf '%s' "$NATIVE_PAIRS_BEFORE" | paste -sd, -) after=$(printf '%s' "$NATIVE_PAIRS_AFTER" | paste -sd, -), foreign=${FOREIGN_AFTER:-absent}"
fi

apply_webhook_server
apply_webhook_source
if wait_source "${PREFIX}-webhook" 'True|Observed' \
  && wait_profile_levels 'topology.gpustack.ai/webhook-domain kubernetes.io/hostname '; then
  record PASS "the authenticated HTTPS webhook applies its topology snapshot" \
    "revision=$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}')"
else
  record FAIL "the authenticated HTTPS webhook applies its topology snapshot" \
    "source=$(source_condition "${PREFIX}-webhook" Ready)"
fi

WEBHOOK_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
WEBHOOK_EXPECTED="$(tas_snapshot_pairs "${TMP_CASE87}/snapshot.json" topology.gpustack.ai/webhook-domain)"
WEBHOOK_EXPECTED_COUNT="$(printf '%s\n' "$WEBHOOK_EXPECTED" | grep -c . || true)"
WEBHOOK_ACTUAL=""
for _ in $(seq 1 60); do
  WEBHOOK_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
  WEBHOOK_ACTUAL="$(tas_node_label_pairs topology.gpustack.ai/e2e-rack=enabled topology.gpustack.ai/webhook-domain)"
  [ "$WEBHOOK_EXPECTED_COUNT" -gt 0 ] && [ "$WEBHOOK_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] \
    && [ "$WEBHOOK_ACTUAL" = "$WEBHOOK_EXPECTED" ] && break
  sleep 2
done
if [ "$WEBHOOK_EXPECTED_COUNT" -gt 0 ] && [ "$WEBHOOK_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] \
  && [ "$WEBHOOK_ACTUAL" = "$WEBHOOK_EXPECTED" ]; then
  record PASS "the webhook snapshot reaches every Node through its own NodeFeatures" \
    "${WEBHOOK_FEATURES} NodeFeatures; $(printf '%s' "$WEBHOOK_ACTUAL" | paste -sd, -)"
else
  record FAIL "the webhook snapshot reaches every Node through its own NodeFeatures" \
    "NodeFeatures=${WEBHOOK_FEATURES:-unreadable}/${WEBHOOK_EXPECTED_COUNT}; snapshot=$(printf '%s' "$WEBHOOK_EXPECTED" | paste -sd, -); nodes=$(printf '%s' "$WEBHOOK_ACTUAL" | paste -sd, -)"
fi

# Between the first failed poll and maxStaleness the source is Stale and keeps what it published.
# The window is the source's maxStaleness less one poll interval, several times this loop's period.
kubectl -n "$SYSTEM_NS" scale deploy/"${PREFIX}-webhook" --replicas=0 >/dev/null
if wait_source "${PREFIX}-webhook" 'False|Stale'; then
  STALE_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
  STALE_LABELS="$(tas_node_label_pairs topology.gpustack.ai/e2e-rack=enabled topology.gpustack.ai/webhook-domain \
    | awk 'NF > 1' | grep -c . || true)"
  if [ "$STALE_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] && [ "$STALE_LABELS" = "$WEBHOOK_EXPECTED_COUNT" ]; then
    record PASS "an unavailable webhook is Stale and retains its last snapshot" \
      "Ready=False/Stale; ${STALE_FEATURES} NodeFeatures and ${STALE_LABELS} webhook-domain labels retained"
  else
    record FAIL "an unavailable webhook is Stale and retains its last snapshot" \
      "Ready=False/Stale but NodeFeatures=${STALE_FEATURES:-unreadable}, labels=${STALE_LABELS} of ${WEBHOOK_EXPECTED_COUNT}"
  fi
else
  record FAIL "an unavailable webhook is Stale and retains its last snapshot" \
    "never read Ready=False/Stale; source=$(source_condition "${PREFIX}-webhook" Ready)"
fi
if wait_source "${PREFIX}-webhook" 'False|Expired'; then
  remaining="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.gpustack\.ai/webhook-domain}{"\n"}{end}' | grep -c . || true)"
  expired_features="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
  if [ "$remaining" = 0 ] && [ "$expired_features" = 0 ]; then
    record PASS "an unavailable webhook expires and removes only its owned labels" "Ready=False/Expired; zero webhook-domain labels and zero NodeFeatures remain"
  else
    record FAIL "an unavailable webhook expires and removes only its owned labels" \
      "${remaining} owned labels and ${expired_features:-unreadable} NodeFeatures remain after Expired"
  fi
else
  record FAIL "an unavailable webhook expires and removes only its owned labels" "source=$(source_condition "${PREFIX}-webhook" Ready)"
fi

kubectl -n "$SYSTEM_NS" create secret generic "${PREFIX}-webhook-token" --from-literal=token=token-two \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
snapshot_yaml webhook-2 topology.gpustack.ai/webhook-domain domain > "${TMP_CASE87}/snapshot.json"
kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-snapshot" --from-file=snapshot.json="${TMP_CASE87}/snapshot.json" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$SYSTEM_NS" scale deploy/"${PREFIX}-webhook" --replicas=1 >/dev/null
kubectl -n "$SYSTEM_NS" rollout status deploy/"${PREFIX}-webhook" --timeout=180s >/dev/null
for _ in $(seq 1 60); do
  [ "$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}' 2>/dev/null)" = webhook-2 ] && break
  sleep 2
done
if [ "$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}' 2>/dev/null)" = webhook-2 ] \
  && [ "$(source_condition "${PREFIX}-webhook" Ready)" = 'True|Observed' ]; then
  record PASS "webhook credential rotation recovers with a new revision" "token rotated; revision webhook-2 observed"
else
  record FAIL "webhook credential rotation recovers with a new revision" \
    "source=$(source_condition "${PREFIX}-webhook" Ready), revision=$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}' 2>/dev/null)"
fi
delete_source "${PREFIX}-webhook"

# The first native-source pass proves selector exclusion. Placement uses a fresh, uniform profile
# so all four schedulable CPU Nodes share one zone-capable profile. An unrelated cordoned GPU Node
# may retain a hostname-only flavor in the same queue; it cannot satisfy a zone-required PodSet.
while read -r node; do
  [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-native=enabled --overwrite >/dev/null
done <<EOF
$CPU_NODES
EOF
apply_native_source
if ! wait_source "${PREFIX}-native" 'True|Observed' \
  || ! wait_profile_levels 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname '; then
  record FAIL "native topology is restored for placement tests" "source=$(source_condition "${PREFIX}-native" Ready)"
else
  record PASS "native topology is restored for placement tests" "$SOURCE_TOPOLOGY"
fi

# Ready records that the source snapshot was accepted. Node and NodeFlavor reconciliation are
# separate eventually-consistent steps, so do not inspect the scheduling chain until
# the selected Nodes actually carry that snapshot's profile and at least one matching flavor
# exists. Otherwise a fast API can produce a fresh ClusterQueue from the flavor being retired.
PLACEMENT_PROFILE="${SOURCE_TOPOLOGY#gpustack-}"
PROFILE_CONVERGED=no
for _ in $(seq 1 90); do
  SELECTED_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-native=enabled -o json 2>/dev/null \
    | jq '[.items[]?] | length')"
  PROFILE_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-native=enabled -o json 2>/dev/null \
    | jq --arg profile "$PLACEMENT_PROFILE" '[.items[]? | select(.metadata.labels["topology.gpustack.ai/profile"] == $profile)] | length')"
  PROFILE_FLAVORS="$(kubectl get resourceflavor.kueue.x-k8s.io -o json 2>/dev/null \
    | jq --arg topology "$SOURCE_TOPOLOGY" '[.items[]? | select(.spec.topologyName == $topology)] | length')"
  if [ "$SELECTED_NODES" -gt 0 ] && [ "$PROFILE_NODES" = "$SELECTED_NODES" ] \
    && [ "$PROFILE_FLAVORS" -gt 0 ]; then
    PROFILE_CONVERGED=yes
    break
  fi
  sleep 2
done
if [ "$PROFILE_CONVERGED" = yes ]; then
  record PASS "the restored profile converges before queue migration" \
    "${PROFILE_NODES}/${SELECTED_NODES} selected Nodes and ${PROFILE_FLAVORS} ResourceFlavor(s) use ${SOURCE_TOPOLOGY}"
else
  record FAIL "the restored profile converges before queue migration" \
    "selected=${SELECTED_NODES:-0}, profiled=${PROFILE_NODES:-0}, flavors=${PROFILE_FLAVORS:-0}, topology=${SOURCE_TOPOLOGY:-missing}"
fi

# Every topology profile exercised above intentionally differs. ResourceFlavors remain immutable,
# but NodeQueue migrates the complete flavor plan in place after draining reservations. Prove the
# restored profile reaches the same ClusterQueue without deleting the InstanceType or LocalQueue.
if [ -z "$IT" ]; then
  IT="$(tas_md_usable_instance_type)"
fi
CQ="$(tas_md_cq_of_it "$NS" "$IT")"
if [ -n "$CQ" ] && wait_cq_topology_plan "$CQ" "$SOURCE_TOPOLOGY" contains "$RACK_TOPOLOGY" \
  && [ "$CQ_PLAN_UID" = "$STABLE_CQ_UID" ]; then
  record PASS "the restored profile converges on the identity-stable ClusterQueue" \
    "InstanceType ${IT} kept ClusterQueue ${CQ} uid=${CQ_PLAN_UID} against ${SOURCE_TOPOLOGY}"
else
  record FAIL "the restored profile converges on the identity-stable ClusterQueue" \
    "instanceType=${IT:-missing}, queue=${CQ:-missing}, beforeUID=${STABLE_CQ_UID:-missing}, afterUID=${CQ_PLAN_UID:-missing}, topologies=$(cq_plan_topologies_inline)"
fi

TAS_OK=yes
if ! kubectl get crd topologies.kueue.x-k8s.io >/dev/null 2>&1; then
  record SKIP "the cluster serves Kueue's Topology API" \
    "no topologies.kueue.x-k8s.io CRD: the deployed Kueue does not serve TAS, so both placement proofs would measure nothing"
  TAS_OK=no
elif [ "$TAS_NODES" -lt 3 ]; then
  record SKIP "the zone shape can carry both placement proofs" \
    "${TAS_NODES} schedulable Linux Node(s) carry a zone label (${TAS_ZONES_SUMMARY:-none}); both proofs need at least three"
  TAS_OK=no
elif [ "$TAS_MAX_ZONE_NODES" -ge 3 ]; then
  record SKIP "the zone shape can carry both placement proofs" \
    "one zone holds ${TAS_MAX_ZONE_NODES} Nodes (${TAS_ZONES_SUMMARY}); a three-Pod group fits there, so the refusal has nothing to refuse"
  TAS_OK=no
elif [ "$TAS_MIN_CPU_M" -le 0 ] || [ "$TAS_MIN_MEM_MI" -le 0 ]; then
  record SKIP "the zone shape can carry both placement proofs" \
    "could not read every counted Node's allocatable (cpu ${TAS_MIN_CPU_M}m, memory ${TAS_MIN_MEM_MI}Mi)"
  TAS_OK=no
else
  record PASS "the zone shape can carry both placement proofs" \
    "${TAS_NODES} schedulable Linux Nodes in zones ${TAS_ZONES_SUMMARY}(largest zone ${TAS_MAX_ZONE_NODES}); a Pod sized ${TAS_POD_SHARE_PERCENT}% of the smallest Node never shares one with a sibling"
fi

# --- proof 1 and proof 2, on the hand-built fixtures ---------------------------------------------

if [ "$TAS_OK" = yes ]; then
  POD_CPU_M="$(tas_pod_cpu_m)"
  POD_MEM_MI="$(tas_pod_mem_mi)"
  QUOTA_CPU_M=$((3 * POD_CPU_M))
  QUOTA_MEM_MI=$((3 * POD_MEM_MI))
  tas_apply_fixtures "$PREFIX" "$NS" "${QUOTA_CPU_M}m" "${QUOTA_MEM_MI}Mi"

  # The queue must be Active before any workload in it can be decided; a queue stuck inactive is a
  # fixture failure, and every row below would time out behind it for the wrong reason.
  CQ_ACTIVE=no
  for _ in $(seq 1 30); do
    [ "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" \
      -o jsonpath='{range .status.conditions[?(@.type=="Active")]}{.status}{end}' 2>/dev/null)" = True ] \
      && { CQ_ACTIVE=yes; break; }
    sleep 2
  done
  if [ "$CQ_ACTIVE" = yes ]; then
    record PASS "the hand-built ClusterQueue turns Active" \
      "${PREFIX}-cq: one TAS flavor, cpu ${QUOTA_CPU_M}m / memory ${QUOTA_MEM_MI}Mi nominal, no cohort, no AdmissionCheck"
  else
    record FAIL "the hand-built ClusterQueue turns Active" \
      "not Active within 60s; apply said: ${TAS_APPLY_OUT:0:240}"
    TAS_OK=no
  fi
fi

if [ "$TAS_OK" = yes ]; then
  # --- proof 1: the fitting two-Pod group ---
  tas_apply_zone_job "${PREFIX}-fit" "$NS" "${PREFIX}-lq" 2 "$POD_CPU_M" "$POD_MEM_MI"

  FIT_WL=""
  FIT_ADMITTED=no
  for _ in $(seq 1 45); do
    FIT_WL="$(tas_job_workload "$NS" "${PREFIX}-fit")"
    [ -n "$FIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$FIT_WL")" ] \
      && { FIT_ADMITTED=yes; break; }
    sleep 2
  done
  if [ "$FIT_ADMITTED" = yes ]; then
    record PASS "the fitting group is admitted into the hand-built TAS queue" \
      "${PREFIX}-fit: two Pods of ${POD_CPU_M}m/${POD_MEM_MI}Mi each, zone-required"
  else
    record FAIL "the fitting group is admitted into the hand-built TAS queue" \
      "no admission within 90s; apply said: ${TAS_APPLY_OUT:0:240}; conditions: $(kubectl -n "$NS" get workload "$FIT_WL" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>/dev/null)"
  fi

  FIT_LEVELS="$(tas_wl_topology_levels "$NS" "$FIT_WL")"
  if [ -n "$FIT_LEVELS" ]; then
    record PASS "the admission carries a topology assignment" \
      "topologyAssignment levels: ${FIT_LEVELS} - written only for a PodSet Kueue placed through a topology"
  else
    record FAIL "the admission carries a topology assignment" \
      "admitted without any topologyAssignment: this is plain quota admission, not TAS placement"
  fi

  # Both Pods bound, and both on Nodes whose OBSERVED zone labels agree. The zone is read from the
  # Nodes the Pods actually landed on, never from the request: placement is proven where it bound.
  FIT_ROWS=""
  FIT_BOUND=no
  for _ in $(seq 1 45); do
    FIT_ROWS="$(tas_pod_nodes "$NS" "batch.kubernetes.io/job-name=${PREFIX}-fit")"
    [ "$(printf '%s\n' "$FIT_ROWS" | grep -c . || true)" = 2 ] \
      && [ -z "$(printf '%s\n' "$FIT_ROWS" | grep ' $' || true)" ] \
      && { FIT_BOUND=yes; break; }
    sleep 2
  done

  if [ "$FIT_BOUND" != yes ]; then
    record FAIL "both Pods of the zone-required group bind inside ONE zone" \
      "not both bound within 90s: $(printf '[%s]' "$(printf '%s' "$FIT_ROWS" | tr '\n' ';')")"
  else
    ZONES_SEEN=""
    ZONE_READ_FAIL=no
    while read -r _pod node; do
      z="$(tas_node_zone "$node")"
      [ -n "$z" ] || { ZONE_READ_FAIL=yes; break; }
      ZONES_SEEN="${ZONES_SEEN}${z} "
    done <<EOF
$FIT_ROWS
EOF
    if [ "$ZONE_READ_FAIL" = yes ]; then
      record FAIL "both Pods of the zone-required group bind inside ONE zone" \
        "a bound Node carries no zone label: $(printf '%s' "$FIT_ROWS" | tr '\n' ';')"
    elif [ "$(printf '%s' "$ZONES_SEEN" | xargs -n1 | sort -u | grep -c . || true)" = 1 ]; then
      record PASS "both Pods of the zone-required group bind inside ONE zone" \
        "Nodes $(printf '%s' "$FIT_ROWS" | cut -d' ' -f2 | tr '\n' ',') both in zone $(printf '%s' "$ZONES_SEEN")- the same zone the request required"
    else
      record FAIL "both Pods of the zone-required group bind inside ONE zone" \
        "zones observed: ${ZONES_SEEN}- the group was split across zones"
    fi
  fi

  # --- proof 2: the refused three-Pod group ---
  #
  # THE REFUSAL IS ONLY A PROOF AGAINST PROVEN-SUFFICIENT AGGREGATE. A group left Pending by a
  # short quota or a thin cluster is a quota verdict, not a topology verdict, so the quota the case
  # itself declared and the Nodes the case itself counted are re-read from the cluster and asserted
  # in this row -- the Pending rows below inherit their meaning from it.
  QUOTA_BACK_CPU="$(tas_millis "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" \
    -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="cpu")].nominalQuota}' 2>/dev/null)")"
  QUOTA_BACK_MEM="$(tas_mib "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" \
    -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="memory")].nominalQuota}' 2>/dev/null)")"
  NEED_CPU=$((3 * POD_CPU_M))
  NEED_MEM=$((3 * POD_MEM_MI))
  if [ "$QUOTA_BACK_CPU" -ge "$NEED_CPU" ] && [ "$QUOTA_BACK_MEM" -ge "$NEED_MEM" ] \
    && [ "$TAS_NODES" -ge 3 ] && [ "$TAS_MAX_ZONE_NODES" -le 2 ]; then
    record PASS "the refused group fits in aggregate: quota and cluster, only no single zone" \
      "quota ${QUOTA_BACK_CPU}m/${QUOTA_BACK_MEM}Mi covers ${NEED_CPU}m/${NEED_MEM}Mi, ${TAS_NODES} Nodes cover three Pods at one per Node, largest zone ${TAS_MAX_ZONE_NODES} holds two"
  else
    record FAIL "the refused group fits in aggregate: quota and cluster, only no single zone" \
      "quota read back ${QUOTA_BACK_CPU}m/${QUOTA_BACK_MEM}Mi against need ${NEED_CPU}m/${NEED_MEM}Mi, ${TAS_NODES} Nodes, largest zone ${TAS_MAX_ZONE_NODES} - the Pending rows below would be a quota verdict, not a topology one"
  fi

  tas_apply_zone_job "${PREFIX}-refuse" "$NS" "${PREFIX}-lq" 3 "$POD_CPU_M" "$POD_MEM_MI"

  REF_COMPOSED=no
  REF_WL=""
  for _ in $(seq 1 20); do
    REF_WL="$(tas_job_workload "$NS" "${PREFIX}-refuse")"
    [ -n "$REF_WL" ] && { REF_COMPOSED=yes; break; }
    sleep 2
  done
  if [ "$REF_COMPOSED" != yes ]; then
    record FAIL "the cross-zone group stays unadmitted with no reservation (two intervals)" \
      "Kueue composed no Workload for ${PREFIX}-refuse at all; apply said: ${TAS_APPLY_OUT:0:240}"
    record FAIL "no Pod of the cross-zone group binds a Node (two intervals)" \
      "no Workload was composed, so no Pod was ever ungated"
  else
    refuse_sample() {
      REF_READABLE=no
      tas_wl_readable "$NS" "$REF_WL" && REF_READABLE=yes
      REF_ADMITTED="$(tas_wl_admitted "$NS" "$REF_WL")"
      REF_RESERVED="$(tas_wl_condition "$NS" "$REF_WL" QuotaReserved)"
      REF_BOUND="$(kubectl -n "$NS" get pods -l "batch.kubernetes.io/job-name=${PREFIX}-refuse" \
        -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
    }

    refuse_sample
    REF_FIRST_ADMITTED="$REF_ADMITTED"
    REF_FIRST_RESERVED="$REF_RESERVED"
    REF_FIRST_BOUND="$REF_BOUND"
    REF_FIRST_READABLE="$REF_READABLE"
    sleep "$INTERVAL"
    refuse_sample

    if [ "$REF_FIRST_READABLE" = yes ] && [ "$REF_READABLE" = yes ] \
      && [ -z "$REF_FIRST_ADMITTED" ] && [ "$REF_FIRST_RESERVED" != True ] \
      && [ -z "$REF_ADMITTED" ] && [ "$REF_RESERVED" != True ]; then
      record PASS "the cross-zone group stays unadmitted with no reservation (two intervals)" \
        "two samples ${INTERVAL}s apart: no status.admission, QuotaReserved=${REF_RESERVED:-absent}"
    else
      record FAIL "the cross-zone group stays unadmitted with no reservation (two intervals)" \
        "readable=${REF_FIRST_READABLE}/${REF_READABLE}, admission: '${REF_ADMITTED:0:80}', QuotaReserved=${REF_RESERVED:-absent} - three Pods only aggregate capacity could fit were admitted anyway, or the Workload could not be read"
    fi

    if [ "$REF_FIRST_BOUND" = 0 ] && [ "$REF_BOUND" = 0 ]; then
      record PASS "no Pod of the cross-zone group binds a Node (two intervals)" \
        "two samples ${INTERVAL}s apart: 0 of 3 Pods bound"
    else
      record FAIL "no Pod of the cross-zone group binds a Node (two intervals)" \
        "${REF_BOUND} Pod(s) bound - an unadmitted group must keep every Pod gated"
    fi
  fi

  kubectl -n "$NS" delete job.batch "${PREFIX}-fit" "${PREFIX}-refuse" \
    --ignore-not-found --wait=false >/dev/null 2>&1
fi

# --- proof 3: ModelDeployment multi-role joint admission, through the operator's queue ------------

if [ -z "$IT" ]; then
  IT="$(tas_md_usable_instance_type)"
fi
if [ -z "$IT" ]; then
  echo "[case-87] no InstanceType in the cluster; run case-1 first" >&2
  exit_input_required
fi

CQ="$(tas_md_cq_of_it "$NS" "$IT")"
if [ -z "$CQ" ]; then
  echo "[case-87] InstanceType ${IT} names no reachable ClusterQueue in ${NS}; run case-1 first, and" >&2
  echo "          check that ${NS} carries the pool's entrance LocalQueue" >&2
  exit_input_required
fi

# --- ModelDeployment topology request through the operator-managed TAS queue --------------------

tas_md_apply "${PREFIX}-zone-fit" "$NS" "$BINDING" \
  "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 2 topology.kubernetes.io/zone)"
ZONE_FIT_WL=""
if tas_md_wait_pods "$NS" "${PREFIX}-zone-fit" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-fit" 1; then
  ZONE_FIT_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-fit" | sed -n '1p')"
fi
ZONE_FIT_ADMITTED=no
for _ in $(seq 1 60); do
  [ -n "$ZONE_FIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$ZONE_FIT_WL")" ] \
    && { ZONE_FIT_ADMITTED=yes; break; }
  sleep 2
done
if [ "$ZONE_FIT_ADMITTED" = yes ]; then
  required="$(kubectl -n "$NS" get workload "$ZONE_FIT_WL" \
    -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{" "}{end}' 2>/dev/null)"
  assignment="$(tas_wl_topology_levels "$NS" "$ZONE_FIT_WL")"
  if [ "$required" = 'topology.kubernetes.io/zone ' ] && [ -n "$assignment" ]; then
    record PASS "a size-two ModelDeployment reaches Kueue with a zone-required PodSet" \
      "${ZONE_FIT_WL}: required=${required}, assignment=${assignment}"
  else
    record FAIL "a size-two ModelDeployment reaches Kueue with a zone-required PodSet" \
      "${ZONE_FIT_WL}: required=${required:-missing}, assignment=${assignment:-missing}"
  fi
else
  record FAIL "a size-two ModelDeployment reaches Kueue with a zone-required PodSet" \
    "workload=${ZONE_FIT_WL:-missing}, admission absent; apply=${TAS_APPLY_OUT:0:200}"
fi

ZONE_FIT_ROWS=""
for _ in $(seq 1 60); do
  ZONE_FIT_ROWS="$(tas_pod_nodes "$NS" "app.kubernetes.io/instance=${PREFIX}-zone-fit")"
  tas_rows_all_bound "$ZONE_FIT_ROWS" 2 && break
  sleep 2
done
ZONE_FIT_ZONES=""
while read -r _pod node; do
  [ -n "$node" ] && ZONE_FIT_ZONES="${ZONE_FIT_ZONES}$(tas_node_zone "$node") "
done <<EOF
$ZONE_FIT_ROWS
EOF
# Both Pods bound, both bound Nodes zoned, one zone: an unbound Pod, or a bound Node without a zone,
# would otherwise drop out of the zone count and leave a single zone that proves nothing.
if tas_rows_all_bound "$ZONE_FIT_ROWS" 2 \
  && [ "$(printf '%s' "$ZONE_FIT_ZONES" | wc -w | tr -d ' ')" = 2 ] \
  && [ "$(printf '%s' "$ZONE_FIT_ZONES" | xargs -n1 | sort -u | grep -c . || true)" = 1 ]; then
  record PASS "both Pods of the size-two ModelDeployment bind in one observed zone" \
    "$(printf '%s' "$ZONE_FIT_ROWS" | tr '\n' ';') zone=${ZONE_FIT_ZONES}"
else
  record FAIL "both Pods of the size-two ModelDeployment bind in one observed zone" \
    "pods=$(printf '%s' "$ZONE_FIT_ROWS" | tr '\n' ';'), zones=${ZONE_FIT_ZONES:-missing}"
fi
tas_md_force_release "$NS" "${PREFIX}-zone-fit"
kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-fit" --wait=false >/dev/null 2>&1

# THE REFUSAL NEEDS A CONTROL ON THE SAME QUEUE. Four Nodes is a count, not proof that quota and live
# capacity hold three of these Pods; the same size-three group WITHOUT a required level must be
# admitted first, or the refusal below could be a quota or capacity verdict rather than a zone one.
tas_md_apply "${PREFIX}-zone-control" "$NS" "$BINDING" \
  "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 3)"
ZONE_CONTROL_WL=""
if tas_md_wait_pods "$NS" "${PREFIX}-zone-control" 3 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-control" 1; then
  ZONE_CONTROL_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-control" | sed -n '1p')"
fi
ZONE_CONTROL_ADMITTED=no
for _ in $(seq 1 60); do
  [ -n "$ZONE_CONTROL_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$ZONE_CONTROL_WL")" ] \
    && { ZONE_CONTROL_ADMITTED=yes; break; }
  sleep 2
done
if [ "$ZONE_CONTROL_ADMITTED" = yes ]; then
  record PASS "a size-three group without a required level is admitted on the same queue" \
    "${ZONE_CONTROL_WL} admitted: quota and live capacity hold three Pods in aggregate"
else
  record FAIL "a size-three group without a required level is admitted on the same queue" \
    "workload=${ZONE_CONTROL_WL:-missing}, admission absent; the size-three refusal below cannot be read as a zone verdict"
fi
tas_md_force_release "$NS" "${PREFIX}-zone-control"
kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-control" --wait=false >/dev/null 2>&1
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-control" \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
  sleep 2
done

tas_md_apply "${PREFIX}-zone-refuse" "$NS" "$BINDING" \
  "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 3 topology.kubernetes.io/zone)"
ZONE_REFUSE_WL=""
if tas_md_wait_pods "$NS" "${PREFIX}-zone-refuse" 3 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-refuse" 1; then
  ZONE_REFUSE_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-refuse" | sed -n '1p')"
fi
zone_refuse_sample() {
  ZONE_REFUSE_READABLE=no
  tas_wl_readable "$NS" "$ZONE_REFUSE_WL" && ZONE_REFUSE_READABLE=yes
  ZONE_REFUSE_ADMISSION="$(tas_wl_admitted "$NS" "$ZONE_REFUSE_WL")"
  ZONE_REFUSE_RESERVED="$(tas_wl_condition "$NS" "$ZONE_REFUSE_WL" QuotaReserved)"
  ZONE_REFUSE_BOUND="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-refuse" \
    -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
}
if [ -n "$ZONE_REFUSE_WL" ]; then
  zone_refuse_sample
  ZONE_REFUSE_FIRST_ADMISSION="$ZONE_REFUSE_ADMISSION"
  ZONE_REFUSE_FIRST_RESERVED="$ZONE_REFUSE_RESERVED"
  ZONE_REFUSE_FIRST_BOUND="$ZONE_REFUSE_BOUND"
  ZONE_REFUSE_FIRST_READABLE="$ZONE_REFUSE_READABLE"
  sleep "$INTERVAL"
  zone_refuse_sample
fi
if [ -n "$ZONE_REFUSE_WL" ] && [ "$ZONE_CONTROL_ADMITTED" = yes ] \
  && [ "${ZONE_REFUSE_FIRST_READABLE:-no}" = yes ] && [ "${ZONE_REFUSE_READABLE:-no}" = yes ] \
  && [ -z "$ZONE_REFUSE_FIRST_ADMISSION" ] \
  && [ "$ZONE_REFUSE_FIRST_RESERVED" != True ] && [ "$ZONE_REFUSE_FIRST_BOUND" = 0 ] \
  && [ -z "$ZONE_REFUSE_ADMISSION" ] \
  && [ "$ZONE_REFUSE_RESERVED" != True ] && [ "$ZONE_REFUSE_BOUND" = 0 ] \
  && [ "$TAS_NODES" = 4 ] && [ "$TAS_MAX_ZONE_NODES" = 2 ]; then
  record PASS "a size-three zone request stays Pending despite four aggregate Nodes" \
    "two samples ${INTERVAL}s apart: 4 total, max zone 2, the unconstrained control admitted; no admission, reservation, or binding"
else
  record FAIL "a size-three zone request stays Pending despite four aggregate Nodes" \
    "workload=${ZONE_REFUSE_WL:-missing}, readable=${ZONE_REFUSE_FIRST_READABLE:-no}/${ZONE_REFUSE_READABLE:-no}, control=${ZONE_CONTROL_ADMITTED}, admission=${ZONE_REFUSE_ADMISSION:-none}, reserved=${ZONE_REFUSE_RESERVED:-none}, bound=${ZONE_REFUSE_BOUND:-0}, shape=${TAS_ZONES_SUMMARY}, hostname-only=${HOST_ONLY_NODES:-unknown}"
fi
tas_md_force_release "$NS" "${PREFIX}-zone-refuse"
kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-refuse" --wait=false >/dev/null 2>&1

tas_md_apply "${PREFIX}-omit" "$NS" "$BINDING" \
  "$(tas_md_role_block server '' "$IT" 1 "$IMAGE")"
OMIT_WL=""
if tas_md_wait_pods "$NS" "${PREFIX}-omit" 1 && tas_md_wait_workloads "$NS" "${PREFIX}-omit" 1; then
  OMIT_WL="$(tas_md_workloads "$NS" "${PREFIX}-omit" | sed -n '1p')"
fi
for _ in $(seq 1 45); do
  [ -n "$OMIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$OMIT_WL")" ] && break
  sleep 2
done
OMIT_REQUIRED="$(kubectl -n "$NS" get workload "$OMIT_WL" \
  -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{end}' 2>/dev/null)"
if [ -n "$OMIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$OMIT_WL")" ] && [ -z "$OMIT_REQUIRED" ]; then
  record PASS "omitting topology remains admitted without an explicit required level" "$OMIT_WL admitted; required omitted"
else
  record FAIL "omitting topology remains admitted without an explicit required level" \
    "workload=${OMIT_WL:-missing}, admitted=$(tas_wl_admitted "$NS" "$OMIT_WL" | head -c 40), required=${OMIT_REQUIRED:-empty}"
fi
tas_md_force_release "$NS" "${PREFIX}-omit"
kubectl -n "$NS" delete modeldeployment "${PREFIX}-omit" --wait=false >/dev/null 2>&1

# Workload deletion releases Kueue quota before the Pods disappear, and the InstanceType's
# status.cpu.remaining (nominal quota less the ClusterQueue's reservation) follows the ClusterQueue
# only once the InstanceType is reconciled again. Instance admission bounds a CPU request by
# status.cpu.capacity, so neither refuses the next carrier; an Instance created early would instead
# wait for quota or for a node to free up, and its bounded wait would test deletion latency instead
# of whether a plain Instance remains compatible with a topology-aware queue.
PRIOR_PODS_RELEASED=no
for _ in $(seq 1 60); do
  PRIOR_PODS="$(kubectl -n "$NS" get pods \
    -l "app.kubernetes.io/instance in (${PREFIX}-zone-fit,${PREFIX}-zone-refuse,${PREFIX}-omit)" \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  IT_CPU_REMAINING="$(kubectl get instancetype.worker.gpustack.ai "$IT" \
    -o jsonpath='{.status.cpu.remaining}' 2>/dev/null)"
  if [ "$PRIOR_PODS" = 0 ] && [ "${IT_CPU_REMAINING:-0}" -gt 0 ]; then
    PRIOR_PODS_RELEASED=yes
    break
  fi
  sleep 2
done
if [ "$PRIOR_PODS_RELEASED" = yes ]; then
  record PASS "the ModelDeployment probes release live capacity before the Instance probe" \
    "zero prior Pods remain; InstanceType reports ${IT_CPU_REMAINING} CPU available"
else
  record FAIL "the ModelDeployment probes release live capacity before the Instance probe" \
    "pods=${PRIOR_PODS:-unknown}, InstanceType CPU remaining=${IT_CPU_REMAINING:-unknown}"
fi

# A plain Instance is one Pod and has no topology field. It must continue through the same
# topology-aware queue without the operator inventing a required level.
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata:
  name: ${PREFIX}-instance
  namespace: ${NS}
spec:
  type: ${IT}
  image: alpine:3.22
  command: [sleep, "86400"]
  volume: {ephemeral: {capacity: 1Gi}}
YAML
INSTANCE_READY=no
for _ in $(seq 1 60); do
  INSTANCE_PHASE="$(kubectl -n "$NS" get instance "${PREFIX}-instance" -o jsonpath='{.status.phase}' 2>/dev/null)"
  INSTANCE_NODE="$(kubectl -n "$NS" get pod "${PREFIX}-instance" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
  INSTANCE_WL="$(kubectl -n "$NS" get pod "${PREFIX}-instance" \
    -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
  [ "$INSTANCE_PHASE" = Ready ] && [ -n "$INSTANCE_NODE" ] && [ -n "$INSTANCE_WL" ] \
    && [ -n "$(tas_wl_admitted "$NS" "$INSTANCE_WL")" ] && { INSTANCE_READY=yes; break; }
  sleep 2
done
INSTANCE_REQUIRED="$(kubectl -n "$NS" get workload "$INSTANCE_WL" \
  -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{end}' 2>/dev/null)"
if [ "$INSTANCE_READY" = yes ] && [ -z "$INSTANCE_REQUIRED" ]; then
  record PASS "a plain Instance remains admitted without topology awareness" \
    "instance=${PREFIX}-instance phase=Ready node=${INSTANCE_NODE}, workload=${INSTANCE_WL}, required omitted"
else
  record FAIL "a plain Instance remains admitted without topology awareness" \
    "phase=${INSTANCE_PHASE:-missing}, node=${INSTANCE_NODE:-missing}, workload=${INSTANCE_WL:-missing}, required=${INSTANCE_REQUIRED:-empty}"
fi
if [ -n "$INSTANCE_WL" ]; then
  kubectl -n "$NS" delete workload "$INSTANCE_WL" --ignore-not-found --wait=false >/dev/null 2>&1
fi
kubectl -n "$NS" delete instance "${PREFIX}-instance" --ignore-not-found --wait=false >/dev/null 2>&1

# THE SHORTAGE MUST BE A SHORTAGE. Borrowing through a cohort or a second schedulable flavor would
# let the starved deployment reserve anyway. A cordoned GPU Node, or a Node tainted NoSchedule or
# NoExecute such as a kind control-plane, can leave a CPU ResourceFlavor in this queue, but it cannot
# contribute a live TAS domain for Pods that tolerate neither. Both cohort spellings are read because
# the served version carries either.
PRE_RAW="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" \
  -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}|{range .spec.resourceGroups[*].flavors[*]}{.name}{" "}{end}' 2>/dev/null)"
PRE_VER="${PRE_RAW%%|*}"
PRE_REST="${PRE_RAW#*|}"
PRE_COHORT="${PRE_REST%%|*}"
PRE_FLAVORS="$(printf '%s' "${PRE_REST#*|}" | wc -w | tr -d ' ')"
PRE_REACHABLE="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json | jq -r \
  --slurpfile nodes <(kubectl get nodes -o json) \
  --slurpfile flavors <(kubectl get resourceflavors.kueue.x-k8s.io -o json) '
    [.spec.resourceGroups[]?.flavors[]?.name as $name |
      $flavors[0].items[] | select(.metadata.name == $name) as $flavor |
      select(any($nodes[0].items[];
        .spec.unschedulable != true and
        ([(.spec.taints // [])[] | select(.effect == "NoSchedule" or .effect == "NoExecute")] | length == 0) and
        (.metadata.labels as $labels |
          all($flavor.spec.nodeLabels | to_entries[]; $labels[.key] == .value)))) |
      .metadata.name] | unique | .[]' 2>/dev/null)"
PRE_REACHABLE_FLAVORS="$(printf '%s\n' "$PRE_REACHABLE" | grep -c . || true)"

MD_NEG=yes
if [ -z "$PRE_RAW" ]; then
  record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
    "could not read cluster queue '${CQ}'"
  MD_NEG=no
elif [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; then
  record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
    "cluster queue '${CQ}' is served as '${PRE_VER}', whose cohort field this case does not know how to read"
  MD_NEG=no
elif [ -n "$PRE_COHORT" ]; then
  record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
    "cluster queue '${CQ}' is in cohort '${PRE_COHORT}', so occupying its nominalQuota does not make the pool short"
  MD_NEG=no
elif [ "$PRE_REACHABLE_FLAVORS" != 1 ]; then
  record SKIP "the shortage cannot be relieved by borrowing or by another flavor" \
    "cluster queue '${CQ}' offers ${PRE_FLAVORS} flavors, ${PRE_REACHABLE_FLAVORS} with schedulable matching Nodes; this case needs one live flavor"
  MD_NEG=no
else
  record PASS "the shortage cannot be relieved by borrowing or by another flavor" \
    "cluster queue '${CQ}' (${PRE_VER}) is in no cohort; ${PRE_REACHABLE_FLAVORS} of ${PRE_FLAVORS} flavors have schedulable matching Nodes"
fi

# --- the fitting half: both roles admitted jointly ---

tas_md_apply "${PREFIX}-md" "$NS" "$BINDING" \
"$(tas_md_role_block prefill prefill "$IT" 1 "$IMAGE")
$(tas_md_role_block decode decode "$IT" 1 "$IMAGE")"

MD_WLS=""
if tas_md_wait_pods "$NS" "${PREFIX}-md" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-md" 2; then
  MD_WLS="$(tas_md_workloads "$NS" "${PREFIX}-md")"
fi

if [ "$(printf '%s\n' "$MD_WLS" | grep -c . || true)" != 2 ]; then
  record FAIL "two Workloads are attributable to the deployment through their owner Pods" \
    "$(printf '%s' "$MD_WLS" | grep -c . || true) attributable for two roles; apply said: ${TAS_APPLY_OUT:0:240}"
  record FAIL "both roles are admitted together" "no two-Workload set was ever composed to admit"
  record FAIL "the joint admission check reads Ready on every Workload" "no two-Workload set was ever composed to gate"
  MD_NEG=no
else
  record PASS "two Workloads are attributable to the deployment through their owner Pods" \
    "$(printf '%s' "$MD_WLS" | tr '\n' ' ')each found by following its owner Pod references back to this deployment's Pods"

  MD_ADMITTED=no
  for _ in $(seq 1 40); do
    BAD=0
    while read -r wl; do
      [ -n "$(tas_wl_admitted "$NS" "$wl")" ] || BAD=1
    done <<EOF
$MD_WLS
EOF
    [ "$BAD" = 0 ] && { MD_ADMITTED=yes; break; }
    sleep 3
  done
  if [ "$MD_ADMITTED" = yes ]; then
    record PASS "both roles are admitted together" \
      "every Workload of the set holds an admission"
  else
    record FAIL "both roles are admitted together" \
      "some Workload holds none: $(for w in $MD_WLS; do printf '%s:%s ' "$w" "$(tas_wl_admitted "$NS" "$w" | head -c 20)"; done)"
  fi

  CHECKS_READY=yes
  CHECKS_READ=""
  CHECK_IDX=0
  while read -r wl; do
    CHECK_IDX=$((CHECK_IDX + 1))
    s="$(tas_wl_check_state "$NS" "$wl" "$JOINT_CHECK")"
    CHECKS_READ="${CHECKS_READ}wl${CHECK_IDX}=${s:-absent} "
    [ "$s" = Ready ] || CHECKS_READY=no
  done <<EOF
$MD_WLS
EOF
  if [ "$CHECKS_READY" = yes ]; then
    record PASS "the joint admission check reads Ready on every Workload" \
      "${JOINT_CHECK}: ${CHECKS_READ}- the barrier opened only once the whole set had quota"
  else
    record FAIL "the joint admission check reads Ready on every Workload" \
      "${JOINT_CHECK}: ${CHECKS_READ}- admitted workloads behind a check that never turned Ready"
  fi
fi

# --- the negative half: pool occupied to its last unit ---

# One replica's CPU request is read from a Pod the operator actually rendered: the request is what
# Kueue charges, and it is not the InstanceType's unit verbatim.
REQ_RAW="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-md" \
  -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' 2>/dev/null)"
REQ="$(tas_millis "$REQ_RAW")"
QUOTA_RAW="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" \
  -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="cpu")].nominalQuota}' 2>/dev/null)"
QUOTA="$(tas_millis "$QUOTA_RAW")"

kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${PREFIX}-md" \
  --ignore-not-found --wait=false >/dev/null 2>&1
for _ in $(seq 1 30); do
  [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-md" \
    --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break
  sleep 2
done
tas_md_force_release "$NS" "${PREFIX}-md"

# THE FILLER OCCUPIES EVERY LIVE HOSTNAME DOMAIN, NOT THE QUEUE'S PAPER QUOTA. TAS subtracts
# non-Kueue Pods from each domain, so nominalQuota can describe ten replicas while four system-loaded
# Nodes can admit only one each. Sizing from nominalQuota would test an inadmissible filler instead
# of the intended shortage. One admitted replica per observed Node is the live-capacity boundary the
# first half of this case already established.
FILL="${TAS_NODES:-0}"
if [ "$MD_NEG" = yes ] && { [ "$REQ" -le 0 ] || [ "$FILL" -lt 1 ]; }; then
  record SKIP "every live hostname domain can be occupied" \
    "could not read the inputs: request='${REQ_RAW}', observed Nodes='${FILL}'"
  MD_NEG=no
fi

if [ "$MD_NEG" = yes ]; then
  tas_md_apply "${PREFIX}-filler" "$NS" "$BINDING" \
    "$(tas_md_role_block bulk '' "$IT" "$FILL" "$IMAGE")"

  FILLER_WLS=""
  FILLER_OK=no
  if tas_md_wait_workloads "$NS" "${PREFIX}-filler" "$FILL"; then
    FILLER_WLS="$(tas_md_workloads "$NS" "${PREFIX}-filler")"
    for _ in $(seq 1 40); do
      FILLER_ADMITTED=0
      while read -r filler_wl; do
        [ -n "$filler_wl" ] || continue
        [ -n "$(tas_wl_admitted "$NS" "$filler_wl")" ] && FILLER_ADMITTED=$((FILLER_ADMITTED + 1))
      done <<EOF
$FILLER_WLS
EOF
      [ "$FILLER_ADMITTED" = "$FILL" ] && { FILLER_OK=yes; break; }
      sleep 3
    done
  fi
  if [ "$FILLER_OK" = yes ]; then
    record PASS "every live hostname domain is occupied" \
      "${FILL} observed Nodes each hold one admitted ${REQ}m filler replica; nominal quota is ${QUOTA}m"
  else
    record FAIL "every live hostname domain is occupied" \
      "${FILLER_ADMITTED:-0} of ${FILL} filler Workloads admitted; apply said: ${TAS_APPLY_OUT:0:200}"
    MD_NEG=no
  fi
fi

# --- the negative probe, across two intervals ---

if [ "$MD_NEG" = yes ]; then
  tas_md_apply "${PREFIX}-starved" "$NS" "$BINDING" \
"$(tas_md_role_block prefill prefill "$IT" 1 "$IMAGE")
$(tas_md_role_block decode decode "$IT" 1 "$IMAGE")"

  STARVED_WLS=""
  if tas_md_wait_pods "$NS" "${PREFIX}-starved" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-starved" 2; then
    STARVED_WLS="$(tas_md_workloads "$NS" "${PREFIX}-starved")"
  fi
  N_WLS="$(printf '%s\n' "$STARVED_WLS" | grep -c . || true)"

  if [ "$N_WLS" != 2 ]; then
    record FAIL "every Workload of the short deployment stays unadmitted and unreserved (two intervals)" \
      "only ${N_WLS} Workload(s) attributable for two roles; apply said: ${TAS_APPLY_OUT:0:240}"
    record FAIL "the joint admission check holds every group Pending (two intervals)" \
      "no two-Workload set was ever composed to gate"
    record FAIL "no role Pod of the short deployment is bound to a Node (two intervals)" \
      "no two-Workload set was ever composed to read a binding from"
  else
    # ONE SAMPLING PASS, USED TWICE: everything the probe asserts is read together, so the two
    # intervals can only differ by time and never by what was read.
    starved_sample() {
      STARVED_ADMITTED=""
      STARVED_RESERVED=""
      STARVED_UNREADABLE=""
      STARVED_CHECKS=""
      STARVED_CHECK_BAD=""
      STARVED_PODS=0
      STARVED_BOUND=0
      local wl pods idx s
      idx=0
      while read -r wl; do
        idx=$((idx + 1))
        tas_wl_readable "$NS" "$wl" || STARVED_UNREADABLE="${STARVED_UNREADABLE}${wl} "
        [ -n "$(tas_wl_admitted "$NS" "$wl")" ] && STARVED_ADMITTED="${STARVED_ADMITTED}${wl} "
        [ "$(tas_wl_condition "$NS" "$wl" QuotaReserved)" = True ] && STARVED_RESERVED="${STARVED_RESERVED}${wl} "
        s="$(tas_wl_check_state "$NS" "$wl" "$JOINT_CHECK")"
        STARVED_CHECKS="${STARVED_CHECKS}wl${idx}=${s:-absent} "
        [ "$s" = Pending ] || STARVED_CHECK_BAD="${STARVED_CHECK_BAD}wl${idx}=${s:-absent} "
      done <<EOF
$STARVED_WLS
EOF
      # Read both counts from the Pod list itself. A jsonpath made only of blank nodeName lines is
      # destroyed by command substitution's trailing-newline trimming, so counting those rendered
      # lines turns two real gated Pods into one synthetic line and tests the shell instead of the API.
      pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-starved" \
        -o json 2>/dev/null)"
      STARVED_PODS="$(printf '%s' "$pods" | jq '[.items[]?] | length')"
      STARVED_BOUND="$(printf '%s' "$pods" | jq '[.items[]? | select(.spec.nodeName != null and .spec.nodeName != "")] | length')"
    }

    starved_sample
    STARVED_FIRST_ADMITTED="$STARVED_ADMITTED"
    STARVED_FIRST_RESERVED="$STARVED_RESERVED"
    STARVED_FIRST_CHECK_BAD="$STARVED_CHECK_BAD"
    STARVED_FIRST_PODS="$STARVED_PODS"
    STARVED_FIRST_BOUND="$STARVED_BOUND"
    STARVED_FIRST_UNREADABLE="$STARVED_UNREADABLE"
    sleep "$INTERVAL"
    starved_sample

    if [ -z "$STARVED_FIRST_ADMITTED$STARVED_FIRST_RESERVED$STARVED_ADMITTED$STARVED_RESERVED" ] \
      && [ -z "$STARVED_FIRST_UNREADABLE$STARVED_UNREADABLE" ]; then
      record PASS "every Workload of the short deployment stays unadmitted and unreserved (two intervals)" \
        "two samples ${INTERVAL}s apart over $(printf '%s' "$STARVED_WLS" | tr '\n' ' '): no admission, no quota reservation"
    else
      record FAIL "every Workload of the short deployment stays unadmitted and unreserved (two intervals)" \
        "admitted: '${STARVED_ADMITTED:-none}', reserved: '${STARVED_RESERVED:-none}', unreadable: '${STARVED_FIRST_UNREADABLE}${STARVED_UNREADABLE}' - the pool was not actually short, or a Workload could not be read"
    fi

    if [ -z "$STARVED_FIRST_CHECK_BAD$STARVED_CHECK_BAD" ]; then
      record PASS "the joint admission check holds every group Pending (two intervals)" \
        "${JOINT_CHECK}: ${STARVED_CHECKS}across both samples"
    else
      record FAIL "the joint admission check holds every group Pending (two intervals)" \
        "${JOINT_CHECK} left Pending nowhere: ${STARVED_CHECK_BAD}"
    fi

    # THE POD COUNT IS ASSERTED WITH THE BINDING, not only the bound count: zero Pods bound is also
    # what a deployment that never rendered anything produces, and that vacuous shape must not pass.
    if [ "$STARVED_FIRST_BOUND" = 0 ] && [ "$STARVED_FIRST_PODS" = 2 ] \
      && [ "$STARVED_BOUND" = 0 ] && [ "$STARVED_PODS" = 2 ]; then
      record PASS "no role Pod of the short deployment is bound to a Node (two intervals)" \
        "${STARVED_PODS} Pods exist, 0 bound, across both samples"
    else
      record FAIL "no role Pod of the short deployment is bound to a Node (two intervals)" \
        "${STARVED_BOUND} of ${STARVED_PODS} bound - an unadmitted group must keep every role Pod gated"
    fi
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-87] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-87] all checks passed (rows that SKIP name what the cluster could not supply)"
