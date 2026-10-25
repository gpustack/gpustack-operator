#!/usr/bin/env bash
#
# _kvcache-ha-lib.sh — the shared queries of the HA KV cache cases: the leader's Lease, the member
#                     DaemonSet, the failover handoff and the backend-plus-Lease teardown.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table; those stay with the case that sources it.
#
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_kvcache-ha-lib.sh"
#
# Every helper takes what it needs as arguments and returns what the caller needs. Nothing here
# reaches into a case's variables, and nothing here prints the results table.

# kvha_wait_for <ns> <kind> <name> <path> <want> [secs] polls one jsonpath until it equals what is
# wanted, and prints the LAST value seen, so a timeout says what the object was doing.
kvha_wait_for() {
  local ns="$1" kind="$2" name="$3" path="$4" want="$5" secs="${6:-180}"
  local got="" i
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl -n "$ns" get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

# kvha_teardown <ns> <backend> <lease> deletes the backend and then the Lease it leaves behind: the
# Lease carries no owner reference, so it goes after the backend.
kvha_teardown() {
  local ns="$1" backend="$2" lease="$3"
  kubectl delete kvcachebackends.worker.gpustack.ai "$backend" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl wait --for=delete "kvcachebackends.worker.gpustack.ai/${backend}" --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$ns" delete leases.coordination.k8s.io "$lease" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# kvha_ready_leader_pod <ns> <leader-sel> prints the one leader Pod that is Ready, or nothing.
kvha_ready_leader_pod() {
  local ns="$1" leader_sel="$2"
  kubectl -n "$ns" get pod -l "$leader_sel" \
    -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | head -1
}

# kvha_lease_holder <ns> <lease> prints the Lease's holderIdentity, empty when it is absent.
kvha_lease_holder() {
  local ns="$1" lease="$2"
  kubectl -n "$ns" get leases.coordination.k8s.io "$lease" \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null
}

# kvha_lease_renew_time <ns> <lease> prints the Lease's renewTime, empty when it is absent.
kvha_lease_renew_time() {
  local ns="$1" lease="$2"
  kubectl -n "$ns" get leases.coordination.k8s.io "$lease" \
    -o jsonpath='{.spec.renewTime}' 2>/dev/null
}

# kvha_lease_transitions <ns> <lease> prints the Lease's handover count, zero when it is absent.
kvha_lease_transitions() {
  local ns="$1" lease="$2" t
  t="$(kubectl -n "$ns" get leases.coordination.k8s.io "$lease" -o jsonpath='{.spec.leaseTransitions}' 2>/dev/null)"
  echo "${t:-0}"
}

# kvha_holder_pod_uid <ns> <leader-sel> <holder> prints the UID of the Pod the holderIdentity
# names: the holder string is the serving process's rpc address, so it names a Pod by its IP.
kvha_holder_pod_uid() {
  local ns="$1" leader_sel="$2" holder="$3"
  local holder_ip pod_uid pod_ip deleting phase
  case "$holder" in
    \[*\]:*)
      holder_ip="${holder#\[}"
      holder_ip="${holder_ip%%\]*}"
      ;;
    *:*) holder_ip="${holder%:*}" ;;
    *) holder_ip="$holder" ;;
  esac
  while IFS='|' read -r pod_uid pod_ip deleting phase; do
    if [ "$phase" != "Running" ] || [ -n "$deleting" ] || [ -z "$pod_ip" ] \
      || [ "$pod_ip" != "$holder_ip" ]; then
      continue
    fi
    echo "$pod_uid"
    return 0
  done < <(kubectl -n "$ns" get pod -l "$leader_sel" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"|"}{.status.podIP}{"|"}{.metadata.deletionTimestamp}{"|"}{.status.phase}{"\n"}{end}' \
    2>/dev/null)
}

# kvha_pod_uid <ns> <pod> prints one Pod's UID, empty when the Pod cannot be read.
kvha_pod_uid() {
  local ns="$1" pod="$2"
  kubectl -n "$ns" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null
}

# kvha_member_snapshot <ns> <member-sel> prints the member Pods as name=UID:restarts, so a
# replacement Pod is caught by its UID.
kvha_member_snapshot() {
  local ns="$1" member_sel="$2"
  kubectl -n "$ns" get pod -l "$member_sel" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}:{.status.containerStatuses[0].restartCount}{"\n"}{end}' \
    2>/dev/null | sort
}

# kvha_members_mounted_sample <ns> <backend> prints one reading of the backend's own report, as
# phase|MembersMounted status/reason.
kvha_members_mounted_sample() {
  local ns="$1" backend="$2"
  kubectl -n "$ns" get kvcachebackends.worker.gpustack.ai "$backend" \
    -o jsonpath='{.status.phase}{"|"}{.status.conditions[?(@.type=="MembersMounted")].status}{"/"}{.status.conditions[?(@.type=="MembersMounted")].reason}{"\n"}' \
    2>/dev/null
}

