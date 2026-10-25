#!/usr/bin/env bash
#
# _rows-lib.sh — the helpers a case builds on: the one printer for the STATUS | CHECK | OBJECT table
#                 a case ends with, the jsonpath poller, the Settings store and the release of a KV
#                 cache Binding the operator is holding.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace. The rows and the
# verdict stay with the case that sources it; this file prints the rows and provides helpers.
#
# A row is "STATUS|CHECK|OBJECT", as every case's `record` writes it, and only its first two "|" are
# separators. A CHECK name is several words, so splitting on whitespace moves most of it into OBJECT.
# An OBJECT is free text -- a condition message, a status|reason|message reading, a trajectory -- and
# splitting on every "|" cuts it at the first one it carries, so the part a FAIL row needs is exactly
# the part that goes missing.
#
# A case uses it as:
#
#     record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"
#     ...
#     print_rows
#     [ "$FAILS" -eq 0 ] || { echo "[case-N] ${FAILS} check(s) FAILED"; exit 1; }

# The kubectl shim every case routes its kubectl calls through. Against a remote API endpoint a
# read can fail on transport alone, and a check that takes such a failure for an answer reports a
# verdict about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
if [ -n "$E2E_SHIM_DIR" ]; then
  PATH="$E2E_SHIM_DIR:$PATH"
fi

# The counters and the row list a case starts from. A case that counts something else keeps its own
# record(), defined after it sources this file.
FAILS=0
SKIPS=0
ROWS=()

# record STATUS CHECK OBJECT appends one row and counts it. A FAIL raises the failure count and a
# SKIP raises the skip count.
record() {
  ROWS+=("$1|$2|$3")
  [ "$1" = FAIL ] && FAILS=$((FAILS + 1))
  [ "$1" = SKIP ] && SKIPS=$((SKIPS + 1))
  return 0
}

# results prints the rows and returns the verdict: 0 when nothing failed, 1 otherwise. A case that
# wants another verdict overrides this, after it sources this file.
results() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID:-unknown}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID:-unknown}] all checks passed"
  return 0
}

# print_rows prints a blank line, the header and every row of ROWS. The expansion is guarded because
# bash 3.2 under `set -u` treats an empty array as unbound, and a case that fails before its first
# row would abort here instead of printing the failure that stopped it.
print_rows() {
  local r rest
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do
    rest="${r#*|}"
    printf '%s | %s | %s\n' "${r%%|*}" "${rest%%|*}" "${rest#*|}"
  done
}

# wait_for <kind> <name> <path> <want> [secs] [ns] polls one jsonpath until it reads what is wanted,
# and prints the LAST value seen, so a timeout says what the object was doing rather than only that
# it timed out.
#
# The namespace comes last and is optional: a cluster-scoped object is waited for without one, and
# that is the common case rather than the edge one. The poll runs every three seconds until the
# budget is spent.
wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-120}" ns_args=()
  [ -n "${6:-}" ] && ns_args=(-n "$6")
  local got="" i
  # ${ns_args[@]+...} rather than a bare "${ns_args[@]}": under set -u, bash 3.2 - macOS's stock
  # shell, which this family is usually launched from - treats expanding an EMPTY array as an unbound
  # variable and aborts the case mid-run. Measured both ways on bash 3.2.57.
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl ${ns_args[@]+"${ns_args[@]}"} get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

# kvi_release_binding_finalizer <ns> <binding> waits for a KVCachePoolBinding the operator is
# holding, and when it is still held after 60s it reports what the operator says about it and forces
# the finalizer off.
#
# A domain that still holds objects makes the master refuse to drop its quota, and the operator's
# finalizer is right to wait. Forcing it off bypasses the operator's own release path, which removes
# it only once the master's ledger entry for that tenant is gone, so a tenant can remain registered
# in a master with no Binding owning it. It is still the right trade in a cleanup - a Binding left
# held forever poisons every later run of the family - but it is not clean, and a forced action that
# prints nothing reads as a clean one.
#
# The tag on the two lines it prints is the case that is cleaning up, so a reader of a suite log
# knows which run left the binding held.
kvi_release_binding_finalizer() {
  local ns="$1" binding="$2" left="" tag
  tag="${0##*/}"
  tag="${tag%.sh}"
  for _ in $(seq 1 20); do
    left="$(kubectl -n "$ns" get kvcachepoolbindings.worker.gpustack.ai "$binding" -o name 2>/dev/null)"
    [ -z "$left" ] && break
    sleep 3
  done
  [ -z "$left" ] && return 0
  echo "[${tag}] the binding ${binding} is still held after 60s; what the operator says about it:"
  kubectl -n "$ns" get kvcachepoolbindings.worker.gpustack.ai "$binding" \
    -o 'jsonpath={range .status.conditions[*]}{.type}={.status}({.reason}) {.message}{"\n"}{end}' 2>/dev/null \
    | sed 's/^/    /'
  echo "[${tag}] forcing the finalizer off; a tenant may remain registered in the master."
  kubectl -n "$ns" patch kvcachepoolbindings.worker.gpustack.ai "$binding" \
    --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
  return 0
}

