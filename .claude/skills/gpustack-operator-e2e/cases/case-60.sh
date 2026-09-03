#!/usr/bin/env bash
#
# CASE 60 — The connector name we render is one that engine's own factory has registered
#   (MUTATING, self-recovering, AUTO-SKIPS each engine without its image)
#
#   case-60.sh <NS>
#
# Goal:        This is the first case that checks the connector NAME. Everything upstream proves we
#              render what we believe is right; CASE 59 proves an engine's config reader accepts the
#              file. Neither touches the factory - and the factory is where an engine turns
#              `kv_connector` into a class, and where a name it does not know raises
#              `ValueError: Unsupported connector type` before any config file is opened.
#
#              TWO ROWS, ONE ASSERTION. This is not "a case for vLLM-Ascend": the vLLM row had never
#              been checked either. We had verified that vLLM's reader accepts our file, never that
#              its factory accepts the name we pair with that file. That row happened to be correct.
#              A dimension nothing covered is the finding here; one engine failing it was the symptom.
#
#              WHY THE WRONG NAME READ AS RIGHT, which is why the FAIL below prints the whole
#              registry rather than only the verdict. After plugin registration vLLM-Ascend's factory
#              holds four names beginning with Mooncake - MooncakeConnector, MooncakeConnectorV1,
#              MooncakeConnectorStoreV1 and MooncakeLayerwiseConnector - and the name we rendered,
#              MooncakeStoreConnector, is not one of them. It is not a typo of any of the four: it is
#              a legal-looking combination of their parts that names nothing. Surrounded by four real
#              neighbours a name like that survives every review that reads it, so the check has to
#              be a lookup rather than a reading, and a reader who sees the failure needs the
#              neighbours in front of them to pick the right one.
#
#              The name is NOT hard-coded. It is read back out of the Pod the webhook just mutated
#              and handed to that engine's factory. A hard-coded expectation would split this into
#              two independent assertions - "we render X" and "the engine knows X" - and the defect
#              lives exactly between them: both halves can be individually right about different X.
#
# Environment: as CASE 53, plus one image per engine. Each row AUTO-SKIPS INDEPENDENTLY when its own
#              image is unset, and says which name is left unverified.
#
#                E2E_VLLM_IMAGE=gpustack/runner:cuda12.9-vllm0.25.1
#                E2E_VLLM_ASCEND_IMAGE=quay.io/ascend/vllm-ascend:v0.19.1rc1
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
#              WHY THIS IS NOT A THIRD HALF OF CASE 59. That case's vLLM half has a hard floor of
#              vLLM 0.21.1, because it imports `...v1.mooncake.store.worker`, added 2026-05-13
#              (case-59.sh:44-48). vLLM-Ascend v0.19.1rc1 pins vLLM v0.19.1, where that module does
#              not exist at all - `v1/mooncake/` there holds only `__init__`, `mooncake_connector`
#              and `mooncake_utils`. Hanging this on that half would gate it behind a floor its
#              subject can never meet, and the red would read as "the ascend image is broken". The
#              two APIs this case needs both exist at v0.19.1, so its gate is a different gate.
#
#              Neither image runs inference: no GPU, no NPU, no weights, no model. Each execs one
#              python3 -c that imports a registry and looks up a string.
# Inputs:      one Pod per engine, annotated for that engine, running that engine's own image. The
#              webhook injects; the case reads the rendered name back off the Pod spec and asks that
#              engine's factory to resolve it.
# Expected:    four outcomes are told apart, never collapsed into one try:
#
#                1. the factory module imports - if it does not, this run says NOTHING about the
#                   name we render, in either direction, and that is reported as its own failure;
#                2. plugin registration completes - an engine's own connectors arrive through vLLM's
#                   general-plugin entry points, not through importing the factory;
#                3. THE VERDICT: the rendered name is in that engine's registry;
#                4. the class behind the name loads. This is EVIDENCE, not a verdict, and the split
#                   is the point: the loader imports the implementation module, so a failure there
#                   is about the image's dependencies while a failure at 3 is about the name WE
#                   render. One outcome covering both would be red for two opposite reasons, and
#                   would look most like it was working exactly when it was not.
#
#              PRECONDITION, AND THE FAILURE CONDITION OF THIS CASE: we do not render
#              `kv_connector_module_path` (vllm.go renders `kv_connector` alone). The engine's real
#              `create_connector` path falls back to that module path when the registry misses, and
#              raises `Unsupported connector type` only when the key is unset - so while we never
#              render it, registry membership and "the engine starts" are the same question. On the
#              day we do render it, this assertion turns into a false negative - reporting missing
#              what the engine would resolve - and it must move to the version-specific entry point
#              then (`_get_connector_class_with_compat` at v0.19.1, `get_connector_class` at v0.25.1;
#              the names differ, which is why the registry is read directly while that is equivalent).
#
#              SKIPS: each row independently, on its own image variable. One image present must not
#              let the other row report green by omission.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. It changes no shared baseline - every
#              object it touches is one it created.
#
# NOT YET EXERCISED AS A CASE. No run of this file exists, on any cluster. It was written from source
# read at vllm v0.25.1 (752a3a50) and vllm-ascend v0.19.1rc1 (da421afa).
#
# What HAS been executed, 2026-09-04, outside this file - one `docker run --rm` on a host with no
# accelerator of any kind, against quay.io/ascend/vllm-ascend:v0.19.1rc1 (linux/arm64):
#   - `import vllm` and `load_general_plugins()` both succeed with no accelerator present;
#   - the registry then holds 19 names; `AscendStoreConnector` is among them and
#     `MooncakeStoreConnector` is NOT, which is what this case's vllm-ascend row asserts;
#   - the class behind the name also loaded, so step 4 above is not hypothetical either.
# That retires the earlier note that plugin loading without an accelerator was untested. What is
# still unexercised here is this FILE: the webhook injecting, the name being read back off the
# mutated Pod, and the probe running through kubectl exec. The vLLM row remains unmeasured on both
# counts.
set -uo pipefail

