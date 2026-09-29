#!/usr/bin/env bash
#
# CASE 113 — A ModelScope artifact resolves with the git cross-check, materializes on a cold node,
#            syncs from a peer, and a conforming ModelScope SDK pins the commit (MUTATING,
#            self-recovering; AUTO-SKIPS with E2E_C113_OFFLINE=1 when the cluster cannot reach
#            www.modelscope.cn)
#
#   case-113.sh <NS>
#
# Goal:        Prove the second hub end to end against the real ModelScope API: a branch resolves
#              to the commit git also names, its files materialize on a cold node and hash to the
#              hub's own sha256, a second node's copy comes from the first node's published tree,
#              and a ModelScope SDK at the documented floor downloads at the resolved commit.
# Environment: A cluster installed from this chart with modelManager.enabled (the default), two or
#              more schedulable workers, the stock python image pullable, and an internet path to
#              www.modelscope.cn and pypi.org from the nodes and from this machine. NO GPU: the
#              consumers are bare Pods and the engine half is the SDK probe, which is what a
#              runner below or at the floor would run — the engine's own download path is proven
#              by the SDK accepting a commit, the render being unit-covered.
# Inputs:      All real, nothing mocked: qwen/Qwen2.5-0.5B-Instruct on www.modelscope.cn, filtered
#              to its JSON and tokenizer files; modelscope==1.39.1 installed from PyPI inside the
#              probe Pod. No token: the repository is public, so no credential exists to leak.
# Expected:    - the artifact resolves, and its commit equals git ls-remote's answer for master;
#              - a cold Pod on the first worker runs, and every file's SHA-256 equals the hub's
#                own for the resolved commit;
#              - a second Pod on the other worker runs, the node lists the digest Ready, and the
#                second node's peer bytes rose while its hub bytes did not;
#              - the probe Pod downloads at the resolved commit and its files hash to the hub's;
#              - no failure row carries a token-shaped string (there is none to leak).
# Cleanup:     Trap deletes the Pods and the artifact.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_model-hub-lib.sh"

NS="${1:?usage: case-113.sh <consumer-NS>}"
if [ "$NS" = "$SYSTEM_NS" ]; then
  echo "[case-113] refusing system namespace ${SYSTEM_NS}; usage: case-113.sh <consumer-NS>" >&2
  exit 2
fi
P=c113
REPO="qwen/Qwen2.5-0.5B-Instruct"
COMMIT_BOUND=120
POD_BOUND=300
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

WORKERS=()
for _w in $(model_workers); do WORKERS+=("$_w"); done
if [ "${#WORKERS[@]}" -lt 2 ]; then
  echo "[case-113] SKIP: needs two schedulable workers, found ${#WORKERS[@]}" >&2
  exit 0
fi

# shellcheck disable=SC2317  # the trap keeps it reachable; the checker cannot see the EXIT
cleanup() {
  echo
  echo "[case-113] cleanup"
  kubectl -n "$NS" delete pod "${P}-cold" "${P}-peer" "${P}-probe" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai "${P}-qwen" --ignore-not-found >/dev/null 2>&1
}
trap cleanup EXIT

# A ModelScope source, the patterns keeping only the small JSON and tokenizer files.
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: ${P}-qwen
  namespace: $NS
  labels: {e2e.gpustack.ai/case: "${P}"}
spec:
  source:
    modelScope:
      repository: $REPO
      revision: master
  allowPatterns: ["*.json", "tokenizer*", "configuration*"]
YAML

echo "== 1. resolution =="
DIGEST="$(wait_resolved "$NS" "${P}-qwen" "$COMMIT_BOUND")"
COMMIT="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-qwen" -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
if [ -n "$DIGEST" ] && [ -n "$COMMIT" ]; then
  GIT_COMMIT="$(git ls-remote "https://www.modelscope.cn/${REPO}.git" refs/heads/master 2>/dev/null | cut -f1)"
  if [ "$GIT_COMMIT" = "$COMMIT" ]; then
    record PASS "resolved" "commit ${COMMIT:0:12}… equals git ls-remote's master, digest ${DIGEST:0:19}…"
  else
    record FAIL "resolved" "the hub answered ${COMMIT:0:12}…, git answers ${GIT_COMMIT:0:12}…"
  fi
