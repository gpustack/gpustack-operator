#!/usr/bin/env bash
#
# CASE 97 — A ModelArtifact resolves against the real Hub, refuses what it cannot read, revokes
#           a lost credential without touching a running Pod, and holds deletion while referenced
#           (MUTATING, self-recovering; needs the public Hugging Face Hub)
#
#   case-97.sh <NS>
#
# Goal:        Prove the ModelArtifact contract end to end against the real Hub: a branch and an
#              annotated tag resolve to the commit git itself reports, the digest equals the one an
#              independent implementation computes from the same tree, a private and a gated
#              repository are AccessDenied without a token and resolve with one, a token the Hub
#              rejects is reported, a revoked token stops new Pods and leaves the running one alone,
#              and a referenced artifact waits in Terminating for its last reference.
# Environment: Any cluster whose worker reaches the Hub, with a materialized scheduling chain (run
#              case-1 first), an InstanceType and, in <NS>, the pool's entrance LocalQueue. NO GPU:
#              the deployment's only role replaces its command with the pause binary. This machine
#              needs git, curl and python3 for the independent readings. The private-repository
#              rows need HF_TOKEN_READONLY in the environment, holding a token that can read
#              E2E_C97_PRIVATE (default XyX824/mam-d-private-tiny); without it they SKIP.
# Inputs:      All real, nothing mocked. Public Qwen/Qwen2.5-0.5B-Instruct and
#              bigscience/bloom-560m (annotated tag gs555750); private E2E_C97_PRIVATE; gated
#              E2E_C97_GATED (default XyX824/mam-d-gated-tiny). The token only ever reaches a Secret
#              created from the environment; it is never written to a file or printed.
# Expected:    - admission refuses ModelScope, two sources, no source, a three-part repository, a
#                revision with whitespace, an absolute or dot-dot claim path, and a spec edit;
#              - a tree of four pages resolves to the digest recorded for that commit;
#              - the branch resolves to `git ls-remote`'s main, the tag to its peeled commit, and
#                the digest equals testdata/manifest/canonical_manifest.py over the same tree;
#              - private and gated without a token: Resolved=False, AccessDenied;
#              - private with the token: Resolved=True;
#              - a rejected token on a public repository: a Warning InvalidToken event;
#              - revocation: the Secret's token replaced, Resolved=False within the bound, the
#                running Pod keeps its uid, and a scale-up creates no Pod;
#              - deletion: the referenced artifact stays Terminating, and goes once its deployment does;
#              - the token appears in no artifact status, no event and no worker log line, measured
#                by a scan shown to find a planted copy first.
# Cleanup:     Trap deletes the deployment, the artifacts and the Secrets it created.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"
REPO_ROOT="$(cd "${CASES_DIR}/../../../.." && pwd)"
REFERENCE="${REPO_ROOT}/pkg/modelartifact/testdata/manifest/canonical_manifest.py"

NS="${1:?usage: case-97.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
PRIVATE="${E2E_C97_PRIVATE:-XyX824/mam-d-private-tiny}"
GATED="${E2E_C97_GATED:-XyX824/mam-d-gated-tiny}"
REVOKE_BOUND="${E2E_C97_REVOKE_BOUND:-180}"
HUB=https://huggingface.co

P=c97
MD="${P}-md"
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

SCRATCH="$(mktemp -d)"

cleanup() {
  echo
  echo "[case-97] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 24); do
    [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o name 2>/dev/null)" ] && break
    sleep 5
  done
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l "e2e.gpustack.ai/case=97" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete secret -l "e2e.gpustack.ai/case=97" --ignore-not-found >/dev/null 2>&1
  rm -rf "$SCRATCH"
}
trap cleanup EXIT

artifact() { # name repository revision [secret]
  local secret=""
  [ -n "${4:-}" ] && secret="
      secretRef: {name: $4}"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: $1
  namespace: ${NS}
  labels: {e2e.gpustack.ai/case: "97"}
spec:
  source:
    huggingFace:
      repository: $2
      revision: $3${secret}
YAML
}

