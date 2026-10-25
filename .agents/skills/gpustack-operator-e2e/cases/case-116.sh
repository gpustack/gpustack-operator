#!/usr/bin/env bash
#
# CASE 116 — Every encoder-role admission refusal fires, each naming what its rule refuses, and
#            the admitted matrix is read on the platforms it admits and on the one it refuses
#           (MUTATING, self-recovering: one settings key and one injected node label, both restored
#            by the trap; every verdict below is a server-side dry run that persists nothing)
#           AUTO-SKIPS its matrix rows without an InstanceType that reports an admitted
#            accelerator platform, which this case builds by injecting a CPU vendor label
#
#   case-116.sh <NS>
#
# Goal:        A deployment that declares an Encode role is refused by a set of rules that a
#              deployment without one never meets, and each refusal names its own rule. This case
#              pins every rule on the encoder path and, like case 45 before it, pins WHICH rule
#              answered: a row that only checked "it was rejected" passes identically whether the
#              rule exists or not, so every row asserts a fragment of the operator's own wording
#              read from the code, and row 0 is the same shape ACCEPTED.
#
#              THE MATRIX IS THE OPERATOR'S ADMITTED SET, and this case reads it three ways, in
#              this order, and the order is the point:
#
#              1. ON THE CLUSTER AS IT STANDS, before anything here touches it. The platforms the
#                 cluster's own InstanceTypes report are read, and an unadmitted one is refused
#                 naming the platform beside the router and the engine it was checked against, and
#                 the admitted rows a user can compare against. This phase runs FIRST because the
#                 injected CPU vendor label of the next phase adds a manufacturer the cluster does
#                 not have, and this phase must read what the cluster really holds.
#              2. ON THE PLATFORM THE CLUSTER DOES NOT HAVE. A CPU vendor label is injected, so
#                 the CPU-only pool reports it, and the whole refusal matrix becomes reachable
#                 without a single accelerator: the wrong router, the wrong engine, the
#                 shared-claim rules and the mixed-platform rule. The rows read what that platform
#                 admits (SGLang over llm-d-router and over sglang-gateway) and what it refuses.
#              3. ON THE NETWORK LEG. The same injected label carries the NVIDIA vendor instead,
#                 so the rows only the NVIDIA and AMD platform admits are reachable too.
#
#              THE SHAPE RULES RUN BEFORE ANYTHING READS THE CLUSTER, so a role set that misses a
#              sibling kind, names a router-less shape, carries a deployment-wide cache, or
#              replaces a role's command is refused from the submitted object alone. THE MATRIX
#              RULES RUN ON THE PLATFORM THE ROLES' INSTANCE TYPES REPORT, which no CPU-only
#              cluster hands over by itself: the platform is read from each role's instance type's
#              observed manufacturer, and a CPU pool reports one only while CPU-manufacturer
#              awareness is on. So the case turns that setting on and injects a fake CPU vendor
#              label ("Ascend", lowercased to the platform the shared-directory row admits) on one
#              node, and the pool that forms behind it makes the whole matrix reachable without a
#              single accelerator: the wrong router, the wrong engine, the shared-claim rules and the
#              mixed-platform rule all become assertable.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator and
#              no engine image: nothing here schedules or runs a replica. The gpustack-settings
#              Secret must exist in the system namespace (E2E_SYSTEM_NS, default gpustack-system),
#              and jq must be on PATH.
#
#              AUTO-SKIP: the matrix rows need a derived InstanceType reporting the injected
#              platform. The label rides a NodeFeature object, and NFD owns the vendor label this
#              overrides — if this cluster's NFD declines to let an injected object override a
#              label its cpuid source reports, no such type appears, and the matrix rows SKIP
#              naming that rather than failing. The shape rows still run: they are answered from
#              the submitted object and fire before the platform is ever read.
#
# Existing platform: if an InstanceType already reports the manufacturer named by E2E_C116_VENDOR
#              (an accelerator cluster, for example E2E_C116_VENDOR=Ascend), the case uses it and
#              injects nothing, so the label, the setting and the settle wait are skipped.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and
#              one settings key (CPU-manufacturer awareness) flipped on — both restored by the
#              trap. The label carries the second vendor in the last phase, so the same object
#              serves both. REAL: every manifest goes through `kubectl apply --dry-run=server`, so
#              the schema and the webhook answer exactly as they would for a real create.
#
# Expected:    - the platform the cluster holds is read as it stands, and an unadmitted platform it
#                holds is refused naming the platform, the router and the engine, beside the
#                admitted rows;
#              - the full encoder/prefill/decode shape on the injected platform, with the two
#                shared-claim mounts, is ACCEPTED;
#              - each rule below refuses with its own wording: a missing sibling kind (each of
#                prefill and decode), a Server role beside the encoder, no router, a deployment-
#                wide kvCache, a role command, an encoder of more than one instance, an empty
#                claim name, a second volume source on one entry, a read-only encoder mount,
#                disagreeing mount paths, an encoder or a prefiller without its claim, a router
#                that does not admit the injected platform for an encoder role, an engine a router
#                does not pair with, and a platform the matrix does not admit;
#              - two roles on two different platforms are refused as a mixed deployment;
#              - what the injected platform admits is ACCEPTED too: llm-d-router and sglang-gateway
#                over SGLang, mounting no claim, and the pairing the matrix still refuses is
#                refused naming BOTH halves and the admitted rows;
#              - the network leg is read on the NVIDIA platform: vllm-router with vLLM and with an
#                encoder role is ACCEPTED, vllm-router with SGLang is refused naming the pairing,
#                a user-supplied --epd-config is refused as operator-owned, and vllm-router with
#                no encoder role is admitted.
#
# Cleanup:     The trap deletes the NodeFeature object, restores the settings key to its exact
#              original value (removing it if it was absent) and waits for the chain to settle back:
#              the derived CPU pool returns only once the setting cache flips and the pool
#              re-derives. Idempotent, runs on pass AND fail, safe to re-run. No ModelDeployment is
#              ever persisted: every apply is a server-side dry run.
#
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-116.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
# The vendor string becomes the platform after lowercasing; "Ascend" selects the shared-directory
# row, which is the one whose claim rules this case refuses against.
VENDOR="${E2E_C116_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
# The last phase reads the matrix on the network leg, which the NVIDIA rows are admitted on.
VENDOR_NVIDIA="${E2E_C123_VENDOR:-NVIDIA}"
NF=case116-cpu-vendor
MD=case116-probe
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE_ROUNDS="${E2E_C116_SETTLE:-60}"   # 60 x 5s = 5 min for the settings cache and re-derivation
ROUTER_NAME=llm-d-router
ENGINE=vLLM
ROUTER_EXTRA_ARGS=""
KVCACHE_BLOCK=""

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_epd-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-116] jq is required." >&2; exit 2; }

