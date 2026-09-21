#!/usr/bin/env bash
#
# CASE 83 — Preflight reports the TopologyManager policy the node's kubelet is actually running
#   (MUTATING, self-recovering; AUTO-SKIPS without a node running a device manager, or where the
#    kubelet's own configuration endpoint cannot be read)
#
#   case-83.sh <NS>
#
# <NS> is the operator's own namespace, where the device manager DaemonSet lives. The preflight Pod
# runs in `default` (or RDMA_TEST_NS).
#
# Goal:        The policy decides whether the NUMA hint this operator publishes changes any
#              admission at all, and an operator reads it out of preflight. So preflight has to
#              report the policy the kubelet is RUNNING, not the one a file it happened to look at
#              names, and not the kubelet's compiled-in default.
#              The instrument is the kubelet's own live configuration endpoint, because judging
#              preflight's answer against the same configuration files preflight reads asks one
#              source twice: a policy written somewhere the reader does not look produces exactly
#              the reading a policy nobody set produces. That is the defect this row caught once —
#              a node configured the way its distribution documents was the node whose policy could
#              not be read, because the distribution copies a caller's drop-in into a SUBDIRECTORY
#              of its managed tree.
#              WHAT THE INSTRUMENT CANNOT SAY, and the reason the comparison is one-way: the
#              endpoint reports the EFFECTIVE configuration, defaults included, so it always names a
#              policy — `none` on a node where nobody ever set one. Preflight deliberately does not
#              publish a default nobody wrote. So "preflight equals the endpoint" is NOT the
#              criterion: it fails every node with an unset policy, which is preflight behaving
#              correctly, and it was measured doing exactly that.
#              The criterion is the one-way inference the endpoint does support. The kubelet's
#              default is `none`, so an effective value that is NOT `none` came from a configuration
#              source and preflight is required to have found it — a report of `unknown` there is
#              the failure. Where the endpoint says `none` the two cases "nobody set it" and
#              "somebody set it to none" are indistinguishable from outside, so preflight's
#              `unknown` is RECORDED rather than judged, and the one direction still decidable is
#              asserted: preflight must never name a policy the kubelet is not running, and a policy
#              it does name must come with a depth.
# Environment: A node running a device manager (the preflight image is taken from that DaemonSet, so
#              the binary under test is the one this cluster is running). The preflight Pod mounts
#              the host root read-only and runs as uid 0 to read the kubelet's configuration, which a
#              `restricted` PodSecurity namespace refuses; there the case skips.
#              AUTO-SKIPS (exit 0, printing NOTHING WAS VERIFIED) when no node qualifies, when the
#              kubelet endpoint is unreadable, or when the preflight run produced no topology
#              section at all — another preflight holding the node's lock produces that, and it is
#              an answer about the node rather than about the report.
# Inputs:      All real, nothing mocked. One Pod running this cluster's own device-manager image.
#              It is given NO device access on purpose: accelerator detection then finds nothing and
#              no probe container is started on the host, and the topology section is read from
#              files and never touches an accelerator. The run's overall exit code is therefore
#              expected to be non-zero and is not read.
# Expected:    - where the kubelet is running a policy that is NOT its default, the topology section
#                names that same policy;
#              - where it is running the default, preflight's `unknown` is recorded as an answer
#                this node cannot discriminate, while a policy preflight DOES name must agree with
#                the kubelet and must carry a depth;
#              - a reported policy carries no note, and `unknown` carries one — a note is present
#                exactly when the policy is unknown, so the pair is asserted rather than the value
#                alone. Only the note's PRESENCE is asserted, never its wording: which sources it
#                names is a property of the reader and has already moved once, so a case pinning
#                today's phrasing would go red the day the reader is corrected.
# Cleanup:     Trap deletes the preflight Pod. It writes nothing to the host: the host root is
#              mounted read-only and, with no accelerator detected, nothing is staged and no probe
#              container is started. The trap runs on pass AND fail and is safe to re-run.
set -uo pipefail

NS="${1:?usage: case-83.sh <NS>}"
CASE_ID=83
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

trap rdma_cleanup EXIT

# Any node with a device manager. This case does not need RDMA hardware at all: the topology section
# is about the kubelet, and a node with no RDMA still has a policy and still needs it read correctly.
rdma_select devices

if [ -z "$RDMA_KUBELET_POLICY" ]; then
  rdma_skip "The kubelet on ${RDMA_NODE} did not answer /api/v1/nodes/${RDMA_NODE}/proxy/configz." \
    "Without it there is no instrument independent of preflight to judge preflight's answer" \
    "against, and accepting its own report would make this check pass for any value it printed." \
    "The endpoint needs the nodes/proxy subresource; grant it, or run this case where it is" \
    "reachable."
