#!/usr/bin/env bash
#
# CASE 59 — Each rendered artifact is one its own engine accepts: both vehicles, each gated
#   (MUTATING, self-recovering, AUTO-SKIPS each half without its engine image)
#
#   case-59.sh <NS>
#
# Goal:        Everything upstream of this case proves we render what we BELIEVE is right. Only this
#              one proves an engine accepts it. The two halves ask the same question of different
#              artifacts, which is why they are one case rather than two:
#
#                vLLM   - feed the projected file to MooncakeStoreConfig.from_file and assert
#                         __post_init__ does not raise. The mode/global_segment_size pair is what is
#                         being proven: getting it wrong aborts the engine at startup, and it is a
#                         pairing no unit test of ours can check, because the rule lives in the
#                         engine.
#                SGLang - call the engine's own _load_config and assert on the config it returns,
#                         never on os.environ. Reading the environment directly would prove only that
#                         we set the variables we meant to set; what has to be proven is WHICH BRANCH
#                         SGLang takes, and a fixture reading os.environ would be green even if the
#                         file branch had swallowed the injection whole. Then three things about that
#                         config: local_hostname is the Pod's IP rather than "localhost" (the value
#                         that decided the vehicle), tenant_id is the Binding's own reuse domain, and
#                         that tenant differs from the engine default - because the store call only
#                         forwards a non-default one, so a build that read our value and then dropped
#                         it would satisfy the first check and fail this one.
#
#              Each half is gated on its own image and SKIPs INDEPENDENTLY. One image present must not
#              let the other half report green by omission, and a SKIP says the schema is unverified
#              against that engine rather than passing quietly.
#
# Environment: as case 53, plus an engine image per half: E2E_VLLM_IMAGE and E2E_SGLANG_IMAGE. Each
#              half AUTO-SKIPS INDEPENDENTLY when its image is unset, and says which schema is left
#              unverified - one image present must not let the other half report green by omission.
#
#              DISK, AND WHY THIS IS A STEP COUNT RATHER THAN A PARAMETER. Measured 2026-09-04 from
#              the arm64 manifests, scaled by a ratio measured on a third image (6.0GB of layers ->
#              22.7GB resident, 3.8x): vLLM 13.6GB of layers / ~52GB resident, SGLang 18.1GB / ~69GB,
#              vllm-ascend 6.0GB / 22.7GB. `docker image inspect .Size` reports the FIRST column and
#              `docker system df -v` the second, both in GB, so a plan built on the wrong one is off
#              by nearly 4x. NO TWO OF THESE IMAGES FIT AT ONCE on an ordinary workstation.
#              => Set ONE image variable per run and remove the image before the next run. A run with
#              two set does not fail on an assertion - it fails on the pull, and an ImagePullBackOff
#              looks exactly the same when the image is genuinely broken, so the red points at the
#              wrong subsystem. The independent SKIPs make one-at-a-time a supported way to run this
#              rather than a degraded one.
#
#              Images, and why these rather than the newest. The reason outlives the tag: a tag gets
#              bumped by whoever needs a newer engine, and then only the reason says whether the bump
#              was safe.
#
#                E2E_VLLM_IMAGE=gpustack/runner:cuda12.9-vllm0.25.1
#                  0.25.1 because that is the version engine.go's facts table was READ at. This case
#                  proves an engine accepts what we render, and what we render was decided from that
#                  version's source; testing a different one silently changes the question. There is
#                  also a hard FLOOR of 0.21.1: vLLM's Mooncake store connector - the module holding
#                  MooncakeStoreConfig, from_file and load_from_config - was added 2026-05-13 and
#                  first shipped in v0.21.1rc0. On anything older this half cannot fail meaningfully,
#                  it can only ImportError, and the injected file has no reader at all. Bumping this
#                  tag means re-reading the facts table at the new version, not just changing a string.
#
#                E2E_SGLANG_IMAGE=gpustack/runner:cuda12.9-sglang0.5.18
#                  0.5.18 because that is now the version engine.go's facts table is read at, and the
#                  two must match for the same reason the vLLM half must. They did NOT match before:
#                  the table recorded gateway-v0.3.1-1689, a tag this project neither deploys nor
#                  tests, and this header used to say so as an accepted gap. It was not a gap, it was
#                  a wrong entry - that build does forward a tenant - and the mismatch is what hid it.
#                  A version named here that nothing runs is worse than no version at all.
#
#              Neither image is used for inference: no GPU, no weights, no model. Both halves exec one
#              python3 -c against the engine's own configuration reader, which is why cuda12.9 is
#              picked over cuda13.0 - a wider driver floor for a container that only imports.
# Inputs:      a real Pod per engine, running that engine's OWN configuration reader against the
#              artifact this webhook injected. Nothing is mocked, and that is the point: everything
#              upstream of this case proves we render what we believe is right.
# Expected:    with E2E_VLLM_IMAGE, the file parses and does not raise; with E2E_SGLANG_IMAGE,
#              _load_config takes the env branch and reports the Pod's IP; without an image, that half
#              SKIPs and says what is unverified.
#
#              SKIPS: each half independently, when its own engine image is unset. One image present
#              must not let the other half report green by omission, and a SKIP says which schema is
#              left unverified rather than passing quietly. The footer counts skips separately, so a
#              run of two SKIPs cannot be read as a run that verified both engines.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
#
# NOT RE-RUN since the Pod IP gained an empty-value check. On that run the IP was present, so the new
# branch was never taken; it exists so an unreadable IP is reported as an unreadable IP rather than as
# every expected value being absent.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster, both halves passing - AND THAT RUN IS NOT EVIDENCE
# FOR WHAT THIS FILE NOW ASSERTS. It exercised an earlier revision whose SGLang verdict keyed on
# HOSTNAME alone: the probe printed five branch booleans that no pass condition ever read, so the case
# was green while the facts underneath it were wrong. An external review found the wrong fact, not
# this case.
#
# What that run did establish, and what it did not:
#
#   vLLM    IMPORT_OK, then PARSED mode=standalone-store segment=0 buffer=134217728, in a container
#           with no GPU and no CUDA runtime. Still valid - though the verdict now matches that line
#           whole rather than by its PARSED prefix, because a prefix match passes a reader that
#           ignored every injected key and returned its own defaults.
#   SGLang  Reported the env branch and the Pod's IP. Says nothing about the tenant, which this
#           revision injects and asserts, because that behaviour did not exist yet.
#
# An earlier run of this file FAILED the SGLang half after 1200s with the Pod still ContainerCreating.
# That failure is why run_in distinguishes a pull that has not finished from a probe that produced
# nothing: the message named the pull and said it reported nothing about the engine, where the
# previous shape would have said the probe printed nothing and pointed at the wrong thing entirely.
#
# The pull is also why the two halves are run one at a time; the sizes and the reason are under
# Environment above.
set -uo pipefail