# ---------------------------------------------------------------- the fixture, and its restore

# The label rides a NodeFeature object (case-4's mechanism): the object stays and keeps re-asserting
# the label, where a bare `kubectl label` is a cpuid-owned value NFD reverts on its next pass. The
# same object carries the second vendor in the last phase, so one delete restores both. The label,
# the awareness setting, the type it waits for and the undo of all three are the library's.
cleanup() {
  epd_teardown_cpu_vendor_fixture "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS"
}
trap cleanup EXIT

# The rows the earlier phases recorded are printed before an environment failure ends the case, so
# a skip recorded before the failure is not lost with it.
abort() { # message
  echo "[case-116] $1" >&2
  print_rows
  exit 2
}

# ---------------------------------------------------------------- the manifests and the rows

# ENGINE, ROUTER_NAME, ROUTER_EXTRA_ARGS and KVCACHE_BLOCK are the dials the rows turn; each is
# reset right after the row that turns it, because an assignment prefixed to a function call
# persists in bash after the call returns and would silently move every later row off the baseline.
manifest() { # roles-block
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: ${ENGINE:-vLLM}
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${ROUTER_NAME:-llm-d-router}
${ROUTER_EXTRA_ARGS:-}
${KVCACHE_BLOCK:-}
  roles:
${1}
YAML
}

