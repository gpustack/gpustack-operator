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
      cp -R "${src_root}/${rel}" "${dst_root}/$(dirname "${rel}")/"
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
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_PRIORS_FIXTURE_DIR="${FIXTURE_DIGEST}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/${PREFLIGHT_REL}" "${STAGE_DIGEST}" >/dev/null
) && \
python3 - "${STAGE_DIGEST}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "priors-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["changed"] is True, report
assert report["digest_refresh_only"] is True, report
assert report["source_revision"] == base_sha, report
assert "records.md" in report.get("reason", ""), report

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = {r["upstreamId"]: r for r in json.load(f)}
avail = recs["inferencex-availability"]
assert avail["status"] == "available", avail
assert "records.md digests" in avail["reason"], avail
PY
then
  pass "preflight-priors.sh sets digest_refresh_only=true and explicit records.md reason"
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
cp -R "${CAND_BASE}" "${CAND_POS}"
if [[ -x "${CAND_POS}/${VALIDATOR_REL}" ]] && \
   run_validator_env_i "${CAND_POS}" "${STAGE_SAME}" >/dev/null 2>&1 && \
   [[ ! -e "${CAND_POS}/.git" ]] && [[ ! -e "${CAND_POS}/.sbin" ]]; then
  pass "validate_priors.sh passes positive gitless env -i run and cleans up .git/.sbin"
else
  fail "validate_priors.sh passes positive gitless env -i run and cleans up .git/.sbin"
fi

# 3b. Negative case 1: mismatched SHA-256 digest in records.md vs priors-preflight-report.json.
CAND_BAD_SHA="${SCRATCH}/cand-bad-sha"
cp -R "${CAND_BASE}" "${CAND_BAD_SHA}"
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
cp -R "${CAND_BASE}" "${CAND_BAD_CNT}"
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
cp -R "${CAND_BASE}" "${CAND_UNFMT}"
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
cp -R "${CAND_BASE}" "${CAND_BAD_PROP}"
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

echo
if (( FAILS > 0 )); then
  echo "PRIORS SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "PRIORS SELFTEST PASSED: ${PASSES} check(s) passed"
