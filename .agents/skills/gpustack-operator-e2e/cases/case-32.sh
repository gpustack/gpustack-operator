#!/usr/bin/env bash
#
# CASE 32 — Live routed P/D serving over direct KV leg and continuous-traffic
#           router or role scaling with zero dropped requests
#   (MUTATING when Phase 2 runs, self-recovering; requires real accelerators)
#
#   E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-32.sh <NS> <MODEL_DEPLOYMENT> [router|prefill|decode|server|none]
#
# Prerequisites: nodes=2; gpu=any; features=multi-host; cluster=real; inputs=E2E_PD_MODEL,E2E_PD_PROBE_IMAGE,E2E_PD_REQUESTS,E2E_PD_VLLM_VERSION,E2E_SCALE_LOG_DIR,E2E_SCALE_READY_BOUND,E2E_SCALE_SETTLE
# Goal:        Verify live disaggregated serving end to end across two complementary phases:
#                1. Direct KV leg transfer (multi-host routed P/D pair): verifies operator-rendered
#                   transport pinning (MC_FORCE_TCP / argv / log, no cross-node NVLink), completes
#                   alternating streaming and non-streaming requests, confirms prefill sent and
#                   decode received transfer counters grow with zero failed transfers, verifies
#                   optional bound Mooncake store batch-put growth (plus SGLang hierarchical cache
#                   and cpu_tensor retraction backup), and confirms zero restarts.
#                2. Continuous-traffic scaling (router, prefill, decode, or server): under steady
#                   chat completion traffic scales the target 1 -> 2 -> 1, confirms the new Pod
#                   reaches Ready and serves real work via its own counter (SKIPping if a
#                   cache-aware router concentrates server traffic), confirms zero failed requests
#                   during both scale-up and scale-down, and verifies persistent Pod restarts stay
#                   unchanged.
# Environment: An already Ready routed ModelDeployment on real accelerators. Phase 1 requires
#              one Ready Prefill Pod and one Ready Decode Pod on two distinct nodes (else Phase 1
#              SKIPs). Phase 2 requires the chosen scale target (default: router) to start at
#              replicas=1 with cluster headroom for a second replica (pass target "none" to run
#              Phase 1 read-only). jq must be on PATH.
# Inputs:      REAL: live engine Pods, router Service, Prometheus metrics, and temporary curl
#              probe Pod sending chat completions.
# Expected:    All Phase 1 direct KV transfer checks and Phase 2 zero-drop scaling checks PASS
#              with unchanged container restart counts.
# Cleanup:     EXIT trap stops background traffic, restores the scaled target to replicas=1 (if
#              mutated), and deletes the temporary probe Pod on pass or fail.
set -uo pipefail

CASE_ID=32
export CASE_ID
CASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
. "${CASE_DIR}/_rows-lib.sh"
# shellcheck source=/dev/null
. "${CASE_DIR}/_serving-lib.sh"

NS="${1:-}"
MD="${2:-}"
TARGET="${3:-router}"
IMAGE="${E2E_PD_PROBE_IMAGE:-}"
MODEL="${E2E_PD_MODEL:-}"
VERSION="${E2E_PD_VLLM_VERSION:-0.29.0}"
REQUESTS="${E2E_PD_REQUESTS:-20}"
READY_BOUND="${E2E_SCALE_READY_BOUND:-900}"
SETTLE="${E2E_SCALE_SETTLE:-60}"

if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$IMAGE" ] || [ -z "$MODEL" ] \
  || [[ ! "$TARGET" =~ ^(router|prefill|decode|server|none)$ ]]; then
  echo "usage: E2E_PD_PROBE_IMAGE=<curl-image> E2E_PD_MODEL=<model> case-32.sh <NS> <MODEL_DEPLOYMENT> [router|prefill|decode|server|none]" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "[case-32] jq is required" >&2; exit 2; }

md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
ROUTER="$(jq -r '.spec.router.name // empty' <<<"$md_json")"
[ -n "$ROUTER" ] || { echo "[case-32] requires a routed ModelDeployment" >&2; exit 2; }
ENGINE="$(jq -r '.spec.engine.name' <<<"$md_json")"
VENDOR="$(serving_vendor "$md_json")" || { echo "[case-32] cannot read deployment vendor" >&2; exit 2; }
serving_profile "$VENDOR" "$ENGINE" || exit 2

PROBE="e2e-c32-probe-$$"
TRAFFIC="/tmp/e2e-c32-traffic"
SCALED_PATCH_PATH=""

