#!/usr/bin/env bash
#
# CASE 20 — ModelArtifact Hub resolution (public, private, gated, revoked, terminating, secret
#           redaction) and consumer claim placement affinity, pinned argv, and Instance PV placement
#           (MUTATING, self-recovering; private/revocation rows AUTO-SKIP without HF_TOKEN_READONLY;
#           consumer placement phase AUTO-SKIPS with fewer than two workers)
#
#   case-20.sh <NS>
#
# Prerequisites: nodes=2; gpu=none; features=multi-host; cluster=any; inputs=E2E_C20_GATED,E2E_C20_PRIVATE,E2E_C20_REVOKE_BOUND,E2E_MD_IMAGE,E2E_MD_INSTANCE_TYPE,E2E_SYSTEM_NS,HF_TOKEN_READONLY
# Goal:        Prove both halves of the ModelArtifact and persistent-claim placement contract end to
#              end against the real Hugging Face Hub, API server, scheduler, kubelet and Kueue:
#                Phase 1 — Hub resolution & credentials:
#                  admission matrix (ModelScope valid/invalid, two sources, three-part repo, no
#                  source, whitespace revision, absolute/dot-dot claim path, immutable spec); branch
#                  and annotated tag resolve to git ls-remote commits; manifestDigest equals
#                  canonical_manifest.py; multi-page tree resolves to its recorded digest; private
#                  and gated repos without a token report AccessDenied; rejected token on public
#                  repo emits Warning InvalidToken; with HF_TOKEN_READONLY, private repo resolves,
#                  revoking the token sets Resolved=False while keeping the running Pod and blocking
#                  scale-up, deletion waits for the last reference, and the token is redacted from
#                  all statuses, events and worker logs.
#                Phase 2 — Consumer claim placement, Engine argv & Instance PV placement:
#                  bound PV node affinity reaches ModelDeployment and Instance Pods on the PV's
#                  node; unplaceable claims (RWO x2 AccessModeConflict, unbound static/immediate
#                  ClaimNotBound) create no Pod; dynamic WaitForFirstConsumer claim does not roll
#                  its binding Pod; missing artifact reports ArtifactNotFound then Engine delivery
#                  renders pinned argv, HF_TOKEN via secretKeyRef, sized cache and raised
#                  ephemeral-storage limit; admission refuses conflicting flags/mounts; Instance
#                  workspace and additional-volume claims place on the PV's node across restarts,
#                  wait without reserving quota when full, bind WFFC claims, hold unbound immediate
#                  claims, and drop affinity when instance-persistent-volume-placement is false.
# Environment: Any cluster whose worker reaches the public Hugging Face Hub, with a materialized
#              scheduling chain (run case-1 first), an InstanceType, in <NS> the pool's entrance
#              LocalQueue, and at least two schedulable workers for Phase 2. NO GPU: managed roles
#              run the pause image. This machine needs git, curl and python3.
# Inputs:      All real. Public Qwen/Qwen2.5-0.5B-Instruct, bigscience/bloom-560m (gs555750),
#              rhasspy/piper-voices; private E2E_C20_PRIVATE and gated E2E_C20_GATED; optional
#              HF_TOKEN_READONLY; hostPath PVs pinned to the second worker. Settings
#              model-artifact-delivery-mode and instance-persistent-volume-placement are saved and
#              restored in cleanup.
# Expected:    - Phase 1: all 8 admission checks pass; branch/tag match git ls-remote and reference
#                digest; spec edit refused; multi-page tree matches recorded digest; anon private &
#                gated AccessDenied; rejected token emits InvalidToken; with HF_TOKEN_READONLY,
#                private resolves, revocation holds scale-up and keeps running Pod, referenced
#                artifact stays Terminating until deployment goes, and token scan finds 0 hits.
#              - Phase 2: claim deployment and Instance mount read-only on PV's node; unplaceable
#                claims create no Pod; dynamic claim keeps binding Pod; missing artifact then Engine
#                Pod with pinned commit, secretKeyRef, sized cache and raised ephemeral limit;
#                admission refuses override args/volumes; workspace claim (3 starts) and additional
#                claim place on PV's node; full PV node holds Workload without quota then runs when
#                freed; WFFC claim binds; unbound immediate claim holds Starting; Setting off
#                restores render from claim names alone.
# Cleanup:     Trap deletes every case-labeled ModelDeployment, Instance, Pod, ModelArtifact, PVC,
#              Secret, PV, StorageClass, scratch dir, and restores both Settings. Idempotent.
set -uo pipefail

CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASES_DIR}/_md-lib.sh"
REPO_ROOT="$(cd "${CASES_DIR}/../../../.." && pwd)"
REFERENCE="${REPO_ROOT}/pkg/modelartifact/testdata/manifest/canonical_manifest.py"

NS="${1:?usage: case-20.sh <NS>}"
export CASE_ID=20
# shellcheck disable=SC2034 # Read by _rows-lib.sh Settings helpers.
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
AT="${E2E_MD_ACCELERATOR_TYPE:-}"
IT="${E2E_MD_ACCELERATOR_TYPE:-${E2E_MD_INSTANCE_TYPE:-}}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
PRIVATE="${E2E_C20_PRIVATE:-XyX824/mam-d-private-tiny}"
GATED="${E2E_C20_GATED:-XyX824/mam-d-gated-tiny}"
REVOKE_BOUND="${E2E_C20_REVOKE_BOUND:-180}"
HUB=https://huggingface.co

