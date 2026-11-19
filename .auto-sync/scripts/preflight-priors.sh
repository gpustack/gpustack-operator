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
# shellcheck source=../checks/lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/../checks/lib.sh"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/autosync-priors-work.XXXXXXXX")"
cleanup_work() {
  if [[ -n "${work_dir:-}" && -d "${work_dir}" ]]; then
    rm -rf "${work_dir}"
  fi
}
trap cleanup_work EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

stage_dir="$(autosync::preflight::init_stage "priors" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
repo_root="$(autosync::preflight::repo_root)"
base_sha="$(git -C "${repo_root}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${stage_dir}" \
  "pkg/worker/scaling/priors/priors.yaml" \
  "pkg/worker/scaling/priors/records.md"
if [[ -n "${AUTOSYNC_PRIORS_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
  autosync::preflight::warm_caches "${stage_dir}" --sbin --go
else
  autosync::preflight::warm_caches "${stage_dir}" --sbin --go --tools hugo
fi

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
  curl -fsSL --compressed --connect-timeout 15 --max-time 60 --retry 3 --retry-delay 5 --max-filesize 104857600 \
    "${availability_url}" -o "${snapshots_dir}/availability.json"
fi

query_ids_file="${work_dir}/query-ids.txt"
unmapped_slugs_json="${work_dir}/unmapped-slugs.json"
python3 - "${snapshots_dir}/availability.json" "${repo_root}/pkg/worker/scaling/priors/records.md" "${query_ids_file}" "${unmapped_slugs_json}" "${AUTOSYNC_PRIORS_FIXTURE_DIR:-}" <<'PY'
import json
import os
import re
import sys

avail_path, records_path, out_path, unmapped_out_path, fixture_dir = sys.argv[1:6]
with open(avail_path, "r", encoding="utf-8") as f:
    data = json.load(f)

if not isinstance(data, (list, dict)):
    raise SystemExit("FAIL: availability.json must be a JSON array or object")

with open(records_path, "r", encoding="utf-8") as f:
    records_txt = f.read()

sec_m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", records_txt, re.DOTALL)
if not sec_m:
    raise SystemExit(f"FAIL: missing '## Active Sources' section in {records_path}")
active_sec = sec_m.group(1)

active_record_qids = []
for line in active_sec.splitlines():
    stripped = line.strip()
    if not stripped.startswith("|"):
        continue
    cells = [c.strip().strip("`") for c in stripped.strip("|").split("|")]
    if len(cells) >= 5 and re.fullmatch(r"[0-9a-f]{64}", cells[4]):
        active_record_qids.append(cells[0])

SLUG_TO_QUERY_ID = {
    "dsr1": "DeepSeek-R1-0528",
    "dsv4": "DeepSeek-V4-Pro",
    "glm5": "GLM-5",
    "glm5.1": "GLM-5",
    "glm5.2": "GLM-5.2",
    "glm5.3": "GLM-5.2",
    "gptoss120b": "gpt-oss-120b",
    "kimik2.5": "Kimi-K2.5",
    "kimik2.6": "Kimi-K2.5",
    "kimik3": "Kimi-K3",
    "llama70b": "Llama-3.3-70B-Instruct-FP8",
    "minimaxm2.5": "MiniMax-M2.5",
    "minimaxm3": "MiniMax-M3",
    "qwen3.5": "Qwen-3.5-397B-A17B",
    "qwen3.8next": "Qwen3.8-Flash-Next",
}

known_qids_by_lower = {}
for q in list(active_record_qids) + list(SLUG_TO_QUERY_ID.values()):
    known_qids_by_lower.setdefault(q.lower(), q)

QID_RE = re.compile(r"^[A-Za-z0-9._-]+$")
MAX_QUERY_IDS = 256
seen = set()
query_ids = []
unmapped_slugs = set()
saw_token = False


def add_qid(qid):
    if qid not in seen:
        if len(query_ids) >= MAX_QUERY_IDS:
            raise SystemExit(f"FAIL: availability.json exceeds max query ID cap ({MAX_QUERY_IDS})")
        seen.add(qid)
        query_ids.append(qid)


def has_fixture(tok):
    if not fixture_dir:
        return False
    return os.path.isfile(os.path.join(fixture_dir, f"{tok}.json")) or os.path.isfile(
        os.path.join(fixture_dir, "snapshots", f"{tok}.json")
    )


def handle_token(val):
    global saw_token
    if not isinstance(val, str):
        raise SystemExit(f"FAIL: non-string query ID in availability.json: {val!r}")
    tok = val.strip()
    if not tok or ".." in tok or not QID_RE.fullmatch(tok):
        raise SystemExit(f"FAIL: unsafe or malformed query ID in availability.json: {val!r}")
    saw_token = True
    lower_tok = tok.lower()
    if lower_tok in SLUG_TO_QUERY_ID:
        add_qid(SLUG_TO_QUERY_ID[lower_tok])
    elif lower_tok in known_qids_by_lower:
        add_qid(known_qids_by_lower[lower_tok])
    elif has_fixture(tok):
        add_qid(tok)
    else:
        unmapped_slugs.add(tok)


def extract(node):
    if isinstance(node, list):
        for item in node:
            if isinstance(item, str):
                handle_token(item)
            elif isinstance(item, dict):
                found = False
                for key in ("query_id", "query_model", "model", "id", "name"):
                    if isinstance(item.get(key), str) and item[key].strip():
                        handle_token(item[key])
                        found = True
                        break
                if not found:
                    raise SystemExit(f"FAIL: malformed item in availability.json: {item!r}")
            else:
                raise SystemExit(f"FAIL: unexpected entry type in availability.json: {item!r}")
    elif isinstance(node, dict):
        for wrapper_key in ("models", "available", "data", "items", "results"):
            if wrapper_key in node and isinstance(node[wrapper_key], (list, dict)):
                extract(node[wrapper_key])
                return
        for key in sorted(node.keys()):
            handle_token(key)
    else:
        raise SystemExit(f"FAIL: unexpected JSON node in availability.json: {type(node).__name__}")


extract(data)
if not saw_token:
    raise SystemExit("FAIL: availability.json yielded 0 model entries")

if unmapped_slugs:
    joined = ", ".join(sorted(unmapped_slugs))
    print(
        f"NOTICE: skipping {len(unmapped_slugs)} unmapped InferenceX availability slug(s) absent from SLUG_TO_QUERY_ID and gen/priors identity registry ({joined})",
        file=sys.stderr,
    )

if not query_ids:
    raise SystemExit("FAIL: availability.json yielded 0 active query IDs")

with open(out_path, "w", encoding="utf-8") as f:
    for qid in query_ids:
        f.write(qid + "\n")

with open(unmapped_out_path, "w", encoding="utf-8") as f:
    json.dump(sorted(unmapped_slugs), f)
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
    curl -fsSL --compressed --connect-timeout 15 --max-time 60 --retry 3 --retry-delay 5 --max-filesize 104857600 \
      "https://inferencex.semianalysis.com/api/v1/benchmarks?model=${encoded_qid}" \
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
  fi
fi

profile_counts="$(autosync::count_priors_profiles "${tmp_catalog}")"
agg_count="${profile_counts%% *}"
disagg_count="${profile_counts##* }"

report_json="${stage_dir}/priors-preflight-report.json"
upstream_records_json="${stage_dir}/upstream-records.json"

python3 - "${analysis_json}" "${unmapped_slugs_json}" "${report_json}" "${upstream_records_json}" \
  "${capture_date}" "${base_sha}" "${source_revision}" "${inferencex_commit}" \
  "${inferencex_repo_url}" "${merge_attempted}" "${catalog_changed}" \
  "${agg_count}" "${disagg_count}" "${tmp_catalog}" "${stage_dir}" \
  "${repo_root}/pkg/worker/scaling/priors/identity.go" <<'PY'
import json
import os
import re
import sys

(
    analysis_path,
    unmapped_path,
    report_path,
    records_out_path,
    capture_date,
    base_sha,
    source_revision,
    inferencex_commit,
    inferencex_repo_url,
    merge_attempted_str,
    catalog_changed_str,
    agg_count_str,
    disagg_count_str,
    catalog_path,
    stage_dir,
    identity_go_path,
) = sys.argv[1:17]

with open(analysis_path, "r", encoding="utf-8") as f:
    analysis = json.load(f)
with open(unmapped_path, "r", encoding="utf-8") as f:
    unmapped_slugs = json.load(f)

SLUG_TO_QUERY_ID = {
    "dsr1": "DeepSeek-R1-0528",
    "dsv4": "DeepSeek-V4-Pro",
    "glm5": "GLM-5",
    "glm5.1": "GLM-5",
    "glm5.2": "GLM-5.2",
    "glm5.3": "GLM-5.2",
    "gptoss120b": "gpt-oss-120b",
    "kimik2.5": "Kimi-K2.5",
    "kimik2.6": "Kimi-K2.5",
    "kimik3": "Kimi-K3",
    "llama70b": "Llama-3.3-70B-Instruct-FP8",
    "minimaxm2.5": "MiniMax-M2.5",
    "minimaxm3": "MiniMax-M3",
    "qwen3.5": "Qwen-3.5-397B-A17B",
    "qwen3.8next": "Qwen3.8-Flash-Next",
}

canonical_by_slug = {}
if os.path.isfile(identity_go_path):
    with open(identity_go_path, "r", encoding="utf-8") as f:
        id_txt = f.read()
    m_map = re.search(r"var\s+canonicalModelNames\s*=\s*map\[string\]string\s*\{(.*?)\}", id_txt, re.DOTALL)
    if m_map:
        for slug, repo in re.findall(r'"([^"]+)"\s*:\s*"([^"]+)"', m_map.group(1)):
            canonical_by_slug[slug.strip().lower()] = repo.strip()

qid_to_canonical = {}
for slug, qid in SLUG_TO_QUERY_ID.items():
    if slug.lower() in canonical_by_slug and qid not in qid_to_canonical:
        qid_to_canonical[qid] = canonical_by_slug[slug.lower()]


def parse_catalog_models_by_mode(path: str) -> dict[str, dict[str, int]]:
    by_model: dict[str, dict[str, int]] = {}
    try:
        import yaml  # type: ignore

        with open(path, "r", encoding="utf-8") as f:
            doc = yaml.safe_load(f) or {}
        for prof in doc.get("profiles") or []:
            if not isinstance(prof, dict):
                continue
            mode = str(prof.get("mode") or "").strip()
            model = str(prof.get("model") or "").strip()
            if not model or mode not in ("aggregated", "disaggregated"):
                continue
            entry = by_model.setdefault(model, {"aggregated": 0, "disaggregated": 0})
            entry[mode] += 1
        return by_model
    except ImportError:
        pass

    cur_mode = ""
    cur_model = ""
    in_profiles = False

    def flush():
        nonlocal cur_mode, cur_model
        if cur_model and cur_mode in ("aggregated", "disaggregated"):
            entry = by_model.setdefault(cur_model, {"aggregated": 0, "disaggregated": 0})
            entry[cur_mode] += 1
        cur_mode = ""
        cur_model = ""

    with open(path, "r", encoding="utf-8") as f:
        for raw in f:
            st = raw.strip()
            if not st or st.startswith("#"):
                continue
            if not raw.startswith(" ") and st.startswith("profiles:"):
                in_profiles = True
                continue
            if not in_profiles:
                continue
            if re.match(r"^\s{2}-\s+", raw):
                flush()
                m_mode = re.match(r"^\s{2}-\s+mode:\s*(\S+)", raw)
                if m_mode:
                    cur_mode = m_mode.group(1).strip("'\"")
            elif re.match(r"^\s{4}mode:\s*", raw):
                cur_mode = raw.split(":", 1)[1].strip().strip("'\"")
            elif re.match(r"^\s{4}model:\s*", raw):
                cur_model = raw.split(":", 1)[1].strip().strip("'\"")
    flush()
    return by_model


def resolve_query_model(qid: str, snap_rel: str) -> str:
    snap_abs = os.path.join(stage_dir, snap_rel)
    if os.path.isfile(snap_abs):
        try:
            with open(snap_abs, "r", encoding="utf-8") as f:
                raw_snap = json.load(f)
            rows = raw_snap if isinstance(raw_snap, list) else (raw_snap.get("rows") or raw_snap.get("data") or [])
            for row in rows:
                if not isinstance(row, dict):
                    continue
                can = str(row.get("canonical_model") or "").strip()
                if can and "/" in can:
                    return can
                for key in ("model", "query_model"):
                    val = str(row.get(key) or "").strip()
                    if val.lower() in canonical_by_slug:
                        return canonical_by_slug[val.lower()]
        except Exception:
            pass
    if qid in qid_to_canonical:
        return qid_to_canonical[qid]
    if qid.lower() in canonical_by_slug:
        return canonical_by_slug[qid.lower()]
    return ""


catalog_models_by_mode = parse_catalog_models_by_mode(catalog_path)

agg_count = int(agg_count_str)
disagg_count = int(disagg_count_str)
merge_attempted = merge_attempted_str == "true"
catalog_changed = catalog_changed_str == "true"
snapshots_changed = bool(analysis["snapshots_changed"])
digest_refresh_only = bool(snapshots_changed and not catalog_changed)
overall_changed = snapshots_changed or catalog_changed

for q in analysis["queries"]:
    model_name = resolve_query_model(q["query_id"], q["snapshot_path"])
    counts = catalog_models_by_mode.get(model_name, {"aggregated": 0, "disaggregated": 0})
    q["model"] = model_name
    q["aggregated"] = counts["aggregated"]
    q["disaggregated"] = counts["disaggregated"]

unresolved_queries = [q["query_id"] for q in analysis["queries"] if not q.get("model")]
if unresolved_queries:
    print(
        f"WARN: unable to resolve canonical Hugging Face model for query ID(s): {', '.join(unresolved_queries)}",
        file=sys.stderr,
    )

represented_models = {q["model"] for q in analysis["queries"] if q.get("model")}
if not unresolved_queries and set(catalog_models_by_mode.keys()).issubset(represented_models):
    sum_agg = sum(q["aggregated"] for q in analysis["queries"])
    sum_disagg = sum(q["disaggregated"] for q in analysis["queries"])
    if sum_agg != agg_count or sum_disagg != disagg_count:
        raise SystemExit(
            f"FAIL: per-query profile count sum ({sum_agg} agg / {sum_disagg} disagg) != "
            f"catalog totals ({agg_count} agg / {disagg_count} disagg)"
        )

digest_refresh_reason = (
    "InferenceX snapshot SHA-256 changed without altering priors.yaml profiles; "
    "update pkg/worker/scaling/priors/records.md digests and commit"
)

if digest_refresh_only:
    avail_reason = digest_refresh_reason
elif overall_changed:
    avail_reason = "InferenceX availability or benchmark snapshots changed relative to records.md"
else:
    avail_reason = "All InferenceX availability and benchmark snapshot SHA-256 digests match records.md"

report = {
    "schema_version": 1,
    "capture_date": capture_date,
    "base_sha": base_sha,
    "source_revision": source_revision,
    "changed": overall_changed,
    "digest_refresh_only": digest_refresh_only,
    "reason": avail_reason,
    "enforce_snapshot_digests": True,
    "inferencex_repo": {
        "url": inferencex_repo_url,
        "commit": inferencex_commit,
        "changed": inferencex_commit != analysis["baseline_repo_commit"],
    },
    "availability": analysis["availability"],
    "unmapped_availability_slugs": unmapped_slugs,
    "unresolved_query_models": unresolved_queries,
    "queries": analysis["queries"],
    "removed_query_ids": analysis["removed_query_ids"],
    "catalog_models_by_mode": catalog_models_by_mode,
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
        "reason": avail_reason,
    },
]

with open(records_out_path, "w", encoding="utf-8") as f:
    json.dump(upstream_records, f, indent=2)
    f.write("\n")
PY

autosync::preflight::apply_force_to_upstream_records "${upstream_records_json}"

if [[ "${AUTOSYNC_SKIP_SOURCE_REVISION:-false}" != "true" ]] && \
   autosync::preflight::has_available_upstreams "${upstream_records_json}"; then
  staged_tree="${work_dir}/staged-tree"
  mkdir -p "${staged_tree}"
  autosync::preflight::populate_stage_bundle "${staged_tree}" "priors" \
    "${report_json}" \
    "${upstream_records_json}"
  commit_paths=(".auto-sync/.stage/priors")
  if [[ "${catalog_changed}" == "true" ]]; then
    mkdir -p "${staged_tree}/pkg/worker/scaling/priors"
    cp -f "${tmp_catalog}" "${staged_tree}/pkg/worker/scaling/priors/priors.yaml"
    commit_paths+=("pkg/worker/scaling/priors/priors.yaml")
  fi
  source_revision="$(
    autosync::preflight::commit_staged_tree_revision \
      "${repo_root}" "${base_sha}" \
      "chore(priors): stage InferenceX preflight bundle and catalog" \
      "${staged_tree}" \
      "${commit_paths[@]}"
  )"
  if [[ "${source_revision}" != "${base_sha}" ]]; then
    python3 - "${report_json}" "${source_revision}" <<'PY'
import json
import sys

rep_path, src_rev = sys.argv[1:3]
with open(rep_path, "r", encoding="utf-8") as f:
    rep = json.load(f)
rep["source_revision"] = src_rev
with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(rep, f, indent=2)
    f.write("\n")
PY
  fi
fi

rm -f "${analysis_json}" "${unmapped_slugs_json}"

compact_records="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",", ":")))' "${upstream_records_json}")"
autosync::preflight::write_github_output "stage_dir" "${stage_dir}"
autosync::preflight::write_github_output "upstream_records" "${compact_records}"
autosync::preflight::write_github_output "source_revision" "${source_revision}"