encode_role() { # [claimName mountPath readOnly]
  printf '  - name: encode\n    kind: Encode\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  [ -n "${3:-}" ] && printf '        readOnly: %s\n' "$3"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

encode_role_sized() { # size claimName
  printf '  - name: encode\n    kind: Encode\n    instanceType: %s\n    image: %s\n    size: %s\n' \
    "$IT" "$IMAGE" "$1"
  printf '    additionalVolumes:\n      - mountPath: /ec-cache\n'
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$2"
}

prefill_role() { # [claimName mountPath]
  printf '  - name: prefill\n    kind: Prefill\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] || return 0
  printf '    additionalVolumes:\n      - mountPath: %s\n' "${2:-/ec-cache}"
  printf '        persistentVolumeClaim:\n          claimName: %s\n' "$1"
  return 0
}

decode_role() { # [command]
  printf '  - name: decode\n    kind: Decode\n    instanceType: %s\n    image: %s\n' "$IT" "$IMAGE"
  [ -n "${1:-}" ] && printf '    command: ["/pause"]\n'
  return 0
}

# The accepted baseline of the shared-directory leg: encode and prefill mount one claim at one
# path, decode mounts nothing. Every refusal of the encoder path varies exactly one thing from it.
shared_claim_roles() {
  encode_role case116-shared /ec-cache
  prefill_role case116-shared /ec-cache
  decode_role
}

# The admitted row's shape, per platform: on ascend, encode and prefill mount one claim at one
# path and decode mounts nothing; on nvidia and amd the leg is carried over the network and no
# role mounts anything, because the network connector forbids the shared mount. It names no claim
# that has to exist — the matrix is read from the object and the claim rules only compare the two
# mounts to each other, so no PVC is created anywhere in this case.
platform_baseline_roles() { # [instanceType] [manufacturer]
  local it="${1:-${IT:-unset}}" mfg="${2:-${PLATFORM:-ascend}}" claim=""
  if [ "$mfg" = ascend ]; then
    claim="    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared"
  fi
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${it}
    image: ${IMAGE}
${claim}
  - name: prefill
    kind: Prefill
    instanceType: ${it}
    image: ${IMAGE}
${claim}
  - name: decode
    kind: Decode
    instanceType: ${it}
    image: ${IMAGE}
ROLES
}

# The network leg's roles: the baseline minus the claim. SGLang carries the encoder's output by
# address rather than through a mounted directory, so no role declares additionalVolumes at all —
# which is a field the address leg makes unneeded, not an omission this case forgot. Everything else
# about the shape is the baseline, so an acceptance below is about the leg and not about the roles.
network_roles() { # instanceType
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${1:-${IT:-unset}}
    image: ${IMAGE}
  - name: prefill
    kind: Prefill
    instanceType: ${1:-${IT:-unset}}
    image: ${IMAGE}
  - name: decode
    kind: Decode
    instanceType: ${1:-${IT:-unset}}
    image: ${IMAGE}
ROLES
}

# The same deployment with the encoder REMOVED: an ordinary prefill/decode pair.
pair_roles() { # instanceType
  cat <<ROLES
  - name: prefill
    kind: Prefill
    instanceType: ${1:-${IT:-unset}}
    image: ${IMAGE}
  - name: decode
    kind: Decode
    instanceType: ${1:-${IT:-unset}}
    image: ${IMAGE}
ROLES
}

# Assert that a roles block is refused AND that the refusal quotes every fragment after the roles
# block. An empty apply output is a FAIL on its own: deleting anything from "" leaves "". A
# refusal carrying other messages too passes — several rules aggregate into one response — but a
# MISSING fragment means the manifest tripped a rule this row is not about.
# MISSING fragment means the manifest tripped a rule this row is not about. The harness itself
# lives in _md-lib.sh: it reads the manifest, prints the text of the row and returns the verdict.
# The row stays here, so the table and the verdict never leave the case that owns them.
refuses() {
  local check="$1" want="$2" roles="$3" obj status=0
  shift 3
  obj="$(md_dryrun_refuses "$want" "$@" < <(manifest "$roles"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# The positive side of the same instrument, and it carries the headline: three roles being accepted
# at all is the shape this whole rule set exists to guard, so the baseline row proves the refusals
# below are each their own rule rather than one blanket rejection.
accepts() {
  local check="$1" roles="$2" obj status=0
  obj="$(md_dryrun_accepts < <(manifest "$roles"))" || status=1
  if [ "$status" -eq 0 ]; then
    record PASS "$check" "$obj"
  else
    record FAIL "$check" "$obj"
  fi
  return 0
}

# ---------------------------------------------------------------- phase 1: the cluster as it stands

# This phase runs BEFORE anything here mutates the cluster. The unadmitted-platform rows read the
# manufacturers the cluster's own InstanceTypes report, and the injected CPU vendor label of the
# next phase adds a manufacturer the cluster does not have, so the reading has to happen first.
#
# TWO TYPES ARE SOUGHT, and neither is invented. The platform row needs a type reporting a
# manufacturer the matrix does NOT cover, and the baseline needs one that reports one it DOES.
# On a cluster holding any unadmitted accelerator the first is found and the rows run; on a
# cluster whose only accelerated types are the three admitted platforms the first is empty, the
# rows skip naming what was seen, and the baseline still runs against the second.
#
# THE UNADMITTED MANUFACTURERS ARE THE OPERATOR'S OWN LIST MINUS THE THREE IT ADMITS, so every
# accelerator this project supports is covered without the case having to know which of them is
# common. Three of the nine declared are admitted here and are therefore excluded; the general CPU
# fallback is not a manufacturer of an accelerator and is not in the list either.
UNADMITTED_PLATFORMS="cambricon hygon iluvatar metax mthreads thead"

OTHER_IT=""
OTHER_MANUFACTURER=""
for m in $UNADMITTED_PLATFORMS; do
  OTHER_IT="$(epd_platform_type "$m")"
  if [ -n "$OTHER_IT" ]; then
    OTHER_MANUFACTURER="$m"
    break
  fi
done

# The baseline is ANY admitted platform, tried in the order the matrix admitted them, and the
# manufacturer is kept beside the type because the shape differs by row: ascend carries the
# shared claim, nvidia and amd carry the network leg and forbid the mount.
ADMITTED_IT=""
ADMITTED_MFG=""
for m in ascend nvidia amd; do
  ADMITTED_IT="$(epd_platform_type "$m")"
  if [ -n "$ADMITTED_IT" ]; then
    ADMITTED_MFG="$m"
    break
  fi
done

SEEN="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.detail.manufacturer}{" "}{end}' 2>/dev/null)"

if [ -z "$OTHER_IT" ] && [ -z "$ADMITTED_IT" ]; then
  record SKIP "the platform could be read" \
    "no usable InstanceType reported any manufacturer (the cluster holds: ${SEEN:-<none>}), so there is no platform to submit and no baseline to compare against"
else
  # Three roles being accepted at all is the shape this whole rule set exists to guard, so the
  # baseline proves the refusals below are each their own rule rather than one blanket rejection.
  # The shape is whichever admitted platform this cluster has: the ascend row carries the two
  # shared-claim mounts, and the nvidia or amd rows carry none, because the network connector
  # forbids the shared mount on its row.
  if [ -n "$ADMITTED_IT" ]; then
    accepts "an admitted combination is ACCEPTED by the same instrument" \
      "$(platform_baseline_roles "$ADMITTED_IT" "$ADMITTED_MFG")"
  else
    record SKIP "an admitted combination is ACCEPTED by the same instrument" \
      "no usable InstanceType reports an admitted platform (the cluster holds: ${SEEN:-<none>}), so there is no accepted shape to compare the refusals against"
  fi

  if [ -n "$OTHER_IT" ]; then
    # THE HEADLINE OF THIS PHASE. The matrix gap is read router-first, then engine, then platform,
    # and it returns ONE message: llm-d-router and vLLM are both admitted here, so the gap that
    # fires is the platform one, which names the platform AND quotes the router and engine it
    # ACCEPTED beside it. The word "and" before the engine is the fragment that proves this is the
    # platform arm and not the engine arm above it — the two messages read alike and only that word
    # tells them apart. The admitted rows come from the same sentence, appended by the rule that
    # fires it.
    refuses "a platform the matrix does not admit is refused, all three named" \
      "is not admitted for encoder roles under router" \
      "$(platform_baseline_roles "$OTHER_IT" "$OTHER_MANUFACTURER")" \
      'and engine' \
      "the admitted rows are"

    # THE SAME PLATFORM WITH A DIFFERENT ENGINE, which reaches a different arm and stops earlier:
    # the engine is checked before the platform, so this refusal is the engine message and never
    # reaches the platform row above. The pair is what tells a user which of the two to change, and
    # either alone leaves the other looking fine.
    ENGINE=SGLang refuses "an unadmitted platform is refused naming the engine too" \
      'is not admitted with engine "SGLang"' \
      "$(platform_baseline_roles "$OTHER_IT" "$OTHER_MANUFACTURER")"
    ENGINE=""

    # TWO PLATFORMS IN ONE DEPLOYMENT answer a DIFFERENT rule, and the difference is the point: the
    # matrix lookup is per platform, so the mixed-platform rule runs first and names both
    # manufacturers instead of the matrix. A case that only submitted one unadmitted platform would
    # never tell the two refusals apart.
    #
    # THE MINIMUM THAT REACHES IT IS ONE ROLE OF EACH KIND ON TWO TYPES. Six roles would reach it
    # too, but a second role of a kind trips the at-most-one-role-of-each-kind rule alongside it, and
    # this case states elsewhere that a manifest which trips a rule its row is not about is a row
    # that has stopped being about its rule.
    if [ -n "$ADMITTED_IT" ]; then
      refuses "two platforms in one encoder deployment are refused by the mixing rule" \
        "more than one accelerator platform" \
        "$(platform_baseline_roles "$ADMITTED_IT" "$ADMITTED_MFG")
  - name: decode
    kind: Decode
    instanceType: ${OTHER_IT}
    image: ${IMAGE}"
    else
      record SKIP "two platforms in one encoder deployment are refused by the mixing rule" \
        "an unadmitted platform exists (${OTHER_MANUFACTURER}) but no usable InstanceType reports an admitted one, so there is nothing to mix it with"
    fi
  else
    record SKIP "a platform the matrix does not admit is refused, all three named" \
      "no usable InstanceType reports any of the unadmitted manufacturers this operator declares (${UNADMITTED_PLATFORMS// /, }; the cluster holds: ${SEEN:-<none>}), so there is no unadmitted platform to name"
    record SKIP "an unadmitted platform is refused naming the engine too" \
      "no usable InstanceType reports any of the unadmitted manufacturers this operator declares (${UNADMITTED_PLATFORMS// /, }; the cluster holds: ${SEEN:-<none>}), so there is no unadmitted platform to name"
    record SKIP "two platforms in one encoder deployment are refused by the mixing rule" \
      "no usable InstanceType reports any of the unadmitted manufacturers this operator declares (${UNADMITTED_PLATFORMS// /, }; the cluster holds: ${SEEN:-<none>}), so there is no second manufacturer to mix with"
  fi

  # ------------------------------------------------- why the functional half lives elsewhere
  #
  # Stated as a row rather than left to the header, because a case that reads as a refusal-only
  # case with no functional rows is otherwise indistinguishable from one that forgot to write them.
  # The admitted platforms' functional readings are their own cases, and no unadmitted platform has
  # an object to run until a live proof of concept admits its row.
  record SKIP "an unadmitted platform serves a multimodal request through the router" \
    "the matrix admits ascend, nvidia and amd only, so any other platform has no object to run. The admitted platforms' functional readings are case 120 (the NVIDIA and AMD network leg) and case 121 (the Ascend shared-directory leg); this case asserts the refusal for every platform that has no row, and nothing else"
fi

# ---------------------------------------------------------------- phase 2: the shared-directory leg

# The pool needs the settings cache to expire, the node flavor to re-derive and the type to be
# authored, so this poll IS the wait. A timeout is a skip, not a failure: NFD may decline to let an
# injected object override a label its cpuid source reports, and that is an environment answer.
NODE=""
PLATFORM_TYPE="$(epd_platform_type "$PLATFORM")"
if [ -z "$PLATFORM_TYPE" ]; then
  NODE="$(epd_pick_worker_node)"
  if [ -z "$NODE" ]; then
    abort "no Ready schedulable worker node; this case cannot place its vendor label"
  fi

  echo "[case-116] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"
  epd_setup_cpu_vendor_fixture "$VENDOR" "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE"
  PLATFORM_TYPE="$EPD_FIXTURE_TYPE"
fi

# Any InstanceType a deployment can name, for the shape rows: they fire before the platform is
# read, so the type's own platform does not matter to them. The exclusions and the order live in
# _md-lib.sh, and the first name it prints is the one the case used to pick.
FALLBACK_TYPE="$(md_usable_instance_types | sed -n '1p')"
if [ -z "$PLATFORM_TYPE" ] && [ -z "$FALLBACK_TYPE" ]; then
  abort "no InstanceType in the cluster; run case-1 first"
fi

# The shape rows embed whichever type exists: their rules fire before the platform is read. The
# matrix rows below name the platform type directly and are skipped without it.
IT="${PLATFORM_TYPE:-$FALLBACK_TYPE}"

if [ -n "$PLATFORM_TYPE" ]; then
  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${PLATFORM_TYPE} reports manufacturer '${PLATFORM}' from the node labels"
else
  record SKIP "a CPU-only pool reports an admitted platform" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so every matrix row below skips: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived. The shape rows still run: they are refused before the platform is read"
fi

# The same platform, read the way the rows below that hang on it read it: the pool reports the
# target platform, whether the cluster already had that platform or the injected label built it.
if [ "$EPD_FIXTURE_INJECTED" = false ] && [ -n "$PLATFORM_TYPE" ]; then
  record PASS "an active pool reports the target platform" \
    "instancetype ${PLATFORM_TYPE} reports manufacturer '${PLATFORM}'"
else
  if [ -z "$PLATFORM_TYPE" ]; then
    record SKIP "the platform rows could be read" \
      "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  else
    record PASS "an active pool reports the target platform" \
      "instancetype ${PLATFORM_TYPE} reports manufacturer '${PLATFORM}' from the injected vendor label"
  fi
fi

# ---------------------------------------------------------------- row 0: the accepted baseline

if [ -n "$PLATFORM_TYPE" ]; then
  accepts "the full encoder shape on the admitted platform is ACCEPTED" "$(shared_claim_roles)"
else
  record SKIP "the full encoder shape on the admitted platform is ACCEPTED" \
    "no platform InstanceType exists, so there is no shape to accept and every refusal below would pass for the wrong reason"
fi

# ---------------------------------------------------------------- the shape rules: object-only

refuses "an encoder with no decoder is refused naming the missing role" \
  'needs a "Decode" role beside it' \
  "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)"

refuses "an encoder with no prefill is refused naming the missing role" \
  'needs a "Prefill" role beside it' \
  "$(encode_role case116-shared /ec-cache)
$(decode_role)"

refuses "a Server role beside the encoder is refused" \
  "cannot be combined with another kind" \
  "  - name: server
    kind: Server
    instanceType: ${IT}
    image: ${IMAGE}
$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# The no-router row needs a manifest with the router block absent, which the manifest builder
# cannot spell through ROUTER_NAME: an empty name is a different refusal. Build it inline.
out="$(cat <<YAML | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' '
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  roles:
$(shared_claim_roles)
YAML
)"
case "$out" in
  *"an Encode role requires a router"*)
    record PASS "an encoder with no router is refused" "refused naming: an Encode role requires a router" ;;
  "")
    record FAIL "an encoder with no router is refused" "the apply produced no output at all, so nothing was refused" ;;
  *)
    record FAIL "an encoder with no router is refused" "refused with the wrong message, or accepted: ${out:0:220}" ;;
