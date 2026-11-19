#!/usr/bin/env bash
# .auto-sync/checks/selftest_priors.sh - offline self-tests for priors-sync preflight and validator.
set -euo pipefail

ROOT_DIR="${1:-}"
if [[ -z "${ROOT_DIR}" ]]; then
  ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
else
  ROOT_DIR="$(cd "${ROOT_DIR}" && pwd -P)"
fi

PREFLIGHT_REL=".auto-sync/scripts/preflight-priors.sh"
VALIDATOR_REL=".auto-sync/checks/validate_priors.sh"

FAILS=0
PASSES=0

pass() {
  printf 'PASS  [priors] %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  [priors] %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"
cleanup() {
  rm -rf "${SCRATCH}"
}
trap cleanup EXIT

FAST_CP_MODE="plain"
detect_fast_cp_mode() {
  local probe_src="${SCRATCH}/.cp-probe-src"
  local probe_dst="${SCRATCH}/.cp-probe-dst"
  mkdir -p "${probe_src}"
  : > "${probe_src}/f"
  if cp -Rc "${probe_src}" "${probe_dst}" 2>/dev/null; then
    FAST_CP_MODE="apfs"
  else
    rm -rf "${probe_dst}"
    if cp -R --reflink=auto "${probe_src}" "${probe_dst}" 2>/dev/null && [[ -f "${probe_dst}/f" ]]; then
      FAST_CP_MODE="reflink"
    fi
  fi
  rm -rf "${probe_src}" "${probe_dst}"
}
detect_fast_cp_mode

fast_cp_dir() {
  case "${FAST_CP_MODE}" in
    apfs) cp -Rc "$1" "$2" ;;
    reflink) cp -R --reflink=auto "$1" "$2" ;;
    *) cp -R "$1" "$2" ;;
  esac
}

copy_priors_go_tree() {
  local src_root="$1"
  local dst_root="$2"
  local rel_paths=(
    "go.mod"
    "go.sum"
    "gen/priors"
    "pkg/worker/scaling"
    "pkg/nodefeature"
    "pkg/device"
    "pkg/kubemeta"
    "pkg/systemname"
    "pkg/utils"
    "api/v1"
    "api/worker"
    "binding"
    "staging"
  )
  local rel
  for rel in "${rel_paths[@]}"; do
    if [[ -e "${src_root}/${rel}" ]]; then
      mkdir -p "${dst_root}/$(dirname "${rel}")"
      fast_cp_dir "${src_root}/${rel}" "${dst_root}/$(dirname "${rel}")/"
    fi
  done
}

HOST_GOMODCACHE="$(go env GOMODCACHE)"
HOST_GOCACHE="$(go env GOCACHE)"

# Build an isolated scratch git repo for offline preflight tests so the real working tree stays untouched.
PREFLIGHT_REPO="${SCRATCH}/preflight-repo"
mkdir -p "${PREFLIGHT_REPO}/.auto-sync/scripts" "${PREFLIGHT_REPO}/.auto-sync/checks" "${PREFLIGHT_REPO}/.auto-sync/proposals"
copy_priors_go_tree "${ROOT_DIR}" "${PREFLIGHT_REPO}"
cp "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh" "${PREFLIGHT_REPO}/.auto-sync/scripts/lib-preflight.sh"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${PREFLIGHT_REPO}/.auto-sync/checks/lib.sh"
if [[ -f "${ROOT_DIR}/${PREFLIGHT_REL}" ]]; then
  cp "${ROOT_DIR}/${PREFLIGHT_REL}" "${PREFLIGHT_REPO}/${PREFLIGHT_REL}"
  chmod +x "${PREFLIGHT_REPO}/${PREFLIGHT_REL}"
fi
if [[ -f "${ROOT_DIR}/.auto-sync/proposals/priors.json" ]]; then
  cp "${ROOT_DIR}/.auto-sync/proposals/priors.json" "${PREFLIGHT_REPO}/.auto-sync/proposals/priors.json"
else
  printf '{"schema_version":1,"summary":"stub","versions":{},"subscriptions":[]}\n' > "${PREFLIGHT_REPO}/.auto-sync/proposals/priors.json"
fi

git -C "${PREFLIGHT_REPO}" init -q -b main
git -C "${PREFLIGHT_REPO}" config user.name "priors-selftest"
git -C "${PREFLIGHT_REPO}" config user.email "priors-selftest@gpustack.local"
git -C "${PREFLIGHT_REPO}" config commit.gpgsign false
git -C "${PREFLIGHT_REPO}" add -A
git -C "${PREFLIGHT_REPO}" commit -q -m "baseline"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

# 1. Offline preflight unchanged fixture test.
FIXTURE_SAME="${SCRATCH}/fixture-same"
mkdir -p "${FIXTURE_SAME}"
printf '1111111111111111111111111111111111111111\n' > "${FIXTURE_SAME}/inferencex-head.txt"

python3 - "${ROOT_DIR}/pkg/worker/scaling/priors/records.md" "${FIXTURE_SAME}" <<'PY'
import hashlib
import json
import os
import re
import sys

records_path, fixture_dir = sys.argv[1:3]
with open(records_path, "r", encoding="utf-8") as f:
    text = f.read()

m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", text, re.DOTALL)
assert m, "missing ## Active Sources"
section = m.group(1)
avail_m = re.search(r"SHA-256\s+`([0-9a-f]{64})`", section)
assert avail_m, "missing availability SHA-256"
target_avail_sha = avail_m.group(1)

queries = []
for line in section.splitlines():
    if not line.strip().startswith("|"):
        continue
    cells = [c.strip().strip("`") for c in line.strip().strip("|").split("|")]
    if len(cells) >= 5 and re.fullmatch(r"[0-9a-f]{64}", cells[4]):
        queries.append((cells[0], int(cells[1]), cells[4]))