P=c20
LABEL="e2e.gpustack.ai/case=20"
MD_REV="${P}-md-rev"
FAKE_TOKEN="e2e-c20-not-a-real-token"
SETTING=instance-persistent-volume-placement

SCRATCH="$(mktemp -d)"

settings_save model-artifact-delivery-mode "$SETTING"
pin_engine_delivery() {
  setting_set model-artifact-delivery-mode Engine
  settings_settle
}
set_placement() {
  setting_set "$SETTING" "$1"
  settings_settle
}

cleanup() {
  echo
  echo "[case-20] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD_REV" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 24); do
    [ -z "$(kubectl -n "$NS" get pods -l "$LABEL" -o name 2>/dev/null)$(kubectl -n "$NS" get pods -o name 2>/dev/null | command grep "/${P}-")" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete pods,modelartifacts.worker.gpustack.ai,pvc,secret -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "${P}-pv" "${P}-pv-ws" "${P}-pv-add" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete storageclass "${P}-static" "${P}-immediate" --ignore-not-found >/dev/null 2>&1
  settings_restore
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

if [ -z "$AT" ]; then
  AT="$(kubectl get acceleratortypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [ -n "$IT" ] || IT="$AT"
fi
ROLE_AT_YAML=""
[ -n "$AT" ] && ROLE_AT_YAML="
      acceleratorType: \"${AT}\""
ROLE_AT_INLINE=""
[ -n "$AT" ] && ROLE_AT_INLINE="acceleratorType: \"${AT}\", "

# =============================================================================================
# Phase 1: ModelArtifact Hub resolution, access control, revocation & secret redaction
# =============================================================================================
hf_artifact() { # name repository revision [secret]
  local secret=""
  [ -n "${4:-}" ] && secret="
      secretRef: {name: $4}"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: $1
  namespace: ${NS}
  labels: {e2e.gpustack.ai/case: "20"}
spec:
  source:
    huggingFace:
      repository: $2
      revision: $3${secret}
YAML
}

token_secret() { # name value-from-stdin
  kubectl -n "$NS" create secret generic "$1" --from-file=token=/dev/stdin --dry-run=client -o yaml \
    | kubectl label --local -f - "$LABEL" -o yaml \
    | kubectl apply -f - >/dev/null
}

cond() { # name type -> status|reason
  kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$1" \
    -o jsonpath="{.status.conditions[?(@.type=='$2')].status}|{.status.conditions[?(@.type=='$2')].reason}" 2>/dev/null
}

wait_cond() { # name type want-status bound -> prints the last reading
  local reading=""
  for _ in $(seq 1 $(( $4 / 3 ))); do
    reading="$(cond "$1" "$2")"
    [ "${reading%%|*}" = "$3" ] && break
    sleep 3
  done
  printf '%s' "$reading"
}

ma_field() { kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$1" -o jsonpath="$2" 2>/dev/null; }

refused() { # check manifest-on-stdin
  local check="$1" text
  if text="$(md_dryrun_rejects 200)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

admitted() { # check manifest-on-stdin
  local check="$1" text
  if text="$(md_dryrun_admits)"; then
    record PASS "$check" "$text"
  else
    record FAIL "$check" "$text"
  fi
}

echo "== Phase 1.1: ModelArtifact admission =="
admitted "a well-formed ModelScope source is admitted" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-ms, namespace: ${NS}}
spec: {source: {modelScope: {repository: qwen/Qwen2.5-0.5B-Instruct, revision: master}}}
YAML
refused "a ModelScope three-part repository is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-ms-bad, namespace: ${NS}}
spec: {source: {modelScope: {repository: a/b/c, revision: master}}}
YAML
refused "two sources are refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-two, namespace: ${NS}}
spec: {source: {huggingFace: {repository: a/b}, persistentVolumeClaim: {claimName: c}}}
YAML
refused "a three-part repository is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-bad, namespace: ${NS}}
spec: {source: {huggingFace: {repository: a/b/c}}}
YAML
refused "no source is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-none, namespace: ${NS}}
spec: {source: {}}
YAML
refused "a revision with whitespace is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-space, namespace: ${NS}}
spec: {source: {huggingFace: {repository: a/b, revision: "ma in"}}}
YAML
refused "an absolute claim path is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-abs, namespace: ${NS}}
spec: {source: {persistentVolumeClaim: {claimName: c, path: /qwen}}}
YAML
refused "a claim path with a dot-dot element is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-path, namespace: ${NS}}
spec: {source: {persistentVolumeClaim: {claimName: c, path: "a/../b"}}}
YAML

echo "== Phase 1.2: a branch and an annotated tag resolve to git's own commits =="
hf_artifact "${P}-branch" Qwen/Qwen2.5-0.5B-Instruct main
hf_artifact "${P}-tag" bigscience/bloom-560m gs555750
reading="$(wait_cond "${P}-branch" Resolved True 90)"
got="$(ma_field "${P}-branch" '{.status.resolved.revision}')"
want="$(git ls-remote "${HUB}/Qwen/Qwen2.5-0.5B-Instruct" refs/heads/main 2>/dev/null | cut -f1)"
if [ -n "$want" ] && [ "$got" = "$want" ]; then
  record PASS "a branch resolves to git's commit" "main -> ${got}"
else
  record FAIL "a branch resolves to git's commit" "resolved=${reading} revision=${got:-<none>} git=${want:-<unreadable>}"
fi

digest="$(ma_field "${P}-branch" '{.status.resolved.manifestDigest}')"
ref=""
if [ -n "$got" ] && curl -fsS "${HUB}/api/models/Qwen/Qwen2.5-0.5B-Instruct/tree/${got}?recursive=true" -o "${SCRATCH}/tree.json" 2>/dev/null \
  && ref="$(python3 "$REFERENCE" "${SCRATCH}/tree.json" 5)" && [ "$ref" = "$digest" ]; then
  record PASS "the digest equals the reference implementation's" "${digest}"
else
  record FAIL "the digest equals the reference implementation's" "status=${digest:-<none>} reference=${ref:-<unreadable>}"
fi

if out="$(kubectl -n "$NS" patch modelartifacts.worker.gpustack.ai "${P}-branch" --type merge \
  -p '{"spec":{"source":{"huggingFace":{"revision":"v2"}}}}' 2>&1)"; then
  record FAIL "a spec edit is refused" "admitted: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
else
  record PASS "a spec edit is refused" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
fi

wait_cond "${P}-tag" Resolved True 90 >/dev/null
got="$(ma_field "${P}-tag" '{.status.resolved.revision}')"
want="$(git ls-remote "${HUB}/bigscience/bloom-560m" 'refs/tags/gs555750^{}' 2>/dev/null | cut -f1)"
if [ -n "$want" ] && [ "$got" = "$want" ]; then
  record PASS "an annotated tag resolves to its peeled commit" "gs555750 -> ${got}"
else
  record FAIL "an annotated tag resolves to its peeled commit" "revision=${got:-<none>} git peeled=${want:-<unreadable>}"
fi

hf_artifact "${P}-large" rhasspy/piper-voices c10ece1aade47bb51c153c893d14e5bf8e5b7117
wait_cond "${P}-large" Resolved True 180 >/dev/null
got="$(ma_field "${P}-large" '{.status.resolved.manifestDigest} {.status.resolved.fileCount}')"
if [ "$got" = "sha256:445d77d45d4bd47a162701de4f7b75f55209ba2fea5d4b9fae398575c40ecf2f 3301" ]; then
  record PASS "a tree of several pages has its recorded digest" "$got"
else
  record FAIL "a tree of several pages has its recorded digest" "${got:-<none>}"
fi

echo "== Phase 1.3: private and gated repositories without a token =="
hf_artifact "${P}-private-anon" "$PRIVATE" main
hf_artifact "${P}-gated-anon" "$GATED" main
for name in "${P}-private-anon" "${P}-gated-anon"; do
  reading="$(wait_cond "$name" Resolved False 60)"
  if [ "$reading" = "False|AccessDenied" ]; then
    record PASS "no token is AccessDenied" "${name}: ${reading}"
  else
    record FAIL "no token is AccessDenied" "${name}: ${reading:-<no condition>}"
  fi
done

echo "== Phase 1.4: a rejected token on a public repository =="
printf 'hf_%s' "e2erejectedrejectedrejected" | token_secret "${P}-rejected"
hf_artifact "${P}-rejected" Qwen/Qwen2.5-0.5B-Instruct main "${P}-rejected"
wait_cond "${P}-rejected" Resolved True 90 >/dev/null
events=""
for _ in $(seq 1 10); do
  events="$(kubectl -n "$NS" get events --field-selector "involvedObject.name=${P}-rejected,reason=InvalidToken" -o name 2>/dev/null)"
  [ -n "$events" ] && break
  sleep 3
done
if [ -n "$events" ] && [ "$(cond "${P}-rejected" Resolved)" = "True|Resolved" ]; then
  record PASS "a rejected token is reported and the public repository still resolves" "$(printf '%s' "$events" | head -1)"
else
  record FAIL "a rejected token is reported and the public repository still resolves" \
    "events=${events:-<none>} resolved=$(cond "${P}-rejected" Resolved)"
fi

if [ -z "${HF_TOKEN_READONLY:-}" ]; then
  record SKIP "the private, revocation and token-scan rows" \
    "HF_TOKEN_READONLY is not set; NOTHING WAS VERIFIED for the private, revocation and scan rows"
else
  echo "== Phase 1.5: private repository, revocation, deletion hold, and token scan =="
  printf '%s' "$HF_TOKEN_READONLY" | token_secret "${P}-token-ro"
  hf_artifact "${P}-private" "$PRIVATE" main "${P}-token-ro"
  reading="$(wait_cond "${P}-private" Resolved True 90)"
  if [ "$reading" = "True|Resolved" ]; then
    record PASS "the token resolves the private repository" "$(ma_field "${P}-private" '{.status.resolved.revision} {.status.resolved.manifestDigest}')"
  else
    record FAIL "the token resolves the private repository" "${reading:-<no condition>}"
  fi

  pin_engine_delivery
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${MD_REV}, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c20, artifactRef: {name: ${P}-private}}
  roles:
    - {name: server, ${ROLE_AT_INLINE}replicas: 1, image: "${IMAGE}", command: ["/pause"]}
YAML
  uid=""
  for _ in $(seq 1 60); do
    uid="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_REV}" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null)"
    [ -n "$uid" ] && break
    sleep 3
  done
  if [ -z "$uid" ]; then
    record FAIL "the deployment runs a replica before the revocation" "no Running replica within 180s on InstanceType ${IT:-<none>}"
  else
    record PASS "the deployment runs a replica before the revocation" "uid=${uid}"
    printf 'hf_%s' "e2erevokedrevokedrevokedrevoked" | token_secret "${P}-token-ro"
    reading="$(wait_cond "${P}-private" Resolved False "$REVOKE_BOUND")"
    if [ "$reading" = "False|AccessDenied" ]; then
      record PASS "a revoked token sets Resolved=False after its confirmation" "$reading"
    else
      record FAIL "a revoked token sets Resolved=False after its confirmation" "${reading:-<no condition>} after ${REVOKE_BOUND}s"
    fi
    kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD_REV" --type json \
      -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' >/dev/null
    sleep 30
    now="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD_REV}" -o jsonpath='{range .items[*]}{.metadata.uid}={.status.phase} {end}' 2>/dev/null)"
    weights="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD_REV" -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null)"
    if [ "$now" = "${uid}=Running " ] && [ "$weights" = "ArtifactNotResolved" ]; then
      record PASS "the running replica is untouched and a scale-up creates nothing" "pods=${now} WeightsReady=${weights}"
    else
      record FAIL "the running replica is untouched and a scale-up creates nothing" "pods=${now:-<none>} WeightsReady=${weights:-<none>}"
    fi

    kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai "${P}-private" --wait=false >/dev/null 2>&1
    sleep 10
    if [ -n "$(ma_field "${P}-private" '{.metadata.deletionTimestamp}')" ]; then
      record PASS "a referenced artifact stays Terminating" "${P}-private"
    else
      record FAIL "a referenced artifact stays Terminating" "it is gone, or was never marked for deletion"
    fi
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD_REV" --wait=false >/dev/null 2>&1
    gone=false
    for _ in $(seq 1 30); do
      kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-private" >/dev/null 2>&1 || { gone=true; break; }
      sleep 3
    done
    if [ "$gone" = true ]; then
      record PASS "the artifact goes with its last reference" "${P}-private"
    else
      record FAIL "the artifact goes with its last reference" "still present 90s after its deployment was deleted"
    fi
  fi

  {
    kubectl -n "$NS" get modelartifacts.worker.gpustack.ai -o yaml
    kubectl -n "$NS" get modeldeployments.worker.gpustack.ai -o yaml
    kubectl -n "$NS" get events -o yaml
    kubectl -n "$SYSTEM_NS" logs deploy/gpustack-operator-worker --tail=-1
  } > "${SCRATCH}/scan.txt" 2>/dev/null
  cp "${SCRATCH}/scan.txt" "${SCRATCH}/planted.txt"
  printf 'planted %s line\n' "$HF_TOKEN_READONLY" >> "${SCRATCH}/planted.txt"
  planted="$(command grep -cF -- "$HF_TOKEN_READONLY" "${SCRATCH}/planted.txt")"
  hits="$(command grep -cF -- "$HF_TOKEN_READONLY" "${SCRATCH}/scan.txt")"
  if [ "$planted" = 1 ] && [ "$hits" = 0 ] && [ -s "${SCRATCH}/scan.txt" ]; then
    record PASS "the token is in no status, event or worker log" \
      "planted copy finds 1, scan of $(wc -l < "${SCRATCH}/scan.txt" | tr -d ' ') lines found 0"
  else
    record FAIL "the token is in no status, event or worker log" "planted=${planted} hits=${hits}"
  fi