esac

KVCACHE_BLOCK="$(printf '  kvCache:\n    poolRef:\n      name: case116-no-such-binding\n')" \
  refuses "a deployment-wide kvCache is refused on the encoder shape" \
  "spec.kvCache is refused" \
  "$(shared_claim_roles)"
KVCACHE_BLOCK=""

refuses "a role command is refused on the encoder shape" \
  "replaces the managed arguments and the routing sidecar" \
  "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /ec-cache)
$(decode_role command)"

# An encoder runs one instance per Pod, so its replica size is fixed at 1. This is a shape rule like
# the rows above it: it is answered from the submitted object, before the platform is read, so it
# needs no matrix row and does not skip when the injected platform never derived.
refuses "an encoder of more than one instance is refused" \
  "replica size is 1" \
  "$(encode_role_sized 2 case116-shared)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# ---------------------------------------------------------------- the claim-source rules: object-only

# An empty claim name is refused wherever a claim entry appears, encoder role or not, and before
# anything reads the cluster. The entry itself must exist: a role with NO claim entry is a
# different row's refusal, so the claimName is written empty rather than the block dropped.
refuses "an empty claim name is refused" \
  "claim name must not be empty" \
  "  - name: encode
    kind: Encode
    instanceType: ${IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: \"\"
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

# One entry mounts one volume: a configMap beside the claim leaves the renderer a choice the API
# never makes.
out="$(manifest "  - name: encode
    kind: Encode
    instanceType: ${IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
        configMap:
          name: case116-anything
$(prefill_role case116-shared /ec-cache)
$(decode_role)" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
case "$out" in
  *"set beside persistentVolumeClaim on the same entry"*)
    record PASS "a second source on one claim entry is refused" "refused naming: set beside persistentVolumeClaim on the same entry" ;;
  "")
    record FAIL "a second source on one claim entry is refused" "the apply produced no output at all, so nothing was refused" ;;
  *)
    record FAIL "a second source on one claim entry is refused" "refused with the wrong message, or accepted: ${out:0:220}" ;;