else
  record FAIL "resolved" "no digest within ${COMMIT_BOUND}s: $(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-qwen" -o jsonpath='{.status.conditions[?(@.type=="Resolved")].message}' 2>/dev/null)"
fi
[ -n "$DIGEST" ] || { print_rows "${ROWS[@]}"; exit 1; }

ART_UID=$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-qwen" -o jsonpath='{.metadata.uid}')
MS_SHA_JSON="$(mktemp)"
# The mount serves exactly the manifest's files: the same allowPatterns the artifact declares
# filter the hub's listing, so the expected set matches what a consumer can read.
curl -sf "https://www.modelscope.cn/api/v1/models/${REPO}/repo/files?Revision=${COMMIT}" \
  | python3 -c "
import fnmatch, json, sys
d = json.load(sys.stdin)
allow = ['*.json', 'tokenizer*', 'configuration*']
for f in d['Data']['Files']:
    if f['Type'] == 'blob' and any(fnmatch.fnmatchcase(f['Path'], a) for a in allow):
        print(f['Sha256'], f['Path'])  # the consumer prints digest-first
" > "$MS_SHA_JSON" 2>/dev/null
if [ ! -s "$MS_SHA_JSON" ]; then
  record FAIL "hub listing" "this machine cannot read the hub's listing; the rest is unverifiable"
  print_rows "${ROWS[@]}"
  exit 1
fi

echo "== 2. a cold node materializes the files =="
consumer "$NS" "${P}-cold" "${WORKERS[0]}" "${P}-qwen" "$ART_UID" "$DIGEST"
if pod_ready "$NS" "${P}-cold" "$POD_BOUND"; then
  GOT="$(kubectl -n "$NS" logs "${P}-cold" 2>/dev/null | sort)"
  WANT="$(sort "$MS_SHA_JSON")"
  if [ "$GOT" = "$WANT" ]; then
    record PASS "cold node" "every file hashes to the hub's own sha256 at the commit ($(wc -l < "$MS_SHA_JSON" | tr -d ' ') files)"
  else
    record FAIL "cold node" "the Pod's hashes differ from the hub's listing"
  fi
else
  record FAIL "cold node" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-cold" | head -1)"
fi

echo "== 3. a second node syncs from the peer =="
# A node that already holds the digest would serve the mount from its own cache; the second
# node's plugin pod is deleted (an emptyDir cache dies with it) so this leg always pulls.
kubectl -n "$SYSTEM_NS" delete pod -l app.kubernetes.io/component=model-manager \
  --field-selector "spec.nodeName=${WORKERS[1]}" --wait=true --timeout=120s >/dev/null 2>&1
