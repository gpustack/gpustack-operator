#!/usr/bin/env bash
#
# CASE 102 — Only the plugin on a node writes that node's NodeModelStore status, only the worker writes
#            a ModelArtifact's; a Setting reaches every node's effective configuration; Node delivery
#            needs the CSIDriver; and an HTTPS proxy carries every download (MUTATING, self-recovering)
#
#   case-102.sh <NS>
#
# Goal:        Prove the control-plane contract of node delivery on a real API server, so that
#              admission and kubelet-issued tokens are the real ones: each status guard admits its
#              writer and refuses the others by the rule it names; a Setting change rewrites every
#              node's spec and every plugin applies it; the delivery Setting refuses Node while the
#              plugin is absent, the worker removes every NodeModelStore then and recreates them after;
#              and a cold download through an HTTPS proxy reaches the hub only from the proxy.
# Environment: A cluster installed from this chart with modelManager.enabled, two or more workers, the
#              stock python image pullable, and openssl on this machine. NO GPU.
# Inputs:      MOCKED: a test hub over HTTPS with a self-signed certificate made here, handed to the
#              plugin and the controller through the model-artifact-ca-bundle Setting, and a forward
#              proxy. Real: the webhooks, the kubelet-issued tokens and the plugin's downloads.
# Expected:    - the plugin's token bound to its Pod on node A updates node A's status, and is refused
#                on node B by the node rule; a token bound to no Pod is refused by the binding rule; the
#                cluster administrator is refused by the identity rule;
#              - the administrator's ModelArtifact status update is refused by the worker rule, and the
#                worker's own writes land: a new artifact resolves;
#              - model-store-download-concurrency reaches every NodeModelStore's spec, and every plugin
#                reports that generation, within 60 s;
#              - without the CSIDriver, writing Node is refused and every NodeModelStore is removed;
#                with it back, Node is accepted and every NodeModelStore returns;
#              - through the HTTPS proxy every file the hub serves is fetched from the proxy's address,
#                and without it from the plugin's; equal addresses make that row undecidable.
# Cleanup:     Trap restores the Settings and the CSIDriver, and deletes the Pods, artifacts, hub,
#              proxy, the CA ConfigMap and the TLS Secret.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-102.sh <NS>}"
P=c102
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
SCRATCH="$(mktemp -d)"
SA="${E2E_MODEL_MANAGER_SA:-gpustack-operator-model-manager}"
KEYS="model-artifact-huggingface-endpoint model-artifact-https-proxy model-artifact-no-proxy
      model-artifact-ca-bundle model-store-download-concurrency model-artifact-delivery-mode"
mkdir -p "$SCRATCH/orig"
for k in $KEYS; do
  kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.$k}" >"$SCRATCH/orig/$k" 2>/dev/null
done
# restore KEY: the value KEY had before the case, or no value when it had none.
restore() {
  if [ -s "$SCRATCH/orig/$1" ]; then setting_set "$1" "$(base64 -d <"$SCRATCH/orig/$1")"; else setting_unset "$1"; fi
}