esac

# ---------------------------------------------------------------- the matrix rules: platform-bound

if [ -n "$PLATFORM_TYPE" ]; then
  # vllm-router admits NVIDIA and AMD for vLLM encoder roles, not Ascend (vLLM-Ascend's P/D KV leg
  # requires llm-d-router's decode sidecar handshake), so the refusal names the platform beside the
  # router and engine.
  ROUTER_NAME=vllm-router refuses "a router that does not admit this platform for an encoder role is refused" \
    'platform "ascend" is not admitted for encoder roles under router "vllm-router" and engine "vLLM"' \
    "$(shared_claim_roles)"
  ROUTER_NAME=""

  # sglang-gateway drives the encoder leg on SGLang alone, so pairing it with vLLM is the pair the
  # matrix still refuses. Both SGLang rows are admitted behind that router now, so the engine below is
  # vLLM and the refusal has to name the pairing rather than either half on its own.
  ROUTER_NAME=sglang-gateway ENGINE=vLLM refuses "an engine this router does not pair with the encoder is refused" \
    'router "sglang-gateway" is not admitted with engine "vLLM"' \
    "$(shared_claim_roles)"
  ROUTER_NAME=""
  ENGINE=""

  # The shared-claim rules run only on the row whose connector is a mounted directory, which is the
  # platform this case injects. Each row varies exactly one mount from the baseline.
  refuses "a read-only encoder mount is refused" \
    "must not be read-only" \
    "$(encode_role case116-shared /ec-cache true)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

  refuses "disagreeing mount paths are refused" \
    "must name the same mountPath" \
    "$(encode_role case116-shared /ec-cache)
