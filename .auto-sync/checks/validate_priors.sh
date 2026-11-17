#!/usr/bin/env bash
# .auto-sync/checks/validate_priors.sh - command.v1 verification gate for priors-sync.
#
# Verifies deterministic formatting of pkg/worker/scaling/priors/priors.yaml,
# SHA-256 and profile-count parity in pkg/worker/scaling/priors/records.md,
# Go unit tests for gen/priors and pkg/worker/scaling, and repository docs lint.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env
cd "${AUTOSYNC_CANDIDATE_ROOT}"

tmp_priors=""
cleanup() {
  if [[ -n "${tmp_priors}" ]]; then
    rm -f "${tmp_priors}"
  fi
  autosync::cleanup_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"
}
trap cleanup EXIT

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

priors_yaml="${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/scaling/priors/priors.yaml"
records_md="${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/scaling/priors/records.md"

if [[ ! -f "${priors_yaml}" || ! -f "${records_md}" ]]; then
  printf 'FAIL: missing priors.yaml or records.md under pkg/worker/scaling/priors/\n' >&2
  exit 1
fi

# 1. Verify deterministic formatting of pkg/worker/scaling/priors/priors.yaml.
tmp_priors="$(mktemp "${TMPDIR:-/tmp}/autosync-priors-fmt.XXXXXXXX.yaml")"
cp -f "${priors_yaml}" "${tmp_priors}"
go run ./gen/priors --catalog "${tmp_priors}"
if ! cmp -s "${tmp_priors}" "${priors_yaml}"; then
  printf 'FAIL: pkg/worker/scaling/priors/priors.yaml is not deterministically formatted (run go run ./gen/priors)\n' >&2
  exit 1
fi
rm -f "${tmp_priors}"
tmp_priors=""

# 2. Verify SHA-256 provenance ledger and aggregated/disaggregated profile count parity.
report_json=""
if [[ -n "${AUTOSYNC_STAGE_DIR:-}" && -f "${AUTOSYNC_STAGE_DIR}/priors-preflight-report.json" ]]; then
  report_json="${AUTOSYNC_STAGE_DIR}/priors-preflight-report.json"
fi

python3 - "${priors_yaml}" "${records_md}" "${report_json}" <<'PY'
import json
import re
import sys

priors_path, records_path, report_path = sys.argv[1:4]

actual_agg = 0
actual_disagg = 0
with open(priors_path, "r", encoding="utf-8") as f:
    for line in f:
        m = re.match(r"^\s*-\s+mode:\s*(\S+)\s*$", line)
        if m:
            mode = m.group(1)
            if mode == "aggregated":
                actual_agg += 1
            elif mode == "disaggregated":
                actual_disagg += 1
            else:
                raise SystemExit(f"FAIL: unexpected profile mode in priors.yaml: {mode}")

with open(records_path, "r", encoding="utf-8") as f:
    records_txt = f.read()

sec_m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", records_txt, re.DOTALL)
if not sec_m:
    raise SystemExit("FAIL: missing '## Active Sources' section in records.md")
active_sec = sec_m.group(1)

avail_m = re.search(r"SHA-256\s+`([0-9a-f]{64})`", active_sec)
records_avail_sha = avail_m.group(1) if avail_m else ""

table_rows = {}
sum_agg = 0
sum_disagg = 0
saw_header = False

for line in active_sec.splitlines():
    stripped = line.strip()
    if not stripped.startswith("|"):
        continue
    cells = [c.strip() for c in stripped.strip("|").split("|")]
    if len(cells) < 5:
        continue
    if re.fullmatch(r"[-: ]+", cells[0]):
        continue
    if cells[0].lower().replace("`", "") == "query id":
        saw_header = True
        continue
    if not saw_header:
        continue
    qid = cells[0].strip("`").strip()
    if not qid:
        raise SystemExit("FAIL: empty Query ID in records.md Active Sources table")
    if qid in table_rows:
        raise SystemExit(f"FAIL: duplicate Query ID in records.md Active Sources table: {qid}")
    try:
        rows = int(cells[1])
        agg = int(cells[2])
        disagg = int(cells[3])
    except ValueError as exc:
        raise SystemExit(f"FAIL: invalid integer counts for Query ID {qid} in records.md: {exc}")
    if rows < 0 or agg < 0 or disagg < 0:
        raise SystemExit(f"FAIL: negative counts for Query ID {qid} in records.md")
    sha = cells[4].strip("`").strip()
    if not re.fullmatch(r"[0-9a-f]{64}", sha):
        raise SystemExit(f"FAIL: invalid 64-hex SHA-256 for Query ID {qid} in records.md: {sha!r}")
    table_rows[qid] = {"rows": rows, "agg": agg, "disagg": disagg, "sha256": sha}
    sum_agg += agg
    sum_disagg += disagg

if not table_rows:
    raise SystemExit("FAIL: no active Query ID rows parsed from records.md Active Sources table")

if sum_agg != actual_agg or sum_disagg != actual_disagg:
    raise SystemExit(
        f"FAIL: records.md profile count mismatch: table has {sum_agg} aggregated / {sum_disagg} disaggregated, "
        f"priors.yaml has {actual_agg} aggregated / {actual_disagg} disaggregated"
    )

if report_path:
    with open(report_path, "r", encoding="utf-8") as f:
        report = json.load(f)
    if report.get("enforce_snapshot_digests") is True:
        expected_avail = (report.get("availability") or {}).get("sha256", "")
        if expected_avail:
            if not records_avail_sha:
                raise SystemExit("FAIL: records.md Active Sources missing availability SHA-256")
            if records_avail_sha != expected_avail:
                raise SystemExit(
                    f"FAIL: records.md availability SHA-256 ({records_avail_sha}) != "
                    f"staged snapshot ({expected_avail})"
                )
        staged_queries = {q["query_id"]: q for q in report.get("queries", [])}
        missing = sorted(set(staged_queries.keys()) - set(table_rows.keys()))
        extra = sorted(set(table_rows.keys()) - set(staged_queries.keys()))
        if missing:
            raise SystemExit(f"FAIL: records.md Active Sources missing staged query IDs: {missing}")
        if extra:
            raise SystemExit(f"FAIL: records.md Active Sources has unlisted query IDs: {extra}")
        for qid, qinfo in sorted(staged_queries.items()):
            rec = table_rows[qid]
            if rec["sha256"] != qinfo["sha256"]:
                raise SystemExit(
                    f"FAIL: records.md SHA-256 mismatch for {qid}: "
                    f"{rec['sha256']} != {qinfo['sha256']}"
                )
            if rec["rows"] != qinfo["rows"]:
                raise SystemExit(
                    f"FAIL: records.md row count mismatch for {qid}: "
                    f"{rec['rows']} != {qinfo['rows']}"
                )
PY

if [[ "${AUTOSYNC_FAST_SELFTEST:-false}" == "true" ]]; then
  exit 0
fi

# 3. Run Go unit tests for gen/priors and pkg/worker/scaling.
go test ./gen/priors/... ./pkg/worker/scaling/...

# 4. Run repository docs lint (full make lint docs when hugo + make are available,
#    otherwise direct docs/specs/crossrefs/skills gates).
if command -v hugo >/dev/null 2>&1 && command -v make >/dev/null 2>&1; then
  make lint docs
else
  docs_scripts="${AUTOSYNC_CANDIDATE_ROOT}/.agents/skills/gpustack-operator-docs/scripts"
  bash "${docs_scripts}/check-docs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${docs_scripts}/check-specs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${docs_scripts}/check-crossrefs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/skills.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
fi