scale_target() {
  [ -n "$SCALED_PATCH_PATH" ] || return 0
  kubectl -n "$NS" patch modeldeployment "$MD" --type=json \
    -p "[{\"op\":\"add\",\"path\":\"${SCALED_PATCH_PATH}\",\"value\":${1}}]" >/dev/null 2>&1 || true
}

cleanup() {
  kubectl -n "$NS" exec "$PROBE" -- touch "${TRAFFIC}.stop" >/dev/null 2>&1 || true
  if [ -n "$SCALED_PATCH_PATH" ]; then
    scale_target 1
  fi
  kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

kubectl -n "$NS" run "$PROBE" --image="$IMAGE" --restart=Never --command -- sleep 3600 >/dev/null || exit 2
kubectl -n "$NS" wait "pod/${PROBE}" --for=condition=Ready --timeout=120s >/dev/null || exit 2

scrape() { kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 "$1"; }
read_metric() { local body; body="$(scrape "$1")" || return 1; serving_sum "$body" "$2"; }
log_count() { kubectl -n "$NS" logs "$1" -c main 2>/dev/null | grep -cF "$2" || true; }
grew() { awk -v a="$1" -v b="$2" 'BEGIN { print (b > a) ? "true" : "false" }'; }

check_bool() {
  if [ "$1" = true ]; then
    record PASS "$2" "$MD"
  else
    record FAIL "$2" "$MD"
  fi
}

# =================================================================================================
# PHASE 1: Direct KV leg transfer verification (routed multi-host P/D pair)
# =================================================================================================

pods="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${MD}" -o json)" || exit 2
pod_field() {
  jq -r --arg name "$1" ".items[] | select(.metadata.name == \$name) | $2" <<<"$pods"
}
ready_pod_by_kind() {
  jq -r --arg kind "$1" '[.items[] | select(.metadata.labels["modeldeployment.gpustack.ai/role-kind"] == $kind and
    any(.status.conditions[]?; .type == "Ready" and .status == "True"))] |
    if length == 1 then .[0].metadata.name else empty end' <<<"$pods"
}
has_arg() {
  pod_field "$1" '.spec.containers[] | select(.name == "main") | (.command // []) + (.args // [])' \
    | jq -r --arg flag "$2" --arg value "${3:-}" '. as $argv | [range(length) as $i |
      select($argv[$i] == $flag and ($value == "" or $argv[$i + 1] == $value)) ,
      select($value != "" and $argv[$i] == ($flag + "=" + $value))] | length > 0'
}
metrics_url() {
  local ip port scheme
  ip="$(pod_field "$1" .status.podIP)"
  port="$(pod_field "$1" '.metadata.annotations["prometheus.io/port"] // empty')"
  scheme="$(pod_field "$1" '.metadata.annotations["prometheus.io/scheme"] // "http"')"
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  echo "${scheme}://${ip}:${port}/metrics"
}

p1_skip=""
if ! jq -e '([.spec.roles[] | select(.kind == "Prefill")] | length) == 1 and
  ([.spec.roles[] | select(.kind == "Decode")] | length) == 1' <<<"$md_json" >/dev/null; then
  p1_skip="deployment is not a 1-Prefill/1-Decode pair"
elif [[ " $SP_ROUTERS " != *" $ROUTER "* ]]; then
  p1_skip="router ${ROUTER} renders no transfer leg for ${ENGINE} on ${VENDOR}"
elif [ "$ENGINE" = vLLM ] && [ "$SP_PIN" != none ] \
  && ! jq -e --arg v "$VERSION" '.spec.engine.image as $ei | all(.spec.roles[]; ((.image // $ei) | contains("-vllm" + $v)))' <<<"$md_json" >/dev/null; then
  p1_skip="not every role runs vLLM ${VERSION}"
elif [ "$SP_PIN" != none ] \
  && ! jq -e '(.spec.kvTransfer.backend // "Mooncake") == "Mooncake" and all(.spec.roles[] | select(.kind == "Prefill" or .kind == "Decode"); ((.resources.interface // "0") | tonumber) == 0)' <<<"$md_json" >/dev/null; then
  p1_skip="direct pair carries fabric interfaces or non-Mooncake backend"
elif jq -e 'any(.spec.roles[]; any(.env[]?; .name == "MC_FORCE_TCP") or
  any(.extraArgs[]?; startswith("--enable-hierarchical-cache") or startswith("--disaggregation-decode-retraction-backup")))' <<<"$md_json" >/dev/null; then
  p1_skip="role manually sets MC_FORCE_TCP or cache switches"