fi

# =============================================================================================
# Phase 2: Consumer claim placement affinity, Engine pinned argv, and Instance PV placement
# =============================================================================================
WORKERS="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{range .items[*]}{.metadata.labels.kubernetes\.io/hostname}{"\n"}{end}' | sort)"
if [ -z "$IT" ]; then
  record SKIP "consumer claim and Instance PV placement rows" "no InstanceType; run case-1 first"
elif [ "$(printf '%s\n' "$WORKERS" | command grep -c .)" -lt 2 ]; then
  record SKIP "the claim rows" "fewer than two workers; NOTHING WAS VERIFIED"
  record SKIP "the placement rows" "fewer than two workers: TAS has no wrong node to pick; NOTHING WAS VERIFIED"
else
  PV_NODE="$(printf '%s\n' "$WORKERS" | sed -n 2p)"
  PV_NODE_NAME="$(kubectl get nodes -l "kubernetes.io/hostname=${PV_NODE}" -o jsonpath='{.items[0].metadata.name}')"

  md() { # name artifact replicas [extra role yaml]
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c20, artifactRef: {name: $2}}
  roles:
    - name: server${ROLE_AT_YAML}
      replicas: $3
      image: "${IMAGE}"${4:-}
YAML
  }

  claim_artifact() { # name claim
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec: {source: {persistentVolumeClaim: {claimName: $2, path: qwen}}}
YAML
  }

  inst() { # name volume-yaml [additional-volumes-yaml]
    kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: $2${3:+
  additionalVolumes: $3}
YAML
  }

  pvc() { # name class access [volumeName]
    local vn="" sc=""
    [ -n "${4:-}" ] && vn="
  volumeName: $4"
    [ -n "$2" ] && sc="
  storageClassName: $2"
    kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:${sc}
  accessModes: [$3]
  resources: {requests: {storage: 1Gi}}${vn}
YAML
  }

  pv() { # name
    kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $1, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  storageClassName: ${P}-static
  hostPath: {path: /tmp/$1, type: DirectoryOrCreate}
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions: [{key: kubernetes.io/hostname, operator: In, values: ["${PV_NODE}"]}]
YAML
  }

  weights() {
    kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$1" \
      -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].status}|{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null
  }

  wait_weights() { # md want-reason bound
    local reading=""
    for _ in $(seq 1 $(( $3 / 3 ))); do
      reading="$(weights "$1")"
      [ "${reading#*|}" = "$2" ] && break
      sleep 3
    done
    printf '%s' "$reading"
  }

  pods_of() { kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=$1" -o jsonpath="$2" 2>/dev/null; }
  pod_field() { kubectl -n "$NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }
  affinity_of() { pod_field "$1" '{.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[?(@.key=="kubernetes.io/hostname")].values[0]}'; }

  wait_running() { # pod bound -> node
    local node=""
    for _ in $(seq 1 $(( $2 / 3 ))); do
      if [ "$(pod_field "$1" '{.status.phase}')" = Running ]; then
        node="$(pod_field "$1" '{.spec.nodeName}')"
        break
      fi
      sleep 3
    done
    printf '%s' "$node"
  }

  wait_gone() {
    for _ in $(seq 1 40); do
      kubectl -n "$NS" get pod "$1" >/dev/null 2>&1 || return 0
      sleep 3
    done
    return 1
  }

  workload_of() {
    kubectl -n "$NS" get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c '
import json,sys
pod=sys.argv[1]
for w in json.load(sys.stdin)["items"]:
    if any(o.get("kind")=="Pod" and o.get("name")==pod for o in w["metadata"].get("ownerReferences",[])):
        c={x["type"]:x["status"] for x in w.get("status",{}).get("conditions",[])}
        print("%s|%s" % (c.get("QuotaReserved","<unset>"), "admission" if w.get("status",{}).get("admission") else "none"))
        break
' "$1"
  }

  placed() { # check pod
    local node aff
    node="$(wait_running "$2" 120)"
    aff="$(affinity_of "$2")"
    if [ "$node" = "$PV_NODE_NAME" ] && [ "$aff" = "$PV_NODE" ]; then
      record PASS "$1" "node=${node} affinity=${aff}"
    else
      record FAIL "$1" "node=${node:-<not running>} affinity=${aff:-<none>} pv=${PV_NODE}"
    fi
  }

  echo "== Phase 2 fixtures: static & immediate classes, PVs pinned to ${PV_NODE} =="
  kubectl apply -f - >/dev/null <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-static}
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ${P}-immediate}
provisioner: e2e.gpustack.ai/provisions-nothing
volumeBindingMode: Immediate
YAML
  pv "${P}-pv"
  pv "${P}-pv-ws"
  pv "${P}-pv-add"
  pvc "${P}-bound" "${P}-static" ReadWriteOnce "${P}-pv"
  claim_artifact "${P}-bound" "${P}-bound"

  echo "== Phase 2.1: a claim deployment runs on the PV's node =="
  md "${P}-claim" "${P}-bound" 1
  reading="$(wait_weights "${P}-claim" Mounted 120)"
  node="$(pods_of "${P}-claim" '{.items[0].spec.nodeName}')"
  aff="$(pods_of "${P}-claim" '{.items[0].spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')"
  if [ "$node" = "$PV_NODE_NAME" ] && [ "$aff" = "$PV_NODE" ]; then
    record PASS "the replica carries the PV's node affinity and runs there" "node=${node}"
  else
    record FAIL "the replica carries the PV's node affinity and runs there" "node=${node:-<unscheduled>} affinity=${aff:-<none>} pv=${PV_NODE_NAME}"
  fi
  cmd="$(pods_of "${P}-claim" '{.items[0].spec.containers[0].command}')"
  ro="$(pods_of "${P}-claim" "{.items[0].spec.volumes[?(@.name=='gpustack-model')].persistentVolumeClaim.readOnly}")"
  sub="$(pods_of "${P}-claim" "{.items[0].spec.containers[0].volumeMounts[?(@.name=='gpustack-model')].subPath}")"
  case "$cmd" in
    *'"vllm","serve","/var/lib/gpustack/model"'*'"--served-model-name","e2e/c20"'*)
      if [ "$ro" = true ] && [ "$sub" = qwen ]; then
        record PASS "the engine loads the read-only claim under the served name" "command=${cmd}"
      else
        record FAIL "the engine loads the read-only claim under the served name" "readOnly=${ro} subPath=${sub}"
      fi ;;
    *) record FAIL "the engine loads the read-only claim under the served name" "command=${cmd:-<none>}" ;;
  esac
  delivery="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "${P}-claim" -o jsonpath='{.status.model.delivery}')"
  if [ "$reading" = "True|Mounted" ] && [ "$delivery" = PVC ]; then
    record PASS "WeightsReady reports the claim mounted" "${reading} delivery=${delivery}"
  else
    record FAIL "WeightsReady reports the claim mounted" "${reading:-<none>} delivery=${delivery:-<none>}"
  fi

  echo "== Phase 2.2: an Instance on the same claim =="
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: ${P}-inst, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  type: "${IT}"
  image: "${IMAGE}"
  resources: {cpu: "1", ram: 2Gi, localStorage: 1Gi}
  volume: {ephemeral: {capacity: 1Gi}}
  additionalVolumes:
    - {mountPath: /models/qwen, model: {artifactRef: {name: ${P}-bound}}}
