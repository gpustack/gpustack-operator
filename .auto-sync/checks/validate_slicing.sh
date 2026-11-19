#!/usr/bin/env bash
# .auto-sync/checks/validate_slicing.sh - command.v1 verification gate for slicing-sync.
#
# Enforces Dockerfile ARG diff guard, 40-hex commit format and proposal parity,
# bash -n + symbols.sh checks on NVIDIA/Ascend scripts, CRLF-aware vcann-rt patch
# application against AUTOSYNC_STAGE_DIR/ubs-virt, changed-vendor detection, and
# non-root XB_STAGE xbuild + ELF verification.
set -euo pipefail

SCRIPT_DIR="$(CDPATH="" cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# Inline bootstrap: Loopwright copies gate.script alone to workDir/gate-script.sh.
if [[ ! -f "${SCRIPT_DIR}/lib.sh" && -f "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks/lib.sh" ]]; then
  SCRIPT_DIR="$(CDPATH="" cd -P -- "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks" && pwd -P)"
fi
# shellcheck source=lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env
cd "${AUTOSYNC_CANDIDATE_ROOT}"

_CLEANUP_DIRS=()

cleanup_on_exit() {
  local d
  for d in "${_CLEANUP_DIRS[@]+"${_CLEANUP_DIRS[@]}"}"; do
    [[ -n "${d}" && -d "${d}" ]] && rm -rf "${d}"
  done
  autosync::cleanup_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"
}
trap cleanup_on_exit EXIT

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

cand_dockerfile="${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/Dockerfile"
cand_proposal="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/slicing.json"

