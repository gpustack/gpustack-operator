#!/usr/bin/env bash
#
# CASE 124 — The admitted matrix has no NVIDIA row, and the refusal names the platform beside a
#           list of the rows that do exist (NON-MUTATING: server-side dry runs only)
#           AUTO-SKIPS without a cluster that has an unadmitted platform to submit
#
#   case-124.sh <NS>
#
# Goal:        The admitted matrix carries Ascend and AMD and NOTHING ELSE. NVIDIA is absent, and
#              that is the state of the work rather than an oversight: an NVIDIA combination is
#              admitted only after its proof of concept passes, and none has. This case is the
#              reading of that absence, and it reads the one refusal this design will actually meet
#              in the field — because NVIDIA is the most common accelerator in a cluster that has
#              never run this feature, and an NVIDIA encoder deployment is exactly what a user will
#              write first.
#
#              The refusal names the PLATFORM column, with the router and the engine it checked and
#              rejected alongside it, then lists the admitted rows so the reader can see what to
#              change. That last part is the reason this case exists at all: a refusal that says only
#              "not admitted" leaves a user to find the table themselves, and the table is not
#              something they can see.
#
#              THE PLATFORM IS READ FROM THE INSTANCE TYPE, NOT DECLARED, so the case does not
#              invent a manufacturer: it looks for a type the cluster actually has that reports one
#              the matrix does not cover. Which manufacturers to look for is the OPERATOR'S OWN LIST,
#              minus the two it admits: the case iterates every accelerator manufacturer this project
#              declares and takes the first usable type it finds, rather than guessing a couple of
#              names. Guessing is how a cluster that holds a Hygon or a THead card ends up skipped
#              for want of a name nobody thought to probe. The general CPU fallback is left out:
#              "generic" is what an unrecognised cpu-model.vendor_id collapses to, not an
#              accelerator platform, and submitting it would assert nothing about the matrix.
#
#              WHY THERE IS NO FUNCTIONAL HALF HERE. A passing NVIDIA encoder run would require a
#              proof of concept that has not happened: vLLM with ec-nixl and ec-example on NVIDIA
#              hardware is spec task T3a, and it is BLOCKED ON A CLUSTER. A matrix table gains an
#              NVIDIA entry only for a combination with a passing run, so a functional case for
#              NVIDIA today would assert a behavior the operator does not implement. So the
#              functional half is absent by construction, the header says so, and one row states it
#              at the end so a reader scanning only the table sees it too.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first). Needs at least
#              one usable InstanceType (not inactive, not terminating) whose reported manufacturer is
#              one this operator declares and the matrix does not admit — every accelerator
#              manufacturer the project declares, minus ascend and amd, tried in that order
#              (cambricon, hygon, iluvatar, metax, mthreads, nvidia, thead). A cluster of Ascend and
#              AMD types alone has no such type and skips those rows. NO accelerator of any specific
#              family is required, and none of the roles will ever run: every assertion is a
#              server-side dry run.
#
#              AUTO-SKIP: when no usable InstanceType reports one of those manufacturers, the platform
#              rows record a SKIP naming the manufacturers that were looked for and the types that
#              were seen, and the case exits 0.
#
# Inputs:      MOCKED: nothing. No node label is injected and no setting is changed — this case
#              reads the manufacturers the cluster's own InstanceTypes already report, so it needs
#              no fixture and its trap deletes nothing.
#
# Expected:    - on a cluster with an unadmitted platform, that platform is REFUSED naming the
#                platform column, the router and the engine it was checked against, and the admitted
#                rows;
#              - two roles of DIFFERENT platforms are refused separately: the mixed-platform rule
#                answers first, so the refusal names two manufacturers rather than the matrix;
#              - an admitted combination (llm-d-router + vLLM on an admitted platform) is ACCEPTED
#                by the same instrument, so the refusals are each their own rule;
#              - on a cluster with no unadmitted platform, the platform rows SKIP naming the
#                manufacturers that were looked for and the types that were seen, and the accepted
#                baseline still runs;
#              - the functional half records a SKIP naming the proof of concept that would admit it.
#
# Cleanup:     Nothing to clean. The case creates no object, injects no label, changes no setting
#              and stores nothing; every assertion is a server-side dry run. Safe to re-run.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-124.sh <NS>" >&2
  exit 2