# Construct deterministic JSON fixture payloads? Real records.md hashes real downloaded bytes,
# so for the unchanged test we override expected hashes in a copied records.md that matches
# the exact sha256 of deterministic fixture files.
avail_bytes = json.dumps([q[0] for q in queries], indent=2).encode("utf-8")
with open(os.path.join(fixture_dir, "availability.json"), "wb") as f:
    f.write(avail_bytes)
avail_sha = hashlib.sha256(avail_bytes).hexdigest()

new_section = section.replace(target_avail_sha, avail_sha)
for qid, row_count, old_sha in queries:
    payload = json.dumps([{"query_model": qid, "row_idx": i} for i in range(row_count)], separators=(",", ":")).encode("utf-8")
    with open(os.path.join(fixture_dir, f"{qid}.json"), "wb") as f:
        f.write(payload)
    new_sha = hashlib.sha256(payload).hexdigest()
    new_section = new_section.replace(old_sha, new_sha)

with open(os.path.join(fixture_dir, "records.md"), "w", encoding="utf-8") as f:
    f.write(text[:m.start(1)] + new_section + text[m.end(1):])
PY

cp "${FIXTURE_SAME}/records.md" "${PREFLIGHT_REPO}/pkg/worker/scaling/priors/records.md"
git -C "${PREFLIGHT_REPO}" add pkg/worker/scaling/priors/records.md
git -C "${PREFLIGHT_REPO}" commit -q -m "align fixture digests"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

STAGE_SAME="${SCRATCH}/stage-same/priors"
GHA_SAME="${SCRATCH}/gha-same.txt"
: > "${GHA_SAME}"

if [[ -x "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" ]] && \
  (
    cd "${PREFLIGHT_REPO}"
    AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_SAME}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    GITHUB_OUTPUT="${GHA_SAME}" \
    bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_SAME}" >/dev/null
  ) && \
  python3 - "${STAGE_SAME}" "${GHA_SAME}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, base_sha = sys.argv[1:4]
with open(os.path.join(stage_dir, "priors-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["changed"] is False, report
assert report["source_revision"] == base_sha, report
assert report["enforce_snapshot_digests"] is True, report

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert isinstance(recs, list) and len(recs) == 2, recs
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["inferencex-repo"]["status"] == "up_to_date", recs
assert by_id["inferencex-availability"]["status"] == "up_to_date", recs

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{base_sha}\n" in gha_txt, gha_txt
PY
then
  if [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
    pass "preflight-priors.sh unchanged fixture emits up_to_date and source_revision == base_sha"
  else
    fail "preflight-priors.sh unchanged fixture left dirty git tree"
  fi
else
  fail "preflight-priors.sh unchanged fixture"
fi

# 2. Offline preflight changed fixture test (stages source_revision with merged profile, keeps working tree clean).
FIXTURE_CHANGED="${SCRATCH}/fixture-changed"
cp -R "${FIXTURE_SAME}" "${FIXTURE_CHANGED}"
printf '2222222222222222222222222222222222222222\n' > "${FIXTURE_CHANGED}/inferencex-head.txt"
cat > "${FIXTURE_CHANGED}/DeepSeek-R1-0528.json" <<'JSON'
[
  {
    "model": "dsr1",
    "query_model": "DeepSeek-R1-0528",
    "canonical_model": "deepseek-ai/DeepSeek-R1-0528",
    "framework": "vllm",
    "hardware": "h200",
    "precision": "fp8",
    "disagg": false,
    "conc": 998,
    "isl": 8192,
    "osl": 1024,
    "date": "2026-11-17",
    "prefill_num_workers": 1,
    "prefill_tp": 8,
    "prefill_ep": 1,
    "num_prefill_gpu": 8,
    "metrics": {
      "tput_per_gpu": 400.0,
      "input_tput_per_gpu": 360.0,
      "output_tput_per_gpu": 40.0,
      "mean_itl": 0.01,
      "mean_tpot": 0.01,
      "median_ttft": 0.09,
      "p90_ttft": 0.18,
      "p99_ttft": 0.27,
      "median_itl": 0.01,
      "p90_itl": 0.012,
      "p99_itl": 0.015
    }
  },
  {
    "model": "dsr1",
    "query_model": "DeepSeek-R1-0528",
    "canonical_model": "deepseek-ai/DeepSeek-R1-0528",
    "framework": "vllm",
    "hardware": "h200",
    "precision": "fp8",
    "disagg": false,
    "conc": 999,
    "isl": 8192,
    "osl": 1024,
    "date": "2026-11-17",
    "prefill_num_workers": 1,
    "prefill_tp": 8,
    "prefill_ep": 1,
    "num_prefill_gpu": 8,
    "metrics": {
      "tput_per_gpu": 500.0,
      "input_tput_per_gpu": 450.0,
      "output_tput_per_gpu": 50.0,
      "mean_itl": 0.01,
      "mean_tpot": 0.01,
      "median_ttft": 0.1,
      "p90_ttft": 0.2,
      "p99_ttft": 0.3,
      "median_itl": 0.01,
      "p90_itl": 0.012,
      "p99_itl": 0.015
    }
  }
]
JSON

STAGE_CHANGED="${SCRATCH}/stage-changed/priors"
GHA_CHANGED="${SCRATCH}/gha-changed.txt"
: > "${GHA_CHANGED}"

if [[ -x "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" ]] && \
  (
    cd "${PREFLIGHT_REPO}"
    AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_CHANGED}" \
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    GITHUB_OUTPUT="${GHA_CHANGED}" \
    bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_CHANGED}" >/dev/null
  ) && \
  SRC_REV="$(python3 - "${STAGE_CHANGED}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "priors-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["changed"] is True, report
assert report["source_revision"] != base_sha, report
assert report["catalog_merge"]["catalog_changed"] is True, report

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["inferencex-availability"]["status"] == "available", recs
print(report["source_revision"])
PY
)" && \
  [[ -n "${SRC_REV}" && "${SRC_REV}" != "${BASE_SHA}" ]] && \
  grep -q 'concurrency: 999' <<<"$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:pkg/worker/scaling/priors/priors.yaml")" && \
  [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
  [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
  pass "preflight-priors.sh changed fixture emits available status and clean detached source_revision"
else
  fail "preflight-priors.sh changed fixture"
fi

# 2b. Digest-refresh-only fixture test (snapshot SHA-256 changes without altering priors.yaml profiles).
FIXTURE_DIGEST="${SCRATCH}/fixture-digest"
cp -R "${FIXTURE_SAME}" "${FIXTURE_DIGEST}"
python3 - "${FIXTURE_DIGEST}/availability.json" <<'PY'
import json
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
# Re-serialize with compact formatting so SHA-256 changes while query IDs and snapshots stay identical.
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, separators=(",", ":"))
PY

STAGE_DIGEST="${SCRATCH}/stage-digest/priors"
DIGEST_SRC_REV=""
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_DIGEST}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_DIGEST}" >/dev/null
) && \
DIGEST_SRC_REV="$(python3 - "${STAGE_DIGEST}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "priors-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["changed"] is True, report
assert report["digest_refresh_only"] is True, report
assert report["source_revision"] != base_sha, report
assert "records.md" in report.get("reason", ""), report

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
avail = recs["inferencex-availability"]
assert avail["status"] == "available", avail
assert "records.md digests" in avail["reason"], avail
print(report["source_revision"])
PY
)" && \
[[ -n "${DIGEST_SRC_REV}" && "${DIGEST_SRC_REV}" != "${BASE_SHA}" ]] && \
git -C "${PREFLIGHT_REPO}" cat-file -e "${DIGEST_SRC_REV}:.auto-sync/.stage/priors/priors-preflight-report.json" 2>/dev/null && \
git -C "${PREFLIGHT_REPO}" cat-file -e "${DIGEST_SRC_REV}:.auto-sync/.stage/priors/upstream-records.json" 2>/dev/null && \
cmp -s <(git -C "${PREFLIGHT_REPO}" show "${BASE_SHA}:pkg/worker/scaling/priors/priors.yaml") <(git -C "${PREFLIGHT_REPO}" show "${DIGEST_SRC_REV}:pkg/worker/scaling/priors/priors.yaml"); then
  pass "preflight-priors.sh sets digest_refresh_only=true, stages bundle, and keeps priors.yaml unchanged"
