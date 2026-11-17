#!/usr/bin/env bash
# .auto-sync/checks/validate_kv_transfer.sh - command.v1 verification for kv-transfer-sync.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env "${AUTOSYNC_STAGE_DIR:-}" "${LOOPWRIGHT_CANDIDATE_DIR:-$(pwd -P)}"
cd "${AUTOSYNC_CANDIDATE_ROOT}"

cleanup_on_exit() {
  autosync::cleanup_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"
}
trap cleanup_on_exit EXIT

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

STAGE_DIR="${AUTOSYNC_STAGE_DIR:-}"
if [[ -z "${STAGE_DIR}" || ! -d "${STAGE_DIR}/baseline" ]]; then
  printf 'FAIL: AUTOSYNC_STAGE_DIR must point to a preflight stage directory with baseline/\n' >&2
  exit 1
fi

BASELINE_DIR="${STAGE_DIR}/baseline"

# 1. Shared-file variable-level diff guards.
KV_SETTING_KEYS='kv-cache-backend-image|model-deployment-modelexpress-server-image'
assert_value_go_only_keys \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${KV_SETTING_KEYS}"

assert_settings_md_only_keys \
  "${BASELINE_DIR}/docs/reference/settings.md" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${KV_SETTING_KEYS}"

assert_values_yaml_only_model_transfer_images \
  "${BASELINE_DIR}/deploy/gpustack-operator/chart/values.yaml" \
  "${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/values.yaml"

# 2. Five-block MOONCAKE_VERSION case guard in pack/mirrored-mooncake/Dockerfile.
MOONCAKE_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/mirrored-mooncake/Dockerfile"
if [[ ! -f "${MOONCAKE_DF}" ]]; then
  printf 'FAIL: missing %s\n' "${MOONCAKE_DF}" >&2
  exit 1
fi

python3 - "${MOONCAKE_DF}" <<'PY'
import re
import subprocess
import sys

df_path = sys.argv[1]
with open(df_path, "r", encoding="utf-8") as f:
    text = f.read()

m = re.search(r'^ARG MOONCAKE_VERSION="([^"]+)"', text, re.MULTILINE)
if not m:
    print("FAIL: missing ARG MOONCAKE_VERSION=\"...\" in pack/mirrored-mooncake/Dockerfile", file=sys.stderr)
    sys.exit(1)
pinned = m.group(1).strip()

lines = text.splitlines(keepends=True)
release_guards = []
context_blocks = []
i = 0
while i < len(lines):
    if re.match(r'^\s*case\s+"\$\{MOONCAKE_VERSION\}"\s+in\b', lines[i]):
        start_line = i + 1
        buf = [lines[i]]
        i += 1
        while i < len(lines) and not re.match(r'^\s*esac\b', lines[i]):
            buf.append(lines[i])
            i += 1
        if i >= len(lines):
            print(f"FAIL: unterminated case block at line {start_line}", file=sys.stderr)
            sys.exit(1)
        buf.append(lines[i])
        snippet = "".join(buf)
        if "exit 1" in snippet and "unrecognized release" in snippet:
            release_guards.append((start_line, snippet))
        if "context_manager.cpp" in snippet:
            context_blocks.append((start_line, snippet))
    i += 1

if len(release_guards) < 4:
    print(
        f"FAIL: expected at least 4 MOONCAKE_VERSION release-guard case blocks, found {len(release_guards)}",
        file=sys.stderr,
    )
    sys.exit(1)

if len(context_blocks) < 1:
    print(
        "FAIL: expected 5th MOONCAKE_VERSION context_manager.cpp case block in pack/mirrored-mooncake/Dockerfile",
        file=sys.stderr,
    )
    sys.exit(1)

for start_line, snippet in release_guards:
    res = subprocess.run(
        ["bash", "-e", "-c", 'MOONCAKE_VERSION="$1"\n' + snippet, "_", pinned],
        capture_output=True,
        text=True,
    )
    if res.returncode != 0:
        print(
            f"FAIL: MOONCAKE_VERSION=\"{pinned}\" rejected by release-guard case block at line {start_line}: {res.stderr.strip()}",
            file=sys.stderr,
        )
        sys.exit(1)
PY