token_secret() { # name value-from-stdin
  kubectl -n "$NS" create secret generic "$1" --from-file=token=/dev/stdin --dry-run=client -o yaml \
    | kubectl label --local -f - e2e.gpustack.ai/case=97 -o yaml \
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

field() { kubectl -n "$NS" get modelartifacts.worker.gpustack.ai "$1" -o jsonpath="$2" 2>/dev/null; }

refused() { # check manifest-on-stdin
  local out
  if out="$(kubectl apply --dry-run=server -f - 2>&1)"; then
    record FAIL "$1" "admitted: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  else
    record PASS "$1" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
  fi
}

echo "== 1. admission =="
refused "ModelScope is refused" <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-ms, namespace: ${NS}}
spec: {source: {modelScope: {repository: qwen/Qwen2.5-0.5B-Instruct, revision: master}}}
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

echo "== 2. a branch and an annotated tag resolve to git's own commits =="
artifact "${P}-branch" Qwen/Qwen2.5-0.5B-Instruct main
artifact "${P}-tag" bigscience/bloom-560m gs555750
reading="$(wait_cond "${P}-branch" Resolved True 90)"
got="$(field "${P}-branch" '{.status.resolved.revision}')"
want="$(git ls-remote "${HUB}/Qwen/Qwen2.5-0.5B-Instruct" refs/heads/main 2>/dev/null | cut -f1)"
if [ -n "$want" ] && [ "$got" = "$want" ]; then
  record PASS "a branch resolves to git's commit" "main -> ${got}"
else
  record FAIL "a branch resolves to git's commit" "resolved=${reading} revision=${got:-<none>} git=${want:-<unreadable>}"
fi

digest="$(field "${P}-branch" '{.status.resolved.manifestDigest}')"
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
got="$(field "${P}-tag" '{.status.resolved.revision}')"
want="$(git ls-remote "${HUB}/bigscience/bloom-560m" 'refs/tags/gs555750^{}' 2>/dev/null | cut -f1)"
if [ -n "$want" ] && [ "$got" = "$want" ]; then
  record PASS "an annotated tag resolves to its peeled commit" "gs555750 -> ${got}"
else
  record FAIL "an annotated tag resolves to its peeled commit" "revision=${got:-<none>} git peeled=${want:-<unreadable>}"
fi

# A tree of 3922 entries takes four pages; its digest was computed by three independent
# implementations over every page at this commit, and its paths matched `git ls-tree -r`.
artifact "${P}-large" rhasspy/piper-voices c10ece1aade47bb51c153c893d14e5bf8e5b7117
wait_cond "${P}-large" Resolved True 180 >/dev/null
got="$(field "${P}-large" '{.status.resolved.manifestDigest} {.status.resolved.fileCount}')"
if [ "$got" = "sha256:445d77d45d4bd47a162701de4f7b75f55209ba2fea5d4b9fae398575c40ecf2f 3301" ]; then
  record PASS "a tree of several pages has its recorded digest" "$got"
else
  record FAIL "a tree of several pages has its recorded digest" "${got:-<none>}"
fi

echo "== 3. private and gated repositories without a token =="
artifact "${P}-private-anon" "$PRIVATE" main
artifact "${P}-gated-anon" "$GATED" main
for name in "${P}-private-anon" "${P}-gated-anon"; do
  reading="$(wait_cond "$name" Resolved False 60)"
  if [ "$reading" = "False|AccessDenied" ]; then
    record PASS "no token is AccessDenied" "${name}: ${reading}"
  else
    record FAIL "no token is AccessDenied" "${name}: ${reading:-<no condition>}"
  fi
done

echo "== 4. a rejected token on a public repository =="
printf 'hf_%s' "e2erejectedrejectedrejected" | token_secret "${P}-rejected"
artifact "${P}-rejected" Qwen/Qwen2.5-0.5B-Instruct main "${P}-rejected"
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
  record SKIP "the private rows" "HF_TOKEN_READONLY is not set; NOTHING WAS VERIFIED for the private, revocation and scan rows"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-97] ${FAILS} check(s) FAILED"; exit 1; }
  exit 0
fi

echo "== 5. the private repository with the token =="
printf '%s' "$HF_TOKEN_READONLY" | token_secret "${P}-token"
artifact "${P}-private" "$PRIVATE" main "${P}-token"
reading="$(wait_cond "${P}-private" Resolved True 90)"
if [ "$reading" = "True|Resolved" ]; then
  record PASS "the token resolves the private repository" "$(field "${P}-private" '{.status.resolved.revision} {.status.resolved.manifestDigest}')"
else
  record FAIL "the token resolves the private repository" "${reading:-<no condition>}"
