#!/usr/bin/env bash
#
# CASE 29 — Two partition shapes at once, a replacement inside the reclaim window, and a terminated
#            container's charge   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable
#            node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-29.sh <NS>
#
# Prerequisites: nodes=1; gpu=nvidia; features=partition; cluster=real; inputs=MIG_NODE_SSH,MIG_RECLAIM_BOUND
# Goal:        Three contracts of the hardware-partition path, read on one card in one run, in the
#              order they can be read on.
#
#              ONE: TWO REQUESTS THAT DIFFER ONLY IN THE PROFILE THEY NAME. The device-plugin
#              allocation RPC carries a resource name and a device count and nothing else -- no
#              profile, no Pod identity -- so when two Pods are admitted together, both carrying
#              the same bare partition key and differing only in the profile they name, the plugin
#              must still actuate each one's OWN profile. Getting this wrong is silent rather than
#              loud: the wrong hardware shape is carved and both Pods start, so nothing fails until
#              a tenant notices the instance is not the size they asked for. The shape each Pod
#              actually received is read from two independent sources -- the hardware itself,
#              through the device list inside the container, and the durable per-Pod allocation
#              record the plugin wrote -- and neither Pod's record may be overwritten with the
#              other's, because the record is keyed by container and a second allocation must
#              accumulate rather than erase.
#
#              TWO: THE GAP BETWEEN THE ACCOUNTING FREEING A PARTITION AND THE HARDWARE RELEASING
#              IT, MEASURED RATHER THAN ASSUMED AWAY. Node-level occupancy is rebuilt from the Pod
#              annotations, so a deleted Pod's slot reappears in the per-profile key the moment the
#              Pod is gone -- while the reclaimer only destroys the instance after several
#              consecutive absent sightings on its resync cadence. A replacement scheduled inside
#              that window meets an instance that still exists, and what it then gets is MEASURED
#              here rather than assumed: the allocation may be refused, or it may be granted that
#              very instance which the reclaimer then destroys underneath the starting container.
#              Both cost a retry, they are told apart by WHERE the attempt died, and only the first
#              raises an UnexpectedAdmissionError -- the second leaves that count at zero. So the
#              shape of every failed attempt is recorded, not just how many there were. The
#              containment is that the request is retried and the window closes on its own; the
#              failure mode worth catching is a request that NEVER converges.
#
#              THREE: A TERMINATED INIT CONTAINER STILL CHARGES THE CARD ITS INSTANCE OCCUPIES. A
#              device is scoped to the Pod's life, not the container's. The kubelet keeps a finished
#              init container's devices in its record for as long as the Pod exists, and the
#              operator's reclaimer destroys a hardware partition on Pod deletion -- not on
#              container termination. So a Pod whose init container took a partition and then
#              exited, with the Pod still running, is STILL occupying that hardware. The
#              accounting has to say so: the node's per-profile capacity must keep charging the
#              instance, or the node advertises room it does not have and the next tenant is
#              admitted onto a card that cannot take it. Only hardware shows this -- a ledger
#              fixture passes whether or not the entry survives the container -- so the live
#              instance is read from the card, the node key is read, and a further Pod is admitted
#              against the room the node claims is left, requiring it to actually start.
#
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, the mode switch does not converge, or the node advertises no
#              4-memory-slice profile key -- the shape every section below places.
#
#              The two remaining sections carry their own shape requirement and are SKIPPED ON
#              THEIR OWN when the node does not advertise it, loudly and in their own banner, so
#              that one missing shape never costs the other sections their rows: the two-shape
#              section needs a smallest-profile key distinct from the 4-memory-slice one, and the
#              terminated-container section needs a whole-card profile key to read the charge
#              against.
#
# Inputs:      All real, nothing mocked -- the actuated hardware shapes are the verification. Every
#              Pod is created through the pool's entrance LocalQueue and pinned to the accelerator
#              node: two submitted back to back with no wait between them, one naming the
#              DISCOVERED 4-memory-slice profile and one the smallest profile, chosen so they fit
#              side by side on one card, which is what makes the resolution rather than the
#              geometry the thing under test; then one partition Pod that is deleted and
#              immediately re-requested, the replacement retried the way a controller would retry
#              it; then one Pod whose INIT container requests the 4-memory-slice partition and
#              exits after a few seconds while its app container holds no accelerator at all (the
#              request rules allow accelerator claims in exactly one container group, and this Pod
#              puts them in the init group), followed by a second, ordinary partition Pod of the
#              same profile, to consume the room the node still reports.
#
# Expected:    - both of the first two Pods reach Running, each device list names the profile that
#                Pod requested and shows exactly one instance, and each Pod's durable allocation
#                record names its own profile and not the other's;
#              - neither of those Pods records an UnexpectedAdmissionError;
#              - the first partition of the reclaim section runs and is deleted, and a replacement
#                eventually runs within MIG_RECLAIM_BOUND seconds (default 300) -- that is the
#                containment claim, and the only hard assertion in that section;
#              - the attempt count, the elapsed seconds and any terminal allocation failures along
#                the way are RECORDED as the measurement of the window, not asserted against a
#                threshold;
#              - the init-container Pod reaches Running with its init container Terminated, the
#                card still holds a live hardware instance, the whole-card profile key still reads
#                one card's worth less, and the follow-up partition Pod, admitted against the
#                remaining reported room, reaches Running without a single
#                UnexpectedAdmissionError.
#
# Cleanup:     One trap deletes every Pod this case created, waits for the instances to reclaim,
#              and restores the partitioning mode of the card this case toggled (a card found
#              already partitioned is left as found). Idempotent; runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-29.sh <NS>}"
CASE_ID=29
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