# 3. Byte-level HIXL parity with gpustack/runner & patch dry-run against staged hixl.
ENFORCE_RUNNER_PARITY="true"
if [[ -f "${STAGE_DIR}/kv-transfer-preflight-report.json" ]]; then
  ENFORCE_RUNNER_PARITY="$(python3 - "${STAGE_DIR}/kv-transfer-preflight-report.json" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    rep = json.load(f)
val = rep.get("enforce_runner_hixl_parity", True)
print("true" if val else "false")
PY
)"
fi

MT_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/model-transfer/Dockerfile"
HIXL_PATCH="${AUTOSYNC_CANDIDATE_ROOT}/pack/model-transfer/patches/hixl/001_channel_generation.patch"
CAND_HIXL_COMMIT="$(python3 - "${MT_DF}" <<'PY'
import re
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
m = re.search(r'^ARG HIXL_COMMIT="([^"]+)"', txt, re.MULTILINE)
if not m:
    raise SystemExit("FAIL: missing ARG HIXL_COMMIT in pack/model-transfer/Dockerfile")
print(m.group(1).strip())
PY
)"

if [[ "${ENFORCE_RUNNER_PARITY}" == "true" && -d "${STAGE_DIR}/gpustack-runner" ]]; then
  RUNNER_CANN_DF="${STAGE_DIR}/gpustack-runner/pack/cann/Dockerfile.vllm"
  RUNNER_PATCH="${STAGE_DIR}/gpustack-runner/pack/cann/patches/hixl/001_channel_generation.patch"
  if [[ -f "${RUNNER_CANN_DF}" ]]; then
    RUNNER_HIXL_COMMIT="$(python3 - "${RUNNER_CANN_DF}" <<'PY'
import re
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
m = re.search(r'^ARG VLLM_HIXL_COMMIT=(?:"([^"]+)"|([^\s#]+))', txt, re.MULTILINE)
if m:
    print((m.group(1) or m.group(2)).strip())
PY
)"
    if [[ -n "${RUNNER_HIXL_COMMIT}" && "${CAND_HIXL_COMMIT}" != "${RUNNER_HIXL_COMMIT}" ]]; then
      printf 'FAIL: HIXL_COMMIT (%s) != gpustack/runner VLLM_HIXL_COMMIT (%s)\n' \
        "${CAND_HIXL_COMMIT}" "${RUNNER_HIXL_COMMIT}" >&2
      exit 1
    fi
  fi
  if [[ -f "${RUNNER_PATCH}" ]]; then
    if ! cmp -s "${HIXL_PATCH}" "${RUNNER_PATCH}"; then
      printf 'FAIL: %s is not byte-identical to gpustack/runner %s\n' \
        "pack/model-transfer/patches/hixl/001_channel_generation.patch" \
        "pack/cann/patches/hixl/001_channel_generation.patch" >&2
      exit 1
    fi
  fi
fi

if [[ -d "${STAGE_DIR}/hixl" ]]; then
  HIXL_TMP="$(mktemp -d "${TMPDIR:-/tmp}/autosync-hixl-check.XXXXXXXX")"
  if ! cp -R "${STAGE_DIR}/hixl/." "${HIXL_TMP}/"; then
    rm -rf "${HIXL_TMP}"
    printf 'FAIL: unable to copy staged hixl directory\n' >&2
    exit 1
  fi
  if [[ -d "${HIXL_TMP}/.git" ]]; then
    if ! git -C "${HIXL_TMP}" checkout -q --detach "${CAND_HIXL_COMMIT}" 2>/dev/null; then
      rm -rf "${HIXL_TMP}"
      printf 'FAIL: HIXL_COMMIT %s not present in staged hixl tree\n' "${CAND_HIXL_COMMIT}" >&2
      exit 1
    fi
  fi
  if ! (cd "${HIXL_TMP}" && patch --dry-run --batch --fuzz=0 -p1 < "${HIXL_PATCH}" >/dev/null); then
    rm -rf "${HIXL_TMP}"
    printf 'FAIL: HIXL patch --dry-run failed against %s at %s\n' "${STAGE_DIR}/hixl" "${CAND_HIXL_COMMIT}" >&2
    exit 1
  fi
  rm -rf "${HIXL_TMP}"
fi