fi

prefill=""
decode=""
if [ -z "$p1_skip" ]; then
  prefill="$(ready_pod_by_kind prefill)"
  decode="$(ready_pod_by_kind decode)"
  if [ -z "$prefill" ] || [ -z "$decode" ]; then
    p1_skip="requires exactly one Ready prefill and one Ready decode Pod"
  elif [ "$(pod_field "$prefill" .spec.nodeName)" = "$(pod_field "$decode" .spec.nodeName)" ]; then
    p1_skip="prefill and decode share a node (must cross hosts)"
  fi
fi

if [ -n "$p1_skip" ]; then
  record SKIP "Phase 1 direct P/D KV leg transfer" "$p1_skip"
else
  prefill_url="$(metrics_url "$prefill" || true)"
  decode_url="$(metrics_url "$decode" || true)"
  if [ -z "$prefill_url" ] || [ -z "$decode_url" ]; then
    record FAIL "Phase 1 metrics endpoints advertised" "prefill='${prefill_url:-none}' decode='${decode_url:-none}'"
  else
    STORE="$(jq -r '.spec.kvCache.poolRef.name // empty' <<<"$md_json")"
    store_url=""
    if [ -n "$STORE" ]; then
      protocol="$(pod_field "$prefill" '((.metadata.annotations["kvcache.gpustack.ai/client-config"] // "{}" | fromjson | .protocol) // ([.spec.containers[] | select(.name == "main") | .env[]? | select(.name == "MOONCAKE_PROTOCOL") | .value][0])) // empty')"
      if [ "$protocol" = tcp ]; then
        pool_name="$(kubectl -n "$NS" get kvcachepoolbinding "$STORE" -o jsonpath='{.spec.poolRef.name}' 2>/dev/null || true)"
        endpoint="$(kubectl get kvcachepool "$pool_name" -o jsonpath='{.status.clientEndpoint}' 2>/dev/null || true)"
        [ -n "$endpoint" ] && store_url="http://${endpoint%:*}:9003/metrics"
      fi
    fi

    before_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods")"
    sent_before=""
    received_before=""
    put_before=0
    prom_ok=yes
    if [ "$SP_TRANSFER" = prom ]; then
      if ! sent_before="$(read_metric "$prefill_url" "$SP_SENT_COUNT")"; then
        record FAIL "prefill exports ${SP_SENT_COUNT}" "${SP_TRANSFER_SOURCE:-missing metric}"
        prom_ok=no
      fi
      received_before="$(read_metric "$decode_url" "$SP_RECEIVED" || echo 0)"
    else
      sent_before="$(log_count "$prefill" "$SP_SENT_LOG")"
      received_before="$(log_count "$decode" "$SP_RECEIVED_LOG")"
    fi
    [ -z "$store_url" ] || put_before="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"

    if [ "$prom_ok" = yes ]; then
      completed=0
      tail_id="$(date +%s)-$$"
      for i in $(seq 1 "$REQUESTS"); do
        stream=false
        [ $((i % 2)) -eq 0 ] && stream=true
        payload="$(jq -nc --arg model "$MODEL" --argjson stream "$stream" --arg q "${tail_id}-${i}" '{model:$model,
          messages:[{role:"user",content:("Explain how a Kubernetes controller reconciles a desired state. Case 32 question " + $q + ".")}],
          max_tokens:48,stream:$stream}')"
        response="$(kubectl -n "$NS" exec "$PROBE" -- curl -sS --max-time 60 \
          -w '\nHTTP_STATUS:%{http_code}\n' -H 'Content-Type: application/json' \
          --data-binary "$payload" "http://${MD}-router.${NS}.svc:8000/v1/chat/completions")" || continue
        [[ "$response" == *'HTTP_STATUS:200'* ]] || continue
        if [ "$stream" = true ]; then
          grep -q '"content": *"[^"]' <<<"$response" && ! grep -q '"error"' <<<"$response" \
            && [[ "$response" == *'[DONE]'* ]] && completed=$((completed + 1))
        elif [ -n "$(sed '/^HTTP_STATUS:/d' <<<"$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null)" ]; then
          completed=$((completed + 1))
        fi
      done

      for pod in "$prefill" "$decode"; do
        logs="$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)"
        case "$SP_PIN" in
          env)
            check_bool "$(pod_field "$pod" '[.spec.containers[] | select(.name == "main") | .env[]? | select(.name == "MC_FORCE_TCP")] | (length == 1 and .[0].value == "1")')" \
              "${pod} carries exactly one MC_FORCE_TCP=1"
            check_bool "$([[ "$logs" != *'Using cross-node NVLink'* ]] && echo true || echo false)" \
              "${pod} logs no cross-node NVLink selection"
            ;;
          argv)
            # shellcheck disable=SC2086,SC2153 # SP_PIN_ARG intentionally splits into flag/value args from _serving-lib.sh.
            check_bool "$(has_arg "$pod" $SP_PIN_ARG)" "${pod} renders ${SP_PIN_ARG}"
            ;;
          none)
            record SKIP "${pod} transport pin" "not applicable on ${VENDOR}"
            ;;
        esac
        [ -z "${SP_PIN_LOG:-}" ] \
          || check_bool "$([[ "$logs" == *"$SP_PIN_LOG"* ]] && echo true || echo false)" "${pod} logs TCP-only transport selection"
      done

      check_bool "$([ "$completed" -eq "$REQUESTS" ] && echo true || echo false)" \
        "routed P/D requests completed (${completed}/${REQUESTS})"

      if [ "$SP_TRANSFER" = prom ]; then
        sent_after="$(read_metric "$prefill_url" "$SP_SENT_COUNT" || echo "$sent_before")"
        sent_sum="$(read_metric "$prefill_url" "$SP_SENT_SUM" || echo 0)"
        received_after="$(read_metric "$decode_url" "$SP_RECEIVED" || echo "$received_before")"
        failed="$(awk -v a="$(read_metric "$prefill_url" "$SP_FAILED" || echo 0)" \
          -v b="$(read_metric "$decode_url" "$SP_FAILED" || echo 0)" 'BEGIN { print a + b }')"
        check_bool "$(grew "$sent_before" "$sent_after")" "prefill transfer samples grew (${sent_before} -> ${sent_after})"
        check_bool "$(grew 0 "$sent_sum")" "prefill transferred size positive (${sent_sum})"
        check_bool "$(grew "$received_before" "$received_after")" "decode received KV grew (${received_before} -> ${received_after})"
        check_bool "$(awk -v f="$failed" 'BEGIN { print (f == 0) ? "true" : "false" }')" "zero failed KV transfers (${failed})"
      else
        check_bool "$(grew "$sent_before" "$(log_count "$prefill" "$SP_SENT_LOG")")" "prefill logged sent transfer"
        check_bool "$(grew "$received_before" "$(log_count "$decode" "$SP_RECEIVED_LOG")")" "decode logged received transfer"
      fi

      if [ -n "$store_url" ]; then
        put_after="$(read_metric "$store_url" master_batch_put_end_items_total || echo 0)"
        check_bool "$(grew "$put_before" "$put_after")" "store master batch-put items grew (${put_before} -> ${put_after})"
        if [ "$ENGINE" = SGLang ]; then
          decode_logs="$(kubectl -n "$NS" logs "$decode" -c main 2>/dev/null)"
          check_bool "$([[ "$decode_logs" == *"disaggregation_decode_retraction_backup='cpu_tensor'"* || "$decode_logs" == *"'disaggregation_decode_retraction_backup': 'cpu_tensor'"* ]] && echo true || echo false)" \
            "SGLang decode runs cpu_tensor retraction backup"
          check_bool "$([ "$(has_arg "$decode" --enable-hierarchical-cache)" = false ] && echo true || echo false)" \
            "SGLang decode renders no hierarchical cache"
          check_bool "$(has_arg "$prefill" --enable-hierarchical-cache)" "SGLang prefill renders hierarchical cache"
          for pod in "$prefill" "$decode"; do
            check_bool "$([[ "$(kubectl -n "$NS" logs "$pod" -c main 2>/dev/null)" != *'Not enough host memory'* ]] && echo true || echo false)" \
              "${pod} logs no host-memory refusal"
          done
        fi
      fi

      pods_after="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${MD}" -o json)" || exit 2
      after_restarts="$(jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0' <<<"$pods_after")"
      check_bool "$([ "$after_restarts" -eq "$before_restarts" ] && echo true || echo false)" \
        "Phase 1 engine restart count unchanged (${before_restarts} -> ${after_restarts})"
    fi
  fi