fi

echo "== 6. revocation stops new Pods and leaves the running one =="
if [ -z "$IT" ]; then
  IT="$(kubectl get instancetypes.worker.gpustack.ai -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
fi
kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata: {name: ${MD}, namespace: ${NS}}
spec:
  engine: {name: vllm, version: "0.29.0"}
  model: {name: e2e/c97, artifactRef: {name: ${P}-private}}
  roles:
    - {name: server, instanceType: "${IT}", replicas: 1, image: "${IMAGE}", command: ["/pause"]}
YAML
uid=""
for _ in $(seq 1 60); do
  uid="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null)"
  [ -n "$uid" ] && break
  sleep 3
done
if [ -z "$uid" ]; then
  record FAIL "the deployment runs a replica before the revocation" "no Running replica within 180s on InstanceType ${IT:-<none>}"
else
  record PASS "the deployment runs a replica before the revocation" "uid=${uid}"
  printf 'hf_%s' "e2erevokedrevokedrevokedrevoked" | token_secret "${P}-token"
  reading="$(wait_cond "${P}-private" Resolved False "$REVOKE_BOUND")"
  if [ "$reading" = "False|AccessDenied" ]; then
    record PASS "a revoked token sets Resolved=False after its confirmation" "$reading"
  else
    record FAIL "a revoked token sets Resolved=False after its confirmation" "${reading:-<no condition>} after ${REVOKE_BOUND}s"
  fi
  kubectl -n "$NS" patch modeldeployments.worker.gpustack.ai "$MD" --type json \
    -p '[{"op":"replace","path":"/spec/roles/0/replicas","value":2}]' >/dev/null
  sleep 30
  now="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o jsonpath='{range .items[*]}{.metadata.uid}={.status.phase} {end}' 2>/dev/null)"
  weights="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o jsonpath="{.status.conditions[?(@.type=='WeightsReady')].reason}" 2>/dev/null)"
  if [ "$now" = "${uid}=Running " ] && [ "$weights" = "ArtifactNotResolved" ]; then
    record PASS "the running replica is untouched and a scale-up creates nothing" "pods=${now} WeightsReady=${weights}"
  else
    record FAIL "the running replica is untouched and a scale-up creates nothing" "pods=${now:-<none>} WeightsReady=${weights:-<none>}"
  fi

  echo "== 7. deletion waits for the last reference =="
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai "${P}-private" --wait=false >/dev/null 2>&1
  sleep 10
  if [ -n "$(field "${P}-private" '{.metadata.deletionTimestamp}')" ]; then
    record PASS "a referenced artifact stays Terminating" "${P}-private"
  else
    record FAIL "a referenced artifact stays Terminating" "it is gone, or was never marked for deletion"
  fi
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" --wait=false >/dev/null 2>&1
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

echo "== 8. the token is nowhere =="
{
  kubectl -n "$NS" get modelartifacts.worker.gpustack.ai -o yaml
  kubectl -n "$NS" get modeldeployments.worker.gpustack.ai -o yaml
  kubectl -n "$NS" get events -o yaml
  kubectl -n "$SYSTEM_NS" logs deploy/gpustack-operator-worker --tail=-1
} > "${SCRATCH}/scan.txt" 2>/dev/null
# The planted copy goes into a copy of the corpus itself, so the baseline proves this scan over
# this corpus would find a token, not only that grep works.
cp "${SCRATCH}/scan.txt" "${SCRATCH}/planted.txt"
printf 'planted %s line\n' "$HF_TOKEN_READONLY" >> "${SCRATCH}/planted.txt"
planted="$(command grep -cF -- "$HF_TOKEN_READONLY" "${SCRATCH}/planted.txt")"
hits="$(command grep -cF -- "$HF_TOKEN_READONLY" "${SCRATCH}/scan.txt")"
if [ "$planted" = 1 ] && [ "$hits" = 0 ] && [ -s "${SCRATCH}/scan.txt" ]; then
  record PASS "the token is in no status, event or worker log" "the corpus with a planted copy finds 1, the scan of $(wc -l < "${SCRATCH}/scan.txt" | tr -d ' ') lines found 0"
else
  record FAIL "the token is in no status, event or worker log" "planted=${planted} hits=${hits}"
fi

print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-97] ${FAILS} check(s) FAILED"; exit 1; }
