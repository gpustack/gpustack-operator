#!/usr/bin/env bash
# .auto-sync/scripts/preflight-slicing.sh - host-side preflight for slicing-sync.
#
# Clones/fetches upstream HAMi-core and ubs-virt repositories into $AUTOSYNC_STAGE_DIR,
# evaluates watched-path changes against pinned Dockerfile commits, tests Ascend vcann-rt
# patches with CRLF preservation, snapshots baselines, warms .sbin, and emits
# upstream-records.json + slicing-preflight-report.json.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

_PREFLIGHT_TMP_DIRS=()

cleanup_on_exit() {
  local d
  for d in "${_PREFLIGHT_TMP_DIRS[@]+"${_PREFLIGHT_TMP_DIRS[@]}"}"; do
    [[ -n "${d}" && -d "${d}" ]] && rm -rf "${d}"
  done
}
trap cleanup_on_exit EXIT

parse_dockerfile_commit() {
  local dockerfile_path="${1:?dockerfile path required}"
  local arg_name="${2:?arg name required}"
  python3 - "${dockerfile_path}" "${arg_name}" <<'PY'
import re
import sys

path, arg_name = sys.argv[1:3]
pat = re.compile(rf'^ARG {re.escape(arg_name)}="([0-9a-f]{{40}})"$')
with open(path, "r", encoding="utf-8") as f:
    for raw in f:
        m = pat.match(raw.rstrip("\r\n"))
        if m:
            print(m.group(1))
            sys.exit(0)
print(f"FAIL: missing or invalid 40-hex ARG {arg_name} in {path}", file=sys.stderr)
sys.exit(1)
PY
}

sync_upstream_git() {
  local repo_url="${1:?repo url required}"
  local dest_dir="${2:?dest dir required}"
  local pinned_sha="${3:?pinned sha required}"

  if [[ -d "${dest_dir}/.git" ]]; then
    git -C "${dest_dir}" remote set-url origin "${repo_url}"
    git -C "${dest_dir}" -c core.autocrlf=false -c protocol.file.allow=always fetch --prune origin
  else
    rm -rf "${dest_dir}"
    git -c core.autocrlf=false -c protocol.file.allow=always clone --no-single-branch "${repo_url}" "${dest_dir}"
  fi

  git -C "${dest_dir}" remote set-head origin -a >/dev/null 2>&1 || true
  local head_ref=""
  local head_sha=""
  if head_ref="$(git -C "${dest_dir}" symbolic-ref -q refs/remotes/origin/HEAD 2>/dev/null)"; then
    head_sha="$(git -C "${dest_dir}" rev-parse "${head_ref}^{commit}")"
  else
    local cand_ref
    for cand_ref in refs/remotes/origin/main refs/remotes/origin/master HEAD; do
      if git -C "${dest_dir}" rev-parse --verify -q "${cand_ref}^{commit}" >/dev/null 2>&1; then
        head_sha="$(git -C "${dest_dir}" rev-parse "${cand_ref}^{commit}")"
        break
      fi
    done
  fi

  if [[ -z "${head_sha}" ]]; then
    printf 'FAIL: could not resolve upstream HEAD commit in %s\n' "${dest_dir}" >&2
    return 1
  fi

  if ! git -C "${dest_dir}" cat-file -e "${pinned_sha}^{commit}" 2>/dev/null; then
    git -C "${dest_dir}" -c protocol.file.allow=always fetch origin "${pinned_sha}" >/dev/null 2>&1 || {
      printf 'FAIL: pinned commit %s not found in %s\n' "${pinned_sha}" "${repo_url}" >&2
      return 1
    }
  fi

  git -C "${dest_dir}" -c core.autocrlf=false checkout -f -q "${head_sha}"
  printf '%s\n' "${head_sha}"
}

repo_root="$(autosync::preflight::repo_root)"
stage_dir="$(autosync::preflight::init_stage "slicing" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"

autosync::preflight::snapshot_baselines "${stage_dir}" \
  "pack/gpustack-operator/Dockerfile" \
  "pack/gpustack-operator/external/nvidia" \
  "pack/gpustack-operator/external/ascend"
autosync::preflight::warm_caches "${stage_dir}" --sbin