# One prefix per section, so every Pod carries the name it carried in the case it came from.
PODPFX=gpustack-e2e-concprof

# How long the reclaim window is given to close on its own.
BOUND="${MIG_RECLAIM_BOUND:-300}"

# The sections a cluster did not give the shape for, named for the footer.
SKIPPED=""

part_require_node_ssh "case-29.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-29] cleanup: deleting test Pods and restoring card(s) '${TOGGLED:-<none>}'"
  delete_test_pods
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

# ONE discovery for the whole case: the card geometry and the three profile keys every section
# below places against.
read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
MIDKEY="$(profile_key "$MID")"
SMALLKEY="$(profile_key "$SMALL")"
FULLKEY="$(profile_key "$FULL")"
echo "[case-29] discovered profiles: SMALL=${SMALL}(${SMALL_CNT}/card) MID=${MID}(${MID_CNT}/card) FULL=${FULL}(${FULL_CNT}/card); partitioned cards=${NPART:-0}"

# The 4-memory-slice shape is the one every section places, so its absence skips the whole case.
if [ -z "$MIDKEY" ]; then
  part_skip \
    "The node advertises no 4-memory-slice profile key ('${MIDKEY:-<none>}') — nothing to partition with." \
    "This case needs a card that offers the 4-memory-slice hardware partition profile."
fi

wait_card_idle || echo "[case-29] warning: the card already holds live instances"

# ---------------------------------------------------------------------------------------------------
# Section 1 — two simultaneous requests for DIFFERENT profiles.
#
# The allocation RPC carries no profile and no Pod identity, so two Pods that differ only in the
# profile they name must still each be given their own. Each shape is read from the hardware and
# from the durable per-Pod record.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-29] === section 1: two simultaneous requests: ${MID} and ${SMALL} ==="
PM="${PODPFX}-mid"
PS="${PODPFX}-small"
if [ -z "$SMALLKEY" ] || [ "$SMALL" = "$MID" ]; then
  SKIPPED="${SKIPPED}the two-shape section; "
  echo
  echo "== SECTION 1 — SKIPPED =="
  echo "The node does not advertise two distinct coexisting profile keys (SMALL='${SMALLKEY:-<none>}'," \
    "MID='${MIDKEY:-<none>}'). This section needs two different shapes that fit one card side by side."