# 1. Shared-file diff guard on pack/gpustack-operator/Dockerfile.
has_baseline=false
if [[ -n "${AUTOSYNC_STAGE_DIR:-}" ]]; then
  base_dockerfile="${AUTOSYNC_STAGE_DIR}/baseline/pack/gpustack-operator/Dockerfile"
  if [[ ! -f "${base_dockerfile}" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR is set (%s) but baseline Dockerfile is missing: %s\n' \
      "${AUTOSYNC_STAGE_DIR}" "${base_dockerfile}" >&2
    exit 1
  fi
  has_baseline=true
  assert_dockerfile_only_args \
    "${base_dockerfile}" \
    "${cand_dockerfile}" \
    "LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT"
elif [[ "${AUTOSYNC_ALLOW_MISSING_STAGE_DIR:-false}" != "true" ]]; then
  printf 'FAIL: AUTOSYNC_STAGE_DIR must be set (set AUTOSYNC_ALLOW_MISSING_STAGE_DIR=true only for minimal unit tests)\n' >&2
  exit 1
fi

# 2. Validate 40-hex commit format in Dockerfile and parity with proposals/slicing.json.
cand_hami_commit="$(autosync::parse_dockerfile_arg_commit "${cand_dockerfile}" "LIB_HAMI_CORE_COMMIT")"
cand_ubs_commit="$(autosync::parse_dockerfile_arg_commit "${cand_dockerfile}" "LIB_UBS_VIRT_ENPU_COMMIT")"

python3 - "${cand_proposal}" "${cand_hami_commit}" "${cand_ubs_commit}" <<'PY'
import json
import os
import sys

prop_path, hami_sha, ubs_sha = sys.argv[1:4]
if not os.path.isfile(prop_path):
    print(f"FAIL: missing proposal artifact: {prop_path}", file=sys.stderr)
    sys.exit(1)
with open(prop_path, "r", encoding="utf-8") as f:
    doc = json.load(f)
if not isinstance(doc, dict):
    print("FAIL: slicing.json must be a JSON object", file=sys.stderr)
    sys.exit(1)
if doc.get("schema_version") != 1:
    print(f"FAIL: slicing.json schema_version must be 1, got {doc.get('schema_version')!r}", file=sys.stderr)
    sys.exit(1)
summary = doc.get("summary")
if not isinstance(summary, str) or not summary.strip():
    print("FAIL: slicing.json summary must be a non-empty string", file=sys.stderr)
    sys.exit(1)
if not isinstance(doc.get("subscriptions"), list):
    print("FAIL: slicing.json subscriptions must be a JSON array", file=sys.stderr)
    sys.exit(1)
versions = doc.get("versions")
if not isinstance(versions, dict):
    print("FAIL: slicing.json versions must be a JSON object", file=sys.stderr)
    sys.exit(1)
if versions.get("hami-core") != hami_sha:
    print(
        f"FAIL: slicing.json versions.hami-core ({versions.get('hami-core')!r}) != Dockerfile LIB_HAMI_CORE_COMMIT ({hami_sha})",
        file=sys.stderr,
    )
    sys.exit(1)
if versions.get("ubs-virt") != ubs_sha:
    print(
        f"FAIL: slicing.json versions.ubs-virt ({versions.get('ubs-virt')!r}) != Dockerfile LIB_UBS_VIRT_ENPU_COMMIT ({ubs_sha})",
        file=sys.stderr,
    )
    sys.exit(1)
PY

# 3. Validate bash syntax and ASCII symbol hygiene on external/{nvidia,ascend}/*.sh.
ext_sh_files=()
for ext_dir in \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/external/nvidia" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/external/ascend"; do
  if [[ ! -d "${ext_dir}" ]]; then
    printf 'FAIL: missing external directory: %s\n' "${ext_dir}" >&2
    exit 1
  fi
  while IFS= read -r shf; do
    [[ -n "${shf}" ]] && ext_sh_files+=("${shf}")
  done < <(find "${ext_dir}" -type f -name '*.sh' | sort)
done

if (( ${#ext_sh_files[@]} < 2 )); then
  printf 'FAIL: expected build-libvgpu.sh and build-libvnpu.sh under external/{nvidia,ascend}\n' >&2
  exit 1
fi
for shf in "${ext_sh_files[@]}"; do
  bash -n "${shf}"
done

if [[ ! -f "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/symbols.sh" ]]; then
  printf 'FAIL: missing hack/check/symbols.sh in candidate tree\n' >&2
  exit 1
fi
bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/symbols.sh" "${AUTOSYNC_CANDIDATE_ROOT}" >/dev/null

# 4. CRLF-aware patch dry-run + reject check against AUTOSYNC_STAGE_DIR/ubs-virt.
patch_dir="${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/external/ascend/vcann-rt"
patch_files=()
while IFS= read -r pf; do
  [[ -n "${pf}" ]] && patch_files+=("${pf}")
done < <(find "${patch_dir}" -maxdepth 1 -type f -name '*.patch' | sort)

if (( ${#patch_files[@]} == 0 )); then
  printf 'FAIL: no *.patch files found under %s\n' "${patch_dir}" >&2
  exit 1
fi

if [[ -n "${AUTOSYNC_STAGE_DIR:-}" ]]; then
  ubs_stage_repo="${AUTOSYNC_STAGE_DIR}/ubs-virt"
  if [[ ! -d "${ubs_stage_repo}/.git" ]]; then
    printf 'FAIL: AUTOSYNC_STAGE_DIR is set (%s) but staged ubs-virt git clone is missing: %s\n' \
      "${AUTOSYNC_STAGE_DIR}" "${ubs_stage_repo}" >&2
    exit 1
  fi
  if ! git -C "${ubs_stage_repo}" cat-file -e "${cand_ubs_commit}^{commit}" 2>/dev/null; then
    printf 'FAIL: LIB_UBS_VIRT_ENPU_COMMIT %s not found in %s\n' \
      "${cand_ubs_commit}" "${ubs_stage_repo}" >&2
    exit 1
  fi
  tmp_vcann_root="$(mktemp -d "${TMPDIR:-/tmp}/autosync-vcann-check.XXXXXXXX")"
  _CLEANUP_DIRS+=("${tmp_vcann_root}")

  git -C "${ubs_stage_repo}" archive "${cand_ubs_commit}" ubs-virt-enpu/vcann-rt | tar -x -C "${tmp_vcann_root}"
  vcann_copy="${tmp_vcann_root}/ubs-virt-enpu/vcann-rt"
  if [[ ! -d "${vcann_copy}" ]]; then
    printf 'FAIL: ubs-virt-enpu/vcann-rt missing in ubs-virt at %s\n' "${cand_ubs_commit}" >&2
    exit 1
  fi

  for pf in "${patch_files[@]}"; do
    if ! (cd "${vcann_copy}" && patch -p1 --dry-run --batch --forward < "${pf}"); then
      printf 'FAIL: dry-run patch failed for %s against ubs-virt %s\n' \
        "$(basename "${pf}")" "${cand_ubs_commit}" >&2
      exit 1
    fi
    if ! (cd "${vcann_copy}" && patch -p1 --batch --forward < "${pf}"); then
      printf 'FAIL: patch application failed for %s against ubs-virt %s\n' \
        "$(basename "${pf}")" "${cand_ubs_commit}" >&2
      exit 1
    fi
  done

  rej_found="$(find "${vcann_copy}" -name '*.rej' | sort)"
  if [[ -n "${rej_found}" ]]; then
    printf 'FAIL: patch produced reject file(s): %s\n' "${rej_found}" >&2
    exit 1
  fi
fi

# 5. Changed-vendor detection.
nvidia_changed=false
ascend_changed=false

dir_differs() {
  local base_dir="$1"
  local cand_dir="$2"
  local rc=0
  if [[ ! -d "${base_dir}" || ! -d "${cand_dir}" ]]; then
    printf 'FAIL: missing external comparison dir (%s vs %s)\n' "${base_dir}" "${cand_dir}" >&2
    return 2
  fi
  diff -rq -- "${base_dir}" "${cand_dir}" >/dev/null || rc=$?
  if (( rc == 0 )); then
    return 1
  elif (( rc == 1 )); then
    return 0
  else
    printf 'FAIL: diff -rq exited %d comparing %s vs %s\n' "${rc}" "${base_dir}" "${cand_dir}" >&2
    return 2
  fi
}

if [[ "${has_baseline}" == "true" ]]; then
  base_hami_commit="$(autosync::parse_dockerfile_arg_commit "${base_dockerfile}" "LIB_HAMI_CORE_COMMIT")"
  base_ubs_commit="$(autosync::parse_dockerfile_arg_commit "${base_dockerfile}" "LIB_UBS_VIRT_ENPU_COMMIT")"

  if [[ "${cand_hami_commit}" != "${base_hami_commit}" ]]; then
    nvidia_changed=true
  else
    rc_nv=0
    dir_differs "${AUTOSYNC_STAGE_DIR}/baseline/pack/gpustack-operator/external/nvidia" \
      "${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/external/nvidia" || rc_nv=$?
    if (( rc_nv == 0 )); then
      nvidia_changed=true
    elif (( rc_nv > 1 )); then
      exit 1
    fi
  fi

  if [[ "${cand_ubs_commit}" != "${base_ubs_commit}" ]]; then
    ascend_changed=true
  else
    rc_asc=0
    dir_differs "${AUTOSYNC_STAGE_DIR}/baseline/pack/gpustack-operator/external/ascend" \
      "${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/external/ascend" || rc_asc=$?
    if (( rc_asc == 0 )); then
      ascend_changed=true
    elif (( rc_asc > 1 )); then
      exit 1
    fi
  fi
fi

case "${AUTOSYNC_SLICING_FORCE_XBUILD:-}" in
  "") ;;
  all)
    nvidia_changed=true
    ascend_changed=true
    ;;
  nvidia)
    nvidia_changed=true
    ;;
  ascend)
    ascend_changed=true
    ;;
  none)
    nvidia_changed=false
    ascend_changed=false
    ;;
  *)
    printf 'FAIL: invalid AUTOSYNC_SLICING_FORCE_XBUILD=%s (expected all|nvidia|ascend|none)\n' \
      "${AUTOSYNC_SLICING_FORCE_XBUILD}" >&2
    exit 1
    ;;
esac

# 6. Non-root XB_STAGE xbuild + ELF verification for changed vendors.
run_xbuild_step() {
  local script_rel="${1:?script_rel required}"
  local target="${2:?target required}"
  if [[ -n "${AUTOSYNC_XBUILD_RUNNER:-}" ]]; then
    "${AUTOSYNC_XBUILD_RUNNER}" "${script_rel}" "${target}"
  else
    bash "${AUTOSYNC_CANDIDATE_ROOT}/${script_rel}" "${target}"
  fi
}

if [[ "${nvidia_changed}" == "true" ]]; then
  xb_stage_nvidia="${TMPDIR:-/tmp}/xb-stage-nvidia-$$"
  mkdir -p "${xb_stage_nvidia}"
  _CLEANUP_DIRS+=("${xb_stage_nvidia}")

  export XB_STAGE="${xb_stage_nvidia}"
  export XB_MODE=local
  export XB_REPO="${AUTOSYNC_CANDIDATE_ROOT}"

  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh" "xbuild-nvidia-cuda-12"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh" "xbuild-nvidia-cuda-12"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh" "xbuild-nvidia-cuda-12"

  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh" "xbuild-nvidia-cuda-13"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-1.sh" "xbuild-nvidia-cuda-13"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/nvidia-case-4.sh" "xbuild-nvidia-cuda-13"
fi

if [[ "${ascend_changed}" == "true" ]]; then
  xb_stage_ascend="${TMPDIR:-/tmp}/xb-stage-ascend-$$"
  mkdir -p "${xb_stage_ascend}"
  _CLEANUP_DIRS+=("${xb_stage_ascend}")

  export XB_STAGE="${xb_stage_ascend}"
  export XB_MODE=local
  export XB_REPO="${AUTOSYNC_CANDIDATE_ROOT}"

  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh" "xbuild-ascend-cann-8-910b"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh" "xbuild-ascend-cann-8-910b"

  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/build.sh" "xbuild-ascend-cann-9-910b"
  run_xbuild_step ".agents/skills/gpustack-operator-xbuild-and-verify/cases/ascend-case-1.sh" "xbuild-ascend-cann-9-910b"
fi

printf 'OK: validate_slicing passed (nvidia_changed=%s, ascend_changed=%s)\n' \
  "${nvidia_changed}" "${ascend_changed}"
