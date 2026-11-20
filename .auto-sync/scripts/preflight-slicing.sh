#!/usr/bin/env bash
# .auto-sync/scripts/preflight-slicing.sh - Taskpack 2.0 preflight for slicing-sync.
#
# Clones/fetches upstream HAMi-core and ubs-virt repositories into $AUTOSYNC_STAGE_DIR,
# evaluates watched-path changes against pinned Dockerfile commits, tests Ascend vcann-rt
# patches with CRLF preservation, snapshots baselines, warms .sbin, and emits
# Taskpack 2.0 preflight outputs and staged resources.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"
# shellcheck source=../checks/lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/../checks/lib.sh"

_PREFLIGHT_TMP_DIRS=()

cleanup_on_exit() {
  local d
  for d in "${_PREFLIGHT_TMP_DIRS[@]+"${_PREFLIGHT_TMP_DIRS[@]}"}"; do
    [[ -n "${d}" && -d "${d}" ]] && rm -rf "${d}"
  done
}
trap cleanup_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

sync_upstream_git() {
  local repo_url="${1:?repo url required}"
  local dest_dir="${2:?dest dir required}"
  local pinned_sha="${3:?pinned sha required}"
  local allow_file_proto="${4:-false}"

  local proto_opts=()
  local filter_opts=()
  if [[ "${allow_file_proto}" == "true" ]]; then
    proto_opts=(-c protocol.file.allow=always)
  else
    filter_opts=(--filter=blob:none)
  fi

  if [[ -d "${dest_dir}/.git" ]]; then
    git -C "${dest_dir}" remote set-url origin "${repo_url}"
    if (( ${#filter_opts[@]} > 0 )); then
      git -C "${dest_dir}" -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" fetch "${filter_opts[@]}" --prune origin || \
        git -C "${dest_dir}" -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" fetch --prune origin
    else
      git -C "${dest_dir}" -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" fetch --prune origin
    fi
  else
    rm -rf "${dest_dir}"
    if (( ${#filter_opts[@]} > 0 )); then
      if ! git -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" clone "${filter_opts[@]}" --no-single-branch "${repo_url}" "${dest_dir}"; then
        rm -rf "${dest_dir}"
        git -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" clone --no-single-branch "${repo_url}" "${dest_dir}"
      fi
    else
      git -c core.autocrlf=false "${proto_opts[@]+"${proto_opts[@]}"}" clone --no-single-branch "${repo_url}" "${dest_dir}"
    fi
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
    git -C "${dest_dir}" "${proto_opts[@]+"${proto_opts[@]}"}" fetch origin "${pinned_sha}" >/dev/null 2>&1 || true
  fi

  git -C "${dest_dir}" -c core.autocrlf=false checkout -f -q "${head_sha}"
  printf '%s\n' "${head_sha}"
}

repo_root="$(autosync::preflight::repo_root)"
stage_dir="$(autosync::preflight::init_stage "slicing" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
base_sha="$(git -C "${repo_root}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${stage_dir}" \
  "pack/gpustack-operator/Dockerfile" \
  "pack/gpustack-operator/external/nvidia" \
  "pack/gpustack-operator/external/ascend"
autosync::preflight::warm_caches "${stage_dir}" --sbin

dockerfile="${repo_root}/pack/gpustack-operator/Dockerfile"
pinned_hami="$(autosync::parse_dockerfile_arg_commit "${dockerfile}" "LIB_HAMI_CORE_COMMIT")"
pinned_ubs="$(autosync::parse_dockerfile_arg_commit "${dockerfile}" "LIB_UBS_VIRT_ENPU_COMMIT")"

# AUTOSYNC_SLICING_HAMI_REPO_URL and AUTOSYNC_SLICING_UBS_REPO_URL are test-only
# overrides for offline self-tests using local directories or file:// repositories.
hami_allow_file="false"
if [[ -n "${AUTOSYNC_SLICING_HAMI_REPO_URL:-}" ]]; then
  hami_url="${AUTOSYNC_SLICING_HAMI_REPO_URL}"
  if [[ "${hami_url}" == file://* || "${hami_url}" != *"://"* ]]; then
    hami_allow_file="true"
  fi
else
  hami_url="https://github.com/Project-HAMi/HAMi-core.git"
fi

ubs_allow_file="false"
if [[ -n "${AUTOSYNC_SLICING_UBS_REPO_URL:-}" ]]; then
  ubs_url="${AUTOSYNC_SLICING_UBS_REPO_URL}"
  if [[ "${ubs_url}" == file://* || "${ubs_url}" != *"://"* ]]; then
    ubs_allow_file="true"
  fi
else
  ubs_url="https://gitcode.com/openeuler/ubs-virt.git"
fi

hami_dir="${stage_dir}/hami-core"
ubs_dir="${stage_dir}/ubs-virt"

head_hami="$(sync_upstream_git "${hami_url}" "${hami_dir}" "${pinned_hami}" "${hami_allow_file}")"
head_ubs="$(sync_upstream_git "${ubs_url}" "${ubs_dir}" "${pinned_ubs}" "${ubs_allow_file}")"

hami_pin_reachable="true"
hami_changed_files=""
if git -C "${hami_dir}" cat-file -e "${pinned_hami}^{commit}" 2>/dev/null; then
  hami_changed_files="$(git -C "${hami_dir}" diff --name-only "${pinned_hami}" "${head_hami}" -- src include CMakeLists.txt build.sh)"
else
  hami_pin_reachable="false"
fi

ubs_pin_reachable="true"
ubs_changed_files=""
if git -C "${ubs_dir}" cat-file -e "${pinned_ubs}^{commit}" 2>/dev/null; then
  ubs_changed_files="$(git -C "${ubs_dir}" diff --name-only "${pinned_ubs}" "${head_ubs}" -- ubs-virt-enpu/vcann-rt ubs-virt-enpu/common)"
else
  ubs_pin_reachable="false"
fi

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

python3 - \
  "${stage_dir}" \
  "${pinned_hami}" "${head_hami}" "${hami_changed_files}" "${hami_pin_reachable}" \
  "${pinned_ubs}" "${head_ubs}" "${ubs_changed_files}" "${ubs_pin_reachable}" \
  "${patch_clean}" "${patch_names_nl}" "${patch_errors}" "${base_sha}" <<'PY'
import json
import os
import sys

(
    stage_dir,
    pinned_hami,
    head_hami,
    hami_changed_raw,
    hami_pin_reachable_raw,
    pinned_ubs,
    head_ubs,
    ubs_changed_raw,
    ubs_pin_reachable_raw,
    patch_clean_raw,
    patch_names_raw,
    patch_errors,
    base_sha,
) = sys.argv[1:14]

hami_files = [ln for ln in hami_changed_raw.splitlines() if ln.strip()]
ubs_files = [ln for ln in ubs_changed_raw.splitlines() if ln.strip()]
patch_names = [ln for ln in patch_names_raw.splitlines() if ln.strip()]
patch_clean = patch_clean_raw == "true"
hami_pin_reachable = hami_pin_reachable_raw == "true"
ubs_pin_reachable = ubs_pin_reachable_raw == "true"

hami_head_changed = pinned_hami != head_hami
ubs_head_changed = pinned_ubs != head_ubs
hami_watched_changed = len(hami_files) > 0
ubs_watched_changed = len(ubs_files) > 0

if not hami_pin_reachable:
    hami_status = "available"
    hami_ver = head_hami
    hami_recent = [head_hami]
    hami_reason = (
        f"pinned commit {pinned_hami} is unreachable in upstream history; "
        f"advance to {head_hami} and verify patches"
    )
elif hami_watched_changed:
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

if not ubs_pin_reachable:
    ubs_status = "available"
    ubs_ver = head_ubs
    ubs_recent = [head_ubs]
    ubs_reason = (
        f"pinned commit {pinned_ubs} is unreachable in upstream history; "
        f"advance to {head_ubs} and verify patches (patch_clean={str(patch_clean).lower()})"
    )
elif ubs_watched_changed:
    ubs_status = "available"
    ubs_ver = head_ubs
    ubs_recent = [head_ubs, pinned_ubs] if ubs_head_changed else [head_ubs]
    ubs_reason = (
        f"watched paths ubs-virt-enpu/{{vcann-rt,common}} changed ({len(ubs_files)} file(s)) "
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
        "id": "hami-core",
        "status": hami_status,
        "version": hami_ver,
        **({"currentVersion": pinned_hami} if hami_status == "available" else {}),
        "recentVersions": hami_recent,
        "reason": hami_reason,
    },
    {
        "id": "ubs-virt",
        "status": ubs_status,
        "version": ubs_ver,
        **({"currentVersion": pinned_ubs} if ubs_status == "available" else {}),
        "recentVersions": ubs_recent,
        "reason": ubs_reason,
    },
]

report = {
    "schemaVersion": 1,
    "base_sha": base_sha,
    "hami_core": {
        "pinned_commit": pinned_hami,
        "pinned_commit_reachable": hami_pin_reachable,
        "head_commit": head_hami,
        "head_changed": hami_head_changed,
        "watched_paths": ["src", "include", "CMakeLists.txt", "build.sh"],
        "watched_changed": hami_watched_changed,
        "changed_files": hami_files,
    },
    "ubs_virt": {
        "pinned_commit": pinned_ubs,
        "pinned_commit_reachable": ubs_pin_reachable,
        "head_commit": head_ubs,
        "head_changed": ubs_head_changed,
        "watched_paths": ["ubs-virt-enpu/vcann-rt", "ubs-virt-enpu/common"],
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
PY

autosync::preflight::apply_force_to_upstream_records "${stage_dir}/upstream-records.json"

emit_args=()
if autosync::preflight::has_available_upstreams "${stage_dir}/upstream-records.json"; then
  tmp_stage_bundle="$(mktemp -d "${TMPDIR:-/tmp}/autosync-slicing-bundle.XXXXXXXX")"
  _PREFLIGHT_TMP_DIRS+=("${tmp_stage_bundle}")
  staged_tree="${tmp_stage_bundle}/staged-tree"
  mkdir -p "${staged_tree}"

  bundle_specs=(
    "${stage_dir}/slicing-preflight-report.json"
    "${stage_dir}/upstream-records.json"
  )

  slicing_flags="$(python3 - "${stage_dir}/upstream-records.json" "${patch_clean}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    by_id = {(r.get("id") or r.get("upstreamId")): r["status"] for r in json.load(f)}
hami_avail = by_id.get("hami-core") == "available"
ubs_avail = by_id.get("ubs-virt") == "available" or sys.argv[2] == "false"
print(f"{str(hami_avail).lower()}|{str(ubs_avail).lower()}")
PY
)"
  hami_avail="${slicing_flags%%|*}"
  ubs_stage_ctx="${slicing_flags#*|}"

  if [[ "${ubs_stage_ctx}" == "true" ]]; then
    ubs_extract="${tmp_stage_bundle}/ubs-extract"
    mkdir -p "${ubs_extract}"
    ubs_archive_paths=(ubs-virt-enpu/vcann-rt)
    if git -C "${ubs_dir}" cat-file -e "${head_ubs}:ubs-virt-enpu/common" 2>/dev/null; then
      ubs_archive_paths+=(ubs-virt-enpu/common)
    fi
    if git -C "${ubs_dir}" archive "${head_ubs}" "${ubs_archive_paths[@]}" | tar -x -C "${ubs_extract}" && \
       [[ -d "${ubs_extract}/ubs-virt-enpu/vcann-rt" ]]; then
      bundle_specs+=("${ubs_extract}/ubs-virt-enpu/vcann-rt::ubs-virt/ubs-virt-enpu/vcann-rt")
      if [[ -d "${ubs_extract}/ubs-virt-enpu/common" ]]; then
        bundle_specs+=("${ubs_extract}/ubs-virt-enpu/common::ubs-virt/ubs-virt-enpu/common")
      fi
    fi
  fi

  if [[ "${hami_avail}" == "true" ]]; then
    for hami_rel in "CMakeLists.txt" "src/CMakeLists.txt" "build.sh"; do
      if [[ -f "${hami_dir}/${hami_rel}" ]]; then
        bundle_specs+=("${hami_dir}/${hami_rel}::hami-core/${hami_rel}")
      fi
    done
  fi

  autosync::preflight::populate_stage_bundle "${staged_tree}" "slicing" "${bundle_specs[@]}"
  emit_args+=(--bundle-dir "${staged_tree}/.auto-sync/.stage/slicing")
fi

autosync::preflight::emit_taskpack_v2 \
  "slicing" "${repo_root}" "${base_sha}" "${stage_dir}" \
  "${stage_dir}/upstream-records.json" "${stage_dir}/slicing-preflight-report.json" \
  "${emit_args[@]+"${emit_args[@]}"}"

printf 'OK: preflight-slicing staged at %s (base=%s)\n' "${stage_dir}" "${base_sha}"