else
  mkpod "$PM" "$(partition_reslines "$MIDKEY")"
  mkpod "$PS" "$(partition_reslines "$SMALLKEY")"

  mrun=1; wait_running "$PM" || mrun=0
  srun=1; wait_running "$PS" || srun=0
  if [ "$mrun" = 1 ] && [ "$srun" = 1 ]; then
    record PASS "both distinct-profile requests run" "${PM} (${MID}) and ${PS} (${SMALL}) both Running; node reports $(node_gi_count) live instance(s)"
  else
    record FAIL "both distinct-profile requests run" "${PM} running=${mrun} (held: $(held_reason "$PM")), ${PS} running=${srun} (held: $(held_reason "$PS")) — both shapes fit one card, so both must be placed"
  fi

  # What each Pod actually got, from the hardware and from the durable record.
  if [ "$mrun" = 1 ]; then
    smi="$(kubectl -n default exec "$PM" -- nvidia-smi -L 2>/dev/null)"
    devs="$(printf '%s\n' "$smi" | grep -c 'MIG')"
    rec="$(pod_profiles "$PM")"
    if printf '%s\n' "$smi" | grep -q "$MID" && [ "${devs:-0}" = 1 ]; then
      record PASS "${PM} was given the ${MID} it asked for" "device list names ${MID}, exactly ${devs} instance visible"
    else
      record FAIL "${PM} was given the ${MID} it asked for" "device list = '$(printf '%s' "$smi" | tr '\n' ';')' (want one ${MID} instance) — a request was resolved to the wrong container's profile"
    fi
    if [ "$rec" = "$MID" ]; then
      record PASS "${PM}'s durable record names its own profile" "recorded profile '${rec}', with no trace of ${SMALL}"
    else
      record FAIL "${PM}'s durable record names its own profile" "recorded profile '${rec:-<none>}', want exactly '${MID}' — the per-container allocation record was cross-patched or lost"
    fi
  fi
  if [ "$srun" = 1 ]; then
    smi="$(kubectl -n default exec "$PS" -- nvidia-smi -L 2>/dev/null)"
    devs="$(printf '%s\n' "$smi" | grep -c 'MIG')"
    rec="$(pod_profiles "$PS")"
    if printf '%s\n' "$smi" | grep -q "$SMALL" && [ "${devs:-0}" = 1 ]; then
      record PASS "${PS} was given the ${SMALL} it asked for" "device list names ${SMALL}, exactly ${devs} instance visible"
    else
      record FAIL "${PS} was given the ${SMALL} it asked for" "device list = '$(printf '%s' "$smi" | tr '\n' ';')' (want one ${SMALL} instance) — a request was resolved to the wrong container's profile"
    fi
    if [ "$rec" = "$SMALL" ]; then
      record PASS "${PS}'s durable record names its own profile" "recorded profile '${rec}', with no trace of ${MID}"
    else
      record FAIL "${PS}'s durable record names its own profile" "recorded profile '${rec:-<none>}', want exactly '${SMALL}' — the per-container allocation record was cross-patched or lost"
    fi
  fi

  uae=0
  unreadable=0
  for p in "$PM" "$PS"; do
    n="$(pod_unexpected_admission "$p")"
    case "$n" in
      '') ;;
      # "?" is the answer when the events cannot be read. It is not a count, so it is not added to
      # one -- an unreadable answer is no admission failure seen, and the row below says so.
      *[!0-9]*) unreadable=$((unreadable + 1)) ;;
      *) uae=$((uae + n)) ;;
    esac
  done
  if [ "$uae" = 0 ] && [ "$unreadable" = 0 ]; then
    record PASS "no terminal admission failure" "0 UnexpectedAdmissionError events across both Pods"
  else
    record FAIL "no terminal admission failure" "${uae} UnexpectedAdmissionError event(s)${unreadable:+, ${unreadable} Pod(s) whose events could not be read} — a profile the card could not host was attempted"
  fi

  # The two instances go before the next section places its own: the card is the same one, and
  # the reclaim window the next section measures starts from a card that is idle.
  delpod "$PM"
  delpod "$PS"
  wait_card_idle || echo "[case-29] warning: the card still holds live instances after the two-shape section"