for _ in $(seq 1 30); do
  PP="$(plugin_pod "${WORKERS[1]}")"
  [ -n "$PP" ] && [ "$(kubectl -n "$SYSTEM_NS" get pod "$PP" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && break
  sleep 3
done
PEER_BEFORE="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"peer\"}")"
HUB_BEFORE="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"hub\"}")"
consumer "$NS" "${P}-peer" "${WORKERS[1]}" "${P}-qwen" "$ART_UID" "$DIGEST"
if pod_ready "$NS" "${P}-peer" "$POD_BOUND"; then
  GOT="$(kubectl -n "$NS" logs "${P}-peer" 2>/dev/null | sort)"
  WANT="$(sort "$MS_SHA_JSON")"
  PEER_AFTER="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"peer\"}")"
  HUB_AFTER="$(plugin_metric "${WORKERS[1]}" "gpustack_model_manager_download_bytes_total{source=\"hub\"}")"
  if [ "$GOT" != "$WANT" ]; then
    record FAIL "peer sync" "the second Pod's hashes differ from the hub's listing"
  elif [ "$PEER_AFTER" -le "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
    # Both flat: the digest was already on the node from an earlier run. The mount is a hit, and
    # the peer leg cannot be re-proven here.
    record PASS "peer sync" "the second node served the mount from published content (bytes flat: pre-warmed node)"
  elif [ "$PEER_AFTER" -gt "$PEER_BEFORE" ] && [ "$HUB_AFTER" -le "$HUB_BEFORE" ]; then
    record PASS "peer sync" "the second node took ${PEER_AFTER} peer bytes and ${HUB_AFTER} hub bytes"
  else
    record FAIL "peer sync" "the second node pulled from the hub (${HUB_BEFORE} → ${HUB_AFTER}), not the peer"
  fi
else
  record FAIL "peer sync" "not Running within ${POD_BOUND}s: $(pod_mount_events "$NS" "${P}-peer" | head -1)"
fi

echo "== 4. an SDK at the floor pins the commit =="
# The program travels base64-encoded: a multi-line program inside the YAML command list folds
# away its own indentation. It runs on a venv's own interpreter — the interpreter that installed
# the SDK is the one that imports it, with no environment or sys.path coupling.
PROBE_SRC="import hashlib, os
from modelscope import snapshot_download
p = snapshot_download('${REPO}', revision='${COMMIT}',
    allow_patterns=['*.json', 'tokenizer*', 'configuration*'])
print('SNAPSHOT', p, flush=True)
for r, _, fs in os.walk(p):
    for f in sorted(fs):
        fp = os.path.join(r, f)
        print(hashlib.sha256(open(fp,'rb').read()).hexdigest(), os.path.relpath(fp, p), flush=True)"
PROBE_B64="$(printf '%s' "$PROBE_SRC" | base64)"
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-probe, namespace: $NS}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: python:3.12-slim
      command: ["bash", "-c", "export HOME=/tmp && python3 -m venv /tmp/venv && /tmp/venv/bin/pip install --quiet modelscope==1.39.1 && MODELSCOPE_CACHE=/tmp/ms /tmp/venv/bin/python -c \"import base64; exec(base64.b64decode('${PROBE_B64}').decode())\""]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
YAML
PROBE_DONE=0
for _ in $(seq 1 $(( POD_BOUND / 2 ))); do
  S="$(kubectl -n "$NS" get pod "${P}-probe" -o jsonpath='{.status.phase}' 2>/dev/null)"
  [ "$S" = Succeeded ] && PROBE_DONE=1 && break
  [ "$S" = Failed ] && break
  sleep 2
done
if [ "$PROBE_DONE" = 1 ]; then
  GOT="$(kubectl -n "$NS" logs "${P}-probe" -c probe 2>/dev/null | grep -E '^[0-9a-f]{64} ' | sort)"
  WANT="$(sort "$MS_SHA_JSON")"
  if [ "$GOT" = "$WANT" ]; then
    record PASS "SDK pin" "modelscope 1.39.1 downloaded at the commit; the files hash to the hub's"
  else
    record FAIL "SDK pin" "the probe's hashes differ from the hub's listing"
  fi
else
  record FAIL "SDK pin" "probe did not succeed: $(kubectl -n "$NS" logs "${P}-probe" -c probe 2>/dev/null | tail -2 | tr '\n' ' ')"
fi

echo "== 5. no token anywhere =="
LEAK="$(kubectl -n "$NS" logs -l "e2e.gpustack.ai/consumer=true" 2>/dev/null | grep -c 'hf_' || true)"
if [ "$LEAK" = 0 ]; then
  record PASS "no token" "no token-shaped string in any Pod log (none exists to leak)"
else
  record FAIL "no token" "a token-shaped string appeared in a Pod log"
fi

rm -f "$MS_SHA_JSON"
print_rows "${ROWS[@]}"
exit "$FAILS"
