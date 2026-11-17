#!/usr/bin/env bash
# .auto-sync/scripts/preflight-priors.sh - host-side preflight for priors-sync.
#
# Discovers SemiAnalysisAI/InferenceX commit + availability/benchmark JSON snapshots,
# stages go run ./gen/priors merges onto a detached source_revision when qualifying
# changes occur, and writes priors-preflight-report.json + upstream-records.json.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

stage_dir="$(autosync::preflight::init_stage "priors" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
repo_root="$(autosync::preflight::repo_root)"
base_sha="$(git -C "${repo_root}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${stage_dir}" \
  "pkg/worker/scaling/priors/priors.yaml" \
  "pkg/worker/scaling/priors/records.md"
autosync::preflight::warm_caches "${stage_dir}" --sbin --go

snapshots_dir="${stage_dir}/snapshots"
rm -rf "${snapshots_dir}"
mkdir -p "${snapshots_dir}"

inferencex_repo_url="https://github.com/SemiAnalysisAI/InferenceX.git"
availability_url="https://inferencex.semianalysis.com/api/v1/availability"
capture_date="$(date -u +%Y-%m-%d)"

inferencex_commit=""
if [[ -n "${AUTOSYNC_PRIORS_FIXTURE_DIR:-}" ]]; then
  fixture_dir="${AUTOSYNC_PRIORS_FIXTURE_DIR}"
  if [[ ! -d "${fixture_dir}" ]]; then
    printf 'FAIL: AUTOSYNC_PRIORS_FIXTURE_DIR does not exist: %s\n' "${fixture_dir}" >&2
    exit 1
  fi
  if [[ -f "${fixture_dir}/inferencex-head.txt" ]]; then
    inferencex_commit="$(tr -d '[:space:]' < "${fixture_dir}/inferencex-head.txt")"
  else
    inferencex_commit="${AUTOSYNC_PRIORS_FIXTURE_HEAD:-0000000000000000000000000000000000000000}"
  fi
  if [[ ! -f "${fixture_dir}/availability.json" ]]; then
    printf 'FAIL: missing fixture availability.json in %s\n' "${fixture_dir}" >&2
    exit 1
  fi
  cp -f "${fixture_dir}/availability.json" "${snapshots_dir}/availability.json"
else
  ls_remote_out="$(git ls-remote "${inferencex_repo_url}" HEAD)"
  inferencex_commit="${ls_remote_out%%[[:space:]]*}"
  if [[ ! "${inferencex_commit}" =~ ^[0-9a-f]{40}$ ]]; then
    printf 'FAIL: unable to resolve SemiAnalysisAI/InferenceX HEAD commit: %s\n' "${ls_remote_out}" >&2
    exit 1
  fi
  curl -fsSL --compressed "${availability_url}" -o "${snapshots_dir}/availability.json"
fi

query_ids_file="${stage_dir}/query-ids.txt"
python3 - "${snapshots_dir}/availability.json" "${query_ids_file}" <<'PY'
import json
import re
import sys

avail_path, out_path = sys.argv[1:3]
with open(avail_path, "r", encoding="utf-8") as f:
    data = json.load(f)

seen = set()
query_ids = []


def add_qid(val):
    if not isinstance(val, str):
        return
    qid = val.strip()
    if not qid or "/" in qid or "\\" in qid or ".." in qid:
        raise SystemExit(f"FAIL: unsafe or empty query ID in availability.json: {val!r}")
    if qid not in seen:
        seen.add(qid)
        query_ids.append(qid)


def extract(node):
    if isinstance(node, list):
        for item in node:
            if isinstance(item, str):
                add_qid(item)
            elif isinstance(item, dict):
                for key in ("query_id", "query_model", "model", "id", "name"):
                    if isinstance(item.get(key), str) and item[key].strip():
                        add_qid(item[key])
                        break
    elif isinstance(node, dict):
        for wrapper_key in ("models", "available", "data", "items", "results"):
            if wrapper_key in node and isinstance(node[wrapper_key], (list, dict)):
                extract(node[wrapper_key])
                return
        for key in sorted(node.keys()):
            if re.match(r"^[A-Za-z0-9._-]+$", key):
                add_qid(key)