fi

# ---------------------------------------------------------------------------------------------------
# Section 2 — the reclaim window: a same-profile replacement scheduled right after its predecessor
# was deleted.
#
# The accounting frees the partition on Pod deletion; the hardware releases it a few resync passes
# later. A replacement inside that window is retried, and the window must close on its own.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-29] === section 2: one ${MID} partition, deleted, then re-requested at once ==="
PODPFX=gpustack-e2e-reclaimwin
A="${PODPFX}-first"
mkpod "$A" "$(partition_reslines "$MIDKEY")"
arun=0
if wait_running "$A"; then
  arun=1
  GI_BEFORE="$(node_gi_count)"
  record PASS "the first partition runs" "${A} Running on card(s) $(pod_cards "$A"); ${GI_BEFORE} live hardware instance(s)"
else
  record FAIL "the first partition runs" "${A} did not reach Running (held: $(held_reason "$A")) — cannot set up the reclaim window"
  record SKIP "the replacement converges without intervention" "not attempted: the predecessor never reached Running"
  record SKIP "OBSERVED: cost of the reclaim window" "not attempted: the predecessor never reached Running"
fi

if [ "$arun" = 1 ]; then
  delpod "$A"
  T0="$(date +%s)"
  ATTEMPTS=0
  UAE=0
  # Where the failed attempts died, tallied separately from UAE. A grant that is destroyed under the
  # starting container raises no admission event, so UAE stays 0 through exactly the failure this window
  # is most likely to produce; without this tally the run reads as clean.
  SHAPE_ADMISSION=0
  SHAPE_START=0
  SHAPE_OTHER=0
  OK=0
  WINNER=""
  while [ "$(( $(date +%s) - T0 ))" -lt "$BOUND" ]; do
    ATTEMPTS=$((ATTEMPTS + 1))
    R="${PODPFX}-repl-${ATTEMPTS}"
    mkpod "$R" "$(partition_reslines "$MIDKEY")"
    for _ in $(seq 1 20); do
      ph="$(phase "$R")"
      case "$ph" in
        Running) OK=1; WINNER="$R"; break ;;
        Failed) break ;;
      esac
      sleep 3
    done
    n="$(pod_unexpected_admission "$R")"
    case "$n" in
      '') ;;
      # "?" is the answer when the events cannot be read. It is not a count, so it is not added to
      # one -- the tally below then reports the events it could not read as neither.
      *[!0-9]*) ;;
      *) UAE=$((UAE + n)) ;;
    esac
    [ "$OK" = 1 ] && break
    # Read the shape ONCE: the Pod is live and a second read can answer differently.
    shape="$(pod_failure_shape "$R")"
    case "$shape" in
      admission) SHAPE_ADMISSION=$((SHAPE_ADMISSION + 1)) ;;
      start)     SHAPE_START=$((SHAPE_START + 1)) ;;
      *)         SHAPE_OTHER=$((SHAPE_OTHER + 1)) ;;
    esac
    echo "[case-29]   attempt ${ATTEMPTS} did not run (phase=$(phase "$R"), where=${shape}, held: $(held_reason "$R")) — retrying"
    delpod "$R"
    sleep 5
  done
  T1="$(date +%s)"
  ELAPSED=$((T1 - T0))

  if [ "$OK" = 1 ]; then
    record PASS "the replacement converges without intervention" "${WINNER} Running ${ELAPSED}s after the predecessor was deleted, on attempt ${ATTEMPTS} of at most ${BOUND}s"
  else
    record FAIL "the replacement converges without intervention" "no same-profile replacement reached Running within ${BOUND}s over ${ATTEMPTS} attempt(s) — the window is supposed to close on its own"
  fi
  record PASS "OBSERVED: cost of the reclaim window" "attempts=${ATTEMPTS}, elapsed=${ELAPSED}s, refused before actuating=${SHAPE_ADMISSION} (UnexpectedAdmissionError events=${UAE}), granted then destroyed under the container=${SHAPE_START}, other=${SHAPE_OTHER} (a first attempt that runs immediately means the window was already closed on this cadence; a non-zero 'granted then destroyed' means the window handed out a partition it went on to reclaim, which no admission counter records)"

  echo
  echo "---- fold back into the spec: reclaim-window replacement ----"
  echo "profile          : ${MID}"
  echo "attempts         : ${ATTEMPTS}"
  echo "elapsed          : ${ELAPSED}s (bound ${BOUND}s)"
  echo "refused          : ${SHAPE_ADMISSION} attempt(s) never actuated (${UAE} UnexpectedAdmissionError event(s))"
  echo "granted then lost: ${SHAPE_START} attempt(s) were given a partition no container could then start on"
  if [ "$OK" = 1 ]; then
    echo "converged        : yes"
  else
    echo "converged        : NO"
  fi
  echo "-------------------------------------------------------------"

  # The replacement holds its instance until it is released, and the section below reads its
  # baseline off an idle card.
  if [ -n "$WINNER" ]; then
    delpod "$WINNER"
  fi
  wait_card_idle || echo "[case-29] warning: the card still holds live instances after the reclaim-window section"