# The Settings store: the gpustack-settings Secret in the system namespace, where a Setting's value
# lives. The helpers below read and write it the way a seed from the environment does, bypassing
# admission.

# setting_get KEY prints the value KEY carries, empty when the store does not carry it.
setting_get() {
  kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.$1}" 2>/dev/null | base64 -d 2>/dev/null
}

# setting_set KEY VALUE writes VALUE into the store. An empty VALUE is written as an empty value and
# not as an absent key, because a key that is present but empty is what the caller asked for.
setting_set() {
  local b64
  b64="$(printf '%s' "$2" | base64 | tr -d '\n')"
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"$1\":\"${b64}\"}}" >/dev/null
}

# setting_unset KEY removes KEY from the store, so the Setting reads its default again.
setting_unset() {
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"$1\":null}}" >/dev/null
}

# settings_settle waits out the worker's thirty-second Settings read cache, so its next read of a
# Setting written through the store sees the new value. The plugin needs no wait: it reads its
# NodeModelStore's spec, which the worker rewrites on the Secret's change.
settings_settle() { sleep 35; }

# The keys settings_save remembered, the values they carried, and whether each was present at all.
SETTINGS_SAVED_KEYS=()
SETTINGS_SAVED_VALUES=()
SETTINGS_SAVED_PRESENT=()

# setting_present KEY says whether the store carries KEY at all. A key the store carries with an
# empty value and a key the store does not carry both read as empty through setting_get; telling
# them apart is what lets settings_restore put back an empty value as an empty value and an absent
# key as an absent key.
setting_present() {
  kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o json 2>/dev/null \
    | jq -e --arg k "$1" '.data[$k] != null' >/dev/null 2>&1
}

# settings_save KEY... remembers the value each KEY carries in the store and whether it carried
# one, so settings_restore can put it back exactly: absent restores as absent, empty as empty,
# and a value as itself.
settings_save() {
  local key value present i
  for key in "$@"; do
    value="$(setting_get "$key")"
    if setting_present "$key"; then present=1; else present=0; fi
    i=0
    while [ "$i" -lt "${#SETTINGS_SAVED_KEYS[@]}" ] && [ "${SETTINGS_SAVED_KEYS[$i]}" != "$key" ]; do
      i=$((i + 1))
    done
    if [ "$i" -lt "${#SETTINGS_SAVED_KEYS[@]}" ]; then
      SETTINGS_SAVED_VALUES[$i]="$value"
      SETTINGS_SAVED_PRESENT[$i]="$present"
    else
      SETTINGS_SAVED_KEYS+=("$key")
      SETTINGS_SAVED_VALUES+=("$value")
      SETTINGS_SAVED_PRESENT+=("$present")
    fi
  done
}

# settings_restore puts back every key settings_save remembered, and waits out the worker's read
# cache - but only when a key actually changed. A key that never existed restores as absent; a key
# that existed with an empty value restores as that empty value, because a present-but-empty key
# and a deleted key read differently downstream.
settings_restore() {
  local key current present i changed=0
  i=0
  while [ "$i" -lt "${#SETTINGS_SAVED_KEYS[@]}" ]; do
    key="${SETTINGS_SAVED_KEYS[$i]}"
    present="${SETTINGS_SAVED_PRESENT[$i]}"
    if setting_present "$key"; then current=1; else current=0; fi
    if [ "$current" != "$present" ] \
      || { [ "$present" -eq 1 ] && [ "$(setting_get "$key")" != "${SETTINGS_SAVED_VALUES[$i]}" ]; }; then
      changed=1
      if [ "$present" -eq 0 ]; then
        setting_unset "$key"
      else
        setting_set "$key" "${SETTINGS_SAVED_VALUES[$i]}"
      fi
    fi
    i=$((i + 1))
  done
  [ "$changed" -eq 1 ] && settings_settle
  return 0
}