extract(data)
if not query_ids:
    raise SystemExit("FAIL: availability.json yielded 0 active query IDs")

with open(out_path, "w", encoding="utf-8") as f:
    for qid in query_ids:
        f.write(qid + "\n")
PY

while IFS= read -r qid || [[ -n "${qid}" ]]; do
  [[ -n "${qid}" ]] || continue
  if [[ -n "${AUTOSYNC_PRIORS_FIXTURE_DIR:-}" ]]; then
    src_snap=""
    if [[ -f "${AUTOSYNC_PRIORS_FIXTURE_DIR}/${qid}.json" ]]; then
      src_snap="${AUTOSYNC_PRIORS_FIXTURE_DIR}/${qid}.json"
    elif [[ -f "${AUTOSYNC_PRIORS_FIXTURE_DIR}/snapshots/${qid}.json" ]]; then
      src_snap="${AUTOSYNC_PRIORS_FIXTURE_DIR}/snapshots/${qid}.json"
    else
      printf 'FAIL: missing fixture snapshot for query ID %s in %s\n' "${qid}" "${AUTOSYNC_PRIORS_FIXTURE_DIR}" >&2
      exit 1
    fi
    cp -f "${src_snap}" "${snapshots_dir}/${qid}.json"
  else
    encoded_qid="$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${qid}")"
    curl -fsSL --compressed "https://inferencex.semianalysis.com/api/v1/benchmarks?model=${encoded_qid}" \
      -o "${snapshots_dir}/${qid}.json"
  fi
done < "${query_ids_file}"
rm -f "${query_ids_file}"

analysis_json="${stage_dir}/analysis.json"
python3 - "${repo_root}/pkg/worker/scaling/priors/records.md" \
  "${repo_root}/.auto-sync/proposals/priors.json" \
  "${snapshots_dir}" \
  "${analysis_json}" <<'PY'
import hashlib
import json
import os
import re
import sys
import urllib.parse

records_path, proposal_path, snapshots_dir, out_path = sys.argv[1:5]

with open(records_path, "r", encoding="utf-8") as f:
    records_txt = f.read()

m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", records_txt, re.DOTALL)
if not m:
    raise SystemExit(f"FAIL: missing '## Active Sources' section in {records_path}")
active_sec = m.group(1)

avail_m = re.search(r"SHA-256\s+`([0-9a-f]{64})`", active_sec)
baseline_avail_sha = avail_m.group(1) if avail_m else ""

baseline_queries = {}
for line in active_sec.splitlines():
    stripped = line.strip()
    if not stripped.startswith("|"):
        continue
    cells = [c.strip().strip("`") for c in stripped.strip("|").split("|")]
    if len(cells) >= 5 and re.fullmatch(r"[0-9a-f]{64}", cells[4]):
        baseline_queries[cells[0]] = {
            "rows": int(cells[1]),
            "sha256": cells[4],
        }

baseline_repo_commit = ""
if os.path.isfile(proposal_path):
    try:
        with open(proposal_path, "r", encoding="utf-8") as f:
            prop = json.load(f)
        baseline_repo_commit = str((prop.get("versions") or {}).get("inferencex-repo") or "")
    except Exception:
        baseline_repo_commit = ""

avail_file = os.path.join(snapshots_dir, "availability.json")
with open(avail_file, "rb") as f:
    avail_sha = hashlib.sha256(f.read()).hexdigest()

queries = []
active_ids = set()
for name in sorted(os.listdir(snapshots_dir)):
    if name == "availability.json" or not name.endswith(".json"):
        continue
    qid = name[:-5]
    active_ids.add(qid)
    fp = os.path.join(snapshots_dir, name)
    with open(fp, "rb") as f:
        raw = f.read()
    sha = hashlib.sha256(raw).hexdigest()
    parsed = json.loads(raw.decode("utf-8"))
    if isinstance(parsed, list):
        row_count = len(parsed)
    elif isinstance(parsed, dict):
        row_count = len(parsed.get("rows") or parsed.get("data") or [])
    else:
        raise SystemExit(f"FAIL: unexpected benchmark JSON shape in {fp}")
    base_q = baseline_queries.get(qid)
    changed = base_q is None or base_q["sha256"] != sha
    queries.append({
        "query_id": qid,
        "url": f"https://inferencex.semianalysis.com/api/v1/benchmarks?model={urllib.parse.quote(qid, safe='')}",
        "rows": row_count,
        "sha256": sha,
        "baseline_sha256": base_q["sha256"] if base_q else "",
        "changed": changed,
        "snapshot_path": f"snapshots/{name}",
    })

