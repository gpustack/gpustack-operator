#!/usr/bin/env bash
# .auto-sync/checks/validate_priors.sh - command.v1 verification gate for priors-sync.
#
# Verifies deterministic formatting of pkg/worker/scaling/priors/priors.yaml,
# SHA-256 and profile-count parity in pkg/worker/scaling/priors/records.md,
# Go unit tests for gen/priors and pkg/worker/scaling, and repository docs lint.
set -euo pipefail

SCRIPT_DIR="$(CDPATH="" cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# Inline bootstrap: Loopwright copies gate.script alone to workDir/gate-script.sh.
if [[ ! -f "${SCRIPT_DIR}/lib.sh" && -f "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks/lib.sh" ]]; then
  SCRIPT_DIR="$(CDPATH="" cd -P -- "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks" && pwd -P)"
fi
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

profile_counts="$(autosync::count_priors_profiles "${priors_yaml}")"
actual_agg="${profile_counts%% *}"
actual_disagg="${profile_counts##* }"

python3 - "${records_md}" "${report_json}" "${actual_agg}" "${actual_disagg}" <<'PY'
import json
import re
import sys

records_path, report_path, actual_agg_str, actual_disagg_str = sys.argv[1:5]

actual_agg = int(actual_agg_str)
actual_disagg = int(actual_disagg_str)

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

# 2b. Verify .auto-sync/proposals/priors.json schema and availability SHA parity.
CAND_PROPOSAL="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/priors.json"
python3 - "${CAND_PROPOSAL}" "${records_md}" <<'PY'
import json
import os
import re
import sys

prop_p, records_p = sys.argv[1:3]
if not os.path.isfile(prop_p):
    raise SystemExit(f"FAIL: missing proposal artifact: {prop_p}")
with open(prop_p, "r", encoding="utf-8") as f:
    prop = json.load(f)

if not isinstance(prop, dict) or prop.get("schema_version") != 1:
    raise SystemExit("FAIL: proposals/priors.json must have schema_version == 1")
if not isinstance(prop.get("summary"), str) or not prop["summary"].strip():
    raise SystemExit("FAIL: proposals/priors.json summary must be a non-empty string")
if not isinstance(prop.get("subscriptions"), list):
    raise SystemExit("FAIL: proposals/priors.json subscriptions must be a JSON array")
versions = prop.get("versions")
if not isinstance(versions, dict):
    raise SystemExit("FAIL: proposals/priors.json versions must be a JSON object")

if "inferencex-repo" not in versions or not isinstance(versions["inferencex-repo"], str):
    raise SystemExit("FAIL: proposals/priors.json versions.inferencex-repo must be a string")

avail_v = versions.get("inferencex-availability")
if not isinstance(avail_v, str) or not avail_v.strip():
    raise SystemExit("FAIL: proposals/priors.json versions.inferencex-availability must be a non-empty string")

with open(records_p, "r", encoding="utf-8") as f:
    records_txt = f.read()
sec_m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", records_txt, re.DOTALL)
if sec_m:
    avail_m = re.search(r"SHA-256\s+`([0-9a-f]{64})`", sec_m.group(1))
    if avail_m and avail_v != avail_m.group(1):
        raise SystemExit(
            f"FAIL: proposals/priors.json versions.inferencex-availability ({avail_v}) != records.md ({avail_m.group(1)})"
        )
PY

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

if [[ "${AUTOSYNC_FAST_SELFTEST:-false}" == "true" ]]; then
  exit 0
fi

# 3. Run Go unit tests for gen/priors and pkg/worker/scaling.
autosync::go_test ./gen/priors/... ./pkg/worker/scaling/...

# 4. Run repository docs lint (full make lint docs when hugo + make are available,
#    otherwise direct docs/specs/crossrefs/skills gates).
if command -v hugo >/dev/null 2>&1 && command -v make >/dev/null 2>&1; then
  lint_env=("AUTOSYNC_IN_VALIDATOR=true")
  if [[ -z "${UV_CACHE_DIR:-}" || ! -d "${UV_CACHE_DIR}" ]] || ! command -v uv >/dev/null 2>&1; then
    lint_env+=("GPUSTACK_SKIP_PYTHON_LINT=true")
  fi
  env "${lint_env[@]}" make lint docs
else
  docs_scripts="${AUTOSYNC_CANDIDATE_ROOT}/.agents/skills/gpustack-operator-docs/scripts"
  bash "${docs_scripts}/check-docs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${docs_scripts}/check-specs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${docs_scripts}/check-crossrefs.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
  bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/skills.sh" "${AUTOSYNC_CANDIDATE_ROOT}"
fi