else
  fail "preflight-priors.sh digest_refresh_only test"
fi

# 2c. Uncommitted local edit in repo_root survives preflight-priors.sh untouched.
STAGE_DIRTY="${SCRATCH}/stage-dirty/priors"
printf '\n# local uncommitted edit\n' >> "${PREFLIGHT_REPO}/pkg/worker/scaling/priors/priors.yaml"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_CHANGED}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_DIRTY}" >/dev/null
) && \
grep -q '# local uncommitted edit' "${PREFLIGHT_REPO}/pkg/worker/scaling/priors/priors.yaml"; then
  pass "preflight-priors.sh preserves uncommitted local edits in repo_root"
else
  fail "preflight-priors.sh preserves uncommitted local edits in repo_root"
fi
git -C "${PREFLIGHT_REPO}" checkout -- pkg/worker/scaling/priors/priors.yaml

# 2d. Slug-to-QueryID mapping and unmapped availability slugs test.
FIXTURE_SLUGS="${SCRATCH}/fixture-slugs"
cp -R "${FIXTURE_SAME}" "${FIXTURE_SLUGS}"
cat > "${FIXTURE_SLUGS}/availability.json" <<'JSON'
[
  {"model": "dsr1"},
  {"model": "minimaxm2.5"},
  {"model": "musespark"}
]
JSON

STAGE_SLUGS="${SCRATCH}/stage-slugs/priors"
SLUGS_STDERR="${SCRATCH}/stage-slugs.stderr"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_SLUGS}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_SLUGS}" >/dev/null 2>"${SLUGS_STDERR}"
) && \
python3 - "${STAGE_SLUGS}" "${FIXTURE_SLUGS}/records.md" "${SLUGS_STDERR}" <<'PY'
import json
import os
import re
import sys