dockerfile="${repo_root}/pack/gpustack-operator/Dockerfile"
pinned_hami="$(parse_dockerfile_commit "${dockerfile}" "LIB_HAMI_CORE_COMMIT")"
pinned_ubs="$(parse_dockerfile_commit "${dockerfile}" "LIB_UBS_VIRT_ENPU_COMMIT")"

hami_url="${AUTOSYNC_SLICING_HAMI_REPO_URL:-https://github.com/Project-HAMi/HAMi-core.git}"
ubs_url="${AUTOSYNC_SLICING_UBS_REPO_URL:-https://gitcode.com/openeuler/ubs-virt.git}"

hami_dir="${stage_dir}/hami-core"
ubs_dir="${stage_dir}/ubs-virt"

head_hami="$(sync_upstream_git "${hami_url}" "${hami_dir}" "${pinned_hami}")"
head_ubs="$(sync_upstream_git "${ubs_url}" "${ubs_dir}" "${pinned_ubs}")"

hami_changed_files="$(git -C "${hami_dir}" diff --name-only "${pinned_hami}" "${head_hami}" -- src include CMakeLists.txt build.sh)"
ubs_changed_files="$(git -C "${ubs_dir}" diff --name-only "${pinned_ubs}" "${head_ubs}" -- ubs-virt-enpu/vcann-rt)"

# Dry-run Ascend vcann-rt patches against a throwaway copy at target HEAD (CRLF-exact).
tmp_patch_root="$(mktemp -d "${TMPDIR:-/tmp}/autosync-slicing-patch.XXXXXXXX")"
_PREFLIGHT_TMP_DIRS+=("${tmp_patch_root}")

patch_clean="true"
patch_errors=""
patch_dir="${repo_root}/pack/gpustack-operator/external/ascend/vcann-rt"
patch_files=()
while IFS= read -r pf; do
  [[ -n "${pf}" ]] && patch_files+=("${pf}")
done < <(find "${patch_dir}" -maxdepth 1 -type f -name '*.patch' | sort)