NS="${1:?usage: case-60.sh <NS>}"
CASE_ID=60
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

E2E_ENGINE_READY_TIMEOUT="${E2E_ENGINE_READY_TIMEOUT:-1200}"

# Creating the Pod and running the probe are two steps rather than one, because the name this case
# asserts has to be read off the SAME Pod the probe runs in. Doing it in one step forced a create,
# a read, a delete and a second create - and the second create is a second trip through the mutating
# webhook, so the name that was validated belonged to a Pod that no longer existed.
#
# start_pod returns 0 when the Pod is Ready, 1 when it never appeared, 2 when it appeared but never
# became Ready, and 4 when the image reference was refused before anything was submitted.
# The Ready case is separate because an engine image is several GB: a pull that has not
# finished leaves a Pod that is stored but not running, and that is a different thing to report.
start_pod() {
  local pod="$1" image="$2" engine="$3"
  # The image is interpolated into the rendered YAML. `|` and `&` open a block scalar and an anchor
  # when they lead a value, so an unusual reference would produce a manifest nobody wrote; refusing it
  # keeps a quoting bug from being read as an engine verdict.
  case "$image" in
    *[!A-Za-z0-9./:_@-]* | "") return 4 ;;
  esac
  KVI_IMAGE="$image" kvi_pod_manifest "$pod" "$engine" \
    | kubectl apply -f - >/dev/null 2>&1
  kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null || return 1
  kubectl -n "$TEST_NS" wait --for=condition=Ready "pod/${pod}" \
    --timeout="${E2E_ENGINE_READY_TIMEOUT}s" >/dev/null 2>&1 || return 2
  return 0
}

# exec_probe runs one python3 -c in a Ready Pod and returns 3 when the exec itself did not complete.
# Without that, an exec that never ran - container OOM-killed, runtime gone, no python3 in the image -
# leaves an empty log and reaches the verdicts as "the probe printed no verdict line", which points
# the reader at the probe's output when the probe never produced any.
exec_probe() {
  local pod="$1" script="$2" log="$3" rc=0
  kubectl -n "$TEST_NS" exec "$pod" -c engine -- python3 -c "$script" >"$log" 2>&1 || rc=$?
  [ "$rc" -eq 0 ] || return 3
  return 0
}

# rendered_connector reads the connector name the webhook actually wrote, out of the mutated Pod.
# It reads command and args together because the flag is appended to args while a workload's own
# launcher may sit in command, and the pair is one argv.
rendered_connector() {
  local pod="$1"
  kubectl -n "$TEST_NS" get pod "$pod" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    ctr = json.load(sys.stdin)["spec"]["containers"][0]
except Exception:
    sys.exit(0)
argv = ctr.get("command", []) + ctr.get("args", [])
for i, a in enumerate(argv):
    if a == "--kv-transfer-config" and i + 1 < len(argv):
        try:
            print(json.loads(argv[i + 1]).get("kv_connector", ""))
        except Exception:
            pass
        break
' 2>/dev/null
}