removed_query_ids = sorted(set(baseline_queries.keys()) - active_ids)
avail_changed = avail_sha != baseline_avail_sha
snapshots_changed = avail_changed or any(q["changed"] for q in queries) or len(removed_query_ids) > 0

with open(out_path, "w", encoding="utf-8") as f:
    json.dump({
        "baseline_repo_commit": baseline_repo_commit,
        "availability": {
            "url": "https://inferencex.semianalysis.com/api/v1/availability",
            "sha256": avail_sha,
            "baseline_sha256": baseline_avail_sha,
            "changed": avail_changed,
            "snapshot_path": "snapshots/availability.json",
        },
        "queries": queries,
        "removed_query_ids": removed_query_ids,
        "snapshots_changed": snapshots_changed,
    }, f, indent=2)
PY

snapshots_changed="$(python3 -c 'import json,sys; print("true" if json.load(open(sys.argv[1]))["snapshots_changed"] else "false")' "${analysis_json}")"
source_revision="${base_sha}"
merge_attempted=false
catalog_changed=false

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/autosync-priors-work.XXXXXXXX")"
cleanup_work() {
  rm -rf "${work_dir}"
}
trap cleanup_work EXIT

tmp_catalog="${work_dir}/priors.yaml"
cp -f "${repo_root}/pkg/worker/scaling/priors/priors.yaml" "${tmp_catalog}"

if [[ "${snapshots_changed}" == "true" && "${AUTOSYNC_SKIP_SOURCE_REVISION:-false}" != "true" ]]; then
  merge_attempted=true
  effective_gocache="${GOCACHE:-${stage_dir}/go-cache}"
  effective_gomodcache="${GOMODCACHE:-${stage_dir}/go-mod}"
  effective_goproxy="${GOPROXY:-off}"

  changed_snaps_file="${work_dir}/changed-snaps.txt"
  python3 - "${analysis_json}" "${stage_dir}" "${changed_snaps_file}" <<'PY'
import json
import os
import sys

analysis_path, stage_dir, out_path = sys.argv[1:4]
with open(analysis_path, "r", encoding="utf-8") as f:
    analysis = json.load(f)
with open(out_path, "w", encoding="utf-8") as f:
    for q in analysis.get("queries", []):
        if q.get("changed"):
            f.write(os.path.join(stage_dir, q["snapshot_path"]) + "\n")
PY

  while IFS= read -r snap_file || [[ -n "${snap_file}" ]]; do
    [[ -n "${snap_file}" ]] || continue
    (
      cd "${repo_root}"
      GOCACHE="${effective_gocache}" \
      GOMODCACHE="${effective_gomodcache}" \
      GOPROXY="${effective_goproxy}" \
      GOTOOLCHAIN="${GOTOOLCHAIN:-local}" \
        go run ./gen/priors --catalog "${tmp_catalog}" --merge "${snap_file}"
    )
  done < "${changed_snaps_file}"

  (
    cd "${repo_root}"
    GOCACHE="${effective_gocache}" \
    GOMODCACHE="${effective_gomodcache}" \
    GOPROXY="${effective_goproxy}" \
    GOTOOLCHAIN="${GOTOOLCHAIN:-local}" \
      go run ./gen/priors --catalog "${tmp_catalog}"
  )

  if ! cmp -s "${tmp_catalog}" "${repo_root}/pkg/worker/scaling/priors/priors.yaml"; then
    catalog_changed=true
    restore_catalog_on_err() {
      cp -f "${stage_dir}/baseline/pkg/worker/scaling/priors/priors.yaml" \
        "${repo_root}/pkg/worker/scaling/priors/priors.yaml" || true
    }
    trap 'restore_catalog_on_err; cleanup_work' EXIT
    cp -f "${tmp_catalog}" "${repo_root}/pkg/worker/scaling/priors/priors.yaml"
    source_revision="$(
      autosync::preflight::commit_detached_source_revision \
        "${repo_root}" "${base_sha}" \
        "chore(priors): stage InferenceX catalog merge" \
        "pkg/worker/scaling/priors/priors.yaml"
    )"
    trap cleanup_work EXIT
  fi
