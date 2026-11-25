#!/usr/bin/env bash
#
# CASE 15 — Topology sources drive Kueue TAS placement, joint ModelDeployment admission and per-card accelerator admission agreement
#           (MUTATING, self-recovering)
#
#   case-15.sh <NS>
#
# Prerequisites: nodes=3; gpu=none; features=multi-host; cluster=any; inputs=E2E_MD_BINDING,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_SYSTEM_NAMESPACE,E2E_TAS_INTERVAL
# Goal:        Proves the complete topology-aware scheduling path on real Nodes across two parts:
#              Part 1 (3+ zoned Nodes, Kind or Real):
#                - Cloud nodeLabels, ConfigMap snapshot (rack), and authenticated HTTPS webhook
#                  TopologySources reach Ready/Stale/Expired/Recovered states and generate exact
#                  Kueue Topology levels and NFD NodeFeatures without disturbing unowned labels;
#                - Live profile transition drains a reserved ClusterQueue via HoldAndDrain before
#                  switching flavors while keeping the ClusterQueue name and UID stable, retires
#                  old flavors/Topology, and protects live Topologies behind Kueue finalizers;
#                - Hand-built TAS fixtures prove same-zone 2-Pod placement and cross-zone 3-Pod
#                  refusal against proven-sufficient aggregate quota;
#                - Operator-managed TAS queue proves size-2 zone-required ModelDeployment placement,
#                  size-3 zone refusal (with ballast + unconstrained control), topology omission,
#                  plain Instance compatibility, and 2-role ModelDeployment joint admission
#                  (fitting + symmetric shortage across two intervals).
#              Part 2 (auto-skips cleanly without a free whole accelerator):
#                - On a Ready schedulable Node with a free whole accelerator, a nodeLabels
#                  TopologySource converges Node and Devices on one profile, migrates the
#                  accelerated ClusterQueue in place without UID change, and a zone-required
#                  ModelDeployment passes both Kueue TAS and gpustack-node-devices AdmissionChecks
#                  and binds its accelerator Pod to that exact Node and profile flavor.
# Environment: A cluster with the operator chart's Kueue (TopologyAwareScheduling enabled) and a
#              materialized scheduling chain (run case-1 first). Namespace <NS> must carry the
#              pool's entrance LocalQueue. Part 1 needs zoned Linux Nodes; Part 2 runs whenever a
#              free whole accelerator exists and records SKIP cleanly otherwise.
# Inputs:      All real, nothing mocked — case-prefixed TopologySources, ConfigMaps, Secrets,
#              webhook Deployment/Service, Jobs, ModelDeployments, Instance, ballast Pods, and
#              temporary Node selector labels.
# Expected:    Every applicable proof in Part 1 and Part 2 records PASS; unavailable cluster shapes
#              (such as no free accelerator for Part 2) record SKIP with the measured shape.
# Cleanup:     Trap force-releases every Workload owning each ModelDeployment's Pods, deletes all
#              ModelDeployments, Instances, Jobs, ballast Pods, TopologySources, webhook resources,
#              temporary Node labels, and hand-built TAS fixtures. Idempotent; runs on pass AND fail.
set -o pipefail

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-15.sh <NS>" >&2
  exit 2
fi
SYSTEM_NS="${E2E_SYSTEM_NAMESPACE:-gpustack-system}"

PREFIX="e2e-c15-$(date +%s)-$$"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
BINDING="${E2E_MD_BINDING:-${PREFIX}-no-such-binding}"
JOINT_CHECK="gpustack-model-deployment-joint"
INTERVAL="${E2E_TAS_INTERVAL:-20}"
FOREIGN_LABEL="topology.gpustack.ai/e2e-c15-foreign"
BALLAST_LABEL="e2e.gpustack.ai/c15-ballast"

# Part 2 (accelerator agreement) state tracked for cleanup.
ACC_SOURCE="${PREFIX}-accel-native"
ACC_MD="${PREFIX}-accel-zone"
ACC_SELECTOR_KEY="e2e.gpustack.ai/c15-accel"
ACC_NODE=""
ACC_WL=""

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
CQ_WATCH_PID=""
exit_input_required() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-15] ${FAILS} check(s) FAILED before the input check"; exit 1; }
  exit 2
}

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_topology-tas-lib.sh"

TMP_CASE15="$(mktemp -d "${TMPDIR:-/tmp}/gpustack-case15.XXXXXX")"