$(prefill_role case116-shared /other-cache)
$(decode_role)"

  refuses "an encoder that mounts no claim is refused" \
    "attach the shared claim the prefiller mounts" \
    "$(encode_role)
$(prefill_role case116-shared /ec-cache)
$(decode_role)"

  refuses "a prefiller that mounts no claim is refused" \
    "mounts nothing" \
    "$(encode_role case116-shared /ec-cache)
$(prefill_role)
$(decode_role)"
else
  for row in \
    "a router that does not admit this platform for an encoder role is refused" \
    "an engine this router does not pair with the encoder is refused" \
    "a read-only encoder mount is refused" \
    "disagreeing mount paths are refused" \
    "an encoder that mounts no claim is refused" \
    "a prefiller that mounts no claim is refused"; do
    record SKIP "$row" "needs the platform InstanceType this run did not build; see the first row"
  done
fi

# ---------------------------------------------------------------- the platform rules

# The two platform rows need different second platforms, and since NVIDIA joined the matrix they
# are no longer the same list. The mixing row needs ANY second manufacturer beside the injected
# one, so an NVIDIA cluster (whose own types report nvidia) still runs it against its real types.
# The unadmitted row needs a manufacturer NO admitted row covers -- neither the injected one, nor
# amd, nor nvidia -- so that same cluster skips it cleanly instead of reading one of its own
# admitted platforms as a refusal the first phase already covers directly. With awareness on, the
# cluster's own CPU types report their real vendor, so any such type serves; an all-AMD,
# all-NVIDIA or all-unknown cluster may have none, and the row says so rather than inventing one.
if [ -n "$PLATFORM_TYPE" ]; then
  UNADMITTED_IT="$(kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ -n "$manufacturer" ] && [ "$manufacturer" != "$PLATFORM" ] \
          && [ "$manufacturer" != amd ] && [ "$manufacturer" != nvidia ]; then
          echo "$name"
          break
        fi
      done)"
  SECOND_IT="$(kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ -n "$manufacturer" ] && [ "$manufacturer" != "$PLATFORM" ]; then
          echo "$name"
          break
        fi
      done)"

  # The whole deployment on the unadmitted type: the matrix names the platform, the router and
  # the engine together.
  if [ -n "$UNADMITTED_IT" ]; then
    refuses "a platform the matrix does not admit is refused, all three named" \
      "is not admitted for encoder roles under router" \
      "  - name: encode
    kind: Encode
    instanceType: ${UNADMITTED_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
  - name: prefill
    kind: Prefill
    instanceType: ${UNADMITTED_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
  - name: decode
    kind: Decode
    instanceType: ${UNADMITTED_IT}
    image: ${IMAGE}"
  else
    record SKIP "a platform the matrix does not admit is refused, all three named" \
      "every usable InstanceType reports an admitted platform (ascend, amd, nvidia) or none, so there is no unadmitted platform to name; the first phase of this case covers every unadmitted platform"
  fi

  # Two platforms in one deployment, both claim mounts well formed so only the mixing answers.
  if [ -n "$SECOND_IT" ]; then
    refuses "two platforms in one encoder deployment are refused" \
      "more than one accelerator platform" \
      "$(encode_role case116-shared /ec-cache)
  - name: prefill
    kind: Prefill
    instanceType: ${SECOND_IT}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case116-shared
$(decode_role)"
  else
    record SKIP "two platforms in one encoder deployment are refused" \
      "no second manufacturer exists to mix with the injected one"
  fi
else
  record SKIP "a platform the matrix does not admit is refused, all three named" \
    "needs the platform InstanceType this run did not build; see the first row"
  record SKIP "two platforms in one encoder deployment are refused" \
    "needs the platform InstanceType this run did not build; see the first row"
fi

# ---------------------------------------------------------------- the rows the injected platform admits

if [ -n "$PLATFORM_TYPE" ]; then
  # Three roles being accepted at all is the shape this whole rule set exists to guard, so the
  # baseline proves the refusals below are each their own rule rather than one blanket rejection.
  accepts "the admitted combination is ACCEPTED by the same instrument" \
    "$(platform_baseline_roles "$IT" "$PLATFORM")"
else
  record SKIP "the admitted combination is ACCEPTED by the same instrument" \
    "no platform InstanceType exists, so there is no shape to accept and every refusal below would pass for the wrong reason"
fi

if [ -n "$PLATFORM_TYPE" ]; then
  # The admitted SGLang rows, read through the same instrument as the vLLM baseline above. Both
  # routers are accepted on SGLang with no claim mounted, because an address leg is what carries the
  # encoder's output and a connector there is not a thing a user attaches.
  ENGINE=SGLang accepts "llm-d-router with SGLang is ACCEPTED" \
    "$(network_roles "$IT")"
  ENGINE=SGLang ROUTER_NAME=sglang-gateway \
    accepts "sglang-gateway with SGLang is ACCEPTED" \
    "$(network_roles "$IT")"
  ROUTER_NAME=llm-d-router

  # The pairing the matrix still refuses. sglang-gateway drives the encoder leg on SGLang alone, so
  # vLLM under it names both halves in one message along with the admitted rows.
  ROUTER_NAME=sglang-gateway ENGINE=vLLM refuses "sglang-gateway with vLLM is refused naming the pairing" \
    'router "sglang-gateway" is not admitted with engine "vLLM"' \
    "$(platform_baseline_roles "$IT" "$PLATFORM")" \
    'the admitted rows are'
  ROUTER_NAME=llm-d-router
  ENGINE=vLLM
else
  for label in \
    "llm-d-router with SGLang is ACCEPTED" \
    "sglang-gateway with SGLang is ACCEPTED" \
    "sglang-gateway with vLLM is refused naming the pairing"; do
    record SKIP "$label" "needs the platform InstanceType this run did not build; see the baseline row"
  done
fi

# ---------------------------------------------------------------- phase 3: the network leg

# The NVIDIA rows need a platform the matrix admits on the network leg. A cluster that already
# carries one is read directly and mutates nothing; a CPU-only cluster gets one by carrying the
# NVIDIA vendor on the injected label instead of the Ascend one, so the same object serves both
# phases and one delete restores both.
PHASE3_IT=""
PHASE3_PLATFORM=""
for m in nvidia amd; do
  PHASE3_IT="$(epd_platform_type "$m")"
  if [ -n "$PHASE3_IT" ]; then
    PHASE3_PLATFORM="$m"
    break
  fi
done

if [ -n "$PHASE3_IT" ]; then
  record PASS "an admitted platform InstanceType is available" \
    "instancetype ${PHASE3_IT} reports manufacturer '${PHASE3_PLATFORM}'"
else
  if [ -z "$NODE" ]; then
    NODE="$(epd_pick_worker_node)"
  fi
  if [ -n "$NODE" ]; then
    echo "[case-116] carrying CPU vendor '${VENDOR_NVIDIA}' on node ${NODE} for the network-leg rows"
    epd_setup_cpu_vendor_fixture "$VENDOR_NVIDIA" "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE" nvidia
    PHASE3_IT="$EPD_FIXTURE_TYPE"
  fi
  if [ -n "$PHASE3_IT" ]; then
    PHASE3_PLATFORM=nvidia
    record PASS "a CPU-only pool reports the target platform" \
      "instancetype ${PHASE3_IT} reports manufacturer '${PHASE3_PLATFORM}' from the injected vendor label"
  else
    record SKIP "the platform rows could be read" \
      "no InstanceType reported manufacturer 'nvidia' or 'amd' and no CPU-vendor fixture could be derived; every row below names that type in its manifest, so none of them ran"
  fi
fi

if [ -n "$PHASE3_IT" ]; then
  # The platform the rows below name is the one this phase read.
  PLATFORM="$PHASE3_PLATFORM"

  # Row 0: the admitted vllm-router + vLLM EPD shape on nvidia/amd is accepted.
  ROUTER_NAME=vllm-router accepts \
    "vllm-router + vLLM with an encoder role is ACCEPTED on ${PLATFORM}" \
    "$(network_roles "$PHASE3_IT")"

  # Row 2: vllm-router paired with SGLang is refused naming the router and engine.
  ROUTER_NAME=vllm-router ENGINE=SGLang refuses \
    "vllm-router paired with SGLang is refused" \
    'router "vllm-router" is not admitted with engine "SGLang"' \
    "$(network_roles "$PHASE3_IT")" \
    'the admitted rows are'
  ENGINE=vLLM

  # Row 3: --epd-config is owned by the operator for vllm-router and refused in extraArgs.
  ROUTER_EXTRA_ARGS="$(printf '    extraArgs:\n      - --epd-config={"encoder_urls":[]}\n')" \
    ROUTER_NAME=vllm-router refuses \
    "user-supplied --epd-config on vllm-router is refused as operator-owned" \
    '"--epd-config" is set by the operator for router "vllm-router"' \
    "$(network_roles "$PHASE3_IT")"
  ROUTER_EXTRA_ARGS=""

  # Row 4: vllm-router with no Encode role (plain prefill/decode) remains accepted.
  ROUTER_NAME=vllm-router accepts \
    "vllm-router with NO encoder role is admitted" \
    "$(pair_roles "$PHASE3_IT")"
else
  for label in \
    "vllm-router + vLLM with an encoder role is ACCEPTED" \
    "vllm-router paired with SGLang is refused" \
    "user-supplied --epd-config on vllm-router is refused as operator-owned" \
    "vllm-router with NO encoder role is admitted"; do
    record SKIP "$label" "needs an nvidia or amd InstanceType this run did not have; see the first row"
  done
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-116] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-116] all checks passed"