YAML
  inode=""
  for _ in $(seq 1 40); do
    inode="$(kubectl -n "$NS" get pod "${P}-inst" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
    [ -n "$inode" ] && break
    sleep 3
  done
  imount="$(kubectl -n "$NS" get pod "${P}-inst" -o jsonpath="{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/qwen')].readOnly}/{.spec.containers[0].volumeMounts[?(@.mountPath=='/models/qwen')].subPath}" 2>/dev/null)"
  if [ "$inode" = "$PV_NODE_NAME" ] && [ "$imount" = "true/qwen" ]; then
    record PASS "the Instance mounts the claim read-only on the PV's node" "node=${inode} mount=${imount}"
  else
    record FAIL "the Instance mounts the claim read-only on the PV's node" "node=${inode:-<no pod>} mount=${imount:-<none>}"
  fi

  echo "== Phase 2.3: claims that cannot be placed create nothing =="
  md "${P}-rwo" "${P}-bound" 2
  pvc "${P}-static-pending" "${P}-static" ReadWriteOnce
  claim_artifact "${P}-static-pending" "${P}-static-pending"
  md "${P}-static" "${P}-static-pending" 1
  pvc "${P}-imm" "${P}-immediate" ReadWriteMany
  claim_artifact "${P}-imm" "${P}-imm"
  md "${P}-imm" "${P}-imm" 1
  for pair in "${P}-rwo:AccessModeConflict" "${P}-static:ClaimNotBound" "${P}-imm:ClaimNotBound"; do
    name="${pair%%:*}"
    reading="$(wait_weights "$name" "${pair#*:}" 60)"
    count="$(pods_of "$name" '{range .items[*]}x{end}')"
    if [ "$reading" = "False|${pair#*:}" ] && [ -z "$count" ]; then
      record PASS "an unplaceable claim creates no Pod" "${name}: ${reading}"
    else
      record FAIL "an unplaceable claim creates no Pod" "${name}: ${reading:-<none>} pods=${#count}"
    fi
  done

  echo "== Phase 2.4: a claim bound by its first Pod does not roll that Pod =="
  pvc "${P}-dyn" "" ReadWriteOnce
  claim_artifact "${P}-dyn" "${P}-dyn"
  md "${P}-dyn" "${P}-dyn" 1
  phase=""
  for _ in $(seq 1 60); do
    phase="$(kubectl -n "$NS" get pvc "${P}-dyn" -o jsonpath='{.status.phase}' 2>/dev/null)"
    [ "$phase" = Bound ] && break
    sleep 3
  done
  before="$(pods_of "${P}-dyn" '{.items[*].metadata.uid}')"
  sleep 30
  after="$(pods_of "${P}-dyn" '{.items[*].metadata.uid}')"
  if [ "$phase" = Bound ] && [ -n "$before" ] && [ "$before" = "$after" ]; then
    record PASS "the replica that bound the claim is kept" "uid=${after}"
  else
    record FAIL "the replica that bound the claim is kept" "claim=${phase:-<none>} before=${before:-<none>} after=${after:-<none>}"
  fi

  echo "== Phase 2.5: a missing artifact, then an Engine download =="
  pin_engine_delivery
  md "${P}-engine" "${P}-later" 1
  reading="$(wait_weights "${P}-engine" ArtifactNotFound 60)"
  count="$(pods_of "${P}-engine" '{range .items[*]}x{end}')"
  if [ "$reading" = "False|ArtifactNotFound" ] && [ -z "$count" ]; then
    record PASS "a missing artifact creates no Pod" "$reading"
  else
    record FAIL "a missing artifact creates no Pod" "${reading:-<none>} pods=${#count}"
  fi
  kubectl -n "$NS" create secret generic "${P}-token" --from-literal=token="$FAKE_TOKEN" --dry-run=client -o yaml \
    | kubectl label --local -f - "$LABEL" -o yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-later, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec: {source: {huggingFace: {repository: Qwen/Qwen2.5-0.5B-Instruct, revision: main, secretRef: {name: ${P}-token}}}}
YAML
  pod=""
  for _ in $(seq 1 60); do
    pod="$(pods_of "${P}-engine" '{.items[0].metadata.name}')"
    [ -n "$pod" ] && break
    sleep 3
  done
  commit="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-later" -o jsonpath='{.status.resolved.revision}')"
  size="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "${P}-later" -o jsonpath='{.status.resolved.sizeBytes}')"
  if [ -z "$pod" ]; then
    record FAIL "an Engine Pod is created once the artifact resolves" "no Pod within 180s"
  else
    json="$(kubectl -n "$NS" get pod "$pod" -o json)"
    read -r got_cmd got_ref got_limit got_eph_limit got_eph_req <<EOF
$(printf '%s' "$json" | python3 -c '
import json,sys
p=json.load(sys.stdin); c=p["spec"]["containers"][0]
env={e["name"]:e for e in c.get("env",[])}
ref=env.get("HF_TOKEN",{}).get("valueFrom",{}).get("secretKeyRef",{})
vol=[v for v in p["spec"]["volumes"] if v["name"]=="gpustack-model-cache"]
lim=vol[0]["emptyDir"].get("sizeLimit","") if vol else ""
print(" ".join(c["command"][:5]).replace(" ","+"), ref.get("name","")+"/"+ref.get("key",""), lim,
      c["resources"].get("limits",{}).get("ephemeral-storage",""), c["resources"].get("requests",{}).get("ephemeral-storage",""))
')
EOF
    want_limit=$(( size + (size / 10 > 1073741824 ? size / 10 : 1073741824) ))
    if [ "$got_cmd" = "vllm+serve+Qwen/Qwen2.5-0.5B-Instruct+--revision+${commit}" ]; then
      record PASS "the engine downloads the pinned commit" "${got_cmd//+/ }"
    else
      record FAIL "the engine downloads the pinned commit" "command=${got_cmd//+/ } commit=${commit}"
    fi
    if [ "$got_ref" = "${P}-token/token" ] && ! printf '%s' "$json" | command grep -qF -- "$FAKE_TOKEN"; then
      record PASS "the token arrives by secretKeyRef and never as a value" "HF_TOKEN <- ${got_ref}"
    else
      record FAIL "the token arrives by secretKeyRef and never as a value" "ref=${got_ref:-<none>}"
    fi
    if [ "$got_limit" = "$want_limit" ] && [ "$got_eph_limit" != "$got_eph_req" ]; then
      record PASS "the cache is sized from the manifest and the limit raised over the request" \
        "sizeLimit=${got_limit} ephemeral limit=${got_eph_limit} request=${got_eph_req}"
    else
      record FAIL "the cache is sized from the manifest and the limit raised over the request" \
        "sizeLimit=${got_limit:-<none>} want=${want_limit} limit=${got_eph_limit} request=${got_eph_req}"
    fi
  fi

  echo "== Phase 2.6: ModelDeployment artifact admission refusals =="
  refused "another served name is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c20, artifactRef: {name: ${P}-later}}
  roles: [{name: server, ${ROLE_AT_INLINE}image: "${IMAGE}", extraArgs: ["--served-model-name", "other"]}]
YAML
  refused "a revision beside an artifact is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c20, artifactRef: {name: ${P}-later}}
  roles: [{name: server, ${ROLE_AT_INLINE}image: "${IMAGE}", extraArgs: ["--revision", "v2"]}]
YAML
  refused "a volume on the weights' path is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${P}-refuse, namespace: ${NS}}
spec:
  engine: {name: vLLM, image: "${IMAGE}"}
  model: {name: e2e/c20, artifactRef: {name: ${P}-later}}
  roles:
    - name: server${ROLE_AT_YAML}
      image: "${IMAGE}"
      additionalVolumes: [{mountPath: /var/lib/gpustack, configMap: {name: x}}]
YAML

  echo "== Phase 2.7: a workspace claim runs on the PV's node on every start =="
  pvc "${P}-ws" "${P}-static" ReadWriteOnce "${P}-pv-ws"
  pvc "${P}-add" "${P}-static" ReadWriteOnce "${P}-pv-add"
  for c in "${P}-ws" "${P}-add"; do
    for _ in $(seq 1 20); do
      [ "$(kubectl -n "$NS" get pvc "$c" -o jsonpath='{.status.phase}' 2>/dev/null)" = Bound ] && break
      sleep 3
    done
  done
  inst "${P}-ws" "{persistent: {name: ${P}-ws}}"
  for round in 1 2 3; do
    placed "start ${round}: the workspace claim's Pod carries the PV's affinity and runs there" "${P}-ws"
    [ "$round" = 3 ] && break
    kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":true}}' >/dev/null
    wait_gone "${P}-ws" || record FAIL "start ${round}: the Pod goes on stop" "still present after 120s"
    for _ in $(seq 1 20); do
      [ "$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-ws" -o jsonpath='{.status.phase}')" = Stopped ] && break
      sleep 3
    done
    kubectl -n "$NS" patch instances.worker.gpustack.ai "${P}-ws" --type=merge -p '{"spec":{"stop":false}}' >/dev/null
  done
  REQ="$(pod_field "${P}-ws" '{.spec.containers[0].resources.requests.cpu}')"

  echo "== Phase 2.8: an additional-volume claim runs on the PV's node =="
  inst "${P}-add" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
  placed "the additional claim's Pod carries the PV's affinity and runs there" "${P}-add"

  echo "== Phase 2.9: with the PV's node full, the Workload waits without quota =="
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai,instances.worker.gpustack.ai -l "$LABEL" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 40); do
    if ! kubectl -n "$NS" get pods -o name 2>/dev/null | command grep -q "/${P}-"; then break; fi
    sleep 3
  done
  wait_gone "${P}-ws"
  wait_gone "${P}-add"
  FREE="$(kubectl get pods -A --field-selector "spec.nodeName=${PV_NODE_NAME}" -o json | python3 -c '
import json,sys
def milli(q):
    q=str(q)
    return int(q[:-1]) if q.endswith("m") else int(float(q)*1000)
alloc=milli(sys.argv[1]); used=0
for p in json.load(sys.stdin)["items"]:
    if p["status"].get("phase") in ("Succeeded","Failed"): continue
    for c in p["spec"]["containers"]:
        used+=milli(c.get("resources",{}).get("requests",{}).get("cpu","0"))
print(alloc-used)
' "$(kubectl get node "$PV_NODE_NAME" -o jsonpath='{.status.allocatable.cpu}')")"
  REQ_M="$(printf '%s' "${REQ:-0}" | python3 -c 'import sys; q=sys.stdin.read().strip() or "0"; print(int(q[:-1]) if q.endswith("m") else int(float(q)*1000))')"
  FILL=$(( FREE - REQ_M / 2 ))
  if [ "$REQ_M" -le 0 ] || [ "$FILL" -le 0 ]; then
    record FAIL "the PV's node can be filled" "free=${FREE}m instance request=${REQ:-<none>}"
  else
    kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: ${P}-fill, namespace: ${NS}, labels: {e2e.gpustack.ai/case: "20"}}