cleanup() {
  local md instance_workload
  if [ -n "$CQ_WATCH_PID" ]; then
    kill "$CQ_WATCH_PID" 2>/dev/null || true
    wait "$CQ_WATCH_PID" 2>/dev/null || true
  fi
  for md in "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved" "$ACC_MD"; do
    tas_md_force_release "$NS" "$md"
  done
  if [ -n "$ACC_WL" ]; then
    kubectl -n "$NS" delete workload.kueue.x-k8s.io "$ACC_WL" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  instance_workload="$(kubectl -n "$NS" get pod "${PREFIX}-instance" \
    -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
  [ -n "$instance_workload" ] && kubectl -n "$NS" delete workload "$instance_workload" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete instance.worker.gpustack.ai "${PREFIX}-instance" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai \
    "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved" "$ACC_MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  for md in "${PREFIX}-transition" "${PREFIX}-zone-fit" "${PREFIX}-zone-control" "${PREFIX}-zone-refuse" \
    "${PREFIX}-omit" "${PREFIX}-md" "${PREFIX}-filler" "${PREFIX}-starved" "$ACC_MD"; do
    tas_md_force_release "$NS" "$md"
  done
  kubectl -n "$NS" delete job.batch "${PREFIX}-fit" "${PREFIX}-refuse" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete pods -l "${BALLAST_LABEL}=${PREFIX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete topologysources.worker.gpustack.ai \
    "${PREFIX}-native" "${PREFIX}-rack" "${PREFIX}-webhook" "$ACC_SOURCE" \
    --ignore-not-found --wait=true --timeout=90s >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete deploy "${PREFIX}-webhook" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete service "${PREFIX}-webhook" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete configmap "${PREFIX}-rack" "${PREFIX}-webhook-snapshot" "${PREFIX}-webhook-ca" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete secret "${PREFIX}-webhook-token" "${PREFIX}-webhook-tls" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl label nodes -l topology.gpustack.ai/e2e-c15-native=enabled topology.gpustack.ai/e2e-c15-native- \
    >/dev/null 2>&1 || true
  kubectl label nodes -l topology.gpustack.ai/e2e-c15-rack=enabled topology.gpustack.ai/e2e-c15-rack- \
    >/dev/null 2>&1 || true
  kubectl label nodes -l "$FOREIGN_LABEL" "${FOREIGN_LABEL}-" >/dev/null 2>&1 || true
  if [ -n "$ACC_NODE" ]; then
    kubectl label node "$ACC_NODE" "${ACC_SELECTOR_KEY}-" >/dev/null 2>&1 || true
  fi
  tas_delete_fixtures "$PREFIX" "$NS"
  rm -rf "$TMP_CASE15"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

native_region_zone_pairs() {
  kubectl get nodes -l topology.gpustack.ai/e2e-c15-rack=enabled -o json 2>/dev/null | jq -r '
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
      topology.gpustack.ai/e2e-c15-native: enabled
  levels:
  - topology.kubernetes.io/region
  - topology.kubernetes.io/zone
  nodeLabels: {}
YAML
}

snapshot_yaml() {
  local revision="$1" label="$2" mode="$3"
  kubectl get nodes -o json | jq -r --arg revision "$revision" --arg label "$label" --arg mode "$mode" '
    "apiVersion: worker.gpustack.ai/v1alpha1\nkind: TopologySourceConfiguration\nrevision: " + $revision + "\nnodes:\n" +
    ([.items | map(select(.metadata.labels["topology.gpustack.ai/e2e-c15-rack"] == "enabled"))
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
  snapshot_yaml rack-1 topology.gpustack.ai/rack rack > "${TMP_CASE15}/rack.yaml"
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-rack" --from-file=snapshot.yaml="${TMP_CASE15}/rack.yaml" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${PREFIX}-native
spec:
  nodeSelector:
    matchLabels:
      topology.gpustack.ai/e2e-c15-rack: enabled
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
    -addext "subjectAltName=DNS:${host}" -keyout "${TMP_CASE15}/tls.key" \
    -out "${TMP_CASE15}/tls.crt" >/dev/null 2>&1
  snapshot_yaml webhook-1 topology.gpustack.ai/webhook-domain domain > "${TMP_CASE15}/snapshot.json"
  kubectl -n "$SYSTEM_NS" create secret tls "${PREFIX}-webhook-tls" \
    --cert="${TMP_CASE15}/tls.crt" --key="${TMP_CASE15}/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create secret generic "${PREFIX}-webhook-token" --from-literal=token=token-one \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-ca" --from-file=ca.crt="${TMP_CASE15}/tls.crt" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-snapshot" --from-file=snapshot.json="${TMP_CASE15}/snapshot.json" \
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
      topology.gpustack.ai/e2e-c15-rack: enabled
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

# ===================================================================================================
# Part 1 — Topology sources, live queue transition, TAS placement & joint ModelDeployment admission.
# ===================================================================================================
echo "[case-15] === part 1: topology sources, TAS placement and joint ModelDeployment admission ==="

tas_zone_shape
if [ "$TAS_NODES" -eq 0 ]; then
  record SKIP "part 1: cluster has schedulable Linux Nodes with zone label" \
    "0 Nodes qualify (zones: ${TAS_ZONES_SUMMARY:-none}); skipping Part 1"
else
  TAS_NODE_JQ='select(.metadata.labels["kubernetes.io/os"] == "linux")
    | select((.metadata.labels["topology.kubernetes.io/zone"] // "") != "")
    | select(.spec.unschedulable != true)
    | select([(.spec.taints // [])[] | select(.effect == "NoSchedule" or .effect == "NoExecute")] | length == 0)'
  CPU_NODES="$(kubectl get nodes -o json | jq -r ".items[] | ${TAS_NODE_JQ} | .metadata.name")"
  while read -r node; do
    [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-c15-rack=enabled --overwrite >/dev/null
  done <<EOF
$CPU_NODES
EOF
  FOREIGN_NODE="$(printf '%s\n' "$CPU_NODES" | sed -n '1p')"
  [ -n "$FOREIGN_NODE" ] && kubectl label node "$FOREIGN_NODE" "${FOREIGN_LABEL}=kept" --overwrite >/dev/null
  NATIVE_PAIRS_BEFORE="$(native_region_zone_pairs)"

  NATIVE_NODES="$(kubectl get nodes -o json | jq -r "
    [.items
     | map(${TAS_NODE_JQ})
     | group_by(.metadata.labels[\"topology.kubernetes.io/zone\"])[]] as \$zones |
    ((\$zones[0] // [])[] | .metadata.name), (\$zones[1][0]? // empty | .metadata.name)" | sort -u)"
  while read -r node; do
    [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-c15-native=enabled --overwrite >/dev/null
  done <<EOF
$NATIVE_NODES
EOF

  apply_native_source
  if wait_source "${PREFIX}-native" 'True|Observed' \
    && wait_profile_levels 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname '; then
    ZONE_PROFILE_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-c15-native=enabled --no-headers | wc -l | tr -d ' ')"
    HOST_ONLY_NODES="$(kubectl get nodes -l '!topology.gpustack.ai/e2e-c15-native' --no-headers | wc -l | tr -d ' ')"
    record PASS "native region and zone labels form the active topology profile" \
      "${SOURCE_TOPOLOGY}: ${ZONE_PROFILE_NODES} region/zone Nodes; ${HOST_ONLY_NODES} Node(s) outside selector"
  else
    record FAIL "native region and zone labels form the active topology profile" \
      "source=$(source_condition "${PREFIX}-native" Ready), topology=${SOURCE_TOPOLOGY:-missing}"
  fi

  NATIVE_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-native" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  NATIVE_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$NATIVE_SOURCE_UID")"
  if [ -n "$NATIVE_SOURCE_UID" ] && [ "$NATIVE_FEATURES" = 0 ]; then
    record PASS "read-only nodeLabels source owns no NodeFeature" "${PREFIX}-native owns 0 NodeFeatures"
  else
    record FAIL "read-only nodeLabels source owns no NodeFeature" "uid=${NATIVE_SOURCE_UID:-missing}, owned=${NATIVE_FEATURES:-unreadable}"
  fi

  EXCLUDED_ROWS=""; EXCLUDED_OK=no
  while read -r node; do
    [ -n "$node" ] || continue
    [ "$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/e2e-c15-native}' 2>/dev/null)" = enabled ] && continue
    profile="$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
    levels="$(kubectl get topology.kueue.x-k8s.io "gpustack-${profile}" -o jsonpath='{range .spec.levels[*]}{.nodeLabel}{" "}{end}' 2>/dev/null)"
    EXCLUDED_ROWS="${EXCLUDED_ROWS}${node}=${levels:-missing};"
    if [ "$levels" = 'kubernetes.io/hostname ' ]; then EXCLUDED_OK=yes; else EXCLUDED_OK=bad; break; fi
  done <<EOF
$CPU_NODES
EOF
  if [ "$EXCLUDED_OK" = yes ]; then
    record PASS "schedulable Node outside native selector is hostname-only" "$EXCLUDED_ROWS"
  else
    record FAIL "schedulable Node outside native selector is hostname-only" "${EXCLUDED_ROWS:-no excluded schedulable Node}"
  fi

  [ -n "$IT" ] || IT="$(tas_md_usable_instance_type)"
  CQ="$(tas_md_cq_of_it "$NS" "$IT")"
  STABLE_CQ_UID=""
  if [ -n "$CQ" ] && wait_cq_topology_plan "$CQ" "$SOURCE_TOPOLOGY" contains; then
    STABLE_CQ_UID="$CQ_PLAN_UID"
    record PASS "operator queue consumes native topology profile" "${CQ} uid=${STABLE_CQ_UID}, topologies=$(cq_plan_topologies_inline)"
  else
    record FAIL "operator queue consumes native topology profile" "it=${IT:-missing} cq=${CQ:-missing} topologies=$(cq_plan_topologies_inline)"
  fi

  NATIVE_TOPOLOGY="$SOURCE_TOPOLOGY"
  OLD_FLAVORS="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json 2>/dev/null \
    | jq -r '(.spec.resourceGroups // [])[]?.flavors[]?.name' \
    | while read -r flavor; do
        [ "$(kubectl get resourceflavor "$flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null)" = "$NATIVE_TOPOLOGY" ] && printf '%s\n' "$flavor"
      done | sort -u)"
  OLD_FLAVORS_JSON="$(printf '%s\n' "$OLD_FLAVORS" | jq -Rsc 'split("\n") | map(select(length > 0))')"
  kubectl get clusterqueue.kueue.x-k8s.io "$CQ" --watch --output-watch-events -o json > "${TMP_CASE15}/cq-watch.json" 2>/dev/null &
  CQ_WATCH_PID=$!
  CQ_WATCH_READY=no
  for _ in $(seq 1 20); do [ -s "${TMP_CASE15}/cq-watch.json" ] && { CQ_WATCH_READY=yes; break; }; sleep 1; done
  PRE_TRANSITION_STOP="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.spec.stopPolicy}' 2>/dev/null)"

  tas_md_apply "${PREFIX}-transition" "$NS" "$BINDING" \
    "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 1 topology.kubernetes.io/zone)"
  TRANSITION_WL=""; TRANSITION_RESERVED=no
  if tas_md_wait_pods "$NS" "${PREFIX}-transition" 1 && tas_md_wait_workloads "$NS" "${PREFIX}-transition" 1; then
    TRANSITION_WL="$(tas_md_workloads "$NS" "${PREFIX}-transition" | sed -n '1p')"
  fi
  for _ in $(seq 1 60); do
    transition_cq="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.status.admission.clusterQueue}' 2>/dev/null)"
    transition_flavor="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o json 2>/dev/null | jq -r '[.status.admission.podSetAssignments[0].flavors[]?][0] // empty')"
    if [ -n "$TRANSITION_WL" ] && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved)" = True ] \
      && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted)" = True ] \
      && [ "$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.spec.podSets[0].topologyRequest.required}' 2>/dev/null)" = topology.kubernetes.io/zone ] \
      && [ "$transition_cq" = "$CQ" ] && [ -n "$transition_flavor" ] \
      && printf '%s\n' "$OLD_FLAVORS" | grep -Fxq "$transition_flavor"; then
      TRANSITION_RESERVED=yes; break
    fi
    sleep 2
  done
  if [ "$TRANSITION_RESERVED" = yes ]; then
    record PASS "Workload reserves old topology before migration" "${TRANSITION_WL} admitted on ${transition_flavor}"
  else
    record FAIL "Workload reserves old topology before migration" "wl=${TRANSITION_WL:-missing} cq=${transition_cq:-missing} flavor=${transition_flavor:-missing}"
  fi

  apply_rack_source
  RACK_TOPOLOGY=""; TRANSITION_SWITCHED=no
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
      TRANSITION_SWITCHED=yes; CQ_PLAN_UID="$uid_now"; CQ_PLAN_TOPOLOGIES="$topologies_now"; break
    fi
    sleep 1
  done
  kill "$CQ_WATCH_PID" 2>/dev/null || true; wait "$CQ_WATCH_PID" 2>/dev/null || true; CQ_WATCH_PID=""
  jq -r --argjson olds "$OLD_FLAVORS_JSON" '
    select(.object != null) | .object as $cq |
    [$cq.metadata.uid,
     ($cq.spec.stopPolicy // "None"),
     (($cq.status.reservingWorkloads // 0) | tostring),
     ([$cq.spec.resourceGroups[]?.flavors[]?.name] as $present |
       ($olds | all(. as $old | $present | index($old) != null)) | tostring),
     (([$cq.status.conditions[]? | select(.type == "Active" and .status == "False" and
         .reason == "Stopped" and .observedGeneration >= $cq.metadata.generation)] | length > 0)
       | tostring)] | @tsv' "${TMP_CASE15}/cq-watch.json" > "${TMP_CASE15}/cq-watch.tsv"
  read -r TRANSITION_HELD TRANSITION_HELD_RESERVED TRANSITION_DRAINED TRANSITION_DROPPED_OLD \
    <<<"$(tas_cq_transition_verdict "${TMP_CASE15}/cq-watch.tsv")"

  RACK_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-native" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  RACK_FEATURES="$(kubectl -n "$SYSTEM_NS" get nodefeatures -l "topology.gpustack.ai/source-uid=${RACK_SOURCE_UID}" -o json 2>/dev/null)"
  RACK_FEATURE_COUNT="$(printf '%s' "$RACK_FEATURES" | jq '[.items[]? | select(.spec.labels["topology.gpustack.ai/rack"] != null) | select(.spec.labels["topology.kubernetes.io/region"] == null and .spec.labels["topology.kubernetes.io/zone"] == null)] | length' 2>/dev/null)"
  RACK_NODE_COUNT="$(kubectl get nodes -l topology.gpustack.ai/e2e-c15-rack=enabled -o json 2>/dev/null | jq '.items | length')"
  if [ "$(source_condition "${PREFIX}-native" Ready)" = 'True|Observed' ] \
    && [ -n "$RACK_TOPOLOGY" ] && [ "$RACK_NODE_COUNT" -gt 0 ] && [ "$RACK_FEATURE_COUNT" = "$RACK_NODE_COUNT" ]; then
    record PASS "ConfigMap snapshot publishes rack through NodeFeatures" "${RACK_FEATURE_COUNT}/${RACK_NODE_COUNT} NodeFeatures"
  else
    record FAIL "ConfigMap snapshot publishes rack through NodeFeatures" "features=${RACK_FEATURE_COUNT:-missing}/${RACK_NODE_COUNT:-missing} topology=${RACK_TOPOLOGY:-missing}"
  fi

  RACK_EXPECTED="$(tas_snapshot_pairs "${TMP_CASE15}/rack.yaml" topology.gpustack.ai/rack)"
  RACK_ACTUAL=""
  for _ in $(seq 1 60); do
    RACK_ACTUAL="$(tas_node_label_pairs topology.gpustack.ai/e2e-c15-rack=enabled topology.gpustack.ai/rack)"
    [ -n "$RACK_EXPECTED" ] && [ "$RACK_ACTUAL" = "$RACK_EXPECTED" ] && break
    sleep 2
  done
  if [ -n "$RACK_EXPECTED" ] && [ "$RACK_ACTUAL" = "$RACK_EXPECTED" ]; then
    record PASS "NFD projects every snapshot rack value onto its Node" "$(printf '%s' "$RACK_ACTUAL" | paste -sd, -)"
  else
    record FAIL "NFD projects every snapshot rack value onto its Node" "expected=$(printf '%s' "$RACK_EXPECTED" | paste -sd, -) got=$(printf '%s' "$RACK_ACTUAL" | paste -sd, -)"
  fi

  if [ "$CQ_WATCH_READY" = yes ] && [ -n "$OLD_FLAVORS" ] && [ "$TRANSITION_RESERVED" = yes ] && [ "$TRANSITION_HELD" = yes ] \
    && [ "$TRANSITION_HELD_RESERVED" = yes ] && [ "$TRANSITION_DRAINED" = yes ] && [ "$TRANSITION_DROPPED_OLD" = no ] \
    && [ "$TRANSITION_SWITCHED" = yes ]; then
    record PASS "reserved profile transition drains before switching ClusterQueue" "${CQ} kept uid=${CQ_PLAN_UID}"
  else
    record FAIL "reserved profile transition drains before switching ClusterQueue" "watch=${CQ_WATCH_READY} held=${TRANSITION_HELD} reserved=${TRANSITION_HELD_RESERVED} drained=${TRANSITION_DRAINED} droppedOld=${TRANSITION_DROPPED_OLD} switched=${TRANSITION_SWITCHED}"
  fi

  TRANSITION_READMITTED=no
  for _ in $(seq 1 90); do
    TRANSITION_WL="$(tas_md_workloads "$NS" "${PREFIX}-transition" | sed -n '1p')"
    transition_flavor="$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o json 2>/dev/null | jq -r '.status.admission.podSetAssignments[0].flavors.cpu // empty')"
    transition_topology="$(kubectl get resourceflavor "$transition_flavor" -o jsonpath='{.spec.topologyName}' 2>/dev/null)"
    if [ -n "$TRANSITION_WL" ] && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" QuotaReserved)" = True ] \
      && [ "$(tas_wl_condition "$NS" "$TRANSITION_WL" Admitted)" = True ] \
      && [ "$(kubectl -n "$NS" get workload "$TRANSITION_WL" -o jsonpath='{.spec.podSets[0].topologyRequest.required}' 2>/dev/null)" = topology.kubernetes.io/zone ] \
      && [ "$transition_topology" = "$RACK_TOPOLOGY" ]; then
      TRANSITION_READMITTED=yes; break
    fi
    sleep 2
  done
  if [ "$TRANSITION_READMITTED" = yes ]; then
    record PASS "Kueue readmits drained Workload on replacement topology" "${TRANSITION_WL} on ${transition_topology}"
  else
    record FAIL "Kueue readmits drained Workload on replacement topology" "wl=${TRANSITION_WL:-missing} topology=${transition_topology:-missing}"
  fi
  tas_md_force_release "$NS" "${PREFIX}-transition"
  kubectl -n "$NS" delete modeldeployment "${PREFIX}-transition" --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do [ "$(kubectl get clusterqueue "$CQ" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)" = 0 ] && break; sleep 2; done

  OLD_PLAN_RETIRED=no
  for _ in $(seq 1 90); do
    remaining_old=0
    while read -r old_flavor; do
      [ -z "$old_flavor" ] && continue
      kubectl get resourceflavor.kueue.x-k8s.io "$old_flavor" >/dev/null 2>&1 && remaining_old=$((remaining_old + 1))
    done <<EOF
$OLD_FLAVORS
EOF
    if [ "$remaining_old" = 0 ] && ! kubectl get topology.kueue.x-k8s.io "$NATIVE_TOPOLOGY" >/dev/null 2>&1; then
      OLD_PLAN_RETIRED=yes; break
    fi
    sleep 2
  done
  if [ "$OLD_PLAN_RETIRED" = yes ]; then
    record PASS "unreferenced old flavors and Topology retire after switch" "${NATIVE_TOPOLOGY} and old flavors absent"
  else
    record FAIL "unreferenced old flavors and Topology retire after switch" "remainingFlavors=${remaining_old:-unknown}"
  fi

  if [ -n "${RACK_TOPOLOGY:-}" ]; then
    kubectl delete topology.kueue.x-k8s.io "$RACK_TOPOLOGY" --wait=false >/dev/null 2>&1
    RACK_DELETING=""
    for _ in $(seq 1 30); do
      RACK_DELETING="$(kubectl get topology.kueue.x-k8s.io "$RACK_TOPOLOGY" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)"
      [ -n "$RACK_DELETING" ] && break
      sleep 2
    done
    if [ -n "$RACK_DELETING" ]; then
      record PASS "live generated Topology protected while ResourceFlavors use it" "${RACK_TOPOLOGY} terminating behind finalizer"
    else
      record FAIL "live generated Topology protected while ResourceFlavors use it" "${RACK_TOPOLOGY} never entered terminating state"
    fi
  fi
  delete_source "${PREFIX}-native"
  if [ -n "${RACK_TOPOLOGY:-}" ]; then
    RACK_RELEASED=no
    for _ in $(seq 1 60); do
      ! kubectl get topology.kueue.x-k8s.io "$RACK_TOPOLOGY" >/dev/null 2>&1 && { RACK_RELEASED=yes; break; }
      sleep 2
    done
    if [ "$RACK_RELEASED" = yes ]; then
      record PASS "protected Topology released after source and flavors retire" "${RACK_TOPOLOGY} deleted"
    else
      record FAIL "protected Topology released after source and flavors retire" "${RACK_TOPOLOGY} still present"
    fi
  fi

  RETIRED_OK=no
  for _ in $(seq 1 60); do
    RETIRED_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$RACK_SOURCE_UID")"
    RETIRED_LEFT="$(tas_node_label_pairs topology.gpustack.ai/e2e-c15-rack=enabled topology.gpustack.ai/rack | awk 'NF > 1' | grep -c . || true)"
    NATIVE_PAIRS_AFTER="$(native_region_zone_pairs)"
    FOREIGN_AFTER="$(kubectl get node "$FOREIGN_NODE" -o json 2>/dev/null | jq -r --arg key "$FOREIGN_LABEL" '.metadata.labels[$key] // ""')"
    if [ -n "$RACK_SOURCE_UID" ] && [ "$RETIRED_FEATURES" = 0 ] && [ "$RETIRED_LEFT" = 0 ] \
      && [ -n "$NATIVE_PAIRS_BEFORE" ] && [ "$NATIVE_PAIRS_AFTER" = "$NATIVE_PAIRS_BEFORE" ] \
      && [ "$FOREIGN_AFTER" = kept ]; then
      RETIRED_OK=yes; break
    fi
    sleep 2
  done
  if [ "$RETIRED_OK" = yes ]; then
    record PASS "deleting snapshot source removes only what it published" "0 NodeFeatures, 0 rack labels; region/zone and foreign label kept"
  else
    record FAIL "deleting snapshot source removes only what it published" "features=${RETIRED_FEATURES:-?} rackLabels=${RETIRED_LEFT:-?} foreign=${FOREIGN_AFTER:-absent}"
  fi

  # Authenticated HTTPS webhook source lifecycle (Ready -> Stale -> Expired -> token rotation recovery).
  apply_webhook_server
  apply_webhook_source
  if wait_source "${PREFIX}-webhook" 'True|Observed' \
    && wait_profile_levels 'topology.gpustack.ai/webhook-domain kubernetes.io/hostname '; then
    record PASS "authenticated HTTPS webhook applies topology snapshot" "revision=$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}')"
  else
    record FAIL "authenticated HTTPS webhook applies topology snapshot" "source=$(source_condition "${PREFIX}-webhook" Ready)"
  fi

  WEBHOOK_SOURCE_UID="$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  WEBHOOK_EXPECTED="$(tas_snapshot_pairs "${TMP_CASE15}/snapshot.json" topology.gpustack.ai/webhook-domain)"
  WEBHOOK_EXPECTED_COUNT="$(printf '%s\n' "$WEBHOOK_EXPECTED" | grep -c . || true)"
  WEBHOOK_ACTUAL=""
  for _ in $(seq 1 60); do
    WEBHOOK_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
    WEBHOOK_ACTUAL="$(tas_node_label_pairs topology.gpustack.ai/e2e-c15-rack=enabled topology.gpustack.ai/webhook-domain)"
    [ "$WEBHOOK_EXPECTED_COUNT" -gt 0 ] && [ "$WEBHOOK_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] && [ "$WEBHOOK_ACTUAL" = "$WEBHOOK_EXPECTED" ] && break
    sleep 2
  done
  if [ "$WEBHOOK_EXPECTED_COUNT" -gt 0 ] && [ "$WEBHOOK_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] && [ "$WEBHOOK_ACTUAL" = "$WEBHOOK_EXPECTED" ]; then
    record PASS "webhook snapshot reaches every Node through its NodeFeatures" "${WEBHOOK_FEATURES} NodeFeatures"
  else
    record FAIL "webhook snapshot reaches every Node through its NodeFeatures" "features=${WEBHOOK_FEATURES:-?}/${WEBHOOK_EXPECTED_COUNT}"
  fi

  kubectl -n "$SYSTEM_NS" scale deploy/"${PREFIX}-webhook" --replicas=0 >/dev/null
  if wait_source "${PREFIX}-webhook" 'False|Stale'; then
    STALE_FEATURES="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
    STALE_LABELS="$(tas_node_label_pairs topology.gpustack.ai/e2e-c15-rack=enabled topology.gpustack.ai/webhook-domain | awk 'NF > 1' | grep -c . || true)"
    if [ "$STALE_FEATURES" = "$WEBHOOK_EXPECTED_COUNT" ] && [ "$STALE_LABELS" = "$WEBHOOK_EXPECTED_COUNT" ]; then
      record PASS "unavailable webhook is Stale and retains last snapshot" "${STALE_FEATURES} NodeFeatures, ${STALE_LABELS} labels retained"
    else
      record FAIL "unavailable webhook is Stale and retains last snapshot" "features=${STALE_FEATURES:-?} labels=${STALE_LABELS}"
    fi
  else
    record FAIL "unavailable webhook is Stale and retains last snapshot" "source=$(source_condition "${PREFIX}-webhook" Ready)"
  fi

  if wait_source "${PREFIX}-webhook" 'False|Expired'; then
    for _ in $(seq 1 60); do
      remaining="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.gpustack\.ai/webhook-domain}{"\n"}{end}' | grep -c . || true)"
      expired_features="$(tas_source_nodefeatures "$SYSTEM_NS" "$WEBHOOK_SOURCE_UID")"
      [ "$remaining" = 0 ] && [ "$expired_features" = 0 ] && break
      sleep 2
    done
    if [ "$remaining" = 0 ] && [ "$expired_features" = 0 ]; then
      record PASS "unavailable webhook expires and removes only owned labels" "0 labels, 0 NodeFeatures"
    else
      record FAIL "unavailable webhook expires and removes only owned labels" "${remaining} labels, ${expired_features:-?} NodeFeatures left"
    fi
  else
    record FAIL "unavailable webhook expires and removes only owned labels" "source=$(source_condition "${PREFIX}-webhook" Ready)"
  fi

  kubectl -n "$SYSTEM_NS" create secret generic "${PREFIX}-webhook-token" --from-literal=token=token-two --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  snapshot_yaml webhook-2 topology.gpustack.ai/webhook-domain domain > "${TMP_CASE15}/snapshot.json"
  kubectl -n "$SYSTEM_NS" create configmap "${PREFIX}-webhook-snapshot" --from-file=snapshot.json="${TMP_CASE15}/snapshot.json" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$SYSTEM_NS" scale deploy/"${PREFIX}-webhook" --replicas=1 >/dev/null
  kubectl -n "$SYSTEM_NS" rollout status deploy/"${PREFIX}-webhook" --timeout=180s >/dev/null
  for _ in $(seq 1 60); do
    [ "$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}' 2>/dev/null)" = webhook-2 ] && break
    sleep 2
  done
  if [ "$(kubectl get topologysource "${PREFIX}-webhook" -o jsonpath='{.status.lastSuccessfulRevision}' 2>/dev/null)" = webhook-2 ] \
    && [ "$(source_condition "${PREFIX}-webhook" Ready)" = 'True|Observed' ]; then
    record PASS "webhook credential rotation recovers with new revision" "revision webhook-2 observed"
  else
    record FAIL "webhook credential rotation recovers with new revision" "source=$(source_condition "${PREFIX}-webhook" Ready)"
  fi
  delete_source "${PREFIX}-webhook"

  # Restore native topology over all schedulable CPU Nodes for placement proofs.
  while read -r node; do
    [ -n "$node" ] && kubectl label node "$node" topology.gpustack.ai/e2e-c15-native=enabled --overwrite >/dev/null
  done <<EOF
$CPU_NODES
EOF
  apply_native_source
  if wait_source "${PREFIX}-native" 'True|Observed' \
    && wait_profile_levels 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname '; then
    record PASS "native topology restored for placement tests" "$SOURCE_TOPOLOGY"
  else
    record FAIL "native topology restored for placement tests" "source=$(source_condition "${PREFIX}-native" Ready)"
  fi

  PLACEMENT_PROFILE="${SOURCE_TOPOLOGY#gpustack-}"
  PROFILE_CONVERGED=no
  for _ in $(seq 1 90); do
    SELECTED_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-c15-native=enabled -o json 2>/dev/null | jq '[.items[]?] | length')"
    PROFILE_NODES="$(kubectl get nodes -l topology.gpustack.ai/e2e-c15-native=enabled -o json 2>/dev/null | jq --arg profile "$PLACEMENT_PROFILE" '[.items[]? | select(.metadata.labels["topology.gpustack.ai/profile"] == $profile)] | length')"
    PROFILE_FLAVORS="$(kubectl get resourceflavor.kueue.x-k8s.io -o json 2>/dev/null | jq --arg topology "$SOURCE_TOPOLOGY" '[.items[]? | select(.spec.topologyName == $topology)] | length')"
    if [ "$SELECTED_NODES" -gt 0 ] && [ "$PROFILE_NODES" = "$SELECTED_NODES" ] && [ "$PROFILE_FLAVORS" -gt 0 ]; then
      PROFILE_CONVERGED=yes; break
    fi
    sleep 2
  done
  if [ "$PROFILE_CONVERGED" = yes ]; then
    record PASS "restored profile converges before queue migration" "${PROFILE_NODES}/${SELECTED_NODES} Nodes, ${PROFILE_FLAVORS} flavor(s)"
  else
    record FAIL "restored profile converges before queue migration" "selected=${SELECTED_NODES:-0} profiled=${PROFILE_NODES:-0} flavors=${PROFILE_FLAVORS:-0}"
  fi

  [ -n "$IT" ] || IT="$(tas_md_usable_instance_type)"
  CQ="$(tas_md_cq_of_it "$NS" "$IT")"
  if [ -n "$CQ" ] && wait_cq_topology_plan "$CQ" "$SOURCE_TOPOLOGY" contains "$RACK_TOPOLOGY" && [ "$CQ_PLAN_UID" = "$STABLE_CQ_UID" ]; then
    record PASS "restored profile converges on identity-stable ClusterQueue" "${CQ} uid=${CQ_PLAN_UID}"
  else
    record FAIL "restored profile converges on identity-stable ClusterQueue" "cq=${CQ:-missing} beforeUID=${STABLE_CQ_UID:-missing} afterUID=${CQ_PLAN_UID:-missing}"
  fi

  # --- Hand-built TAS fixture proofs ---
  TAS_OK=yes
  if ! kubectl get crd topologies.kueue.x-k8s.io >/dev/null 2>&1; then
    record SKIP "cluster serves Kueue Topology API" "no topologies.kueue.x-k8s.io CRD"
    TAS_OK=no
  elif [ "$TAS_NODES" -lt 3 ] || [ "$TAS_MAX_ZONE_NODES" -ge 3 ] || [ "$TAS_MIN_CPU_M" -le 0 ] || [ "$TAS_MIN_MEM_MI" -le 0 ]; then
    record SKIP "zone shape can carry both placement proofs" "nodes=${TAS_NODES} maxZone=${TAS_MAX_ZONE_NODES} (${TAS_ZONES_SUMMARY:-none})"
    TAS_OK=no
  else
    record PASS "zone shape can carry both placement proofs" "${TAS_NODES} Nodes (${TAS_ZONES_SUMMARY})"
  fi

  if [ "$TAS_OK" = yes ]; then
    POD_CPU_M="$(tas_pod_cpu_m)"
    POD_MEM_MI="$(tas_pod_mem_mi)"
    QUOTA_CPU_M=$((3 * POD_CPU_M))
    QUOTA_MEM_MI=$((3 * POD_MEM_MI))
    tas_apply_fixtures "$PREFIX" "$NS" "${QUOTA_CPU_M}m" "${QUOTA_MEM_MI}Mi"

    CQ_ACTIVE=no
    for _ in $(seq 1 30); do
      [ "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" -o jsonpath='{range .status.conditions[?(@.type=="Active")]}{.status}{end}' 2>/dev/null)" = True ] && { CQ_ACTIVE=yes; break; }
      sleep 2
    done
    if [ "$CQ_ACTIVE" = yes ]; then
      record PASS "hand-built ClusterQueue turns Active" "${PREFIX}-cq"
    else
      record FAIL "hand-built ClusterQueue turns Active" "not Active within 60s"
      TAS_OK=no
    fi
  fi

  if [ "$TAS_OK" = yes ]; then
    tas_apply_zone_job "${PREFIX}-fit" "$NS" "${PREFIX}-lq" 2 "$POD_CPU_M" "$POD_MEM_MI"
    FIT_WL=""; FIT_ADMITTED=no
    for _ in $(seq 1 45); do
      FIT_WL="$(tas_job_workload "$NS" "${PREFIX}-fit")"
      [ -n "$FIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$FIT_WL")" ] && { FIT_ADMITTED=yes; break; }
      sleep 2
    done
    if [ "$FIT_ADMITTED" = yes ]; then
      record PASS "fitting 2-Pod group admitted into hand-built TAS queue" "${PREFIX}-fit"
    else
      record FAIL "fitting 2-Pod group admitted into hand-built TAS queue" "no admission within 90s"
    fi

    FIT_LEVELS="$(tas_wl_topology_levels "$NS" "$FIT_WL")"
    if [ -n "$FIT_LEVELS" ]; then
      record PASS "hand-built admission carries topologyAssignment" "levels=${FIT_LEVELS}"
    else
      record FAIL "hand-built admission carries topologyAssignment" "admitted without topologyAssignment"
    fi

    FIT_ROWS=""; FIT_BOUND=no
    for _ in $(seq 1 45); do
      FIT_ROWS="$(tas_pod_nodes "$NS" "batch.kubernetes.io/job-name=${PREFIX}-fit")"
      [ "$(printf '%s\n' "$FIT_ROWS" | grep -c . || true)" = 2 ] && [ -z "$(printf '%s\n' "$FIT_ROWS" | grep ' $' || true)" ] && { FIT_BOUND=yes; break; }
      sleep 2
    done
    if [ "$FIT_BOUND" != yes ]; then
      record FAIL "both Pods of zone-required group bind inside ONE zone" "not both bound"
    else
      ZONES_SEEN=""; ZONE_READ_FAIL=no
      while read -r _pod node; do
        z="$(tas_node_zone "$node")"
        [ -n "$z" ] || { ZONE_READ_FAIL=yes; break; }
        ZONES_SEEN="${ZONES_SEEN}${z} "
      done <<EOF
$FIT_ROWS
EOF
      if [ "$ZONE_READ_FAIL" != yes ] && [ "$(printf '%s' "$ZONES_SEEN" | xargs -n1 | sort -u | grep -c . || true)" = 1 ]; then
        record PASS "both Pods of zone-required group bind inside ONE zone" "zone=${ZONES_SEEN}"
      else
        record FAIL "both Pods of zone-required group bind inside ONE zone" "zones=${ZONES_SEEN:-missing}"
      fi
    fi

    QUOTA_BACK_CPU="$(tas_millis "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="cpu")].nominalQuota}' 2>/dev/null)")"
    QUOTA_BACK_MEM="$(tas_mib "$(kubectl get clusterqueue.kueue.x-k8s.io "${PREFIX}-cq" -o jsonpath='{.spec.resourceGroups[0].flavors[0].resources[?(@.name=="memory")].nominalQuota}' 2>/dev/null)")"
    NEED_CPU=$((3 * POD_CPU_M)); NEED_MEM=$((3 * POD_MEM_MI))
    if [ "$QUOTA_BACK_CPU" -ge "$NEED_CPU" ] && [ "$QUOTA_BACK_MEM" -ge "$NEED_MEM" ] && [ "$TAS_NODES" -ge 3 ] && [ "$TAS_MAX_ZONE_NODES" -le 2 ]; then
      record PASS "refused group fits in aggregate (quota & cluster)" "quota=${QUOTA_BACK_CPU}m/${QUOTA_BACK_MEM}Mi, ${TAS_NODES} Nodes"
    else
      record FAIL "refused group fits in aggregate (quota & cluster)" "quota=${QUOTA_BACK_CPU}m/${QUOTA_BACK_MEM}Mi need=${NEED_CPU}m/${NEED_MEM}Mi"
    fi

    tas_apply_zone_job "${PREFIX}-refuse" "$NS" "${PREFIX}-lq" 3 "$POD_CPU_M" "$POD_MEM_MI"
    REF_COMPOSED=no; REF_WL=""
    for _ in $(seq 1 20); do
      REF_WL="$(tas_job_workload "$NS" "${PREFIX}-refuse")"
      [ -n "$REF_WL" ] && { REF_COMPOSED=yes; break; }
      sleep 2
    done
    if [ "$REF_COMPOSED" != yes ]; then
      record FAIL "cross-zone group stays unadmitted (two intervals)" "no Workload composed"
      record FAIL "no Pod of cross-zone group binds (two intervals)" "no Workload composed"
    else
      refuse_sample() {
        REF_READABLE=no; tas_wl_readable "$NS" "$REF_WL" && REF_READABLE=yes
        REF_ADMITTED="$(tas_wl_admitted "$NS" "$REF_WL")"
        REF_RESERVED="$(tas_wl_condition "$NS" "$REF_WL" QuotaReserved)"
        REF_BOUND="$(kubectl -n "$NS" get pods -l "batch.kubernetes.io/job-name=${PREFIX}-refuse" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
      }
      refuse_sample
      REF_FIRST_ADMITTED="$REF_ADMITTED"; REF_FIRST_RESERVED="$REF_RESERVED"; REF_FIRST_BOUND="$REF_BOUND"; REF_FIRST_READABLE="$REF_READABLE"
      sleep "$INTERVAL"
      refuse_sample
      if [ "$REF_FIRST_READABLE" = yes ] && [ "$REF_READABLE" = yes ] && [ -z "$REF_FIRST_ADMITTED" ] && [ "$REF_FIRST_RESERVED" != True ] && [ -z "$REF_ADMITTED" ] && [ "$REF_RESERVED" != True ]; then
        record PASS "cross-zone group stays unadmitted (two intervals)" "two samples ${INTERVAL}s apart: unadmitted"
      else
        record FAIL "cross-zone group stays unadmitted (two intervals)" "admitted='${REF_ADMITTED:0:60}' reserved=${REF_RESERVED:-absent}"
      fi
      if [ "$REF_FIRST_BOUND" = 0 ] && [ "$REF_BOUND" = 0 ]; then
        record PASS "no Pod of cross-zone group binds (two intervals)" "0 of 3 bound"
      else
        record FAIL "no Pod of cross-zone group binds (two intervals)" "${REF_BOUND} Pod(s) bound"
      fi
    fi
    kubectl -n "$NS" delete job.batch "${PREFIX}-fit" "${PREFIX}-refuse" --ignore-not-found --wait=false >/dev/null 2>&1
  fi

  # --- Operator-managed TAS queue & ModelDeployment joint admission ---
  [ -n "$IT" ] || IT="$(tas_md_usable_instance_type)"
  if [ -z "$IT" ]; then
    echo "[case-15] no InstanceType in cluster; run case-1 first" >&2
    exit_input_required
  fi
  CQ="$(tas_md_cq_of_it "$NS" "$IT")"
  if [ -z "$CQ" ]; then
    echo "[case-15] InstanceType ${IT} names no reachable ClusterQueue in ${NS}" >&2
    exit_input_required
  fi

  # shellcheck disable=SC2016 # Dollar names are jq variables.
  REACH_JQ='
    def tolerated($tols): . as $t | any($tols[]?;
      ((.effect // "") == "" or .effect == $t.effect)
      and (if (.operator // "Equal") == "Exists" then ((.key // "") == "" or .key == $t.key)
           else (.key == $t.key and (.value // "") == ($t.value // "")) end));
    def reaches($f): .spec.unschedulable != true
      and (.metadata.labels as $l | all(($f.spec.nodeLabels // {}) | to_entries[]; $l[.key] == .value))
      and all((.spec.taints // [])[] | select(.effect == "NoSchedule" or .effect == "NoExecute");
        tolerated($f.spec.tolerations // []));'

  queue_node_room() {
    # shellcheck disable=SC2016 # Dollar names are jq variables.
    kubectl get clusterqueue.kueue.x-k8s.io "$1" -o json 2>/dev/null | jq -r \
      --slurpfile nodes <(kubectl get nodes -o json 2>/dev/null) \
      --slurpfile flavors <(kubectl get resourceflavors.kueue.x-k8s.io -o json 2>/dev/null) \
      --slurpfile pods <(kubectl get pods -A -o json 2>/dev/null) "${REACH_JQ}"'
      def cpu_m: if . == null then 0 else tostring
        | if endswith("m") then (.[:-1] | tonumber) else (tonumber * 1000) end end;
      def mem_mi: if . == null then 0 else tostring | capture("^(?<n>[0-9.]+)(?<u>[A-Za-z]*)$") as $q
        | ($q.n | tonumber) * ({"": 1, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "Ki": 1024,
            "Mi": 1048576, "Gi": 1073741824, "Ti": 1099511627776}[$q.u] // 0) / 1048576 end;
      def effective(f):
        ([.spec.containers[]? | .resources.requests // {} | f] | add // 0) as $main
        | ([.spec.initContainers[]? | select(.restartPolicy == "Always") | .resources.requests // {} | f] | add // 0) as $side
        | ([.spec.initContainers[]? | select(.restartPolicy != "Always") | .resources.requests // {} | f] | max // 0) as $init
        | ([$main + $side, $init] | max) + (.spec.overhead // {} | f);
      [.spec.resourceGroups[]?.flavors[]?.name] as $names
      | [$flavors[0].items[] | select(.metadata.name as $n | any($names[]; . == $n))] as $queue
      | $nodes[0].items[] | . as $node | select(any($queue[]; . as $f | $node | reaches($f)))
      | .metadata.name as $name
      | [$pods[0].items[] | select(.spec.nodeName == $name)
          | select(.status.phase != "Succeeded" and .status.phase != "Failed")] as $bound
      | [$name, (.metadata.labels["topology.kubernetes.io/zone"] // ""),
         ((.status.allocatable.cpu | cpu_m) - ([$bound[] | effective(.cpu | cpu_m)] | add // 0) | floor | tostring),
         ((.status.allocatable.memory | mem_mi) - ([$bound[] | effective(.memory | mem_mi)] | add // 0) | floor | tostring)]
      | join("|")' 2>/dev/null
  }

  ballast_delete() {
    kubectl -n "$SYSTEM_NS" delete pods -l "${BALLAST_LABEL}=${PREFIX}" --ignore-not-found --wait=false >/dev/null 2>&1
    for _ in $(seq 1 45); do
      [ "$(kubectl -n "$SYSTEM_NS" get pods -l "${BALLAST_LABEL}=${PREFIX}" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && return 0
      sleep 2
    done
    return 1
  }

  ballast_apply() {
    local cq="$1" req_cpu="$2" req_mem="$3" name zone cpu mem size slots idx=0 bound zones=""
    ballast_delete
    while IFS='|' read -r name zone cpu mem; do
      [ -n "$name" ] || continue
      size=$((cpu - req_cpu * 3 / 2))
      [ "$size" -gt 0 ] || continue
      idx=$((idx + 1))
      cat <<YAML | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PREFIX}-ballast-${idx}
  namespace: ${SYSTEM_NS}
  labels:
    ${BALLAST_LABEL}: "${PREFIX}"
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchFields:
              - {key: metadata.name, operator: In, values: ["${name}"]}
  tolerations:
    - operator: Exists
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.10
      resources:
        requests:
          cpu: ${size}m
YAML
    done <<EOF
$(queue_node_room "$cq")
EOF
    for _ in $(seq 1 30); do
      bound="$(kubectl -n "$SYSTEM_NS" get pods -l "${BALLAST_LABEL}=${PREFIX}" -o json 2>/dev/null | jq '[.items[]? | select((.spec.nodeName // "") != "")] | length')"
      [ "${bound:-0}" = "$idx" ] && break
      sleep 2
    done
    SHAPE_OK=yes; SHAPE_ONE=""; SHAPE_READING=""
    while IFS='|' read -r name zone cpu mem; do
      [ -n "$name" ] || continue
      slots=0
      if [ "$cpu" -gt 0 ] && [ "$mem" -gt 0 ]; then
        slots=$((cpu / req_cpu))
        [ $((mem / req_mem)) -lt "$slots" ] && slots=$((mem / req_mem))
      fi
      SHAPE_READING="${SHAPE_READING}${name}=${slots} "
      [ "$slots" -ge 2 ] && SHAPE_OK=no
      if [ "$slots" = 1 ]; then
        SHAPE_ONE="${SHAPE_ONE}${name} "
        [ -n "$zone" ] && zones="${zones}${zone}
"
      fi
    done <<EOF
$(queue_node_room "$cq")
EOF
    [ -n "$SHAPE_ONE" ] || SHAPE_OK=no
    SHAPE_ZONED="$(printf '%s' "$zones" | grep -c . || true)"
    SHAPE_MAX_ZONE="$(printf '%s' "$zones" | sort | uniq -c | awk '$1 > m {m = $1} END {print m + 0}')"
    SHAPE_READING="${SHAPE_READING}(ballast ${bound:-0} of ${idx} bound)"
  }

  tas_md_apply "${PREFIX}-zone-fit" "$NS" "$BINDING" \
    "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 2 topology.kubernetes.io/zone)"
  ZONE_FIT_WL=""
  if tas_md_wait_pods "$NS" "${PREFIX}-zone-fit" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-fit" 1; then
    ZONE_FIT_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-fit" | sed -n '1p')"
  fi
  ZONE_FIT_ADMITTED=no
  for _ in $(seq 1 60); do
    [ -n "$ZONE_FIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$ZONE_FIT_WL")" ] && { ZONE_FIT_ADMITTED=yes; break; }
    sleep 2
  done
  if [ "$ZONE_FIT_ADMITTED" = yes ]; then
    required="$(kubectl -n "$NS" get workload "$ZONE_FIT_WL" -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{" "}{end}' 2>/dev/null)"
    assignment="$(tas_wl_topology_levels "$NS" "$ZONE_FIT_WL")"
    if [ "$required" = 'topology.kubernetes.io/zone ' ] && [ -n "$assignment" ]; then
      record PASS "size-2 ModelDeployment reaches Kueue with zone-required PodSet" "${ZONE_FIT_WL}: assignment=${assignment}"
    else
      record FAIL "size-2 ModelDeployment reaches Kueue with zone-required PodSet" "required=${required:-missing} assignment=${assignment:-missing}"
    fi
  else
    record FAIL "size-2 ModelDeployment reaches Kueue with zone-required PodSet" "wl=${ZONE_FIT_WL:-missing} unadmitted"
  fi

  ZONE_FIT_ROWS=""
  for _ in $(seq 1 60); do
    ZONE_FIT_ROWS="$(tas_pod_nodes "$NS" "app.kubernetes.io/instance=${PREFIX}-zone-fit")"
    tas_rows_all_bound "$ZONE_FIT_ROWS" 2 && break
    sleep 2
  done
  ZONE_FIT_ZONES=""
  while read -r _pod node; do [ -n "$node" ] && ZONE_FIT_ZONES="${ZONE_FIT_ZONES}$(tas_node_zone "$node") "; done <<EOF
$ZONE_FIT_ROWS
EOF
  if tas_rows_all_bound "$ZONE_FIT_ROWS" 2 && [ "$(printf '%s' "$ZONE_FIT_ZONES" | wc -w | tr -d ' ')" = 2 ] \
    && [ "$(printf '%s' "$ZONE_FIT_ZONES" | xargs -n1 | sort -u | grep -c . || true)" = 1 ]; then
    record PASS "both Pods of size-2 ModelDeployment bind in one observed zone" "zone=${ZONE_FIT_ZONES}"
  else
    record FAIL "both Pods of size-2 ModelDeployment bind in one observed zone" "zones=${ZONE_FIT_ZONES:-missing}"
  fi

  ZONE_REQ_CPU="$(tas_millis "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-fit" -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' 2>/dev/null)")"
  ZONE_REQ_MEM="$(tas_mib "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-fit" -o jsonpath='{.items[0].spec.containers[0].resources.requests.memory}' 2>/dev/null)")"
  tas_md_force_release "$NS" "${PREFIX}-zone-fit"
  kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-fit" --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-fit" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break; sleep 2; done

  ZONE_SHAPE_OK=no
  if [ "$ZONE_REQ_CPU" -le 0 ] || [ "$ZONE_REQ_MEM" -le 0 ] || [ -z "$CQ" ]; then
    record SKIP "every queue Node holds one replica for zone refusal" "req=${ZONE_REQ_CPU}m/${ZONE_REQ_MEM}Mi cq=${CQ:-missing}"
  else
    ballast_apply "$CQ" "$ZONE_REQ_CPU" "$ZONE_REQ_MEM"
    if [ "$SHAPE_OK" = yes ] && [ "$SHAPE_ZONED" -ge 3 ] && [ "$SHAPE_MAX_ZONE" -le 2 ]; then
      ZONE_SHAPE_OK=yes
      record PASS "every queue Node holds one replica for zone refusal" "${SHAPE_READING}"
    else
      record SKIP "every queue Node holds one replica for zone refusal" "${SHAPE_READING}; zoned=${SHAPE_ZONED} maxZone=${SHAPE_MAX_ZONE}"
    fi
  fi

  tas_md_apply "${PREFIX}-zone-control" "$NS" "$BINDING" "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 3)"
  ZONE_CONTROL_WL=""
  if tas_md_wait_pods "$NS" "${PREFIX}-zone-control" 3 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-control" 1; then
    ZONE_CONTROL_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-control" | sed -n '1p')"
  fi
  ZONE_CONTROL_ADMITTED=no
  for _ in $(seq 1 60); do
    [ -n "$ZONE_CONTROL_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$ZONE_CONTROL_WL")" ] && { ZONE_CONTROL_ADMITTED=yes; break; }
    sleep 2
  done
  if [ "$ZONE_CONTROL_ADMITTED" = yes ]; then
    record PASS "unconstrained size-3 control group admits on same queue" "${ZONE_CONTROL_WL} admitted"
  else
    record FAIL "unconstrained size-3 control group admits on same queue" "wl=${ZONE_CONTROL_WL:-missing} unadmitted"
  fi
  tas_md_force_release "$NS" "${PREFIX}-zone-control"
  kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-control" --wait=false >/dev/null 2>&1
  for _ in $(seq 1 60); do [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-control" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break; sleep 2; done

  tas_md_apply "${PREFIX}-zone-refuse" "$NS" "$BINDING" "$(tas_md_role_block server '' "$IT" 1 "$IMAGE" 3 topology.kubernetes.io/zone)"
  ZONE_REFUSE_WL=""
  if tas_md_wait_pods "$NS" "${PREFIX}-zone-refuse" 3 && tas_md_wait_workloads "$NS" "${PREFIX}-zone-refuse" 1; then
    ZONE_REFUSE_WL="$(tas_md_workloads "$NS" "${PREFIX}-zone-refuse" | sed -n '1p')"
  fi
  zone_refuse_sample() {
    ZONE_REFUSE_READABLE=no; tas_wl_readable "$NS" "$ZONE_REFUSE_WL" && ZONE_REFUSE_READABLE=yes
    ZONE_REFUSE_ADMISSION="$(tas_wl_admitted "$NS" "$ZONE_REFUSE_WL")"
    ZONE_REFUSE_RESERVED="$(tas_wl_condition "$NS" "$ZONE_REFUSE_WL" QuotaReserved)"
    ZONE_REFUSE_BOUND="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-zone-refuse" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
  }
  if [ -n "$ZONE_REFUSE_WL" ]; then
    zone_refuse_sample
    ZONE_REFUSE_FIRST_ADMISSION="$ZONE_REFUSE_ADMISSION"; ZONE_REFUSE_FIRST_RESERVED="$ZONE_REFUSE_RESERVED"
    ZONE_REFUSE_FIRST_BOUND="$ZONE_REFUSE_BOUND"; ZONE_REFUSE_FIRST_READABLE="$ZONE_REFUSE_READABLE"
    sleep "$INTERVAL"
    zone_refuse_sample
  fi
  if [ "$ZONE_SHAPE_OK" != yes ]; then
    record SKIP "size-3 zone request stays Pending despite aggregate Nodes" "no zone-refusal shape"
  elif [ -n "$ZONE_REFUSE_WL" ] && [ "$ZONE_CONTROL_ADMITTED" = yes ] \
    && [ "${ZONE_REFUSE_FIRST_READABLE:-no}" = yes ] && [ "${ZONE_REFUSE_READABLE:-no}" = yes ] \
    && [ -z "$ZONE_REFUSE_FIRST_ADMISSION" ] && [ "$ZONE_REFUSE_FIRST_RESERVED" != True ] && [ "$ZONE_REFUSE_FIRST_BOUND" = 0 ] \
    && [ -z "$ZONE_REFUSE_ADMISSION" ] && [ "$ZONE_REFUSE_RESERVED" != True ] && [ "$ZONE_REFUSE_BOUND" = 0 ] \
    && [ "$TAS_NODES" -ge 3 ] && [ "$TAS_MAX_ZONE_NODES" -le 2 ]; then
    record PASS "size-3 zone request stays Pending despite aggregate Nodes" "two samples ${INTERVAL}s apart: unadmitted, 0 bound"
  else
    record FAIL "size-3 zone request stays Pending despite aggregate Nodes" "wl=${ZONE_REFUSE_WL:-missing} admission=${ZONE_REFUSE_ADMISSION:-none} bound=${ZONE_REFUSE_BOUND:-0}"
  fi
  tas_md_force_release "$NS" "${PREFIX}-zone-refuse"
  kubectl -n "$NS" delete modeldeployment "${PREFIX}-zone-refuse" --wait=false >/dev/null 2>&1
  ballast_delete

  tas_md_apply "${PREFIX}-omit" "$NS" "$BINDING" "$(tas_md_role_block server '' "$IT" 1 "$IMAGE")"
  OMIT_WL=""
  if tas_md_wait_pods "$NS" "${PREFIX}-omit" 1 && tas_md_wait_workloads "$NS" "${PREFIX}-omit" 1; then
    OMIT_WL="$(tas_md_workloads "$NS" "${PREFIX}-omit" | sed -n '1p')"
  fi
  for _ in $(seq 1 45); do [ -n "$OMIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$OMIT_WL")" ] && break; sleep 2; done
  OMIT_REQUIRED="$(kubectl -n "$NS" get workload "$OMIT_WL" -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{end}' 2>/dev/null)"
  if [ -n "$OMIT_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$OMIT_WL")" ] && [ -z "$OMIT_REQUIRED" ]; then
    record PASS "omitting topology remains admitted without required level" "${OMIT_WL} admitted"
  else
    record FAIL "omitting topology remains admitted without required level" "wl=${OMIT_WL:-missing} required=${OMIT_REQUIRED:-empty}"
  fi
  tas_md_force_release "$NS" "${PREFIX}-omit"
  kubectl -n "$NS" delete modeldeployment "${PREFIX}-omit" --wait=false >/dev/null 2>&1

  PRIOR_PODS_RELEASED=no
  for _ in $(seq 1 60); do
    PRIOR_PODS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance in (${PREFIX}-zone-fit,${PREFIX}-zone-refuse,${PREFIX}-omit)" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    IT_CPU_REMAINING="$(kubectl get instancetype.worker.gpustack.ai "$IT" -o jsonpath='{.status.cpu.remaining}' 2>/dev/null)"
    if [ "$PRIOR_PODS" = 0 ] && [ "${IT_CPU_REMAINING:-0}" -gt 0 ]; then PRIOR_PODS_RELEASED=yes; break; fi
    sleep 2
  done
  if [ "$PRIOR_PODS_RELEASED" = yes ]; then
    record PASS "ModelDeployment probes release live capacity before Instance probe" "0 prior Pods; CPU remaining=${IT_CPU_REMAINING}"
  else
    record FAIL "ModelDeployment probes release live capacity before Instance probe" "pods=${PRIOR_PODS:-?} cpu=${IT_CPU_REMAINING:-?}"
  fi

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
    INSTANCE_WL="$(kubectl -n "$NS" get pod "${PREFIX}-instance" -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
    [ "$INSTANCE_PHASE" = Ready ] && [ -n "$INSTANCE_NODE" ] && [ -n "$INSTANCE_WL" ] && [ -n "$(tas_wl_admitted "$NS" "$INSTANCE_WL")" ] && { INSTANCE_READY=yes; break; }
    sleep 2
  done
  INSTANCE_REQUIRED="$(kubectl -n "$NS" get workload "$INSTANCE_WL" -o jsonpath='{range .spec.podSets[*]}{.topologyRequest.required}{end}' 2>/dev/null)"
  if [ "$INSTANCE_READY" = yes ] && [ -z "$INSTANCE_REQUIRED" ]; then
    record PASS "plain Instance remains admitted without topology awareness" "node=${INSTANCE_NODE} wl=${INSTANCE_WL}"
  else
    record FAIL "plain Instance remains admitted without topology awareness" "phase=${INSTANCE_PHASE:-missing} wl=${INSTANCE_WL:-missing}"
  fi
  [ -n "$INSTANCE_WL" ] && kubectl -n "$NS" delete workload "$INSTANCE_WL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete instance "${PREFIX}-instance" --ignore-not-found --wait=false >/dev/null 2>&1

  # Multi-role joint admission (fitting + symmetric shortage).
  PRE_RAW="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o jsonpath='{.apiVersion}|{.spec.cohort}{.spec.cohortName}|{range .spec.resourceGroups[*].flavors[*]}{.name}{" "}{end}' 2>/dev/null)"
  PRE_VER="${PRE_RAW%%|*}"; PRE_REST="${PRE_RAW#*|}"; PRE_COHORT="${PRE_REST%%|*}"
  PRE_FLAVORS="$(printf '%s' "${PRE_REST#*|}" | wc -w | tr -d ' ')"
  # shellcheck disable=SC2016 # Dollar names are jq variables.
  PRE_REACHABLE="$(kubectl get clusterqueue.kueue.x-k8s.io "$CQ" -o json | jq -r \
    --slurpfile nodes <(kubectl get nodes -o json) \
    --slurpfile flavors <(kubectl get resourceflavors.kueue.x-k8s.io -o json) "${REACH_JQ}"'
      [.spec.resourceGroups[]?.flavors[]?.name as $name |
        $flavors[0].items[] | select(.metadata.name == $name) as $flavor |
        select(any($nodes[0].items[]; reaches($flavor))) |
        .metadata.name] | unique | .[]' 2>/dev/null)"
  PRE_REACHABLE_FLAVORS="$(printf '%s\n' "$PRE_REACHABLE" | grep -c . || true)"

  MD_NEG=yes
  if [ -z "$PRE_RAW" ] || { [ "$PRE_VER" != "kueue.x-k8s.io/v1beta1" ] && [ "$PRE_VER" != "kueue.x-k8s.io/v1beta2" ]; } || [ -n "$PRE_COHORT" ] || [ "$PRE_REACHABLE_FLAVORS" -lt 1 ]; then
    record SKIP "shortage cannot be relieved by borrowing or another flavor" "cq=${CQ} ver=${PRE_VER:-?} cohort=${PRE_COHORT:-none} reachable=${PRE_REACHABLE_FLAVORS}"
    MD_NEG=no
  else
    record PASS "shortage cannot be relieved by borrowing or another flavor" "${CQ} (${PRE_VER}) no cohort, ${PRE_REACHABLE_FLAVORS}/${PRE_FLAVORS} reachable"
  fi

  tas_md_apply "${PREFIX}-md" "$NS" "$BINDING" \
"$(tas_md_role_block prefill Prefill "$IT" 1 "$IMAGE")
$(tas_md_role_block decode Decode "$IT" 1 "$IMAGE")"
  MD_WLS=""
  if tas_md_wait_pods "$NS" "${PREFIX}-md" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-md" 2; then
    MD_WLS="$(tas_md_workloads "$NS" "${PREFIX}-md")"
  fi
  if [ "$(printf '%s\n' "$MD_WLS" | grep -c . || true)" != 2 ]; then
    record FAIL "two Workloads attributable to deployment through owner Pods" "$(printf '%s' "$MD_WLS" | grep -c . || true) found"
    record FAIL "both roles admitted together" "no 2-Workload set"
    record FAIL "joint admission check reads Ready on every Workload" "no 2-Workload set"
    MD_NEG=no
  else
    record PASS "two Workloads attributable to deployment through owner Pods" "$(printf '%s' "$MD_WLS" | tr '\n' ' ')"
    MD_ADMITTED=no
    for _ in $(seq 1 40); do
      BAD=0
      while read -r wl; do [ -n "$(tas_wl_admitted "$NS" "$wl")" ] || BAD=1; done <<EOF
$MD_WLS
EOF
      [ "$BAD" = 0 ] && { MD_ADMITTED=yes; break; }
      sleep 3
    done
    if [ "$MD_ADMITTED" = yes ]; then
      record PASS "both roles admitted together" "all Workloads admitted"
    else
      record FAIL "both roles admitted together" "some Workload unadmitted"
    fi
    CHECKS_READY=yes; CHECKS_READ=""; CHECK_IDX=0
    while read -r wl; do
      CHECK_IDX=$((CHECK_IDX + 1))
      s="$(tas_wl_check_state "$NS" "$wl" "$JOINT_CHECK")"
      CHECKS_READ="${CHECKS_READ}wl${CHECK_IDX}=${s:-absent} "
      [ "$s" = Ready ] || CHECKS_READY=no
    done <<EOF
$MD_WLS
EOF
    if [ "$CHECKS_READY" = yes ]; then
      record PASS "joint admission check reads Ready on every Workload" "${CHECKS_READ}"
    else
      record FAIL "joint admission check reads Ready on every Workload" "${CHECKS_READ}"
    fi
  fi

  REQ_RAW="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-md" -o jsonpath='{.items[0].spec.containers[0].resources.requests.cpu}' 2>/dev/null)"
  REQ="$(tas_millis "$REQ_RAW")"
  REQ_MEM="$(tas_mib "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-md" -o jsonpath='{.items[0].spec.containers[0].resources.requests.memory}' 2>/dev/null)")"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${PREFIX}-md" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 30); do [ "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-md" --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0 ] && break; sleep 2; done
  tas_md_force_release "$NS" "${PREFIX}-md"

  FILL=0
  if [ "$MD_NEG" = yes ] && { [ "$REQ" -le 0 ] || [ "$REQ_MEM" -le 0 ]; }; then
    record SKIP "every live hostname domain can be occupied" "req=${REQ_RAW} mem=${REQ_MEM}Mi"
    MD_NEG=no
  fi
  if [ "$MD_NEG" = yes ]; then
    ballast_apply "$CQ" "$REQ" "$REQ_MEM"
    FILL="$(printf '%s' "$SHAPE_ONE" | wc -w | tr -d ' ')"
    if [ "$SHAPE_OK" = yes ]; then
      record PASS "every queue Node holds one replica for shortage" "${SHAPE_READING}"
    else
      record SKIP "every queue Node holds one replica for shortage" "${SHAPE_READING}"
      MD_NEG=no
    fi
  fi
  if [ "$MD_NEG" = yes ]; then
    tas_md_apply "${PREFIX}-filler" "$NS" "$BINDING" "$(tas_md_role_block bulk '' "$IT" "$FILL" "$IMAGE")"
    FILLER_WLS=""; FILLER_OK=no
    if tas_md_wait_workloads "$NS" "${PREFIX}-filler" "$FILL"; then
      FILLER_WLS="$(tas_md_workloads "$NS" "${PREFIX}-filler")"
      for _ in $(seq 1 40); do
        FILLER_ADMITTED=0
        while read -r filler_wl; do
          [ -n "$filler_wl" ] && [ -n "$(tas_wl_admitted "$NS" "$filler_wl")" ] && FILLER_ADMITTED=$((FILLER_ADMITTED + 1))
        done <<EOF
$FILLER_WLS
EOF
        [ "$FILLER_ADMITTED" = "$FILL" ] && { FILLER_OK=yes; break; }
        sleep 3
      done
    fi
    if [ "$FILLER_OK" = yes ]; then
      record PASS "every live hostname domain is occupied" "${FILL} Nodes filled"
    else
      record FAIL "every live hostname domain is occupied" "${FILLER_ADMITTED:-0}/${FILL} filler Workloads admitted"
      MD_NEG=no
    fi
  fi

  if [ "$MD_NEG" = yes ]; then
    tas_md_apply "${PREFIX}-starved" "$NS" "$BINDING" \
"$(tas_md_role_block prefill Prefill "$IT" 1 "$IMAGE")
$(tas_md_role_block decode Decode "$IT" 1 "$IMAGE")"
    STARVED_WLS=""
    if tas_md_wait_pods "$NS" "${PREFIX}-starved" 2 && tas_md_wait_workloads "$NS" "${PREFIX}-starved" 2; then
      STARVED_WLS="$(tas_md_workloads "$NS" "${PREFIX}-starved")"
    fi
    N_WLS="$(printf '%s\n' "$STARVED_WLS" | grep -c . || true)"
    if [ "$N_WLS" != 2 ]; then
      record FAIL "every Workload of short deployment stays unadmitted (two intervals)" "${N_WLS} Workloads found"
      record FAIL "joint admission check holds every group Pending (two intervals)" "no 2-Workload set"
      record FAIL "no role Pod of short deployment binds (two intervals)" "no 2-Workload set"
    else
      starved_sample() {
        STARVED_ADMITTED=""; STARVED_RESERVED=""; STARVED_UNREADABLE=""; STARVED_CHECKS=""; STARVED_CHECK_BAD=""
        local wl pods idx=0 s
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
        pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PREFIX}-starved" -o json 2>/dev/null)"
        STARVED_PODS="$(printf '%s' "$pods" | jq '[.items[]?] | length')"
        STARVED_BOUND="$(printf '%s' "$pods" | jq '[.items[]? | select(.spec.nodeName != null and .spec.nodeName != "")] | length')"
      }
      starved_sample
      STARVED_FIRST_ADMITTED="$STARVED_ADMITTED"; STARVED_FIRST_RESERVED="$STARVED_RESERVED"
      STARVED_FIRST_CHECK_BAD="$STARVED_CHECK_BAD"; STARVED_FIRST_PODS="$STARVED_PODS"
      STARVED_FIRST_BOUND="$STARVED_BOUND"; STARVED_FIRST_UNREADABLE="$STARVED_UNREADABLE"
      sleep "$INTERVAL"
      starved_sample
      if [ -z "$STARVED_FIRST_ADMITTED$STARVED_FIRST_RESERVED$STARVED_ADMITTED$STARVED_RESERVED" ] && [ -z "$STARVED_FIRST_UNREADABLE$STARVED_UNREADABLE" ]; then
        record PASS "every Workload of short deployment stays unadmitted (two intervals)" "two samples ${INTERVAL}s apart: unadmitted, unreserved"
      else
        record FAIL "every Workload of short deployment stays unadmitted (two intervals)" "admitted='${STARVED_ADMITTED:-none}' reserved='${STARVED_RESERVED:-none}'"
      fi
      if [ -z "$STARVED_FIRST_CHECK_BAD$STARVED_CHECK_BAD" ]; then
        record PASS "joint admission check holds every group Pending (two intervals)" "${STARVED_CHECKS}"
      else
        record FAIL "joint admission check holds every group Pending (two intervals)" "bad=${STARVED_CHECK_BAD}"
      fi
      if [ "$STARVED_FIRST_BOUND" = 0 ] && [ "$STARVED_FIRST_PODS" = 2 ] && [ "$STARVED_BOUND" = 0 ] && [ "$STARVED_PODS" = 2 ]; then
        record PASS "no role Pod of short deployment binds (two intervals)" "2 Pods exist, 0 bound"
      else
        record FAIL "no role Pod of short deployment binds (two intervals)" "${STARVED_BOUND} of ${STARVED_PODS} bound"
      fi
    fi
  fi

  # Release Part 1 objects before Part 2 so accelerator Nodes are clean.
  tas_md_force_release "$NS" "${PREFIX}-filler"
  tas_md_force_release "$NS" "${PREFIX}-starved"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "${PREFIX}-filler" "${PREFIX}-starved" --ignore-not-found --wait=false >/dev/null 2>&1
  ballast_delete
  delete_source "${PREFIX}-native"
fi

# ===================================================================================================
# Part 2 — Per-card accelerator admission agreement with TAS (auto-skips without a free accelerator).
# ===================================================================================================
echo
echo "[case-15] === part 2: topology-aware ModelDeployment passes per-card device admission ==="

ACC_NODE="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null | jq -r '
  [.items[]
   | select(any(.status.groups[]?.accelerators[]?;
       ((.mode // 0) == 0 and (.remaining // 0) >= 1600000)))
   | .metadata.name] | sort | .[0] // ""')"

if [ -z "$ACC_NODE" ]; then
  record SKIP "part 2: TAS + per-card accelerator admission agreement" "no Devices ledger reports a free whole accelerator card"
else
  acc_ready="$(kubectl get node "$ACC_NODE" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)"
  acc_unsched="$(kubectl get node "$ACC_NODE" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)"
  if [ "$acc_ready" != True ] || [ "$acc_unsched" = true ]; then
    record SKIP "part 2: TAS + per-card accelerator admission agreement" "free accelerator Node ${ACC_NODE} is not Ready/schedulable"
  else
    ACCELERATOR_KEY="$(kubectl get devices.worker.gpustack.ai "$ACC_NODE" -o json 2>/dev/null | jq -r '
      [.metadata.labels | to_entries[]
       | select(.key | startswith("acceleratable.feature.gpustack.ai/"))
       | select(.key | endswith(".count") or endswith(".capacity") | not)
       | select(.value == "true")
       | .key | sub("^acceleratable.feature.gpustack.ai/"; "")] | sort | .[0] // ""')"
    ACC_IT="$(kubectl get acceleratortypes.worker.gpustack.ai -o json 2>/dev/null | jq -r --arg key "$ACCELERATOR_KEY" '
      [.items[]
       | select(.spec.acceleratorGroup == $key)
       | select((.spec.inactive // false) == false and .status.phase == "Active")
       | .metadata.name] | sort | .[0] // ""')"
    MANUFACTURER="${ACCELERATOR_KEY%%-*}"
    ACC_RESOURCE=""
    case "$MANUFACTURER" in
      amd) ACC_RESOURCE=amd.com/gpu ;;
      ascend) ACC_RESOURCE=huawei.com/npu ;;
      cambricon) ACC_RESOURCE=cambricon.com/mlu ;;
      hygon) ACC_RESOURCE=hygon.com/dcu ;;
      iluvatar) ACC_RESOURCE=iluvatar.com/gpu ;;
      metax) ACC_RESOURCE=metax-tech.com/gpu ;;
      mthreads) ACC_RESOURCE=mthreads.com/gpu ;;
      nvidia) ACC_RESOURCE=nvidia.com/gpu ;;
      thead) ACC_RESOURCE=alibabacloud.com/ppu ;;
    esac
    ACC_ENTRANCE="$(kubectl get acceleratortype.worker.gpustack.ai "$ACC_IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
    ACC_CQ="$(kubectl -n "$NS" get localqueue.kueue.x-k8s.io "$ACC_ENTRANCE" -o jsonpath='{.spec.clusterQueue}' 2>/dev/null)"
    ACC_CQ_UID="$(kubectl get clusterqueue.kueue.x-k8s.io "$ACC_CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"

    if [ -z "$ACCELERATOR_KEY" ] || [ -z "$ACC_IT" ] || [ -z "$ACC_RESOURCE" ] || [ -z "$ACC_CQ" ] || [ -z "$ACC_CQ_UID" ]; then
      record SKIP "part 2: TAS + per-card accelerator admission agreement" \
        "incomplete accelerated chain on ${ACC_NODE}: key=${ACCELERATOR_KEY:-missing} it=${ACC_IT:-missing} cq=${ACC_CQ:-missing}"
    else
      kubectl label node "$ACC_NODE" "${ACC_SELECTOR_KEY}=${PREFIX}" --overwrite >/dev/null
      cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: TopologySource
metadata:
  name: ${ACC_SOURCE}
spec:
  nodeSelector:
    matchLabels:
      ${ACC_SELECTOR_KEY}: ${PREFIX}
  levels:
    - topology.kubernetes.io/region
    - topology.kubernetes.io/zone
  nodeLabels: {}
YAML

      ACC_PROFILE=""; ACC_TOPOLOGY=""
      for _ in $(seq 1 90); do
        source_ready="$(kubectl get topologysource "$ACC_SOURCE" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null)"
        ACC_PROFILE="$(kubectl get node "$ACC_NODE" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
        device_profile="$(kubectl get devices.worker.gpustack.ai "$ACC_NODE" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
        ACC_TOPOLOGY="gpustack-${ACC_PROFILE}"
        levels="$(kubectl get topology.kueue.x-k8s.io "$ACC_TOPOLOGY" -o jsonpath='{range .spec.levels[*]}{.nodeLabel}{" "}{end}' 2>/dev/null)"
        [ "$source_ready" = True ] && [ -n "$ACC_PROFILE" ] && [ "$device_profile" = "$ACC_PROFILE" ] \
          && [ "$levels" = 'topology.kubernetes.io/region topology.kubernetes.io/zone kubernetes.io/hostname ' ] && break
        sleep 2
      done
      if [ "$source_ready" = True ] && [ -n "$ACC_PROFILE" ] && [ "$device_profile" = "$ACC_PROFILE" ]; then
        record PASS "part 2: Node and Devices converge on one topology profile" "${ACC_NODE}: ${ACC_PROFILE}"
      else
        record FAIL "part 2: Node and Devices converge on one topology profile" "source=${source_ready:-missing} node=${ACC_PROFILE:-missing} devices=${device_profile:-missing}"
      fi

      ACC_RF=""
      for _ in $(seq 1 90); do
        active="$(kubectl get clusterqueue.kueue.x-k8s.io "$ACC_CQ" -o jsonpath='{range .status.conditions[?(@.type=="Active")]}{.status}{end}' 2>/dev/null)"
        topology_ready="$(kubectl get clusterqueue.kueue.x-k8s.io "$ACC_CQ" -o jsonpath='{range .status.conditions[?(@.type=="TopologyReady")]}{.status}{end}' 2>/dev/null)"
        ACC_RF="$(kubectl get clusterqueue.kueue.x-k8s.io "$ACC_CQ" -o json 2>/dev/null | jq -r '.spec.resourceGroups[].flavors[].name' \
          | while read -r f; do
              [ "$(kubectl get resourceflavor.kueue.x-k8s.io "$f" -o jsonpath='{.spec.topologyName}' 2>/dev/null)" = "$ACC_TOPOLOGY" ] && echo "$f"
            done | head -1)"
        uid_now="$(kubectl get clusterqueue.kueue.x-k8s.io "$ACC_CQ" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
        [ "$active" = True ] && [ "$topology_ready" = True ] && [ -n "$ACC_RF" ] && [ "$uid_now" = "$ACC_CQ_UID" ] && break
        sleep 2
      done
      if [ "$active" = True ] && [ "$topology_ready" = True ] && [ -n "$ACC_RF" ] && [ "$uid_now" = "$ACC_CQ_UID" ]; then
        record PASS "part 2: accelerated queue migrates in place to discovered topology" "${ACC_CQ} uid=${ACC_CQ_UID} flavor=${ACC_RF}"
      else
        record FAIL "part 2: accelerated queue migrates in place to discovered topology" "active=${active:-?} topologyReady=${topology_ready:-?} flavor=${ACC_RF:-missing}"
      fi

      cat <<YAML | kubectl -n "$NS" apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${ACC_MD}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${PREFIX}-no-such-binding
  roles:
    - name: server
      acceleratorType: ${ACC_IT}
      replicas: 1
      size: 1
      image: ${IMAGE}
      command: ["/pause"]
      topology:
        requiredLevel: topology.kubernetes.io/zone
YAML

      ACC_POD=""; ACC_BOUND_NODE=""; admitted=""; device_check=""
      for _ in $(seq 1 90); do
        ACC_POD="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${ACC_MD}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
        ACC_WL="$(kubectl -n "$NS" get pod "$ACC_POD" -o jsonpath='{.metadata.annotations.kueue\.x-k8s\.io/workload}' 2>/dev/null)"
        admitted="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$ACC_WL" -o jsonpath='{range .status.conditions[?(@.type=="Admitted")]}{.status}{end}' 2>/dev/null)"
        device_check="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$ACC_WL" -o jsonpath='{range .status.admissionChecks[?(@.name=="gpustack-node-devices")]}{.state}{end}' 2>/dev/null)"
        ACC_BOUND_NODE="$(kubectl -n "$NS" get pod "$ACC_POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
        [ "$admitted" = True ] && [ "$device_check" = Ready ] && [ -n "$ACC_BOUND_NODE" ] && break
        sleep 2
      done

      wl_json="$(kubectl -n "$NS" get workload.kueue.x-k8s.io "$ACC_WL" -o json 2>/dev/null)"
      required="$(printf '%s' "$wl_json" | jq -r '.spec.podSets[0].topologyRequest.required // ""' 2>/dev/null)"
      assignment_count="$(printf '%s' "$wl_json" | jq '[.status.admission.podSetAssignments[]?.topologyAssignment.slices[]?] | length' 2>/dev/null)"
      # shellcheck disable=SC2016 # Dollar names are jq variables.
      assignment_levels="$(printf '%s' "$wl_json" | jq -r '[.status.admission.podSetAssignments[]? | (.topologyAssignment.levels // [])] as $levels
        | if ($levels | length) > 0 and ($levels | all(length > 0)) then $levels | map(join(",")) | join(" ") else empty end' 2>/dev/null)"
      # shellcheck disable=SC2016 # Dollar names are jq variables.
      assigned_hosts="$(printf '%s' "$wl_json" | jq -r '[.status.admission.podSetAssignments[]?.topologyAssignment
        | select(. != null) | (.levels | index("kubernetes.io/hostname")) as $i | select($i != null)
        | .slices[]?.valuesPerLevel[$i]
        | if .universal then .universal
          else (.individual as $v | $v.roots[]? | ($v.prefix // "") + . + ($v.suffix // "")) end] | unique | join(",")' 2>/dev/null)"
      assigned_flavor="$(printf '%s' "$wl_json" | jq -r '[.status.admission.podSetAssignments[]?.flavors[]?] | unique | join(",")' 2>/dev/null)"

      if [ "$required" = topology.kubernetes.io/zone ] && [ "${assignment_count:-0}" -gt 0 ] && [ -n "$assignment_levels" ]; then
        record PASS "part 2: Kueue admits accelerator Workload through TAS" "levels=${assignment_levels} slices=${assignment_count}"
      else
        record FAIL "part 2: Kueue admits accelerator Workload through TAS" "required=${required:-missing} levels=${assignment_levels:-missing} slices=${assignment_count:-0}"
      fi

      if [ "$device_check" = Ready ]; then
        record PASS "part 2: per-card device admission accepts topology-scoped ledger" "gpustack-node-devices=Ready for ${ACC_RF}"
      else
        check_message="$(printf '%s' "$wl_json" | jq -r '.status.admissionChecks[]? | select(.name=="gpustack-node-devices") | .message' 2>/dev/null)"
        record FAIL "part 2: per-card device admission accepts topology-scoped ledger" "state=${device_check:-missing}: ${check_message:-no message}"
      fi

      gpu_request="$(kubectl -n "$NS" get pod "$ACC_POD" -o json 2>/dev/null | jq -r --arg k "$ACC_RESOURCE" '.spec.containers[0].resources.requests[$k] // ""' 2>/dev/null)"
      bound_profile="$(kubectl get node "$ACC_BOUND_NODE" -o jsonpath='{.metadata.labels.topology\.gpustack\.ai/profile}' 2>/dev/null)"
      if [ "$ACC_BOUND_NODE" = "$ACC_NODE" ] && [ "$bound_profile" = "$ACC_PROFILE" ] && [ "$gpu_request" = 1 ]; then
        record PASS "part 2: admitted Pod requests and binds the accelerator" "pod=${ACC_POD} node=${ACC_BOUND_NODE} ${ACC_RESOURCE}=${gpu_request}"
      else
        record FAIL "part 2: admitted Pod requests and binds the accelerator" "node=${ACC_BOUND_NODE:-missing} want=${ACC_NODE} ${ACC_RESOURCE}=${gpu_request:-missing}"
      fi

      if [ -n "$ACC_BOUND_NODE" ] && [ "$assigned_hosts" = "$ACC_BOUND_NODE" ] && [ -n "$ACC_RF" ] && [ "$assigned_flavor" = "$ACC_RF" ]; then
        record PASS "part 2: topology assignment names bound Node and profile flavor" "host=${assigned_hosts} flavor=${assigned_flavor}"
      else
        record FAIL "part 2: topology assignment names bound Node and profile flavor" "host=${assigned_hosts:-missing} vs ${ACC_BOUND_NODE:-missing}; flavor=${assigned_flavor:-missing} vs ${ACC_RF:-missing}"
      fi
    fi
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-15] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-15] all checks passed (rows that SKIP name what the cluster could not supply)"