fi

# The image this cluster is running, taken from the device manager DaemonSet rather than from a
# variable. A tag can be repointed and an image ID only says the image is not the previous one, so
# the ref is recorded with the verdict rather than treated as proof of a revision.
PF_IMAGE=$(kubectl -n "$NS" get daemonset \
  -l app.kubernetes.io/part-of=gpustack-operator,app.kubernetes.io/component=device-manager \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null)
if [ -z "$PF_IMAGE" ]; then
  rdma_skip "No device manager DaemonSet in ${NS} to take the preflight image from." \
    "Naming an image by hand instead would test whatever that image carries rather than what this" \
    "cluster is running."
fi
echo "[case-83] kubelet reports topologyManagerPolicy=${RDMA_KUBELET_POLICY}; preflight image ${PF_IMAGE}"

PF_POD="gpustack-e2e-rdma-preflight-${RANDOM}"
TESTPODS+=("$PF_POD")
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${PF_POD}, namespace: ${RDMA_TEST_NS} }
spec:
  restartPolicy: Never
  hostNetwork: true
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: preflight
      image: ${PF_IMAGE}
      command: ["gpustack-operator", "device-manager", "preflight", "--host-root=/host"]
      securityContext: { runAsUser: 0 }
      volumeMounts:
        - { name: hostroot, mountPath: /host, readOnly: true }
  volumes:
    - { name: hostroot, hostPath: { path: /, type: Directory } }
EOF

PHASE=$(rdma_wait_pod "$PF_POD" Succeeded Failed)
if [ -z "$PHASE" ]; then
  rdma_skip "The preflight Pod never finished on ${RDMA_NODE} within ${RDMA_POD_TIMEOUT}s." \
    "Last state: $(rdma_pod_reason "$PF_POD")" \
    "A Pod that mounts the host root and runs as uid 0 is refused outright by a restricted" \
    "PodSecurity namespace; set RDMA_TEST_NS to a namespace that permits it."
fi

OUT=$(kubectl -n "$RDMA_TEST_NS" logs "$PF_POD" 2>/dev/null)
# The topology block of the YAML document, extracted by indentation. A full YAML parser is not worth
# a dependency here: the section is two or three scalar keys under one top-level key, and the block
# ends at the next unindented line.
TOPO=$(printf '%s\n' "$OUT" | awk '/^topology:/ {inblock=1; next} inblock && /^[^[:space:]]/ {inblock=0} inblock {print}')
if [ -z "$TOPO" ]; then
  rdma_skip "The preflight run produced no topology section on ${RDMA_NODE}." \
    "Only one preflight runs on a node at a time, and a second one refuses before it sweeps" \
    "anything — so this is an answer about the node's state, not about the report. First lines of" \
    "the run: $(printf '%s' "$OUT" | head -5 | tr '\n' ' ')"
fi

