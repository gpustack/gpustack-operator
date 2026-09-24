#!/usr/bin/env bash
#
# _rows-lib.sh — the one printer for the STATUS | CHECK | OBJECT table a case ends with.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace. The rows and the
# verdict stay with the case that sources it; this file only prints the rows.
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
