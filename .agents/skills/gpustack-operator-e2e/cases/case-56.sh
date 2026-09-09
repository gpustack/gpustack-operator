#!/usr/bin/env bash
#
# CASE 56 — A Pod that did not opt in comes back unchanged BY THIS WEBHOOK
#   (MUTATING, self-recovering)
#
#   case-56.sh <NS>
#
# Goal:        A mutating webhook with failurePolicy Fail on every Pod CREATE in the cluster is a
#              blast radius, and the only thing bounding it is the objectSelector. This case asserts
#              the bound from OUTSIDE the operator: submit Pods that did not opt in, read back what
#              the API server stored, and diff it against what was submitted. Nothing here inspects
#              the webhook's own decision - that is the point, since a webhook that believed it had
#              skipped a Pod and mutated it anyway would report the same thing either way.
#
#              Three shapes, because they fail differently: no label at all (the ordinary Pod, which
#              the selector never routes here), the label at "false" (the documented opt-out), and the
#              label at a third value (which the selector's In [true] must not match either).
#
# Environment: a cluster with the operator installed and a node able to run the Mooncake image. The
#              case stands up its own KVCacheBackend (multi-tenancy on, TCP), KVCachePool, namespace
#              and one KVCachePoolBinding, so it needs no pool to pre-exist. Never auto-skips.
# Inputs:      three real Pods that carry the configuring annotations but not the opt-in label at
#              its accepted value. Nothing is mocked; the annotations are present deliberately, so a
#              pass proves the LABEL is what decides.
# Expected:    each submitted Pod is stored with its spec and its annotations unchanged - no env, no
#              args, no volume, no mount, and no injection record.
#
#              SKIPS: none. This case has no conditional half - every check either runs or FAILS, and
#              a precondition it cannot meet is recorded as a failure rather than passed over. The
#              footer counts any SKIP separately from the passes, so a skipped check can never be read
#              off the PASS count.
# Cleanup:     the trap removes the Pods, the Binding, the namespace, the pool and the backend, in
#              that order and idempotently, on pass AND fail. The Binding is given 60s before its
#              finalizer is forced: a domain still holding objects makes the master refuse to drop
#              its quota, and forcing it earlier is how a run leaves a namespace Terminating forever.
#              It changes no shared baseline - every object it touches is one it created.
#
# EXERCISED 2026-09-04 on a three-node k3s cluster. The first run failed all three checks, and the
# fault was this case's: it compared every volume, mount and annotation, so the API server's own
# service-account projection counted as a change admission made. It could not have passed whatever the
# webhook did. Now four checks pass, the fourth being the control that proves the comparison can see
# an injection at all.
set -uo pipefail

NS="${1:?usage: case-56.sh <NS>}"
CASE_ID=56
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-inject-lib.sh"

# Armed BEFORE setup, not after: kvi_setup creates the cluster-scoped backend on its first line and
# has five failure exits after that, so arming afterwards leaks whatever a failed setup left behind.
trap kvi_teardown EXIT
kvi_setup || { kvi_results "$CASE_ID"; exit 1; }

# submitted_shape and stored_shape read the same fields off the two sides of admission. Only the
# fields this webhook is allowed to write are compared: the API server defaults plenty of others
# (serviceAccount, tolerations, the default token mount), and a diff of the whole object would drown
# the signal in them.
stored_shape() {
  local pod="$1"
  {
    # env and args are listed WHOLE: the submitted container declares no env and exactly two args, and
    # the API server adds neither, so any line here that is not in WANT was put there by admission.
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].env[*]}ENV {.name}{"\n"}{end}' 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" -o jsonpath='{range .spec.containers[0].args[*]}ARG {@}{"\n"}{end}' 2>/dev/null
    # Volumes, mounts and annotations are NOT listed whole, and that is not laziness. The API server
    # projects a service-account token volume and its mount onto every Pod, and the two opt-in
    # annotations the Pod was submitted with are annotations too. Listing all of them made this check
    # fail for reasons that have nothing to do with the webhook - it could not pass no matter how the
    # webhook behaved, and a check that cannot pass reports as little as one that cannot fail. So the
    # names this webhook would write are matched exactly, and everything else is left to the API
    # server. A webhook injecting under some OTHER name is not caught here; it is caught by case 53,
    # which asserts the container has what it needs rather than what it lacks.
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.containers[0].volumeMounts[?(@.name=='${CONFIG_VOLUME}')]}MOUNT {.name}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath={range .spec.volumes[?(@.name=='${CONFIG_VOLUME}')]}VOL {.name}{'\n'}{end}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath=STAMP={.metadata.annotations.kvcache\.gpustack\.ai/injected}{'\n'}" 2>/dev/null
    kubectl -n "$TEST_NS" get pod "$pod" \
      -o "jsonpath=CONFIG={.metadata.annotations.kvcache\.gpustack\.ai/client-config}{'\n'}" 2>/dev/null
  } | sort
}