fi

# ---------------------------------------------------------------------------------------------------
# Section 3 — a terminated init container still charges the card its instance occupies.
#
# The device is scoped to the Pod's life, not the container's, so the per-profile capacity must
# keep charging an instance whose container is long gone -- and the room the node still reports
# must be room the card really has.
# ---------------------------------------------------------------------------------------------------
echo
echo "[case-29] === section 3: init container takes a ${MID} partition, then terminates ==="
PODPFX=gpustack-e2e-initterm
P="${PODPFX}-pod"
if [ -z "$FULLKEY" ]; then
  SKIPPED="${SKIPPED}the terminated-container section; "
  echo
  echo "== SECTION 3 — SKIPPED =="
  echo "The node advertises no 4-memory-slice key ('${MIDKEY:-<none>}') or no whole-card key" \
    "('${FULLKEY:-<none>}'). This section reads the whole-card key to see the init container's charge."
else
  wait_card_idle || echo "[case-29] warning: the card already holds live instances"
  BASE_FULL="$(node_key "$FULLKEY")"
  echo "[case-29] baseline ${FULLKEY}=${BASE_FULL:-<absent>}"

  TESTPODS+=("$P")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${P}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  initContainers:
    - name: warmup
      image: ${IMAGE}
      command: ["sh", "-c", "nvidia-smi -L || true; sleep 15"]
      resources:
        limits:
$(partition_reslines "$MIDKEY")
        requests:
$(partition_reslines "$MIDKEY")
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { cpu: "200m", memory: 256Mi }
        requests: { cpu: "200m", memory: 256Mi }