cleanup() {
  echo
  echo "[case-102] cleanup"
  kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 || kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null 2>&1
  for k in $KEYS; do restore "$k"; done
  kubectl -n "$NS" delete pods -l e2e.gpustack.ai/consumer=true --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=102 --ignore-not-found >/dev/null 2>&1
  for n in "${P}-hub" "${P}-proxy"; do
    kubectl -n "$NS" delete deploy,svc "$n" --ignore-not-found >/dev/null 2>&1
    kubectl -n "$NS" delete configmap "${n}-script" --ignore-not-found >/dev/null 2>&1
  done
  kubectl -n "$NS" delete secret "${P}-tls" --ignore-not-found >/dev/null 2>&1
  kubectl -n "$SYSTEM_NS" delete configmap "${P}-ca" --ignore-not-found >/dev/null 2>&1
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

read -r -a WORKERS <<<"$(model_workers)"
[ "${#WORKERS[@]}" -ge 2 ] || { echo "[case-102] needs two workers; NOTHING WAS VERIFIED"; exit 2; }
W1="${WORKERS[0]}" W2="${WORKERS[1]}"
kubectl get csidriver model.csi.gpustack.ai >/dev/null 2>&1 \
  || { echo "[case-102] the model-manager plugin is not installed; NOTHING WAS VERIFIED"; exit 2; }
# The object to put back: its name, labels, annotations and spec, without what the API server set.
kubectl get csidriver model.csi.gpustack.ai -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
m = o['metadata']
print(json.dumps({'apiVersion': o['apiVersion'], 'kind': o['kind'], 'spec': o['spec'],
                  'metadata': {k: m[k] for k in ('name', 'labels', 'annotations') if k in m}}))" >"$SCRATCH/csidriver.yaml"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# status_as TOKEN NODE: an update of NODE's NodeModelStore status as the bearer of TOKEN alone, the
# one verb the plugin's role holds on status and the one it sends, printing the API server's answer
# and succeeding on a 2xx. The object is read as the administrator, with a changed usedPercent, and
# put back as TOKEN.
SERVER="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$SCRATCH/ca.crt"
status_as() {
  local code
  kubectl get nodemodelstores.v1alpha1.worker.gpustack.ai "$2" -o json | python3 -c "
import json, sys
o = json.load(sys.stdin)
o.setdefault('status', {}).setdefault('capacity', {})['usedPercent'] = 95
print(json.dumps(o))" >"$SCRATCH/object"
  printf 'Authorization: Bearer %s\n' "$1" >"$SCRATCH/auth"
  code="$(curl -sS -o "$SCRATCH/answer" -w '%{http_code}' --cacert "$SCRATCH/ca.crt" -H @"$SCRATCH/auth" \
    -X PUT -H 'Content-Type: application/json' --data @"$SCRATCH/object" \
    "${SERVER}/apis/worker.gpustack.ai/v1alpha1/nodemodelstores/$2/status")"
  rm -f "$SCRATCH/auth"
  cat "$SCRATCH/answer"
  case "$code" in 2*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------- the NodeModelStore status guard
PLUGIN1="$(plugin_pod "$W1")"
BOUND="$(kubectl -n "$SYSTEM_NS" create token "$SA" --bound-object-kind Pod --bound-object-name "$PLUGIN1" --duration=10m)"
UNBOUND="$(kubectl -n "$SYSTEM_NS" create token "$SA" --duration=10m)"
patch='{"status":{"capacity":{"totalBytes":1,"storedBytes":1,"usedPercent":95}}}'
if status_as "$BOUND" "$W1" >"$SCRATCH/own" 2>&1; then
  record PASS "the plugin's token bound to its Pod writes its own node's status (the baseline)" "$W1"
else
  record FAIL "the plugin writes its own node's status" "$(tr -d "\n" <"$SCRATCH/own" | cut -c1-300)"
fi
for row in "$BOUND|$W2|the plugin writes only the status of the node its Pod runs on|the plugin on another node" \
           "$UNBOUND|$W1|the plugin's token must be bound to its Pod|a token bound to no Pod" \
           "|$W1|only the model-manager plugin writes a NodeModelStore's status|the cluster administrator"; do
  IFS='|' read -r token node rule who <<<"$row"
  if [ -n "$token" ]; then
    status_as "$token" "$node" >"$SCRATCH/out" 2>&1
  else
    kubectl patch nodemodelstores.v1alpha1.worker.gpustack.ai "$node" --subresource=status --type=merge -p "$patch" >"$SCRATCH/out" 2>&1
  fi
  rc=$?
  if [ "$rc" != 0 ] && grep -q -F "$rule" "$SCRATCH/out"; then
    record PASS "${who} is refused: ${rule}" "$node"
  else
    record FAIL "${who} is refused: ${rule} (rc ${rc})" "$(tr -d "\n" <"$SCRATCH/out" | cut -c1-300)"
  fi
done

# ---------------------------------------------------------------- the ModelArtifact status guard
REPOS='{"e2e/guard":{"files":{"config.json":{"size":100}}},"e2e/proxied":{"files":{"config.json":{"size":200},"w.bin":{"size":300000,"lfs":true}}},"e2e/direct":{"files":{"config.json":{"size":210},"w.bin":{"size":310000,"lfs":true}}}}'
PLAIN_URL="$(mh_deploy "$NS" "${P}-plain" "$REPOS")"
setting_set model-artifact-huggingface-endpoint "$PLAIN_URL"
settings_settle
artifact "$NS" "${P}-guard" e2e/guard "" 102
if [ -n "$(wait_resolved "$NS" "${P}-guard" 120)" ]; then
  record PASS "the worker's own status writes are admitted: a new artifact resolves" "${NS}/${P}-guard"
else
  record FAIL "the worker's own status writes are admitted" "${NS}/${P}-guard"
fi
kubectl -n "$NS" patch modelartifacts.v1alpha1.worker.gpustack.ai "${P}-guard" --subresource=status --type=merge \
  -p '{"status":{"resolved":{"manifestDigest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' >"$SCRATCH/ma" 2>&1
rc=$?
if [ "$rc" != 0 ] && grep -q -F "only the worker writes a ModelArtifact's status" "$SCRATCH/ma"; then
  record PASS "the administrator's ModelArtifact status write is refused: only the worker writes it" "${NS}/${P}-guard"
else
  record FAIL "the administrator's ModelArtifact status write is refused (rc ${rc})" "$(head -1 "$SCRATCH/ma")"
fi
kubectl -n "$NS" delete deploy,svc "${P}-plain" --ignore-not-found >/dev/null 2>&1
kubectl -n "$NS" delete configmap "${P}-plain-script" --ignore-not-found >/dev/null 2>&1

# ---------------------------------------------------------------- a Setting reaches every node
cur="$(setting_get model-store-download-concurrency)"
want=5
[ "$cur" = 5 ] && want=6
setting_set model-store-download-concurrency "$want"
converged=0
for _ in $(seq 1 30); do
  converged=1
  for n in $(kubectl get nodemodelstores.worker.gpustack.ai -o name); do
    got="$(kubectl get "$n" -o jsonpath='{.spec.download.concurrency}|{.metadata.generation}|{.status.observedGeneration}')"
    IFS='|' read -r c g o <<<"$got"
    { [ "$c" = "$want" ] && [ "$g" = "$o" ]; } || converged=0
  done
  [ "$converged" = 1 ] && break
  sleep 2
done
count="$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l | tr -d ' ')"
if [ "$converged" = 1 ] && [ "$count" -ge 2 ]; then
  record PASS "model-store-download-concurrency=${want} reached ${count} nodes' spec and every plugin applied it" "all nodes"
else
  record FAIL "the Setting reached every node's spec and observedGeneration within 60 s" "$(kubectl get nodemodelstores.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}={.spec.download.concurrency}/{.metadata.generation}/{.status.observedGeneration} {end}')"
fi
restore model-store-download-concurrency

# ---------------------------------------------------------------- Node delivery needs the CSIDriver
set_delivery() { kubectl -n "$SYSTEM_NS" patch settings.gpustack.ai model-artifact-delivery-mode --type=merge -p "{\"spec\":{\"value\":\"$1\"}}" >"$SCRATCH/set" 2>&1; }
set_delivery Engine || record FAIL "Engine is accepted" "$(head -1 "$SCRATCH/set")"
kubectl delete csidriver model.csi.gpustack.ai >/dev/null
gone=0
for _ in $(seq 1 30); do [ -z "$(kubectl get nodemodelstores.worker.gpustack.ai -o name)" ] && { gone=1; break; }; sleep 2; done
if [ "$gone" = 1 ]; then
  record PASS "without the CSIDriver every NodeModelStore is removed" "all nodes"
else
  record FAIL "without the CSIDriver every NodeModelStore is removed" "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | tr '\n' ' ')"
fi
if set_delivery Node; then
  record FAIL "Node is refused while the CSIDriver is absent" "model-artifact-delivery-mode"
else
  if grep -q "does not exist" "$SCRATCH/set"; then
    record PASS "Node is refused while the CSIDriver is absent" "model-artifact-delivery-mode"
  else
    record FAIL "Node is refused for the CSIDriver" "$(head -1 "$SCRATCH/set")"
  fi
fi
kubectl apply -f "$SCRATCH/csidriver.yaml" >/dev/null
back=0
for _ in $(seq 1 60); do [ "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l | tr -d ' ')" -ge "$count" ] && { back=1; break; }; sleep 2; done
if [ "$back" = 1 ]; then
  record PASS "with the CSIDriver back every NodeModelStore returns" "${count} nodes"
else
  record FAIL "with the CSIDriver back every NodeModelStore returns" "$(kubectl get nodemodelstores.worker.gpustack.ai -o name | wc -l)"
fi
if set_delivery Node; then
  record PASS "with the CSIDriver back Node is accepted" "model-artifact-delivery-mode"
else
  record FAIL "with the CSIDriver back Node is accepted" "$(head -1 "$SCRATCH/set")"
fi
for _ in $(seq 1 30); do
  [ "$(kubectl get nodemodelstore "$W1" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null)" = True ] && break
  sleep 2
done

# ---------------------------------------------------------------- an HTTPS proxy, with its control
HOST="${P}-hub.${NS}.svc"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=${HOST}" -addext "subjectAltName=DNS:${HOST}" \
  -keyout "$SCRATCH/tls.key" -out "$SCRATCH/tls.crt" >/dev/null 2>&1
kubectl -n "$NS" create secret tls "${P}-tls" --cert="$SCRATCH/tls.crt" --key="$SCRATCH/tls.key" >/dev/null
kubectl -n "$SYSTEM_NS" create configmap "${P}-ca" --from-file=ca.crt="$SCRATCH/tls.crt" >/dev/null
HUB_URL="$(mh_deploy "$NS" "${P}-hub" "$REPOS" "${P}-tls")"
PROXY_URL="$(mh_proxy "$NS" "${P}-proxy")"
setting_set model-artifact-ca-bundle "${P}-ca"
setting_set model-artifact-no-proxy ""
setting_set model-artifact-https-proxy "$PROXY_URL"
setting_set model-artifact-huggingface-endpoint "$HUB_URL"
settings_settle

# remotes REPO: the distinct client addresses the hub served REPO's file downloads to.
remotes() {
  mh_log "$NS" "${P}-hub" | python3 -c "
import json, sys
print(' '.join(sorted({json.loads(l)['remote'] for l in sys.stdin if l.strip() and json.loads(l)['method'] == 'GET'
                        and json.loads(l)['path'].startswith('/$1/') and json.loads(l).get('status') in (200, 206)})))"
}
fetch() { # name repo pod
  artifact "$NS" "$1" "$2" "" 102
  local d
  d="$(wait_resolved "$NS" "$1" 120)"
  [ -n "$d" ] || return 1
  consumer "$NS" "$3" "$W1" "$1" "$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$1" -o jsonpath='{.metadata.uid}')" "$d"
  pod_ready "$NS" "$3" 240
}
PROXY_IP="$(mh_pod_ip "$NS" "${P}-proxy")"
PLUGIN_IP="$(kubectl -n "$SYSTEM_NS" get pod "$(plugin_pod "$W1")" -o jsonpath='{.status.podIP}')"
if fetch "${P}-proxied" e2e/proxied "${P}-pp"; then
  via="$(remotes e2e/proxied)"
  if [ "$via" = "$PROXY_IP" ]; then
    record PASS "through the HTTPS proxy every file reached the hub from the proxy (${via})" "e2e/proxied"
  else
    record FAIL "every file reached the hub from the proxy ${PROXY_IP} (saw ${via})" "e2e/proxied"
  fi
  tunnels="$(mh_log "$NS" "${P}-proxy" | grep -c -F "\"connect\": \"${HOST}:8443\"")"
  if [ "$tunnels" -ge 1 ]; then
    record PASS "the proxy tunneled to the hub (${tunnels} tunnels)" "${P}-proxy"
  else
    record FAIL "the proxy tunneled to the hub" "${P}-proxy"
  fi
else
  record FAIL "a cold download through the HTTPS proxy and the CA bundle mounts" "$(pod_mount_events "$NS" "${P}-pp" | tail -1)"
fi
setting_set model-artifact-https-proxy ""
settings_settle
if fetch "${P}-direct" e2e/direct "${P}-pd"; then
  direct="$(remotes e2e/direct)"
  if [ "$PROXY_IP" = "$PLUGIN_IP" ]; then
    record SKIP "undecidable: the proxy and the plugin share the address ${PLUGIN_IP}" "e2e/direct"
  else
    if [ "$direct" = "$PLUGIN_IP" ]; then
      record PASS "without the proxy the files reached the hub from the plugin (${direct}), so the address tells the paths apart" "e2e/direct"
    else
      record FAIL "without the proxy the files reached the hub from the plugin ${PLUGIN_IP} (saw ${direct})" "e2e/direct"
    fi
  fi
else
  record FAIL "a cold download without the proxy mounts" "$(pod_mount_events "$NS" "${P}-pd" | tail -1)"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-102] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-102] PASS"