NS="${1:?usage: case-59.sh <NS>}"
CASE_ID=59
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

# run_in returns 0 when the probe ran, 1 when the Pod never appeared, 2 when it appeared but never
# became Ready, and 4 when the image reference was refused before anything was submitted.
# The Ready case is separated because an engine image is several GB: a pull that has not
# finished leaves a Pod that is stored but not running, the exec then fails, and its empty log is
# indistinguishable from a probe that ran and printed nothing. Those two call for opposite actions -
# wait longer, versus go read why the module produced no output - so the caller is told which it is.
# The exact values this webhook renders, so the vLLM half matches the whole line rather than its
# prefix. Under a prefix match any PARSED output passes - including one where every injected key was
# ignored and the reader returned its own defaults, which is the outcome this half exists to rule out.
# ALL THREE differ from this reader's defaults, which is what makes the whole-line match decisive:
# the reader defaults to mode=embedded and 4 GiB for BOTH sizes (worker.py:74-75 at 752a3a50), while
# we render standalone-store, segment 0 (an engine container contributes no memory to the pool -
# client_config.go:25-29) and a 128 MiB buffer (client_config.go:31-40). Read the constants below as
# the authority: they decide the verdict, and this comment is only what a reader trusts afterwards.
VLLM_WANT_MODE=standalone-store
VLLM_WANT_SEGMENT=0
VLLM_WANT_BUFFER=134217728