# kvha_observe_handoff_snapshot <ns> <backend> <lease> <leader-sel> <traj-file> <delete-epoch>
# <old-ready-uid> <old-holder> watches the Lease until the handoff is observable and prints what it
# saw, as one pipe-separated record:
#
#   moved-at|moved-holder|moved-holder-pod-uid|move-evidence|handoff-observed|new-holder|
#   new-holder-pod-uid|new-ready|new-ready-uid|wait-secs
#
# The watch and the detection are one function because the detection is only meaningful between two
# reads of the same Lease: a changed holderIdentity or a changed renewTime timestamps the move
# directly, while a Ready holder is an upper bound. The deadline is the delete epoch plus 240s.
kvha_observe_handoff_snapshot() {
  local ns="$1" backend="$2" lease="$3" leader_sel="$4" traj_file="$5"
  local delete_epoch="$6" old_ready_uid="$7" old_holder="$8"
  local new_holder="" new_holder_pod_uid="" new_ready="" new_ready_uid="" new_renew_time=""
  local reused_holder_pod_uid="" reused_holder_renew_time=""
  local moved_at="" moved_holder="" moved_holder_pod_uid="" move_evidence="" handoff_observed=0
  local observed_at deadline wait_secs

  deadline=$((delete_epoch + 240))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    kvha_members_mounted_sample "$ns" "$backend" >>"$traj_file"
    new_holder="$(kvha_lease_holder "$ns" "$lease")"
    new_holder_pod_uid="$(kvha_holder_pod_uid "$ns" "$leader_sel" "$new_holder")"
    new_renew_time=""
    if [ -n "$new_holder_pod_uid" ] && [ "$new_holder_pod_uid" != "$old_ready_uid" ] \
      && [ "$new_holder" = "$old_holder" ]; then
      new_renew_time="$(kvha_lease_renew_time "$ns" "$lease")"
    fi
    new_ready="$(kvha_ready_leader_pod "$ns" "$leader_sel")"
    new_ready_uid=""
    if [ -n "$new_ready" ]; then
      new_ready_uid="$(kvha_pod_uid "$ns" "$new_ready")"
    fi

    observed_at="$(date +%s)"
    if [ -z "$moved_at" ] && [ -n "$new_holder_pod_uid" ] \
      && [ "$new_holder_pod_uid" != "$old_ready_uid" ]; then
      if [ "$new_holder" != "$old_holder" ]; then
        moved_at="$observed_at"
        moved_holder="$new_holder"
        moved_holder_pod_uid="$new_holder_pod_uid"
        move_evidence="holderIdentity changed"
      elif [ -n "$new_renew_time" ]; then
        if [ "$reused_holder_pod_uid" != "$new_holder_pod_uid" ] \
          || [ -z "$reused_holder_renew_time" ]; then
          reused_holder_pod_uid="$new_holder_pod_uid"
          reused_holder_renew_time="$new_renew_time"
        elif [ "$new_renew_time" != "$reused_holder_renew_time" ]; then
          moved_at="$observed_at"
          moved_holder="$new_holder"
          moved_holder_pod_uid="$new_holder_pod_uid"
          move_evidence="replacement renewed the reused holderIdentity"
        fi
      fi
    fi
    if [ -n "$new_holder_pod_uid" ] && [ "$new_holder_pod_uid" != "$old_ready_uid" ] \
      && [ "$new_holder_pod_uid" = "$new_ready_uid" ]; then
      handoff_observed=1
      if [ -z "$moved_at" ]; then
        moved_at="$observed_at"
        moved_holder="$new_holder"
        moved_holder_pod_uid="$new_holder_pod_uid"
        move_evidence="Ready replacement confirmed the reused holderIdentity"
      fi
    fi
    [ "$handoff_observed" = "1" ] && break
    sleep 2
  done
  wait_secs=$(( $(date +%s) - delete_epoch ))
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$moved_at" "$moved_holder" "$moved_holder_pod_uid" "$move_evidence" "$handoff_observed" \
    "$new_holder" "$new_holder_pod_uid" "$new_ready" "$new_ready_uid" "$wait_secs"
}

# kvha_failover_bound_verdict <moved-at> <delete-epoch> <bound-secs> <evidence> judges one failover
# measurement and prints the verdict as STATUS|object: PASS, FAIL or SKIP, and the text that goes
# into the row. The row itself stays with the case, which calls `record`. The Ready replacement is
# an upper bound rather than a direct reading of the move, so it SKIPs.
kvha_failover_bound_verdict() {
  local moved_at="$1" delete_epoch="$2" bound_secs="$3" evidence="$4"
  local moved_secs=$((moved_at - delete_epoch))
  case "$evidence" in
    "holderIdentity changed"|"replacement renewed the reused holderIdentity")
      if [ "$moved_secs" -le "$bound_secs" ]; then
        printf 'PASS|%ss <= %ss; %s\n' "$moved_secs" "$bound_secs" "$evidence"
      else
        printf 'FAIL|%ss > %ss -- %s; the election still works but no longer meets the failover budget\n' \
          "$moved_secs" "$bound_secs" "$evidence"
      fi
      ;;
    "Ready replacement confirmed the reused holderIdentity")
      printf 'SKIP|%ss after the failover trigger; the Ready replacement upper bound includes Pod scheduling, image pulling, and readiness probing\n' \
        "$moved_secs"
      ;;
    *)
      printf 'FAIL|unrecognized move evidence: %s\n' "${evidence:-<none>}"
      ;;
  esac
}

# kvha_resolve_rollout_images <default-start> <default-roll-to> prints the two images of a rollout
# pair, separated by a space: the start image, then the rollout target.
kvha_resolve_rollout_images() {
  printf '%s %s\n' "${E2E_MOONCAKE_IMAGE:-$1}" "${E2E_MOONCAKE_ROLLOUT_IMAGE:-$2}"
}