fi

# =================================================================================================
# PHASE 2: Continuous-traffic router/role scaling (1 -> 2 -> 1) with zero dropped requests
# =================================================================================================

if [ "$TARGET" = none ]; then
  record SKIP "Phase 2 continuous-traffic scaling" "skipped by caller (target=none)"
else
  md_json="$(kubectl -n "$NS" get modeldeployment "$MD" -o json)" || exit 2
  SPLIT="$(jq -r 'any(.spec.roles[]; .kind == "Prefill")' <<<"$md_json")"
  p2_skip=""
  SELECTOR=""
  COUNTER=""

  if [ "$(jq -r '.status.phase' <<<"$md_json")" != Ready ]; then
    p2_skip="${MD} is not Ready"
  elif [ "$TARGET" = router ]; then
    if [ "$(jq -r '.spec.router.replicas // 1' <<<"$md_json")" != 1 ]; then
      p2_skip="router does not start at replicas=1"
    else
      SCALED_PATCH_PATH="/spec/router/replicas"
      SELECTOR="app.kubernetes.io/instance=${MD},modeldeployment.gpustack.ai/router=${ROUTER}"
      COUNTER="$(serving_router_requests "$ROUTER" "$SPLIT")"
    fi
  else
    role_idx="$(jq -r --arg k "$TARGET" '[.spec.roles | to_entries[] | select((.value.kind | ascii_downcase) == $k) | .key] | if length == 1 then .[0] else empty end' <<<"$md_json")"
    if [ -z "$role_idx" ]; then
      p2_skip="deployment does not have exactly one role of kind ${TARGET}"
    elif [ "$(jq -r ".spec.roles[${role_idx}].replicas" <<<"$md_json")" != 1 ]; then
      p2_skip="${TARGET} role does not start at replicas=1"
    else
      SCALED_PATCH_PATH="/spec/roles/${role_idx}/replicas"
      SELECTOR="app.kubernetes.io/instance=${MD},app.kubernetes.io/component=$(jq -r ".spec.roles[${role_idx}].name" <<<"$md_json")"
      case "$TARGET" in
        prefill) COUNTER="$SP_SENT_COUNT" ;;
        decode) COUNTER="$SP_RECEIVED" ;;
        server) COUNTER="$SP_SERVED" ;;
      esac
    fi
  fi

  if [ -z "$p2_skip" ] && [ -z "$COUNTER" ]; then
    p2_skip="no counter available for ${TARGET} on ${ENGINE}/${VENDOR}/${ROUTER}"
  fi

  if [ -n "$p2_skip" ]; then
    SCALED_PATCH_PATH=""
    record SKIP "Phase 2 continuous-traffic scaling (${TARGET})" "$p2_skip"
  else
    ready_target_pods() {
      kubectl -n "$NS" get pods -l "$SELECTOR" -o json | jq -r '.items[] | select(.metadata.deletionTimestamp == null and
        any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name' | sort
    }
    wait_ready_count() {
      local deadline=$((SECONDS + $2))
      while [ "$SECONDS" -lt "$deadline" ]; do
        [ "$(ready_target_pods | grep -c . || true)" -eq "$1" ] \
          && [ "$(kubectl -n "$NS" get pods -l "$SELECTOR" --no-headers 2>/dev/null | grep -c . || true)" -eq "$1" ] && return 0
        sleep 5
      done
      return 1
    }
    counter_of() {
      local ip port scheme
      ip="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}')"
      port="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/port}')"
      scheme="$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.annotations.prometheus\.io/scheme}')"
      serving_sum "$(kubectl -n "$NS" exec "$PROBE" -- curl -fsS --max-time 10 \
        "${scheme:-http}://${ip}:${port}/metrics" 2>/dev/null)" "$COUNTER" || echo 0
    }
    restarts_map() {
      kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" -o json \
        | jq -c '[.items[] | {key: .metadata.name, value: ([.status.containerStatuses[]?.restartCount] | add // 0)}] | from_entries'
    }

    kubectl -n "$NS" exec "$PROBE" -- sh -c "rm -f ${TRAFFIC}.stop ${TRAFFIC}.log; echo base >${TRAFFIC}.phase; nohup sh -c '
      i=0
      while [ ! -e ${TRAFFIC}.stop ]; do
        i=\$((i + 1)); s=false; [ \$((i % 2)) -eq 0 ] && s=true
        b=\"{\\\"model\\\":\\\"${MODEL}\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"Explain how a controller reconciles state. Case 32 scale question \$i \$(date +%s%N).\\\"}],\\\"max_tokens\\\":32,\\\"stream\\\":\$s}\"
        c=\$(curl -sS --max-time 60 -o /tmp/e2e-c32-r -w %{http_code} -H Content-Type:application/json --data-binary \"\$b\" http://${MD}-router.${NS}.svc:8000/v1/chat/completions 2>/dev/null)
        v=\"BAD \$c \$(head -c 200 /tmp/e2e-c32-r | tr -d \"\\n\")\"
        if [ \"\$c\" = 200 ] && grep -q \"\\\"content\\\": *\\\"[^\\\"]\" /tmp/e2e-c32-r && ! grep -q \"\\\"error\\\"\" /tmp/e2e-c32-r; then v=ok; fi
        echo \"\$(date +%s) \$(cat ${TRAFFIC}.phase) \$v\" >>${TRAFFIC}.log
      done' >/dev/null 2>&1 &"

    set_phase() { kubectl -n "$NS" exec "$PROBE" -- sh -c "echo $1 >${TRAFFIC}.phase"; }
    tally() {
      kubectl -n "$NS" exec "$PROBE" -- sh -c "cat ${TRAFFIC}.log 2>/dev/null" \
        | awk -v p="$1" -v want="$2" '$2 == p && $3 == want { n++ } END { print n + 0 }'
    }

    sleep "$SETTLE"
    old_pods="$(ready_target_pods)"
    before_restarts_map="$(restarts_map)"
    set_phase up
    scale_target 2
    if ! wait_ready_count 2 "$READY_BOUND"; then
      record FAIL "second ${TARGET} Pod becomes Ready within ${READY_BOUND}s" "$MD"
    else
      new_pod="$(comm -13 <(printf '%s\n' "$old_pods") <(ready_target_pods) | head -1)"
      check_bool "$([ -n "$new_pod" ] && echo true || echo false)" "second ${TARGET} Pod becomes Ready (${new_pod:-none})"
      set_phase two
      ready_count="$(counter_of "$new_pod")"
      sleep "$SETTLE"
      work="$(awk -v a="$(counter_of "$new_pod")" -v b="$ready_count" 'BEGIN { printf "%.10g", a - b }')"
      concentrates=""
      [ "$TARGET" = server ] && concentrates="$(serving_router_concentrates "$ROUTER")"
      if [ -n "$concentrates" ] && awk -v w="$work" 'BEGIN { exit !(w == 0) }'; then
        record SKIP "new ${TARGET} Pod did work" "${COUNTER} grew by 0 on ${new_pod}: ${concentrates}"
      else
        check_bool "$(awk -v w="$work" 'BEGIN { print (w > 0) ? "true" : "false" }')" \
          "new ${TARGET} Pod did work (${COUNTER} +${work} on ${new_pod})"
      fi
      if [ -n "${E2E_SCALE_LOG_DIR:-}" ]; then
        mkdir -p "$E2E_SCALE_LOG_DIR"
        kubectl -n "$NS" logs "$new_pod" --all-containers --prefix >"${E2E_SCALE_LOG_DIR}/${new_pod}.log" 2>&1 || true
      fi

      set_phase down
      scale_target 1
      wait_ready_count 1 "$READY_BOUND" || true
      check_bool "$([ "$(ready_target_pods | grep -c . || true)" -eq 1 ] && echo true || echo false)" \
        "one ${TARGET} Pod remains Ready after scale-down"
      set_phase one
      sleep "$SETTLE"
      kubectl -n "$NS" exec "$PROBE" -- touch "${TRAFFIC}.stop" >/dev/null 2>&1 || true
      sleep 5

      for p in up two; do
        ok_n="$(tally "$p" ok)"
        bad_n="$(tally "$p" BAD)"
        check_bool "$([ "$bad_n" -eq 0 ] && [ "$ok_n" -gt 0 ] && echo true || echo false)" \
          "zero dropped requests during scale-up (${p}: ok=${ok_n}, bad=${bad_n})"
      done
      for p in down one; do
        ok_n="$(tally "$p" ok)"
        bad_n="$(tally "$p" BAD)"
        check_bool "$([ "$bad_n" -eq 0 ] && [ "$ok_n" -gt 0 ] && echo true || echo false)" \
          "zero dropped requests during scale-down (${p}: ok=${ok_n}, bad=${bad_n})"
      done

      after_restarts_map="$(restarts_map)"
      check_bool "$(jq -rn --argjson a "$before_restarts_map" --argjson b "$after_restarts_map" \
        '[$a | keys[] | select($b[.] != null) | $a[.] == $b[.]] | all')" \
        "restart counts of persistent Pods unchanged across scale"
    fi
  fi
fi

results