E2E_ENGINE_READY_TIMEOUT="${E2E_ENGINE_READY_TIMEOUT:-1200}"
run_in() {
  local pod="$1" image="$2" engine="$3" script="$4" log="$5"
  # The image is interpolated into the rendered YAML. `|` and `&` open a block scalar and an anchor
  # when they lead a value, so an unusual reference would produce a manifest nobody wrote; refusing it
  # keeps a quoting bug from being read as an engine verdict.
  case "$image" in
    *[!A-Za-z0-9./:_@-]* | "") return 4 ;;
  esac
  KVI_IMAGE="$image" kvi_pod_manifest "$pod" "$engine" \
    | kubectl apply -f - >/dev/null 2>&1
  kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null || return 1
  if ! kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${pod}" \
      --timeout="${E2E_ENGINE_READY_TIMEOUT}s" >/dev/null 2>&1; then
    return 2
  fi
  kubectl -n "$TEST_NS" exec "$pod" -c engine -- python3 -c "$script" >"$log" 2>&1 || true
  return 0
}

# not_ready_reason reports what the kubelet says about a Pod that never became Ready, so a timeout
# names the pull rather than leaving the reader to guess it.
not_ready_reason() {
  local pod="$1"
  kubectl -n "$TEST_NS" get pod "$pod" \
    -o 'jsonpath={range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{end}' 2>/dev/null \
    | cut -c1-120
}

# vLLM: the file, through the engine's own reader.
if [ -z "${E2E_VLLM_IMAGE:-}" ]; then
  record SKIP "vLLM's own reader accepts the projected file" \
    "E2E_VLLM_IMAGE is unset. The file schema is UNVERIFIED against the real engine: the Go tests \
prove we render what we believe is right, and only this half proves vLLM accepts it"
else
  LOG_V="/tmp/kvc-inject-59-vllm-${SFX}.log"
  # No `if !` around the call: the negation would be what $? then reports, collapsing run_in's three
  # outcomes into two and losing exactly the one that had to be told apart.
  if run_in vllm-probe "$E2E_VLLM_IMAGE" vllm '
import os, sys
# The import is a step this case OBSERVES, not a precondition it assumes. The dataclass shares a
# worker-side module with transfer-thread and lookup-server scaffolding, so importing it can pull up
# heavy vLLM initialisation. An import failure and a schema rejection call for opposite actions - one
# says nothing at all about what we render - so they are reported as different outcomes.
try:
    from vllm.distributed.kv_transfer.kv_connector.v1.mooncake.store.worker import MooncakeStoreConfig