EOF

  prun=0
  if wait_running "$P" 60; then
    prun=1
    initstate="$(kubectl -n default get pod "$P" -o jsonpath='{.status.initContainerStatuses[0].state.terminated.reason}' 2>/dev/null)"
    if [ -n "$initstate" ]; then
      record PASS "the Pod runs on past its init container" "${P} Running with init container Terminated/${initstate}, card(s) $(pod_cards "$P"), profile '$(pod_profiles "$P")'"
    else
      record FAIL "the Pod runs on past its init container" "${P} is Running but its init container reports no terminated state — cannot exercise the terminated-container charge"
    fi
  else
    record FAIL "the Pod runs on past its init container" "${P} did not reach Running (phase=$(phase "$P"), held: $(held_reason "$P")) — an init-group accelerator claim must be admitted"
    record SKIP "the hardware instance outlives the container" "not attempted: the init-group Pod never reached Running"
    record SKIP "the terminated container's instance is still charged" "not attempted: the init-group Pod never reached Running"
    record SKIP "the reported free room is real" "not attempted: the init-group Pod never reached Running"
  fi

  # The hardware is still occupied, and the accounting says so.
  if [ "$prun" = 1 ]; then
    gi="$(node_gi_count)"
    if [ "${gi:-0}" -ge 1 ]; then
      record PASS "the hardware instance outlives the container" "${gi} live instance(s) on the card while the init container is long gone — the reclaimer destroys on Pod deletion, not container exit"
    else
      record FAIL "the hardware instance outlives the container" "the card reports ${gi:-0} live instance(s) — the instance was destroyed while its Pod is still alive"
    fi

    exp_full=$((BASE_FULL - FULL_CNT))
    cur_full=""
    for _ in $(seq 1 30); do
      cur_full="$(node_key "$FULLKEY")"
      [ "${cur_full:-x}" = "$exp_full" ] && break
      sleep 3
    done
    if [ "${cur_full:-x}" = "$exp_full" ]; then
      record PASS "the terminated container's instance is still charged" "${FULLKEY}: ${BASE_FULL} → ${cur_full} (want ${exp_full}) — the occupied card can no longer host a whole-card profile"
    else
      record FAIL "the terminated container's instance is still charged" "${FULLKEY}='${cur_full:-<absent>}', want ${exp_full} — the ledger dropped a terminated container's entry and is advertising room the card does not have"
    fi

    # Admit against the room the node still claims: it must be real room.
    echo
    echo "[case-29] === section 3: a further request against the reported free room ==="
    Q="${PODPFX}-next"
    mkpod "$Q" "$(partition_reslines "$MIDKEY")"
    if wait_running "$Q"; then
      uae="$(pod_unexpected_admission "$Q")"
      if [ -z "$uae" ]; then
        record PASS "the reported free room is real" "${Q} (${MID}) Running beside the init container's instance, 0 UnexpectedAdmissionError"
      else
        record FAIL "the reported free room is real" "${Q} Running but recorded ${uae} UnexpectedAdmissionError event(s) — the node advertised room the card could not place"
      fi
    else
      record FAIL "the reported free room is real" "${Q} did not reach Running (held: $(held_reason "$Q")) — the node's remaining per-profile room must be placeable"
    fi
  fi
fi

part_results "Two partition shapes at once, a replacement inside the reclaim window, and a terminated container's charge"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Three contracts are read here, and the failure text above names which"
  echo "one broke. A shape resolved to the wrong container's profile is a resolution bug; a replacement"
  echo "that never converges is a reclaim-window bug; a terminated container whose instance is no longer"
  echo "charged is an accounting bug. Diagnose:"
  echo "  kubectl -n default get pod ${PM} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  kubectl -n default get pod ${PS} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  kubectl -n default get pod ${P} -o jsonpath='{.metadata.annotations.${ANNO}}'; echo"
  echo "  kubectl get devices ${GPU_NODE} -o json | jq '.status.groups[].accelerators[] | {id, allocatedProfiles, remainingProfiles}'"
  echo "  ${MIG_NODE_SSH} sudo nvidia-smi mig -lgi"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=300 | grep -i reclaim"
  exit 1
fi
if [ -n "$SKIPPED" ]; then
  echo
  echo "SKIPPED section(s): ${SKIPPED}the run above does not cover them."
fi
echo "CASE 29 PASS"