if (( ${#patch_files[@]} == 0 )); then
  patch_clean="false"
  patch_errors="no *.patch files found under pack/gpustack-operator/external/ascend/vcann-rt"
else
  if git -C "${ubs_dir}" archive "${head_ubs}" ubs-virt-enpu/vcann-rt | tar -x -C "${tmp_patch_root}" && \
     [[ -d "${tmp_patch_root}/ubs-virt-enpu/vcann-rt" ]]; then
    vcann_copy="${tmp_patch_root}/ubs-virt-enpu/vcann-rt"
    for pf in "${patch_files[@]}"; do
      patch_out=""
      if ! patch_out="$(cd "${vcann_copy}" && patch -p1 --dry-run --batch --forward < "${pf}" 2>&1)"; then
        patch_clean="false"
        patch_errors="${patch_errors}${patch_errors:+$'\n'}dry-run failed for $(basename "${pf}"): ${patch_out}"
        continue
      fi
      if ! patch_out="$(cd "${vcann_copy}" && patch -p1 --batch --forward < "${pf}" 2>&1)"; then
        patch_clean="false"
        patch_errors="${patch_errors}${patch_errors:+$'\n'}apply failed for $(basename "${pf}"): ${patch_out}"
      fi
    done
    rej_files="$(find "${vcann_copy}" -name '*.rej' | sort)"
    if [[ -n "${rej_files}" ]]; then
      patch_clean="false"
      patch_errors="${patch_errors}${patch_errors:+$'\n'}reject files produced: ${rej_files}"
    fi
  else
    patch_clean="false"
    patch_errors="ubs-virt-enpu/vcann-rt missing in ubs-virt at ${head_ubs}"
  fi
fi

patch_names_nl=""
for pf in "${patch_files[@]+"${patch_files[@]}"}"; do
  patch_names_nl="${patch_names_nl}${patch_names_nl:+$'\n'}$(basename "${pf}")"
done

records_compact="$(python3 - \
  "${stage_dir}" \
  "${pinned_hami}" "${head_hami}" "${hami_changed_files}" \
  "${pinned_ubs}" "${head_ubs}" "${ubs_changed_files}" \
  "${patch_clean}" "${patch_names_nl}" "${patch_errors}" <<'PY'
import json
import os
import sys

(
    stage_dir,
    pinned_hami,
    head_hami,
    hami_changed_raw,
    pinned_ubs,
    head_ubs,
    ubs_changed_raw,
    patch_clean_raw,
    patch_names_raw,
    patch_errors,
) = sys.argv[1:11]

hami_files = [ln for ln in hami_changed_raw.splitlines() if ln.strip()]
ubs_files = [ln for ln in ubs_changed_raw.splitlines() if ln.strip()]
patch_names = [ln for ln in patch_names_raw.splitlines() if ln.strip()]
patch_clean = patch_clean_raw == "true"

hami_head_changed = pinned_hami != head_hami
ubs_head_changed = pinned_ubs != head_ubs
hami_watched_changed = len(hami_files) > 0
ubs_watched_changed = len(ubs_files) > 0

if hami_watched_changed:
    hami_status = "available"
    hami_ver = head_hami
    hami_recent = [head_hami, pinned_hami] if hami_head_changed else [head_hami]
    hami_reason = f"watched paths changed ({len(hami_files)} file(s)) between {pinned_hami[:12]} and {head_hami[:12]}"
else:
    hami_status = "up_to_date"
    hami_ver = pinned_hami
    hami_recent = [pinned_hami]
    if hami_head_changed:
        hami_reason = f"upstream HEAD {head_hami[:12]} changed only unwatched paths; keeping pinned {pinned_hami[:12]}"
    else:
        hami_reason = f"already at upstream HEAD {pinned_hami[:12]}"

if ubs_watched_changed:
    ubs_status = "available"
    ubs_ver = head_ubs
    ubs_recent = [head_ubs, pinned_ubs] if ubs_head_changed else [head_ubs]
    ubs_reason = (
        f"watched path ubs-virt-enpu/vcann-rt changed ({len(ubs_files)} file(s)) "
        f"between {pinned_ubs[:12]} and {head_ubs[:12]}; patch_clean={str(patch_clean).lower()}"
    )
else:
    ubs_status = "up_to_date"
    ubs_ver = pinned_ubs
    ubs_recent = [pinned_ubs]
    if ubs_head_changed:
        ubs_reason = f"upstream HEAD {head_ubs[:12]} changed only unwatched paths; keeping pinned {pinned_ubs[:12]}"
    else:
        ubs_reason = f"already at upstream HEAD {pinned_ubs[:12]}"

records = [
    {
        "upstreamId": "hami-core",
        "status": hami_status,
        "version": hami_ver,
        "recentVersions": hami_recent,
        "reason": hami_reason,
    },
    {
        "upstreamId": "ubs-virt",
        "status": ubs_status,
        "version": ubs_ver,
        "recentVersions": ubs_recent,
        "reason": ubs_reason,
    },
]

report = {
    "schemaVersion": 1,
    "hami_core": {
        "pinned_commit": pinned_hami,
        "head_commit": head_hami,
        "head_changed": hami_head_changed,
        "watched_paths": ["src", "include", "CMakeLists.txt", "build.sh"],
        "watched_changed": hami_watched_changed,
        "changed_files": hami_files,
    },
    "ubs_virt": {
        "pinned_commit": pinned_ubs,
        "head_commit": head_ubs,
        "head_changed": ubs_head_changed,
        "watched_paths": ["ubs-virt-enpu/vcann-rt"],
        "watched_changed": ubs_watched_changed,
        "changed_files": ubs_files,
        "patch_clean": patch_clean,
        "patch_files": patch_names,
        "patch_errors": patch_errors,
    },
    "patch_clean": patch_clean,
}

with open(os.path.join(stage_dir, "upstream-records.json"), "w", encoding="utf-8") as f:
    json.dump(records, f, indent=2)
    f.write("\n")

with open(os.path.join(stage_dir, "slicing-preflight-report.json"), "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")

print(json.dumps(records, separators=(",", ":")))
PY
)"

base_sha="$(git -C "${repo_root}" rev-parse HEAD)"
autosync::preflight::write_github_output "stage_dir" "${stage_dir}"
autosync::preflight::write_github_output "upstream_records" "${records_compact}"
autosync::preflight::write_github_output "source_revision" "${base_sha}"

printf 'OK: preflight-slicing staged at %s (base=%s)\n' "${stage_dir}" "${base_sha}"