except BaseException as e:
    print("IMPORT_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("IMPORT_OK")
try:
    cfg = MooncakeStoreConfig.from_file(os.environ["MOONCAKE_CONFIG_PATH"])
except Exception as e:
    print("RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("PARSED mode=%s segment=%s buffer=%s" % (cfg.mode, cfg.global_segment_size, cfg.local_buffer_size))
' "$LOG_V"; rc_v=$?; [ "$rc_v" -ne 0 ]; then
    if [ "$rc_v" -eq 2 ]; then
      record FAIL "vLLM's own reader accepts the projected file" \
        "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s: $(not_ready_reason vllm-probe). \
An engine image is several GB, so this is most likely still pulling; it says NOTHING about the file \
we render. Raise E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    elif [ "$rc_v" -eq 4 ]; then
      record FAIL "vLLM's own reader accepts the projected file" \
        "E2E_VLLM_IMAGE ('${E2E_VLLM_IMAGE}') carries a character this case will not interpolate into \
a sed replacement. Nothing was submitted, so this says NOTHING about the file we render"
    else
      record FAIL "vLLM's own reader accepts the projected file" "the probe Pod never appeared"
    fi
  elif grep -q '^IMPORT_RAISED ' "$LOG_V"; then
    record FAIL "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^IMPORT_RAISED ' "$LOG_V") - the module would not import, so this run says NOTHING \
about the file we render, in either direction; see ${LOG_V}"
  elif grep -qxF "PARSED mode=${VLLM_WANT_MODE} segment=${VLLM_WANT_SEGMENT} buffer=${VLLM_WANT_BUFFER}" "$LOG_V"; then
    record PASS "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^PARSED ' "$LOG_V") - every value is the one we rendered, and ALL THREE differ from \
this reader's own defaults (mode=embedded, and 4GiB for both sizes - worker.py:74-75), so the file \
was read rather than fallen back from. __post_init__ did not raise, so the mode/segment pair agrees"
  else
    record FAIL "vLLM's own reader accepts the projected file" \
      "$(grep -m1 '^RAISED ' "$LOG_V" || grep -m1 '^PARSED ' "$LOG_V" || \
echo 'the probe printed neither PARSED nor RAISED'); expected exactly \
'PARSED mode=${VLLM_WANT_MODE} segment=${VLLM_WANT_SEGMENT} buffer=${VLLM_WANT_BUFFER}'. A PARSED line \
with other values means the reader fell back to its own defaults rather than reading what we \
projected, which is a pass under a prefix match and a failure under this one; see ${LOG_V}"
  fi
fi

# SGLang: the environment, through the engine's own selection.
if [ -z "${E2E_SGLANG_IMAGE:-}" ]; then
  record SKIP "SGLang takes the env branch and resolves the Pod's IP" \
    "E2E_SGLANG_IMAGE is unset. The environment vehicle is UNVERIFIED against the real engine, and it \
is the half where a wrong answer is silent: the file branch would return localhost and still start"
else
  LOG_S="/tmp/kvc-inject-59-sglang-${SFX}.log"
  # As above: no `if !`, so run_in's three outcomes survive into $?.
  if run_in sglang-probe "$E2E_SGLANG_IMAGE" sglang '
import io, logging, sys
from sglang.srt.mem_cache.storage.mooncake_store.mooncake_store import MooncakeBaseStore
from sglang.srt import environ as envs

# _load_config is the SELECTOR, and calling it is the whole point: load_from_env is a staticmethod,
# so calling that one IS choosing the env branch, and a probe built on it proves only that the env
# branch parses - never that the engine would have picked it. Picking it is what this webhook bet on.
#
# MooncakeBaseStore rather than MooncakeStore: __init__ there assigns two Nones and nothing else, so
# constructing one needs no GPU and, like the module top level, never imports the mooncake package.
buf = io.StringIO()
logging.getLogger().addHandler(logging.StreamHandler(buf))
logging.getLogger().setLevel(logging.INFO)

print("CONFIG_PATH_SET=%s" % envs.envs.SGLANG_HICACHE_MOONCAKE_CONFIG_PATH.is_set())
try:
    cfg = MooncakeBaseStore()._load_config(None)
except Exception as e:
    print("RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
# Each branch announces itself with its own line. All three are asserted, not just the wanted one: an
# implementation that logged all three would pass a test that only looked for the one we want.
log = buf.getvalue()
print("BRANCH_ENV=%s"   % ("loaded from env successfully" in log))
print("BRANCH_FILE=%s"  % ("loaded from file successfully" in log))
print("BRANCH_EXTRA=%s" % ("loaded from extra_config successfully" in log))
print("HOSTNAME=%s" % cfg.local_hostname)
print("MASTER=%s SEGMENT=%s PROTOCOL=%s" % (cfg.master_server_address, cfg.global_segment_size, cfg.protocol))
# The tenant the engine ended up holding, and then whether it would REACH the store. Those are two
# steps: the config carrying a tenant proves the variable was read, while the store call only forwards
# it when it differs from the engine default - so a build that read our value and then dropped it
# would satisfy the first and fail the second.
print("TENANT=%s" % cfg.tenant_id)
print("TENANT_FORWARDED=%s" % (cfg.tenant_id != "default"))

# Positive control. Without it the extra_config branch is never reached by this case, and the design
# rule it guards - that we must NOT occupy extra_config - reads as an unrelated caution rather than a
# decision with teeth. Its expected value (extra_config) differs from its failure value (env), so it
# discriminates. The branch is chosen on master_server_address alone; the rest falls back to defaults.
buf.truncate(0); buf.seek(0)
class _Populated:
    extra_config = {"master_server_address": "control.invalid:50051"}
try:
    MooncakeBaseStore()._load_config(_Populated())
    log2 = buf.getvalue()
    print("CONTROL_EXTRA=%s" % ("loaded from extra_config successfully" in log2))
    print("CONTROL_ENV=%s"   % ("loaded from env successfully" in log2))
except Exception as e:
    print("CONTROL_RAISED %s: %s" % (type(e).__name__, e))
' "$LOG_S"; rc_s=$?; [ "$rc_s" -ne 0 ]; then
    if [ "$rc_s" -eq 2 ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s: $(not_ready_reason sglang-probe). \
An engine image is several GB, so this is most likely still pulling; it says NOTHING about which \
branch the engine takes. Raise E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    elif [ "$rc_s" -eq 4 ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "E2E_SGLANG_IMAGE ('${E2E_SGLANG_IMAGE}') carries a character this case will not interpolate \
into a sed replacement. Nothing was submitted, so this says NOTHING about which branch the engine takes"
    else
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" "the probe Pod never appeared"
    fi
  else
    POD_IP="$(kubectl -n "$TEST_NS" get pod sglang-probe -o jsonpath='{.status.podIP}' 2>/dev/null)"
    # An empty POD_IP would be embedded into the expected line as "HOSTNAME=", every grep would miss,
    # and the FAIL would list all values as absent - reading as an injection defect when the real
    # cause is that the Pod's IP was never obtained. Named here so the two stay distinguishable.
    if [ -z "$POD_IP" ]; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "the Pod's IP could not be read back, so the expected hostname could not be built; this is \
about reading the Pod, not about what was injected"
      kvi_results "$CASE_ID"; exit 1
    fi
    if ! grep -q '^CONFIG_PATH_SET=' "$LOG_S"; then
      # An empty log and a config path that IS set both fail the next test, and they call for opposite
      # actions - one is a broken probe, the other a real defect in the injection. Separating them here
      # keeps a FAIL from naming the wrong cause, which is the failure mode a refusal message has.
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "the probe printed nothing: the exec did not run, or the image has no sglang module. This says \
nothing about the injection either way; see ${LOG_S}"
    elif ! grep -q '^CONFIG_PATH_SET=False' "$LOG_S"; then
      record FAIL "SGLang takes the env branch and resolves the Pod's IP" \
        "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH is set in the container, so _load_config would take the \
file branch and the injected variables would be inert"
    else
      # EVERY value the probe prints is checked, and the list is written out so a value that is
      # printed but not asserted becomes visible as a missing row rather than as nothing at all.
      # An earlier revision of this case printed the five branch booleans and then keyed its verdict
      # on HOSTNAME alone; the case passed while the branch facts underneath it were wrong, and it
      # took an external review to notice. Absence of a row is the failure mode being guarded here.
      #
      # The three BRANCH_ values are asserted together, positives AND negatives: an implementation
      # that announced every branch would satisfy "took the env branch" on its own.
      # MASTER/SEGMENT/PROTOCOL are here because the probe prints them, and the comment above claims
      # every printed value is checked. It did not: the revision that fixed "printed but not asserted"
      # for the five booleans left three more behind - while asserting in prose that it had not.
      want="BRANCH_ENV=True BRANCH_FILE=False BRANCH_EXTRA=False HOSTNAME=${POD_IP} \
TENANT=${DOMAIN} TENANT_FORWARDED=True CONTROL_EXTRA=True CONTROL_ENV=False"
      want_line="MASTER=${ENDPOINT} SEGMENT=0 PROTOCOL=tcp"
      missing=""
      for expect in $want; do
        grep -qxF "$expect" "$LOG_S" || missing="${missing} ${expect}"
      done
      grep -qxF "$want_line" "$LOG_S" || missing="${missing} [${want_line}]"
      if [ -z "$missing" ]; then
        record PASS "SGLang reads the env branch, the Pod's IP and the injected tenant" \
          "all 9 asserted values matched, including TENANT=${DOMAIN} (the Binding's own reuse domain, \
so the engine will forward it) and local_hostname=${POD_IP} rather than localhost"
      else
        record FAIL "SGLang reads the env branch, the Pod's IP and the injected tenant" \
          "these expected values were absent from the probe output:${missing}. Observed: $(grep -E \
'^(BRANCH_|HOSTNAME|TENANT|CONTROL_)' "$LOG_S" | tr '\n' ' ' | cut -c1-200); see ${LOG_S}"
      fi
    fi
  fi
fi

kvi_results "$CASE_ID"