spec:
  nodeName: ${PV_NODE_NAME}
  containers: [{name: pause, image: "${IMAGE}", resources: {requests: {cpu: "${FILL}m"}}}]
YAML
    wait_running "${P}-fill" 60 >/dev/null
    inst "${P}-full" "{persistent: {name: ${P}-ws}}"
    sleep 45
    node="$(pod_field "${P}-full" '{.spec.nodeName}')"
    wl="$(workload_of "${P}-full")"
    if [ -n "$(pod_field "${P}-full" '{.metadata.uid}')" ] && [ -z "$node" ] && [ "${wl%%|*}" != True ] && [ "${wl#*|}" = none ]; then
      record PASS "the full node's Workload waits without reserving quota" "fill=${FILL}m workload=${wl}"
    else
      record FAIL "the full node's Workload waits without reserving quota" "node=${node:-<none>} workload=${wl:-<none>}"
    fi
    kubectl -n "$NS" delete pod "${P}-fill" --wait=false >/dev/null 2>&1
    placed "once room appears the Pod runs on the PV's node" "${P}-full"
  fi

  echo "== Phase 2.10: a WaitForFirstConsumer claim binds where its first Pod lands =="
  default_class="$(kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}|{.volumeBindingMode}|{.provisioner}{"\n"}{end}' | head -1)"
  case "$default_class" in
    *'|WaitForFirstConsumer|kubernetes.io/no-provisioner'|'')
      record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "no default class provisioning on first Pod's node: '${default_class}'" ;;
    *'|WaitForFirstConsumer|'*)
      pvc "${P}-wffc" "" ReadWriteOnce
      inst "${P}-wffc" "{persistent: {name: ${P}-wffc}}"
      node="$(wait_running "${P}-wffc" 180)"
      aff="$(affinity_of "${P}-wffc")"
      phase="$(kubectl -n "$NS" get pvc "${P}-wffc" -o jsonpath='{.status.phase}' 2>/dev/null)"
      if [ -n "$node" ] && [ -z "$aff" ] && [ "$phase" = Bound ]; then
        record PASS "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node} claim=${phase}"
      else
        record FAIL "a WaitForFirstConsumer claim binds on the first Pod's node" "node=${node:-<not running>} affinity=${aff:-<none>} claim=${phase:-<none>}"
      fi ;;
    *) record SKIP "a WaitForFirstConsumer claim binds on the first Pod's node" "the default class binds immediately: '${default_class}'" ;;
  esac

  echo "== Phase 2.11: an unbound claim on an immediate class creates no Pod =="
  pvc "${P}-imm-inst" "${P}-immediate" ReadWriteOnce
  inst "${P}-imm-inst" "{persistent: {name: ${P}-imm-inst}}"
  sleep 40
  phase="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm-inst" -o jsonpath='{.status.phase}')"
  msg="$(kubectl -n "$NS" get instances.worker.gpustack.ai "${P}-imm-inst" -o jsonpath='{.status.phaseMessage}')"
  if ! kubectl -n "$NS" get pod "${P}-imm-inst" >/dev/null 2>&1 && [ "$phase" = Starting ] &&
    printf '%s' "$msg" | command grep -qF "PersistentVolumeClaim \"${P}-imm-inst\" is Pending"; then
    record PASS "an unbound immediate claim creates no Pod and says why" "${phase}: ${msg}"
  else
    record FAIL "an unbound immediate claim creates no Pod and says why" "pod=$(pod_field "${P}-imm-inst" '{.metadata.name}') phase=${phase:-<none>} message=${msg:-<none>}"
  fi

  echo "== Phase 2.12: the escape Setting restores the render from claim names alone =="
  set_placement false
  pod=""
  for _ in $(seq 1 20); do
    pod="$(pod_field "${P}-imm-inst" '{.metadata.name}')"
    [ -n "$pod" ] && break
    sleep 3
  done
  if [ -n "$pod" ] && [ -z "$(affinity_of "${P}-imm-inst")" ]; then
    record PASS "with the Setting off the waiting Instance's Pod is created" "pod=${pod}"
  else
    record FAIL "with the Setting off the waiting Instance's Pod is created" "pod=${pod:-<none>} within 60s"
  fi
  inst "${P}-off" "{ephemeral: {capacity: 1Gi}}" "[{mountPath: /mnt/data, persistent: {name: ${P}-add}}]"
  pod=""
  for _ in $(seq 1 20); do
    pod="$(pod_field "${P}-off" '{.metadata.name}')"
    [ -n "$pod" ] && break
    sleep 3
  done
  aff="$(affinity_of "${P}-off")"
  if [ -n "$pod" ] && [ -z "$aff" ]; then
    record PASS "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod}"
  else
    record FAIL "with the Setting off a bound claim's Pod carries no affinity" "pod=${pod:-<none>} affinity=${aff:-<none>}"
  fi
  settings_restore
fi

results
