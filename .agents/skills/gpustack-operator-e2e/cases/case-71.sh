#!/usr/bin/env bash
#
# CASE 71 — Router readiness publishes the router endpoint
#           (MUTATING, self-cleaning, image-dependent)
#
#   case-71.sh <NS>
#
# This case is separate from CASE 70 because the upstream router images may be unavailable. An image
# pull failure is a stated SKIP. Any other failure to become Ready is a FAIL. No accelerator or
# serving engine is needed: the router runs outside Kueue and discovers the role Pods dynamically.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-71.sh <NS>}"
KCTX="${E2E_KUBE_CONTEXT:-}"
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }

MD=case71-ready
IT="${E2E_MD_INSTANCE_TYPE:-}"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE="${E2E_ROUTER_SETTLE:-180}"
STABLE="${E2E_ROUTER_STABLE:-30}"

usable_instance_type() {
  k get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null | while IFS='|' read -r name deleting inactive; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      echo "$name"
      break
    done
}

[ -n "$IT" ] || IT="$(usable_instance_type)"
if [ -z "$IT" ]; then
  echo "[case-71] no usable InstanceType; run case-1 first" >&2
  exit 2
fi

cleanup() {
  k -n "$NS" delete modeldeployment "$MD" --ignore-not-found --wait=false >/dev/null 2>&1
  sleep 5
  uids="$(k -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS='|' read -r workload owners; do
    [ -n "$workload" ] || continue
    for uid in $uids; do
      case ",$owners," in *",$uid,"*)
        k -n "$NS" delete workload "$workload" --ignore-not-found --wait=false >/dev/null 2>&1
        break
        ;;
      esac
    done
  done <<EOF
$(k -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{range .metadata.ownerReferences[*]}{.uid}{","}{end}{"\n"}{end}' 2>/dev/null)
EOF
}
trap cleanup EXIT

diagnose_router() {
  echo >&2
  echo "[case-71] router diagnostics" >&2
  k -n "$NS" get deployment "${MD}-router" -o yaml >&2 || true
  k -n "$NS" get configmap "${MD}-router" -o yaml >&2 || true
  k -n "$NS" get pods \
    -l "modeldeployment.gpustack.ai/router=llm-d,app.kubernetes.io/instance=${MD}" \
    -o yaml >&2 || true
  for container in envoy epp; do
    echo "[case-71] ${container} current log" >&2
    k -n "$NS" logs deployment/"${MD}-router" -c "$container" >&2 || true
    echo "[case-71] ${container} previous log" >&2
    k -n "$NS" logs deployment/"${MD}-router" -c "$container" --previous >&2 || true
  done
}

k apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine: vllm
  engineVersion: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: case71-no-such-binding
  router:
    name: llm-d
  roles:
  - name: prefill
    kind: prefill
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${IMAGE}
  - name: decode
    kind: decode
    instanceType: ${IT}
    replicas: 1
    template:
      image: ${IMAGE}
YAML

ready=no
for _ in $(seq 1 "$((SETTLE / 3))"); do
  if [ "$(k -n "$NS" get deployment "${MD}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = 1 ]; then
    ready=yes
    break
  fi
  sleep 3
done

if [ "$ready" != yes ]; then
  waiting="$(k -n "$NS" get pods -l "modeldeployment.gpustack.ai/router=llm-d,app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*].status.containerStatuses[*]}{.name}{"="}{.state.waiting.reason}{" "}{end}' 2>/dev/null)"
  case "$waiting" in
    *ErrImagePull*|*ImagePullBackOff*|*InvalidImageName*)
      echo "SKIP | router readiness and endpoint | upstream router image could not be pulled: ${waiting}"
      exit 0
      ;;
    *)
      echo "FAIL | router readiness and endpoint | router did not become Ready within ${SETTLE}s; waiting=[${waiting:-none}]" >&2
      k -n "$NS" get pods -l "modeldeployment.gpustack.ai/router=llm-d,app.kubernetes.io/instance=${MD}" >&2 || true
      diagnose_router
      exit 1
      ;;
  esac
fi

want="http://${MD}-router.${NS}.svc:8081"
got=""
for _ in $(seq 1 40); do
  got="$(k -n "$NS" get modeldeployment "$MD" -o jsonpath='{.status.endpoint}' 2>/dev/null)"
  [ "$got" = "$want" ] && break
  sleep 3
done

echo
echo "== case-71: router readiness and endpoint =="
if [ "$got" = "$want" ]; then
  pod="$(k -n "$NS" get pods \
    -l "modeldeployment.gpustack.ai/router=llm-d,app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  envoy_restarts="$(k -n "$NS" get pod "$pod" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
  epp_restarts="$(k -n "$NS" get pod "$pod" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
  stable=yes
  for _ in $(seq 1 "$((STABLE / 3))"); do
    sleep 3
    current_ready="$(k -n "$NS" get deployment "${MD}-router" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    current_envoy_restarts="$(k -n "$NS" get pod "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="envoy")].restartCount}' 2>/dev/null)"
    current_epp_restarts="$(k -n "$NS" get pod "$pod" \
      -o jsonpath='{.status.containerStatuses[?(@.name=="epp")].restartCount}' 2>/dev/null)"
    if [ "$current_ready" != 1 ] || [ "$current_envoy_restarts" != "$envoy_restarts" ] || \
      [ "$current_epp_restarts" != "$epp_restarts" ]; then
      stable=no
      break
    fi
  done
  if [ "$stable" != yes ]; then
    echo "FAIL | router readiness remains stable | ready=${current_ready:-none}, restarts envoy=${envoy_restarts:-none}->${current_envoy_restarts:-none} epp=${epp_restarts:-none}->${current_epp_restarts:-none}" >&2
    diagnose_router
    exit 1
  fi
  echo "PASS | a ready router becomes the published endpoint | ${got}"
  echo "PASS | router readiness remains stable | ${STABLE}s with no container restarts"
  echo "all case-71 checks PASS"
  exit 0
fi

echo "FAIL | a ready router becomes the published endpoint | wanted ${want}, got ${got:-none}" >&2
exit 1