fi

MD=case124-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
VERSION="${E2E_C124_VERSION:-0.30.0}"
ROUTER_NAME=llm-d-router
ENGINE=vLLM

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-124] jq is required." >&2; exit 2; }


# ---------------------------------------------------------------- the platforms this cluster has

# TWO TYPES ARE SOUGHT, and neither is invented. The platform row needs a type reporting a
# manufacturer the matrix does NOT cover, and the baseline needs one that reports one it DOES.
# On a cluster holding any unadmitted accelerator the first is found and the case runs its rows; on
# a cluster whose only accelerated types are the two admitted platforms the first is empty, the
# rows skip naming what was seen, and the baseline still runs against the second.
#
# THE UNADMITTED MANUFACTURERS ARE THE OPERATOR'S OWN LIST MINUS THE TWO IT ADMITS, so every
# accelerator this project supports is covered without the case having to know which of them is
# common. Two of the nine declared are admitted here and are therefore excluded; the general CPU
# fallback is not a manufacturer of an accelerator and is not in the list either.
UNADMITTED_PLATFORMS="cambricon hygon iluvatar metax mthreads nvidia thead"

# The manufacturer is read from status, not from spec: the platform of a role is whatever its
# InstanceType reports, which is the same fact the webhook reads. A type reporting nothing is not
# treated as a platform of its own — that is the not-yet-synced state, and the webhook refuses it.
usable_type_for() { # <manufacturer>
  kubectl get instancetypes.worker.gpustack.ai     -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      [ -n "$manufacturer" ] || continue
      if [ "$manufacturer" = "$1" ]; then
        echo "$name"
        break
      fi
    done
}

OTHER_IT=""
OTHER_MANUFACTURER=""
for m in $UNADMITTED_PLATFORMS; do
  OTHER_IT="$(usable_type_for "$m")"
  if [ -n "$OTHER_IT" ]; then
    OTHER_MANUFACTURER="$m"
    break
  fi
done
IT="$(usable_type_for ascend)"

SEEN="$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.detail.manufacturer}{" "}{end}' 2>/dev/null)"

if [ -z "$OTHER_IT" ] && [ -z "$IT" ]; then
  record SKIP "the platform could be read" \
    "no usable InstanceType reported any manufacturer (the cluster holds: ${SEEN:-<none>}), so there is no platform to submit and no baseline to compare against"
  print_rows
  exit 0
fi

# ---------------------------------------------------------------- the manifests

# The engine and the router are the only dials this case turns; each is reset right after the row
# that turns it, because an assignment prefixed to a function call persists in bash after the call
# returns and would silently move every later row off the baseline.
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
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: ${ROUTER_NAME:-llm-d-router}
  roles:
$1
YAML
}

# The ADMITTED Ascend row's shape: encode and prefill mount one claim at one path, the decode role
# mounts nothing. It names no claim that has to exist — the matrix is read from the object and the
# claim rules only compare the two mounts to each other.
baseline_roles() { # instanceType
  local it="${1:-${IT:-unset}}"
  cat <<ROLES
  - name: encode
    kind: Encode
    instanceType: ${it}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case124-shared
  - name: prefill
    kind: Prefill
    instanceType: ${it}
    image: ${IMAGE}
    additionalVolumes:
      - mountPath: /ec-cache
        persistentVolumeClaim:
          claimName: case124-shared
  - name: decode
    kind: Decode
    instanceType: ${it}
    image: ${IMAGE}
ROLES
}