# The probe. NAME is interpolated rather than passed as an argument because exec_probe runs
# `python3 -c`, and the shell-side guard below refuses anything that is not a bare identifier, so
# the interpolation cannot carry quoting.
probe_script() {
  cat <<PY
import sys

NAME = "$1"

# 1. The factory module. Reported on its own: an import failure says nothing about the name we
# render, and calls for a different action than a name that does not resolve.
try:
    from vllm.distributed.kv_transfer.kv_connector.factory import KVConnectorFactory
except BaseException as e:
    print("FACTORY_IMPORT_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("FACTORY_IMPORT_OK")

# 2. Registration. An engine's own connectors are registered by entry points under
# vllm.general_plugins, not by importing the factory. This goes through vLLM's own loader rather
# than calling the plugin's register function directly: the direct call would assert "the name is
# in the registry because I just put it there", and would stay green if the entry point declaration
# were broken - which is a real failure that only shows up in a packaged image.
try:
    from vllm.plugins import load_general_plugins
    load_general_plugins()
except BaseException as e:
    print("PLUGINS_RAISED %s: %s" % (type(e).__name__, e)); sys.exit(0)
print("PLUGINS_OK")

# 3. THE VERDICT: is the rendered name in this engine's registry. That membership is the exact
# property deciding whether a workload starts - the factory consults kv_connector_module_path only
# when the name is ABSENT, and raises `ValueError: Unsupported connector type` when that key is
# unset, which is every Pod we render. The whole registry is printed, not just the answer: a failure
# that also lists the names the engine DOES know says what to render instead, and listing the full
# set does not depend on having guessed which other name to probe.
names = sorted(KVConnectorFactory._registry)
print("REGISTRY %s" % ",".join(names))
if NAME not in KVConnectorFactory._registry:
    print("NOT_IN_REGISTRY %s" % NAME); sys.exit(0)
print("IN_REGISTRY %s" % NAME)

# 4. EVIDENCE, not a verdict. Resolving the class runs the registry's lazy loader, and that loader
# imports the implementation module - torch, zmq and the engine's own worker code. Whether that
# import succeeds is a property of THIS IMAGE's dependencies, not of the name we render, so the two
# have opposite fixes and must not share an outcome. Reported either way, because a load failure is
# still the one place a missing dependency becomes visible from here.
try:
    cls = KVConnectorFactory.get_connector_class_by_name(NAME)
except BaseException as e:
    print("LOAD_FAILED %s %s: %s" % (NAME, type(e).__name__, e)); sys.exit(0)
print("LOADED %s -> %s" % (NAME, getattr(cls, "__name__", "<unnamed>")))
PY
}

# The engine table. Two rows, one assertion. Adding an engine here is adding a row, which is the
# shape this case is built for - the previous gap was a dimension with no rows at all.
resolved_rows=0
for row in "vllm:E2E_VLLM_IMAGE" "vllm-ascend:E2E_VLLM_ASCEND_IMAGE"; do
  engine="${row%%:*}"
  var="${row##*:}"
  image="$(eval "printf '%s' \"\${${var}:-}\"")"
  check="the ${engine} factory has the connector name we render in its registry"
  pod="conn-${engine//-/}"

  if [ -z "$image" ]; then
    record SKIP "$check" \
      "${var} is unset. The name we render for ${engine} is UNVERIFIED against a real factory: the \
Go tests prove which name we write, and only this row proves that engine knows it"
    continue
  fi

  LOG="/tmp/kvc-inject-60-${engine}-${SFX}.log"
  # One create for the whole row. The name asserted below and the registry the probe queries have to
  # come from the SAME Pod: a second create would be a second pass through the mutating webhook, and
  # the validated name would describe a Pod that no longer exists.
  start_pod "$pod" "$image" "$engine"; rc=$?
  if [ "$rc" -eq 2 ]; then
    record FAIL "$check" \
      "the Pod never became Ready in ${E2E_ENGINE_READY_TIMEOUT}s. An engine image is several GB, so \
this is most likely still pulling; it says NOTHING about the name we render. Raise \
E2E_ENGINE_READY_TIMEOUT or pre-pull the image and run again"
    continue
  elif [ "$rc" -eq 4 ]; then
    record FAIL "$check" \
      "the image reference '${image}' carries a character this case will not interpolate into a sed \
replacement. Nothing was submitted, so this says NOTHING about the name we render; fix the variable"
    continue
  elif [ "$rc" -ne 0 ]; then
    record FAIL "$check" "the Pod never appeared, so the webhook's output was never observed"
    continue
  fi

  # Read what was rendered BEFORE running anything: if this is empty the injection itself did not
  # happen, which is a different failure from a name the engine rejects, and reporting it as the
  # latter would send the reader to the engine.
  name="$(rendered_connector "$pod")"
  if [ -z "$name" ]; then
    record FAIL "$check" \
      "no --kv-transfer-config carrying a kv_connector was found on the mutated Pod, so the webhook \
injected nothing for ${engine}; the factory was never asked"
    continue
  fi
  case "$name" in
    *[!A-Za-z0-9_]* | "")
      record FAIL "$check" \
        "the rendered connector name '${name}' is not a bare identifier; refusing to interpolate it \
into the probe rather than shipping a quoting bug"
      continue
      ;;
  esac

  exec_probe "$pod" "$(probe_script "$name")" "$LOG"; rc=$?

  if [ "$rc" -eq 3 ]; then
    record FAIL "$check" \
      "the probe exec did not complete, so the log below is empty for a reason that has nothing to do \
with ${name}: the container may have been OOM-killed, the runtime may be gone, or the image may not \
carry python3. Observed: $(tr '\n' ' ' < "$LOG" | cut -c1-160); see ${LOG}"
  elif grep -q '^FACTORY_IMPORT_RAISED ' "$LOG"; then
    record FAIL "$check" \
      "$(grep -m1 '^FACTORY_IMPORT_RAISED ' "$LOG") - the factory would not import, so this run says \
NOTHING about the name we render, in either direction; see ${LOG}"
  elif grep -q '^PLUGINS_RAISED ' "$LOG"; then
    record FAIL "$check" \
      "$(grep -m1 '^PLUGINS_RAISED ' "$LOG") - plugin registration failed, so the registry this \
lookup would consult was never populated; that is about the image, not about ${name}; see ${LOG}"
  elif grep -q "^NOT_IN_REGISTRY ${name}$" "$LOG"; then
    record FAIL "$check" \
      "${engine}'s factory does not know '${name}', so a real workload aborts at startup with \
'Unsupported connector type' before the projected file is ever opened. The names it DOES know: \
$(grep -m1 '^REGISTRY ' "$LOG" | cut -d' ' -f2- | cut -c1-300); see ${LOG}"
  elif grep -q "^IN_REGISTRY ${name}$" "$LOG" && grep -q "^LOADED ${name} -> " "$LOG"; then
    resolved_rows=$((resolved_rows + 1))
    record PASS "$check" \
      "$(grep -m1 '^LOADED ' "$LOG") - the name came off the mutated Pod, not from this script, so \
what was looked up is what the webhook wrote, and the class behind it loaded in this image too"
  elif grep -q "^IN_REGISTRY ${name}$" "$LOG"; then
    # The verdict is membership, so this is a pass; the load failure rides in the same row rather
    # than becoming a row of its own, because kvi_results counts every non-SKIP row as a pass and an
    # extra INFO row would inflate that count. It is spelled out rather than dropped: a downgraded
    # output that does not carry the reason it was downgraded is just a quieter way of losing it.
    resolved_rows=$((resolved_rows + 1))
    record PASS "$check" \
      "'${name}' IS in ${engine}'s registry, which is the property deciding whether a workload \
starts, so the name we render is right. The class behind it did NOT load in this image - \
$(grep -m1 '^LOAD_FAILED ' "$LOG" | cut -c1-140) - and that is about the image's dependencies, not \
about the name, which is why it is not a failure here. It is not nothing either: a genuinely broken \
engine build would look the same from this side, so read ${LOG} before trusting the image"
  else
    record FAIL "$check" \
      "the probe printed no verdict line. Observed: $(tr '\n' ' ' < "$LOG" | cut -c1-200); see ${LOG}"
  fi
done

# A summary line, and it deliberately does not claim more than the rows produced. Two rows skipped is
# not a pass: it is the state this case was written to end.
if [ "$resolved_rows" -eq 2 ]; then
  record PASS "both rendered connector names are in their own engine's registry" \
    "vllm and vllm-ascend each know the name written for it, which is what tells a per-engine \
decision from a constant that happens to suit one of them"
else
  record SKIP "both rendered connector names are in their own engine's registry" \
    "${resolved_rows} of 2 rows confirmed; the cross-engine claim needs both, and a row that skipped \
or failed above is reported there rather than being folded into this line"
fi

kvi_results "$CASE_ID"