stage_dir, records_path, stderr_path = sys.argv[1:4]
with open(os.path.join(stage_dir, "priors-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)

with open(records_path, "r", encoding="utf-8") as f:
    records_txt = f.read()

sec_m = re.search(r"## Active Sources\b(.*?)(?:\n## |\Z)", records_txt, re.DOTALL)
assert sec_m, f"missing ## Active Sources in {records_path}"
active_qids = set()
for line in sec_m.group(1).splitlines():
    stripped = line.strip()
    if not stripped.startswith("|"):
        continue
    cells = [c.strip().strip("`") for c in stripped.strip("|").split("|")]
    if len(cells) >= 5 and re.fullmatch(r"[0-9a-f]{64}", cells[4]):
        active_qids.add(cells[0])

with open(stderr_path, "r", encoding="utf-8") as f:
    stderr_txt = f.read()

qids = {q["query_id"] for q in report.get("queries", [])}
assert "DeepSeek-R1-0528" in qids, qids
assert "MiniMax-M2.5" in qids, qids
assert "musespark" not in qids, qids
assert report.get("unmapped_availability_slugs") == ["musespark"], report
assert "NOTICE: skipping 1 unmapped InferenceX availability slug(s) absent from SLUG_TO_QUERY_ID and gen/priors identity registry (musespark)" in stderr_txt, stderr_txt
assert "identity.go:" not in stderr_txt, stderr_txt
assert not re.search(r"\.go:(?:\d+(?::\d+)?:)?\s", stderr_txt), stderr_txt
expected_removed = active_qids - {"DeepSeek-R1-0528", "MiniMax-M2.5"}
assert set(report["removed_query_ids"]) == expected_removed, (report["removed_query_ids"], expected_removed)
assert len(report["removed_query_ids"]) == len(active_qids) - 2, report
PY
then
  pass "preflight-priors.sh maps InferenceX slugs to Query IDs and records unmapped_availability_slugs"
else
  fail "preflight-priors.sh maps InferenceX slugs to Query IDs and records unmapped_availability_slugs"
fi

# 3. Gitless sanitized env -i validation tests for validate_priors.sh.
CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}/.auto-sync/checks" "${CAND_BASE}/.auto-sync/proposals"
copy_priors_go_tree "${ROOT_DIR}" "${CAND_BASE}"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${CAND_BASE}/.auto-sync/checks/lib.sh"
if [[ -f "${ROOT_DIR}/${VALIDATOR_REL}" ]]; then
  cp "${ROOT_DIR}/${VALIDATOR_REL}" "${CAND_BASE}/${VALIDATOR_REL}"
  chmod +x "${CAND_BASE}/${VALIDATOR_REL}"
fi
cp "${FIXTURE_SAME}/records.md" "${CAND_BASE}/pkg/worker/scaling/priors/records.md"
python3 - "${ROOT_DIR}/.auto-sync/proposals/priors.json" "${FIXTURE_SAME}/availability.json" "${CAND_BASE}/.auto-sync/proposals/priors.json" <<'PY'
import hashlib
import json
import sys

src_p, avail_p, dst_p = sys.argv[1:4]
with open(src_p, "r", encoding="utf-8") as f:
    doc = json.load(f)
with open(avail_p, "rb") as f:
    avail_sha = hashlib.sha256(f.read()).hexdigest()
doc["versions"]["inferencex-availability"] = avail_sha
with open(dst_p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY

run_validator_env_i() {
  local cand_dir="$1"
  local stage_dir="$2"
  local home_dir="${SCRATCH}/env-home"
  mkdir -p "${home_dir}"
  env -i \
    PATH=/usr/local/bin:/usr/bin:/bin \
    HOME="${home_dir}" \
    TMPDIR="${SCRATCH}" \
    GOMODCACHE="${HOST_GOMODCACHE}" \
    GOCACHE="${HOST_GOCACHE}" \
    AUTOSYNC_STAGE_DIR="${stage_dir}" \
    LOOPWRIGHT_CANDIDATE_DIR="${cand_dir}" \
    AUTOSYNC_FAST_SELFTEST=true \
    bash "${cand_dir}/${VALIDATOR_REL}"
}

# 3a. Positive case: matching priors.yaml, records.md digests & counts, and clean ephemeral git cleanup.
CAND_POS="${SCRATCH}/cand-pos"
fast_cp_dir "${CAND_BASE}" "${CAND_POS}"
if [[ -x "${CAND_POS}/${VALIDATOR_REL}" ]] && \
   run_validator_env_i "${CAND_POS}" "${STAGE_SAME}" >/dev/null 2>&1 && \
   [[ ! -e "${CAND_POS}/.git" ]] && [[ ! -e "${CAND_POS}/.sbin" ]]; then
  pass "validate_priors.sh passes positive gitless env -i run and cleans up .git/.sbin"
else
  fail "validate_priors.sh passes positive gitless env -i run and cleans up .git/.sbin"
fi

# 3b. Negative case 1: mismatched SHA-256 digest in records.md vs priors-preflight-report.json.
CAND_BAD_SHA="${SCRATCH}/cand-bad-sha"
fast_cp_dir "${CAND_BASE}" "${CAND_BAD_SHA}"
python3 - "${CAND_BAD_SHA}/pkg/worker/scaling/priors/records.md" <<'PY'
import re
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(r"(\|`DeepSeek-R1-0528`[^\n]*?`)([0-9a-f]{64})(`)", r"\g<1>" + ("0" * 64) + r"\g<3>", txt, count=1)
if out == txt:
    out = re.sub(r"(\| `DeepSeek-R1-0528` .*?`)([0-9a-f]{64})(`)", r"\g<1>" + ("0" * 64) + r"\g<3>", txt, count=1)
assert out != txt, "failed to mutate DeepSeek-R1-0528 SHA-256"
with open(p, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if [[ -x "${CAND_BAD_SHA}/${VALIDATOR_REL}" ]] && \
   ! bad_err="$(run_validator_env_i "${CAND_BAD_SHA}" "${STAGE_SAME}" 2>&1)" && \
   grep -q "SHA-256 mismatch" <<<"${bad_err}"; then
  pass "validate_priors.sh fails on mismatched SHA-256 digest in records.md"
else
  fail "validate_priors.sh fails on mismatched SHA-256 digest in records.md (${bad_err})"
fi

# 3c. Negative case 2: mismatched aggregated/disaggregated profile counts between records.md and priors.yaml.
CAND_BAD_CNT="${SCRATCH}/cand-bad-cnt"
fast_cp_dir "${CAND_BASE}" "${CAND_BAD_CNT}"
python3 - "${CAND_BAD_CNT}/pkg/worker/scaling/priors/records.md" <<'PY'
import re
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    txt = f.read()
m = re.search(r"(\|\s*`?DeepSeek-R1-0528`?\s*\|\s*\d+\s*\|\s*)(\d+)(\s*\|\s*\d+\s*\|)", txt)
if not m:
    m = re.search(r"(\|\s*`?[A-Za-z0-9._-]+`?\s*\|\s*\d+\s*\|\s*)(\d+)(\s*\|\s*\d+\s*\|)", txt)
assert m, "failed to locate active row counts in records.md"
old_agg = int(m.group(2))
new_agg = old_agg + 83 if old_agg != 99 else 100
out = txt[: m.start(2)] + str(new_agg) + txt[m.end(2) :]
assert out != txt, "failed to mutate active row count"
with open(p, "w", encoding="utf-8") as f:
    f.write(out)
PY
bad_err=""
if [[ -x "${CAND_BAD_CNT}/${VALIDATOR_REL}" ]] && \
   ! bad_err="$(run_validator_env_i "${CAND_BAD_CNT}" "${STAGE_SAME}" 2>&1)" && \
   grep -q "profile count mismatch" <<<"${bad_err}"; then
  pass "validate_priors.sh fails on mismatched aggregated/disaggregated counts"
else
  fail "validate_priors.sh fails on mismatched aggregated/disaggregated counts (${bad_err})"
fi

# 3d. Negative case 3: unformatted priors.yaml.
CAND_UNFMT="${SCRATCH}/cand-unfmt"
fast_cp_dir "${CAND_BASE}" "${CAND_UNFMT}"
printf '\n# trailing unformatted comment\n' >> "${CAND_UNFMT}/pkg/worker/scaling/priors/priors.yaml"
bad_err=""
if [[ -x "${CAND_UNFMT}/${VALIDATOR_REL}" ]] && \
   ! bad_err="$(run_validator_env_i "${CAND_UNFMT}" "${STAGE_SAME}" 2>&1)" && \
   grep -q "not deterministically formatted" <<<"${bad_err}"; then
  pass "validate_priors.sh fails on unformatted priors.yaml"
else
  fail "validate_priors.sh fails on unformatted priors.yaml (${bad_err})"
fi

# 3e. Negative case 4: mismatched proposals/priors.json availability SHA-256.
CAND_BAD_PROP="${SCRATCH}/cand-bad-prop"
fast_cp_dir "${CAND_BASE}" "${CAND_BAD_PROP}"
python3 - "${CAND_BAD_PROP}/.auto-sync/proposals/priors.json" <<'PY'
import json
import sys

p = sys.argv[1]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["inferencex-availability"] = "0" * 64
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
bad_err=""
if [[ -x "${CAND_BAD_PROP}/${VALIDATOR_REL}" ]] && \
   ! bad_err="$(run_validator_env_i "${CAND_BAD_PROP}" "${STAGE_SAME}" 2>&1)" && \
   grep -q "proposals/priors.json" <<<"${bad_err}"; then
  pass "validate_priors.sh fails on mismatched proposals/priors.json availability SHA-256"
else
  fail "validate_priors.sh fails on mismatched proposals/priors.json availability SHA-256 (${bad_err})"
fi

# 3f. Fallback parser tests for autosync::count_priors_profiles (R2-I1 / R2-S4).
NO_YAML_DIR="${SCRATCH}/no-yaml"
mkdir -p "${NO_YAML_DIR}"
printf 'raise ImportError("forced fallback")\n' > "${NO_YAML_DIR}/yaml.py"

PRIORS_TWO_MISSING_MODE="${SCRATCH}/priors-two-missing.yaml"
cat > "${PRIORS_TWO_MISSING_MODE}" <<'YAML'
profiles:
  - name: first
    framework: vllm
  - name: second
    framework: vllm
YAML
bad_err=""
if ! bad_err="$(PYTHONPATH="${NO_YAML_DIR}" bash -c 'source "$1"; autosync::count_priors_profiles "$2"' _ "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${PRIORS_TWO_MISSING_MODE}" 2>&1)" && \
   grep -q "profile entry missing mode key" <<<"${bad_err}"; then
  pass "autosync::count_priors_profiles fallback fails closed when two entries lack mode"
else
  fail "autosync::count_priors_profiles fallback fails closed when two entries lack mode (${bad_err})"
fi

PRIORS_FIRST_MISSING_MODE="${SCRATCH}/priors-first-missing.yaml"
cat > "${PRIORS_FIRST_MISSING_MODE}" <<'YAML'
profiles:
  - name: first
    framework: vllm
  - name: second
    mode: aggregated
YAML
bad_err=""
if ! bad_err="$(PYTHONPATH="${NO_YAML_DIR}" bash -c 'source "$1"; autosync::count_priors_profiles "$2"' _ "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${PRIORS_FIRST_MISSING_MODE}" 2>&1)" && \
   grep -q "profile entry missing mode key" <<<"${bad_err}"; then
  pass "autosync::count_priors_profiles fallback fails closed when first entry lacks mode"
else
  fail "autosync::count_priors_profiles fallback fails closed when first entry lacks mode (${bad_err})"
fi

PRIORS_EMPTY_LIST="${SCRATCH}/priors-empty.yaml"
printf 'profiles: []\n' > "${PRIORS_EMPTY_LIST}"
empty_counts=""
if empty_counts="$(PYTHONPATH="${NO_YAML_DIR}" bash -c 'source "$1"; autosync::count_priors_profiles "$2"' _ "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${PRIORS_EMPTY_LIST}")" && \
   [[ "${empty_counts}" == "0 0" ]]; then
  pass "autosync::count_priors_profiles fallback accepts profiles: [] and returns 0 0"
else
  fail "autosync::count_priors_profiles fallback accepts profiles: [] (${empty_counts})"
fi

# 3g. Ephemeral git restores tracked symlinks in symlink-stripped candidate copy, keeps
#     git status clean (including site/public), passes docs/specs/crossrefs gates, and cleans up.
CAND_SYMLINK="${SCRATCH}/cand-symlink"
(
  # shellcheck source=./lib.sh
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
  autosync::copy_tree_without_git "${ROOT_DIR}" "${CAND_SYMLINK}" .sbin .dist .claude .codex
)
rm -rf "${CAND_SYMLINK}/site/public"

if (
  # shellcheck source=./lib.sh
  # shellcheck disable=SC1091
  source "${CAND_SYMLINK}/.auto-sync/checks/lib.sh"
  autosync::ensure_ephemeral_git "${CAND_SYMLINK}"
  [[ -L "${CAND_SYMLINK}/.claude/skills" && "$(readlink "${CAND_SYMLINK}/.claude/skills")" == "../.agents/skills" ]]
  [[ -L "${CAND_SYMLINK}/.claude/settings.json" && "$(readlink "${CAND_SYMLINK}/.claude/settings.json")" == "../.agents/hooks.json" ]]
  [[ -L "${CAND_SYMLINK}/.codex/hooks.json" && "$(readlink "${CAND_SYMLINK}/.codex/hooks.json")" == "../.agents/hooks.json" ]]
  mkdir -p "${CAND_SYMLINK}/site/public"
  printf '<html></html>\n' > "${CAND_SYMLINK}/site/public/index.html"
  [[ -z "$(git -C "${CAND_SYMLINK}" status --porcelain)" ]]
  docs_scripts="${CAND_SYMLINK}/.claude/skills/gpustack-operator-docs/scripts"
  bash "${docs_scripts}/check-docs.sh" "${CAND_SYMLINK}" >/dev/null
  bash "${docs_scripts}/check-specs.sh" "${CAND_SYMLINK}" >/dev/null
  bash "${docs_scripts}/check-crossrefs.sh" "${CAND_SYMLINK}" >/dev/null
  autosync::cleanup_ephemeral_git "${CAND_SYMLINK}"
  [[ ! -e "${CAND_SYMLINK}/.git" ]]
  [[ ! -L "${CAND_SYMLINK}/.claude/skills" && ! -e "${CAND_SYMLINK}/.claude/skills" ]]
  [[ ! -L "${CAND_SYMLINK}/.claude/settings.json" && ! -e "${CAND_SYMLINK}/.claude/settings.json" ]]
  [[ ! -L "${CAND_SYMLINK}/.codex/hooks.json" && ! -e "${CAND_SYMLINK}/.codex/hooks.json" ]]
  [[ ! -e "${CAND_SYMLINK}/site/public" ]]
); then
  pass "ensure_ephemeral_git restores tracked symlinks, excludes site/public, passes docs/specs/crossrefs, and cleans up"
else
  fail "ensure_ephemeral_git restores tracked symlinks, excludes site/public, passes docs/specs/crossrefs, and cleans up"
fi

# 3h. preflight-priors.sh skips heavy --go/--tools hugo warming on no_change and warms --tools hugo when needed.
WARM_SPY_REPO="${SCRATCH}/warm-spy-repo"
WARM_SPY_BIN="${SCRATCH}/warm-spy-bin"
mkdir -p "${WARM_SPY_REPO}/.auto-sync/scripts" "${WARM_SPY_REPO}/.auto-sync/checks" "${WARM_SPY_REPO}/.auto-sync/proposals" "${WARM_SPY_REPO}/pkg/worker/scaling/priors" "${WARM_SPY_BIN}"
cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${WARM_SPY_REPO}/.auto-sync/checks/lib.sh"
cp "${ROOT_DIR}/.auto-sync/scripts/preflight-priors.sh" "${WARM_SPY_REPO}/.auto-sync/scripts/preflight-priors.sh"
cp "${PREFLIGHT_REPO}/pkg/worker/scaling/priors/priors.yaml" "${WARM_SPY_REPO}/pkg/worker/scaling/priors/priors.yaml"
cp "${FIXTURE_SAME}/records.md" "${WARM_SPY_REPO}/pkg/worker/scaling/priors/records.md"
cp "${PREFLIGHT_REPO}/pkg/worker/scaling/priors/identity.go" "${WARM_SPY_REPO}/pkg/worker/scaling/priors/identity.go"
cp "${PREFLIGHT_REPO}/.auto-sync/proposals/priors.json" "${WARM_SPY_REPO}/.auto-sync/proposals/priors.json"
cat > "${WARM_SPY_REPO}/.auto-sync/scripts/lib-preflight.sh" <<'SH'
autosync::preflight::repo_root() { echo "${WARM_SPY_REPO}"; }
autosync::preflight::init_stage() { mkdir -p "${2}"; echo "${2}"; }
autosync::preflight::snapshot_baselines() { :; }
autosync::preflight::apply_force_to_upstream_records() { :; }
autosync::preflight::has_available_upstreams() { return 1; }
autosync::preflight::write_github_output() { :; }
autosync::preflight::warm_caches() {
  printf '%s\n' "$*" >> "${WARM_CALL_LOG}"
  case " $* " in
    *" --go "*) exit 0 ;;
  esac
}
SH
REAL_GIT_BIN="$(command -v git)"
cat > "${WARM_SPY_BIN}/git" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "ls-remote" ]]; then
  printf '2222222222222222222222222222222222222222\tHEAD\n'
  exit 0