PF_POLICY=$(printf '%s\n' "$TOPO" | awk -F': *' '/^[[:space:]]*policy:/ {print $2; exit}' | tr -d '[:space:]')
PF_DEPTH=$(printf '%s\n' "$TOPO" | awk -F': *' '/^[[:space:]]*depth:/ {print $2; exit}' | tr -d '[:space:]')
# The note is taken whole, from `note:` to the end of the block, and never split on a colon. Its
# wording is not this case's business and has already changed once: the sources it names are the
# ones the reader actually walks, which move when the reader is corrected. Splitting on `: ` would
# have truncated a note at the first source it listed, which looks like a shorter note rather than
# like a parsing mistake.
PF_NOTE=$(printf '%s\n' "$TOPO" | awk '
  /^[[:space:]]*note:/ { innote = 1; sub(/^[[:space:]]*note:[[:space:]]*/, ""); print; next }
  innote && /^[[:space:]]*[a-zA-Z_]+:/ { innote = 0 }
  innote { sub(/^[[:space:]]+/, ""); print }' | tr '\n' ' ' | sed 's/[[:space:]]*$//')

# WHICH COMPARISON IS THE CRITERION, and why the obvious one is wrong.
#
# The kubelet's endpoint reports its EFFECTIVE configuration, defaults included, so it always names
# a policy — `none` on a node where nobody ever set one. Preflight deliberately does not report a
# default nobody wrote: its own note says publishing one would be a value nobody read. So
# "preflight must equal the kubelet's endpoint" FAILS EVERY NODE WHOSE POLICY IS UNSET, which is
# preflight behaving correctly. That comparison was measured doing exactly that and is not used.
#
# What the endpoint can support is a one-way inference: the kubelet's default is `none`, so an
# effective value that is NOT `none` had to come from a configuration source, and preflight is
# required to have found it. That direction is the whole criterion, and it is where the defect this
# reading exists to catch would show.
#
# Where the endpoint says `none`, it cannot tell "nobody set it" from "somebody set it to none",
# and preflight's own answer is the only thing that separates them — so the case reads preflight
# there rather than judging it, except for the one direction that is still decidable: preflight must
# never name a policy the kubelet is not running.
if [ "$RDMA_KUBELET_POLICY" != "none" ]; then
  if [ "$PF_POLICY" = "$RDMA_KUBELET_POLICY" ]; then
    record PASS "preflight names the policy the kubelet is running" \
      "both say ${PF_POLICY} (depth ${PF_DEPTH:-<none>}); the kubelet's default is none, so this value was written somewhere and preflight found it. Read independently: the kubelet's own configz and the report's topology section. Image: ${PF_IMAGE}"
  elif [ "$PF_POLICY" = "unknown" ]; then
    record FAIL "preflight names the policy the kubelet is running" \
      "the kubelet is running ${RDMA_KUBELET_POLICY}, which is not its default, so a configuration source names it — and preflight reports unknown, meaning it did not find that source. Note: ${PF_NOTE:-<none>}"
  else
    record FAIL "preflight names the policy the kubelet is running" \
      "the kubelet is running ${RDMA_KUBELET_POLICY}, preflight reports ${PF_POLICY:-<none>}"
  fi
elif [ "$PF_POLICY" = "unknown" ]; then
  record SKIP "preflight names the policy the kubelet is running" \
    "NOT DISCRIMINATING ON THIS NODE: the kubelet's endpoint reports none, which is its default, so nobody need have written a policy anywhere — and preflight correctly declines to publish a default nobody wrote. 'unknown' is therefore both the right answer here AND what a reader that can read nothing produces, and this node cannot tell the two apart. Answering it needs a node whose policy is EXPLICITLY set; the requirement 'topology-enforced' in the capability matrix is the proxy for that. Note: ${PF_NOTE:-<none>}"
elif [ "$PF_POLICY" = "none" ]; then
  # Preflight named `none` rather than declining, which means it found a source declaring it. That
  # is decidable and agrees with the kubelet, so it is a pass -- and the depth is asserted with it,
  # because a value reported at no depth would be the default leaking out under another name.
  if [ -n "$PF_DEPTH" ]; then
    record PASS "preflight names the policy the kubelet is running" \
      "both say none, and preflight reports depth ${PF_DEPTH}, so it read the value from a source rather than falling back to the kubelet's default. Image: ${PF_IMAGE}"
  else
    record FAIL "preflight names the policy the kubelet is running" \
      "preflight reports policy none at no depth, which is indistinguishable from publishing the kubelet's default -- the one thing it states it does not do"
  fi
else
  record FAIL "preflight names the policy the kubelet is running" \
    "the kubelet is running none and preflight reports ${PF_POLICY}, so the report names a policy this node is not on"
fi

# The note pairs with the value in both directions, so the check has information whichever way the
# node is configured: present exactly when the policy is unknown. Asserting only "a known policy has
# no note" would pass on every node that reports a policy and never exercise the other half.
#
# ONLY PRESENCE IS ASSERTED, never wording. Which sources the note names is a property of the reader
# and has already moved once; a case pinning today's phrasing would fail the day the reader is
# corrected, which is the opposite of what this row is for. The note is printed so a human reading
# the table sees what it said.
if [ "$PF_POLICY" = "unknown" ]; then
  if [ -n "$PF_NOTE" ]; then
    record PASS "an unknown policy says why it could not be read" "note: ${PF_NOTE}"
  else
    record FAIL "an unknown policy says why it could not be read" \
      "policy unknown with no note, so the report does not say which of its reasons applies and a reader cannot tell an unset policy from an unreadable one"
  fi
else
  if [ -z "$PF_NOTE" ]; then
    record PASS "a reported policy carries no note" "policy ${PF_POLICY}, depth ${PF_DEPTH:-<none>}, no note"
  else
    record FAIL "a reported policy carries no note" \
      "policy ${PF_POLICY} reported alongside note '${PF_NOTE}'; a note is present exactly when the policy is unknown"
  fi
fi

rdma_results