fi

report_json="${stage_dir}/priors-preflight-report.json"
upstream_records_json="${stage_dir}/upstream-records.json"

python3 - "${analysis_json}" "${tmp_catalog}" "${report_json}" "${upstream_records_json}" \
  "${capture_date}" "${base_sha}" "${source_revision}" "${inferencex_commit}" \
  "${inferencex_repo_url}" "${merge_attempted}" "${catalog_changed}" <<'PY'
import json
import re
import sys

(
    analysis_path,
    catalog_path,
    report_path,
    records_out_path,
    capture_date,
    base_sha,
    source_revision,
    inferencex_commit,
    inferencex_repo_url,
    merge_attempted_str,
    catalog_changed_str,
) = sys.argv[1:12]

with open(analysis_path, "r", encoding="utf-8") as f:
    analysis = json.load(f)

agg_count = 0
disagg_count = 0
with open(catalog_path, "r", encoding="utf-8") as f:
    for line in f:
        m = re.match(r"^\s*-\s+mode:\s*(aggregated|disaggregated)\s*$", line)
        if m:
            if m.group(1) == "aggregated":
                agg_count += 1
            else:
                disagg_count += 1

merge_attempted = merge_attempted_str == "true"
catalog_changed = catalog_changed_str == "true"
snapshots_changed = bool(analysis["snapshots_changed"])
overall_changed = snapshots_changed or (source_revision != base_sha)

report = {
    "schema_version": 1,
    "capture_date": capture_date,
    "base_sha": base_sha,
    "source_revision": source_revision,
    "changed": overall_changed,
    "enforce_snapshot_digests": True,
    "inferencex_repo": {
        "url": inferencex_repo_url,
        "commit": inferencex_commit,
        "changed": inferencex_commit != analysis["baseline_repo_commit"],
    },
    "availability": analysis["availability"],
    "queries": analysis["queries"],
    "removed_query_ids": analysis["removed_query_ids"],
    "catalog_merge": {
        "attempted": merge_attempted,
        "catalog_changed": catalog_changed,
        "aggregated_profiles": agg_count,
        "disaggregated_profiles": disagg_count,
        "total_profiles": agg_count + disagg_count,
    },
}

with open(report_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")

avail_sha = analysis["availability"]["sha256"]
repo_status = "available" if (overall_changed and inferencex_commit != analysis["baseline_repo_commit"]) else "up_to_date"
avail_status = "available" if overall_changed else "up_to_date"

upstream_records = [
    {
        "upstreamId": "inferencex-repo",
        "status": repo_status,
        "version": inferencex_commit,
        "reason": "InferenceX repository HEAD updated alongside snapshot changes"
        if repo_status == "available"
        else "No qualifying snapshot changes triggered by InferenceX HEAD",
    },
    {
        "upstreamId": "inferencex-availability",
        "status": avail_status,
        "version": avail_sha,
        "reason": "InferenceX availability or benchmark snapshots changed relative to records.md"
        if avail_status == "available"
        else "All InferenceX availability and benchmark snapshot SHA-256 digests match records.md",
    },
]

with open(records_out_path, "w", encoding="utf-8") as f:
    json.dump(upstream_records, f, indent=2)
    f.write("\n")
PY

rm -f "${analysis_json}"

compact_records="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "${upstream_records_json}")"
autosync::preflight::write_github_output "stage_dir" "${stage_dir}"
autosync::preflight::write_github_output "upstream_records" "${compact_records}"
autosync::preflight::write_github_output "source_revision" "${source_revision}"