# Assert that a manifest is refused AND that the refusal quotes every fragment after the first one
# named. An empty apply output is a FAIL on its own: deleting anything from "" leaves "". A refusal
# carrying other messages too passes — several rules aggregate into one response — but a MISSING
# fragment means the manifest tripped a rule this row is not about.
refuses() {
  local check="$1" want="$2" roles="$3" out w
  shift 3
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$out" ]; then
    record FAIL "$check" "the apply produced no output at all, so nothing was refused"
    return 0
  fi
  for w in "$want" "$@"; do
    case "$out" in
      *"$w"*) ;;
      *) record FAIL "$check" "refused with the wrong message, or accepted (missing '${w}'): ${out:0:220}"; return 0 ;;
    esac
  done
  record PASS "$check" "refused naming: ${want}${*:+ and $*}"
  return 0
}

accepts() {
  local check="$1" roles="$2" out
  out="$(manifest "$roles" | kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      record PASS "$check" "accepted by both the schema and the webhook" ;;
    *) record FAIL "$check" "refused: ${out:0:220}" ;;
  esac
  return 0
}

# ---------------------------------------------------------------- row 0: the accepted baseline

# Three roles being accepted at all is the shape this whole rule set exists to guard, so the
# baseline proves the refusals below are each their own rule rather than one blanket rejection.
if [ -n "$IT" ]; then
  accepts "an admitted combination is ACCEPTED by the same instrument" "$(baseline_roles "$IT")"
else
  record SKIP "an admitted combination is ACCEPTED by the same instrument" \
    "no usable InstanceType reports an admitted platform (the cluster holds: ${SEEN:-<none>}), so there is no accepted shape to compare the refusals against"
fi

# ---------------------------------------------------------------- the NVIDIA rows

if [ -n "$OTHER_IT" ]; then
  # THE HEADLINE OF THIS CASE. The matrix gap is read router-first, then engine, then platform, and
  # it returns ONE message: llm-d-router and vLLM are both admitted here, so the gap that fires is
  # the platform one, which names the platform AND quotes the router and engine it ACCEPTED beside
  # it. The word "and" before the engine is the fragment that proves this is the platform arm and
  # not the engine arm above it — the two messages read alike and only that word tells them apart.
  # The admitted rows come from the same sentence, appended by the rule that fires it.
  refuses "a platform the matrix does not admit is refused, all three named" \
    "is not admitted for encoder roles under router" \
    "$(baseline_roles "$OTHER_IT")" \
    'and engine' \
    "the admitted rows are"

  # THE SAME PLATFORM WITH A DIFFERENT ENGINE, which reaches a different arm and stops earlier:
  # the engine is checked before the platform, so this refusal is the engine message and never
  # reaches the platform row above. The pair is what tells a user which of the two to change, and
  # either alone leaves the other looking fine.
  ENGINE=SGLang refuses "an unadmitted platform is refused naming the engine too" \
    'is not admitted with engine "SGLang"' \
    "$(baseline_roles "$OTHER_IT")"
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
  if [ -n "$IT" ]; then
    refuses "two platforms in one encoder deployment are refused by the mixing rule" \
      "more than one accelerator platform" \
      "$(baseline_roles "$IT")
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

# ---------------------------------------------------------------- why there is no functional half

# Stated as a row rather than left to the header, because a case that reads as a refusal-only case
# with no functional rows is otherwise indistinguishable from one that forgot to write them. The
# admission that would admit this combination is spec task T3a, and it is blocked on a cluster.
record SKIP "NVIDIA serves a multimodal request through the router" \
  "the matrix carries no NVIDIA row, so there is no object to run. The functional reading is spec task T3a (ec-nixl and ec-example on NVIDIA hardware), which waits for a cluster; the matrix gains an NVIDIA entry only for a combination with a passing run, so until then this case asserts the refusal and nothing else"

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-124] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-124] all checks passed"