# 4. ModelExpress --locked builder verification when MODELEXPRESS_COMMIT changed.
BASE_MX_DF="${BASELINE_DIR}/pack/mirrored-modelexpress-server/Dockerfile"
CAND_MX_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/mirrored-modelexpress-server/Dockerfile"
if [[ -f "${BASE_MX_DF}" && -f "${CAND_MX_DF}" ]]; then
  MX_CHANGED="$(python3 - "${BASE_MX_DF}" "${CAND_MX_DF}" <<'PY'
import re
import sys

def get_commit(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        m = re.search(r'^ARG MODELEXPRESS_COMMIT=([^\s#]+)', f.read(), re.MULTILINE)
        return m.group(1).strip() if m else ""

print("true" if get_commit(sys.argv[1]) != get_commit(sys.argv[2]) else "false")
PY
)"
  if [[ "${MX_CHANGED}" == "true" ]]; then
    BUILD_CMD="${AUTOSYNC_DOCKER_BUILD_CMD:-docker buildx build -f pack/mirrored-modelexpress-server/Dockerfile --target builder --platform linux/amd64 pack/mirrored-modelexpress-server}"
    if ! bash -c "${BUILD_CMD}"; then
      printf 'FAIL: ModelExpress builder verification failed\n' >&2
      exit 1
    fi
  fi
fi

# 5. Two-Stage Image Publication Gate and value.go <-> settings.md alignment.
MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
IMAGE_META_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-kv-images.XXXXXXXX")"
python3 - \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${BASELINE_DIR}/deploy/gpustack-operator/chart/values.yaml" \
  "${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/values.yaml" \
  > "${IMAGE_META_FILE}" <<'PY'
import re
import sys

base_vg_p, cand_vg_p, cand_sm_p, base_vy_p, cand_vy_p = sys.argv[1:6]


def read(p: str) -> str:
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


def vg_image(txt: str, var_name: str) -> str:
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        txt,
        re.DOTALL,
    )
    if not m:
        raise SystemExit(f"FAIL: missing {var_name} in value.go")
    return m.group(1).strip()


def sm_default(txt: str, key: str) -> str:
    m = re.search(rf'^\| `{re.escape(key)}`<br/>.*?<br/>Default: `([^`]+)`', txt, re.MULTILINE)
    if not m:
        raise SystemExit(f"FAIL: missing {key} Default row in settings.md")
    return m.group(1).strip()


def vy_mt_images(txt: str) -> dict[str, str]:
    out = {}
    in_mt = False
    in_images = False
    for raw in txt.splitlines():
        if raw.strip() == "" or raw.lstrip().startswith("#"):
            continue
        if not raw.startswith(" "):
            in_mt = raw == "modelTransfer:" or raw.startswith("modelTransfer: ")
            in_images = False
        elif in_mt and re.match(r"^  [A-Za-z0-9_-]+:", raw):
            in_images = raw == "  images:" or raw.startswith("  images: ")
        elif in_mt and in_images:
            m = re.match(r'^    (nvidia|amd|ascend):\s*(?:"([^"]+)"|\'([^\']+)\'|([^\s#]+))', raw)
            if m:
                out[m.group(1)] = (m.group(2) or m.group(3) or m.group(4)).strip()
    return out


base_vg = read(base_vg_p)
cand_vg = read(cand_vg_p)
cand_sm = read(cand_sm_p)
base_vy = read(base_vy_p)
cand_vy = read(cand_vy_p)

kv_img = vg_image(cand_vg, "KVCacheBackendImage")
mx_img = vg_image(cand_vg, "ModelDeploymentModelExpressServerImage")
if kv_img != sm_default(cand_sm, "kv-cache-backend-image"):
    raise SystemExit("FAIL: value.go KVCacheBackendImage disagrees with settings.md kv-cache-backend-image Default")
if mx_img != sm_default(cand_sm, "model-deployment-modelexpress-server-image"):
    raise SystemExit("FAIL: value.go ModelDeploymentModelExpressServerImage disagrees with settings.md model-deployment-modelexpress-server-image Default")

base_kv = vg_image(base_vg, "KVCacheBackendImage")
base_mx = vg_image(base_vg, "ModelDeploymentModelExpressServerImage")

