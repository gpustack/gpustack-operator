#!/usr/bin/env bash
#
# _serving-lib.sh — where a serving ModelDeployment's evidence lives, per vendor and engine, for the
# serving cases (90, 91, 94).
#
# NOT A CASE. It carries no case header, no trap and no results table; each stays with the case that
# sources it. What it holds is the ONE place a vendor or engine difference is written: a case reads
# the SP_* variables below instead of branching on a vendor or engine name itself, so aligning a new
# vendor is an edit here rather than a search through every case.
#
# A case uses it as:
#
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_serving-lib.sh"
#     serving_profile "$(serving_vendor "$md_json")" "$(jq -r .spec.engine.name <<<"$md_json")" ||
#       exit 2
#
# serving_profile VENDOR ENGINE sets:
#   SP_TRANSFER        prom | log. Where a moved block is counted: a Prometheus series on the prefill
#                      half, or a per-request log line on each half.
#   SP_SENT_COUNT      prom: the prefill half's transfer sample counter; it grows once per transfer.
#   SP_SENT_SUM        prom: the prefill half's transferred-size sum; positive once anything moved.
#   SP_FAILED          prom: the transfer failure counter, read on both halves and summed. A labeled
#                      counter that exports nothing before its first increment reads as zero.
#   SP_RECEIVED        prom: a counter on a decode Pod that grows only when that Pod was handed KV
#                      computed elsewhere — the evidence a NEW decoder took part.
#   SP_SENT_LOG        log: the prefill half's line for one request id.
#   SP_RECEIVED_LOG    log: the decode half's line for the same request id.
#   SP_TRANSFER_SOURCE the provenance of those series, printed beside a failure, because an absent
#                      series is more often an image that does not carry it than a pair that moved
#                      nothing.
#   SP_PIN             env | argv | none. How the operator pins the direct leg to TCP: the
#                      MC_FORCE_TCP=1 environment variable, an engine argument, or not at all.
#   SP_PIN_ARG         argv: the flag and value that pin it.
#   SP_PIN_LOG         the line each half logs when the pin took effect; empty with SP_PIN=none.
#   SP_ROUTERS         the routers that render a transfer leg in front of this pair, space separated.
#   SP_SERVED          prom: a counter on an engine Pod that grows with each request it finished, for
#                      a role that serves whole requests.
#
# serving_vendor MD_JSON prints the accelerator manufacturer of the deployment's first role's
# InstanceType (nvidia, ascend, ...). E2E_VENDOR overrides it.

serving_vendor() {
  if [ -n "${E2E_VENDOR:-}" ]; then
    echo "$E2E_VENDOR"
    return
  fi
  local it
  it="$(jq -r '.spec.roles[0].instanceType // empty' <<<"$1")"
  [ -n "$it" ] || return 1
  kubectl get instancetypes.worker.gpustack.ai "$it" -o jsonpath='{.status.detail.manufacturer}'
}

# The SP_* variables are this function's output, read by the case that sourced this file.
# shellcheck disable=SC2034
serving_profile() {
  SP_TRANSFER="" SP_SENT_COUNT="" SP_SENT_SUM="" SP_FAILED="" SP_RECEIVED=""
  SP_SENT_LOG="" SP_RECEIVED_LOG="" SP_TRANSFER_SOURCE=""
  SP_PIN=none SP_PIN_ARG="" SP_PIN_LOG="" SP_ROUTERS="" SP_SERVED=""
  case "$1/$2" in
    nvidia/vllm)
      SP_TRANSFER="prom"
      SP_SENT_COUNT=vllm:mooncake_bytes_transferred_count
      SP_SENT_SUM=vllm:mooncake_bytes_transferred_sum
      SP_FAILED=vllm:mooncake_num_failed_transfers_total
      SP_RECEIVED=vllm:external_prefix_cache_hits_total
      # The Mooncake connector upstream exports no Prometheus series of its own; these come from a
      # patch the gpustack runner image carries. An upstream vLLM image logs the same transfers as
      # "KV Transfer metrics" lines instead.
      SP_TRANSFER_SOURCE="the vllm:mooncake_* series exist only in the gpustack runner image"
      SP_PIN="env"
      SP_PIN_LOG="MC_FORCE_TCP is set"
      SP_ROUTERS="llm-d-router vllm-router"
      SP_SERVED=vllm:request_success_total
      ;;
    nvidia/sglang)
      SP_TRANSFER="prom"
      SP_SENT_COUNT=sglang:kv_transfer_total_mb_count
      SP_SENT_SUM=sglang:kv_transfer_total_mb_sum
      SP_FAILED=sglang:num_transfer_failed_reqs_total
      # A decode half generates tokens only for a request whose blocks arrived. Its transfer
      # allocation histogram is no evidence: it is observed before the transfer, and it kept growing
      # while every transfer of a locked pair failed.
      SP_RECEIVED=sglang:generation_tokens_total
      SP_TRANSFER_SOURCE="SGLang records the kv_transfer_* histograms on the prefill half alone"
      SP_PIN="argv"
      SP_PIN_ARG="--disaggregation-transfer-backend mooncake_tcp"
      SP_PIN_LOG="MC_FORCE_TCP is set"
      SP_ROUTERS="llm-d-router sglang-gateway"
      SP_SERVED=sglang:num_requests_total
      ;;
    ascend/vllm)
      # vLLM-Ascend's connector reports each transfer in the log, one line per request id on each
      # half; no Prometheus series for it has been read. Its leg hardcodes its transport, so there
      # is no TCP pin to look for, and only llm-d-router relays the handshake it waits for.
      SP_TRANSFER="log"
      SP_SENT_LOG="Delaying free of"
      SP_RECEIVED_LOG="KV cache transfer for request"
      SP_TRANSFER_SOURCE="vLLM-Ascend logs one line per request on each half"
      SP_ROUTERS="llm-d-router"
      # The engine's own request counter keeps vLLM's name; it has not been read on this vendor.
      SP_SERVED=vllm:request_success_total
      ;;
    *)
      echo "no serving profile for vendor '$1' and engine '$2'" >&2
      return 1
      ;;
  esac
}

# serving_router_requests ROUTER SPLIT prints the router's own request counter; SPLIT is true for a
# prefill/decode deployment, where the vLLM router counts only its pd_* series.
serving_router_requests() {
  case "$1/$2" in
    llm-d-router/*) echo llm_d_epp_request_total ;;
    vllm-router/true) echo vllm_router_pd_requests_total ;;
    vllm-router/false) echo vllm_router_requests_total ;;
    sglang-gateway/*) echo smg_router_requests_total ;;
    *) return 1 ;;
  esac
}

# serving_router_concentrates ROUTER prints why ROUTER may keep every request on one of several
# equal servers, and nothing for a router that spreads them. The vLLM router and the SGLang gateway
# both default to a cache_aware policy that sends requests sharing a prefix to the worker already
# holding it, so a second server can stay idle under traffic that shares one; that is the policy
# working, not a server that failed.
serving_router_concentrates() {
  case "$1" in
    vllm-router | sglang-gateway) echo "$1's cache_aware policy keeps requests sharing a prefix on the worker holding it" ;;
  esac
}

# serving_sum TEXT NAME sums every series of NAME in a Prometheus exposition and fails when none is
# exposed.
serving_sum() {
  awk -v name="$2" 'index($1, name) == 1 && (length($1) == length(name) ||
    substr($1, length(name) + 1, 1) == "{") { total += $2; found = 1 }
    END { if (!found) exit 1; printf "%.10g\n", total }' <<<"$1"
}