# The volume and mount this webhook injects, by the name it uses (inject.ConfigVolumeName). A Pod that
# did not opt in must carry neither.
CONFIG_VOLUME=gpustack-kvcache-config

# The submitted shape, written out once. Every case below submits exactly this container, so anything
# that appears in the stored shape and not here was added by admission.
# No ARG rows: the fixture carries its whole launch line in command, so an untouched Pod has no args
# at all. That makes the ARG listing above a pure detector - any ARG row in the stored shape was put
# there by admission.
WANT="$(printf 'CONFIG=\nSTAMP=\n' | sort)"

submit() {
  local pod="$1" label_line="$2"
  kubectl apply -f - <<YAML >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${TEST_NS}
${label_line}
  annotations:
    kvcache.gpustack.ai/binding: ${BINDING}
    kvcache.gpustack.ai/engine: vllm
spec:
  restartPolicy: Never
  containers:
    - name: engine
      image: ${CLIENT_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(3600)"]
YAML
}

check() {
  local pod="$1" what="$2"
  if ! kvi_wait_for pods "$pod" '{.metadata.name}' "$pod" 60 "$TEST_NS" >/dev/null; then
    record FAIL "$what" "the Pod was never stored; a Pod that did not opt in must not even be refused"
    return
  fi
  local got
  got="$(stored_shape "$pod")"
  if [ "$got" = "$WANT" ]; then
    record PASS "$what" "stored exactly as submitted: no env, no volume, no mount, no record"
  else
    record FAIL "$what" "admission changed it: $(diff <(echo "$WANT") <(echo "$got") | tr '\n' ' ' | cut -c1-200)"
  fi
}

# The annotations are present in all three, deliberately: they would be enough to inject if the label
# were, so this proves the LABEL is what decides and the annotations alone do nothing.
submit no-label ""
check no-label "a Pod with no inject label is untouched"

submit label-false '  labels:
    kvcache.gpustack.ai/inject: "false"'
check label-false "a Pod opting out with false is untouched"

submit label-other '  labels:
    kvcache.gpustack.ai/inject: "yes"'
check label-other "a Pod whose label is a third value is untouched"

# Positive control. The three checks above are all NEGATIVE - each passes by observing nothing - and a
# stored_shape that had gone blind would pass all three for the wrong reason. This submits the one
# label value that MUST be injected and requires the same comparison to see it. Its expected value
# (changed, naming the config path) differs from the failure value (unchanged), so it discriminates.
submit opted-in '  labels:
    kvcache.gpustack.ai/inject: "true"'
if ! kvi_wait_for pods opted-in '{.metadata.name}' opted-in 60 "$TEST_NS" >/dev/null; then
  record FAIL "the same comparison sees an injection when there is one" \
    "the opted-in Pod was never stored, so this control did not run and the three checks above are \
unverified: nothing here proves the comparison can see anything at all"
else
  ctl="$(stored_shape opted-in)"
  if [ "$ctl" = "$WANT" ]; then
    record FAIL "the same comparison sees an injection when there is one" \
      "an opted-in Pod compared EQUAL to the submitted shape - the comparison is blind, and the three \
untouched checks above passed for that reason rather than because nothing was injected"
  elif echo "$ctl" | grep -q '^ENV MOONCAKE_CONFIG_PATH$'; then
    record PASS "the same comparison sees an injection when there is one" \
      "the opted-in Pod differs and the difference names MOONCAKE_CONFIG_PATH, so the three untouched \
checks above passed against a comparison that is looking"
  else
    record FAIL "the same comparison sees an injection when there is one" \
      "the opted-in Pod differs, but not by MOONCAKE_CONFIG_PATH: $(echo "$ctl" | tr '\n' ' ' | cut -c1-160)"
  fi
fi

kvi_results "$CASE_ID"