base_mt = vy_mt_images(base_vy)
cand_mt = vy_mt_images(cand_vy)
for k in ("nvidia", "amd", "ascend"):
    if k not in cand_mt:
        raise SystemExit(f"FAIL: missing modelTransfer.images.{k} in values.yaml")

images_changed = (
    kv_img != base_kv
    or mx_img != base_mx
    or cand_mt != base_mt
)

print(f"IMAGES_CHANGED={'true' if images_changed else 'false'}")
print(f"KV_IMG={kv_img}")
print(f"MX_IMG={mx_img}")
print(f"MT_NVIDIA={cand_mt['nvidia']}")
print(f"MT_AMD={cand_mt['amd']}")
print(f"MT_ASCEND={cand_mt['ascend']}")
PY

IMAGES_CHANGED=""
KV_IMG=""
MX_IMG=""
MT_NVIDIA=""
MT_AMD=""
MT_ASCEND=""
while IFS='=' read -r k v; do
  case "${k}" in
    IMAGES_CHANGED) IMAGES_CHANGED="${v}" ;;
    KV_IMG) KV_IMG="${v}" ;;
    MX_IMG) MX_IMG="${v}" ;;
    MT_NVIDIA) MT_NVIDIA="${v}" ;;
    MT_AMD) MT_AMD="${v}" ;;
    MT_ASCEND) MT_ASCEND="${v}" ;;
  esac
done < "${IMAGE_META_FILE}"
rm -f "${IMAGE_META_FILE}"

assert_rocm_amd64_published() {
  local manifests_json="$1"
  local image_ref="$2"
  if [[ ! -f "${manifests_json}" ]]; then
    printf 'FAIL: missing manifests report: %s\n' "${manifests_json}" >&2
    return 1
  fi
  python3 - "${manifests_json}" "${image_ref}" <<'PY'
import json
import re
import sys

manifests_path, image_ref = sys.argv[1:3]
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
with open(manifests_path, "r", encoding="utf-8") as f:
    data = json.load(f)
images = data.get("images", {})
entry = None
for key in (image_ref, f"docker.io/{image_ref}"):
    if key in images:
        entry = images[key]
        break
if not isinstance(entry, dict):
    print(f"FAIL: image not found in manifests report: {image_ref}", file=sys.stderr)
    sys.exit(1)
amd64 = str(entry.get("amd64Digest") or "")
if not DIGEST_RE.fullmatch(amd64):
    print(f"FAIL: rocm image {image_ref} missing valid amd64Digest", file=sys.stderr)
    sys.exit(1)
arm64 = str(entry.get("arm64Digest") or "")
if arm64:
    if not DIGEST_RE.fullmatch(arm64) or arm64 == amd64 or entry.get("published") is not True:
        print(f"FAIL: image {image_ref} has invalid multi-arch manifest entry", file=sys.stderr)
        sys.exit(1)
PY
}

if [[ "${IMAGES_CHANGED}" == "true" || -f "${MANIFESTS_JSON}" ]]; then
  autosync::assert_image_published "${MANIFESTS_JSON}" "${KV_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MX_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_NVIDIA}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_ASCEND}"
  if [[ "${MT_AMD}" == *-rocm* ]]; then
    assert_rocm_amd64_published "${MANIFESTS_JSON}" "${MT_AMD}"
  else
    autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_AMD}"
  fi
fi

# 6. Chart / settings / docs regeneration and test gate when Stage B files changed.
PROMOTED_FILES_CHANGED="false"
for rel in \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md"; do
  if [[ -f "${BASELINE_DIR}/${rel}" && -f "${AUTOSYNC_CANDIDATE_ROOT}/${rel}" ]]; then
    if ! cmp -s "${BASELINE_DIR}/${rel}" "${AUTOSYNC_CANDIDATE_ROOT}/${rel}"; then
      PROMOTED_FILES_CHANGED="true"
      break
    fi
  fi
done

if [[ "${PROMOTED_FILES_CHANGED}" == "true" && "${AUTOSYNC_FAST_SELFTEST:-false}" != "true" ]]; then
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" generate chart
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint chart
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint docs
  go test ./pkg/worker/settings/...
  git -C "${AUTOSYNC_CANDIDATE_ROOT}" diff --exit-code
fi