fi
exec "${REAL_GIT_BIN}" "$@"
SH
cat > "${WARM_SPY_BIN}/curl" <<'SH'
#!/usr/bin/env bash
out=""
url=""
while (( $# > 0 )); do
  case "$1" in
    -o|--output) out="$2"; shift 2 ;;
    --output=*) out="${1#--output=}"; shift ;;
    -o*) out="${1#-o}"; shift ;;
    http://*|https://*) url="$1"; shift ;;
    -*) shift ;;
    *) url="${url:-$1}"; shift ;;
  esac
done
if [[ -z "${out}" || -z "${url}" ]]; then
  echo "ERROR: mock curl missing output or url" >&2
  exit 1
fi
if [[ "${url}" == *"/availability" ]]; then
  cp "${WARM_LIVE_FIXTURE}/availability.json" "${out}"
elif [[ "${url}" == *"model="* ]]; then
  qid="${url##*model=}"
  cp "${WARM_LIVE_FIXTURE}/${qid}.json" "${out}"
fi
SH
chmod +x "${WARM_SPY_BIN}/git" "${WARM_SPY_BIN}/curl"
if (
  cd "${WARM_SPY_REPO}"
  git init -q -b main 2>/dev/null || git init -q
  git config user.name test
  git config user.email test@example.com
  git config commit.gpgsign false
  git commit -q --allow-empty -m base
  export WARM_SPY_REPO REAL_GIT_BIN

  WARM_CALL_LOG="${SCRATCH}/warm-nochange.log"
  export WARM_CALL_LOG
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_SAME}" bash "${WARM_SPY_REPO}/.auto-sync/scripts/preflight-priors.sh" "${SCRATCH}/warm-stage-1" >/dev/null 2>&1
  [[ -f "${WARM_CALL_LOG}" ]] && ! grep -q -- '--go' "${WARM_CALL_LOG}" && ! grep -q -- '--tools hugo' "${WARM_CALL_LOG}"

  WARM_CALL_LOG="${SCRATCH}/warm-enabled.log"
  export WARM_CALL_LOG
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_SAME}" AUTOSYNC_WARM_TOOLS=true bash "${WARM_SPY_REPO}/.auto-sync/scripts/preflight-priors.sh" "${SCRATCH}/warm-stage-2" >/dev/null 2>&1
  [[ -f "${WARM_CALL_LOG}" ]] && grep -q -- '--tools hugo' "${WARM_CALL_LOG}"

  WARM_CALL_LOG="${SCRATCH}/warm-live.log"
  WARM_LIVE_FIXTURE="${FIXTURE_CHANGED}"
  export WARM_CALL_LOG WARM_LIVE_FIXTURE
  PATH="${WARM_SPY_BIN}:${PATH}" bash "${WARM_SPY_REPO}/.auto-sync/scripts/preflight-priors.sh" "${SCRATCH}/warm-stage-3" >/dev/null 2>&1
  [[ -f "${WARM_CALL_LOG}" ]] && grep -q -- '--tools hugo' "${WARM_CALL_LOG}"
); then
  pass "preflight-priors.sh skips heavy warming on no_change and warms --tools hugo in live/AUTOSYNC_WARM_TOOLS mode"
else
  fail "preflight-priors.sh skips heavy warming on no_change and warms --tools hugo in live/AUTOSYNC_WARM_TOOLS mode"
fi

# 3i. ensure_ephemeral_git hydrates staged Git-LFS pointers in site/ before git commit so Hugo / make site passes offline.
CAND_LFS="${SCRATCH}/cand-lfs"
STAGE_LFS="${SCRATCH}/stage-lfs/priors"
mkdir -p "${STAGE_LFS}"
(
  # shellcheck source=./lib.sh
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
  autosync::copy_tree_without_git "${ROOT_DIR}" "${CAND_LFS}" .sbin .dist .claude .codex
)
rm -rf "${CAND_LFS}/site/public" "${CAND_LFS}/site/resources"
LOGO_POINTER="${SCRATCH}/logo.pointer"
git -C "${ROOT_DIR}" show HEAD:site/assets/images/logo.png > "${LOGO_POINTER}"
cp -f "${LOGO_POINTER}" "${CAND_LFS}/site/assets/images/logo.png"
LOGO_OID="$(awk '$1 == "oid" && $2 ~ /^sha256:[0-9a-f]+$/ && length($2) == 71 { print substr($2, 8); exit }' "${LOGO_POINTER}")"
GIT_COMMON_DIR="$(git -C "${ROOT_DIR}" rev-parse --git-common-dir)"
if [[ "${GIT_COMMON_DIR}" != /* ]]; then
  GIT_COMMON_DIR="${ROOT_DIR}/${GIT_COMMON_DIR}"
fi
LOGO_OBJ_REL="${LOGO_OID:0:2}/${LOGO_OID:2:2}/${LOGO_OID}"
FAVICON_OID="$(git -C "${ROOT_DIR}" show HEAD:pkg/extensionroute/swagger/ui/favicon-16x16.png | awk '$1 == "oid" && $2 ~ /^sha256:[0-9a-f]+$/ && length($2) == 71 { print substr($2, 8); exit }')"
if [[ ! "${FAVICON_OID}" =~ ^[0-9a-f]{64}$ ]]; then
  fail "failed to parse 64-hex Git-LFS OID from pkg/extensionroute/swagger/ui/favicon-16x16.png"
fi

if [[ -n "${LOGO_OID}" && -f "${GIT_COMMON_DIR}/lfs/objects/${LOGO_OBJ_REL}" ]]; then
  if (
    cd "${ROOT_DIR}"
    # shellcheck source=../scripts/lib-preflight.sh
    # shellcheck disable=SC1091
    source "${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh"
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true AUTOSYNC_SKIP_LFS_FETCH=true autosync::preflight::warm_caches "${STAGE_LFS}" --sbin
    [[ -d "${STAGE_LFS}/lfs-objects" && -f "${STAGE_LFS}/lfs-objects/${LOGO_OBJ_REL}" ]]
    [[ ! -e "${STAGE_LFS}/lfs-objects/${FAVICON_OID:0:2}/${FAVICON_OID:2:2}/${FAVICON_OID}" ]]
    staged_lfs_files="$(find -L "${STAGE_LFS}/lfs-objects" -type f)"
    [[ "${staged_lfs_files}" == "${STAGE_LFS}/lfs-objects/${LOGO_OBJ_REL}" ]]
    # shellcheck source=./lib.sh
    # shellcheck disable=SC1091
    source "${CAND_LFS}/.auto-sync/checks/lib.sh"
    AUTOSYNC_STAGE_DIR="${STAGE_LFS}" autosync::ensure_ephemeral_git "${CAND_LFS}"
    hydrated_sha="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "${CAND_LFS}/site/assets/images/logo.png")"
    [[ "${hydrated_sha}" == "${LOGO_OID}" ]]
    [[ -z "$(git -C "${CAND_LFS}" status --porcelain)" ]]
    if [[ -x "${ROOT_DIR}/.sbin/hugo" ]]; then
      mkdir -p "${CAND_LFS}/.sbin"
      cp -f "${ROOT_DIR}/.sbin/hugo" "${CAND_LFS}/.sbin/hugo"
      make -C "${CAND_LFS}" site >/dev/null
      [[ -f "${CAND_LFS}/site/public/index.html" ]]
      [[ -z "$(git -C "${CAND_LFS}" status --porcelain)" ]]
    fi
    autosync::cleanup_ephemeral_git "${CAND_LFS}"
  ); then
    pass "ensure_ephemeral_git hydrates Git-LFS pointers from staged lfs-objects and Hugo make site succeeds"
  else
    fail "ensure_ephemeral_git hydrates Git-LFS pointers from staged lfs-objects and Hugo make site succeeds"
  fi
else
  # Fallback synthetic check when the local checkout has not fetched LFS objects.
  CAND_LFS_SYNTH="${SCRATCH}/cand-lfs-synth"
  STAGE_LFS_SYNTH="${SCRATCH}/stage-lfs-synth"
  mkdir -p "${CAND_LFS_SYNTH}/site/assets/images"
  SYNTH_BYTES="synthetic-lfs-payload"
  SYNTH_OID="$(printf '%s' "${SYNTH_BYTES}" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
  SYNTH_SIZE="${#SYNTH_BYTES}"
  mkdir -p "${STAGE_LFS_SYNTH}/lfs-objects/${SYNTH_OID:0:2}/${SYNTH_OID:2:2}"
  printf '%s' "${SYNTH_BYTES}" > "${STAGE_LFS_SYNTH}/lfs-objects/${SYNTH_OID:0:2}/${SYNTH_OID:2:2}/${SYNTH_OID}"
  printf 'version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %s\n' "${SYNTH_OID}" "${SYNTH_SIZE}" > "${CAND_LFS_SYNTH}/site/assets/images/logo.png"
  if (
    # shellcheck source=./lib.sh
    # shellcheck disable=SC1091
    source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
    AUTOSYNC_STAGE_DIR="${STAGE_LFS_SYNTH}" autosync::ensure_ephemeral_git "${CAND_LFS_SYNTH}"
    [[ "$(cat "${CAND_LFS_SYNTH}/site/assets/images/logo.png")" == "${SYNTH_BYTES}" ]]
    [[ -z "$(git -C "${CAND_LFS_SYNTH}" status --porcelain)" ]]
    autosync::cleanup_ephemeral_git "${CAND_LFS_SYNTH}"
  ); then
    pass "ensure_ephemeral_git hydrates synthetic Git-LFS pointer from staged lfs-objects"
  else
    fail "ensure_ephemeral_git hydrates synthetic Git-LFS pointer from staged lfs-objects"
  fi
fi

# 3j. ensure_ephemeral_git fails closed atomically on corrupted staged Git-LFS object digest without hydrating earlier files.
CAND_LFS_BAD="${SCRATCH}/cand-lfs-bad"
STAGE_LFS_BAD="${SCRATCH}/stage-lfs-bad"
mkdir -p "${CAND_LFS_BAD}/site/assets/images"
GOOD_FIRST_PAYLOAD="first-valid-payload"
GOOD_FIRST_OID="$(printf '%s' "${GOOD_FIRST_PAYLOAD}" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
mkdir -p "${STAGE_LFS_BAD}/lfs-objects/${GOOD_FIRST_OID:0:2}/${GOOD_FIRST_OID:2:2}"
printf '%s' "${GOOD_FIRST_PAYLOAD}" > "${STAGE_LFS_BAD}/lfs-objects/${GOOD_FIRST_OID:0:2}/${GOOD_FIRST_OID:2:2}/${GOOD_FIRST_OID}"
printf 'version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %s\n' "${GOOD_FIRST_OID}" "${#GOOD_FIRST_PAYLOAD}" > "${CAND_LFS_BAD}/site/assets/images/a.png"
BAD_PAYLOAD="good-payload"
BAD_OID="$(printf '%s' "${BAD_PAYLOAD}" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')"
mkdir -p "${STAGE_LFS_BAD}/lfs-objects/${BAD_OID:0:2}/${BAD_OID:2:2}"
printf 'corrupted-payload' > "${STAGE_LFS_BAD}/lfs-objects/${BAD_OID:0:2}/${BAD_OID:2:2}/${BAD_OID}"
printf 'version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %s\n' "${BAD_OID}" "${#BAD_PAYLOAD}" > "${CAND_LFS_BAD}/site/assets/images/z.png"
bad_lfs_err=""
if ! bad_lfs_err="$(
  # shellcheck source=./lib.sh
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
  AUTOSYNC_STAGE_DIR="${STAGE_LFS_BAD}" autosync::ensure_ephemeral_git "${CAND_LFS_BAD}" 2>&1
)" && grep -q "sha256 mismatch" <<<"${bad_lfs_err}" && \
   grep -q '^version https://git-lfs.github.com/spec/v1$' "${CAND_LFS_BAD}/site/assets/images/a.png"; then
  pass "ensure_ephemeral_git fails closed atomically on corrupted staged Git-LFS object digest"
else
  fail "ensure_ephemeral_git fails closed atomically on corrupted staged Git-LFS object digest (${bad_lfs_err})"
fi

echo
if (( FAILS > 0 )); then
  echo "PRIORS SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "PRIORS SELFTEST PASSED: ${PASSES} check(s) passed"
