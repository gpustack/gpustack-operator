#!/usr/bin/env bash
# .auto-sync/scripts/preflight-k8s-deps.sh - host-side preflight for k8s-deps-sync.
#
# Discovers same-minor Kubernetes v0.36.x, controller-runtime, kueue, Helm v3.*,
# and CLI tool releases (skopeo, vals, kustomize, ytt), snapshots baselines, warms
# .sbin + Go + deps-cache, and stages mod_staging() + make generate onto a
# detached source_revision when Go module updates occur and patches apply cleanly.
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

repo_root="$(autosync::preflight::repo_root)"
stage_dir="$(autosync::preflight::init_stage "k8s-deps" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
base_sha="$(git -C "${repo_root}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${stage_dir}" \
  "pack/gpustack-operator/Dockerfile" \
  "hack/deps.sh" \
  "hack/lib/helm.sh" \
  "go.mod" \
  "api"

if [[ -n "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
  autosync::preflight::warm_caches "${stage_dir}" --sbin --go
else
  autosync::preflight::warm_caches "${stage_dir}" --sbin --go \
    --tools protoc,protoc-gen-go,protoc-gen-gogo,goimports,golangci-lint,goimports-reviser,shellcheck,commitsar
fi

if [[ -n "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR:-}" && -d "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR}/deps-cache" ]]; then
  mkdir -p "${stage_dir}/deps-cache"
  cp -RLp "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR}/deps-cache/." "${stage_dir}/deps-cache/"
fi

discovery_json="${stage_dir}/k8s-deps-discovery.json"

python3 - "${repo_root}" "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR:-}" "${discovery_json}" "${stage_dir}" <<'PY'
import json
import os
import re
import subprocess
import sys
import urllib.request

repo_root, fixture_dir, out_path, stage_dir = sys.argv[1:5]


def read_text(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def extract_dockerfile_arg(text: str, name: str) -> str:
    m = re.search(rf'^ARG {re.escape(name)}="([^"]+)"', text, re.MULTILINE)
    if not m:
        raise SystemExit(f"FAIL: missing ARG {name} in pack/gpustack-operator/Dockerfile")
    return m.group(1).strip()


def extract_gomod_require(text: str, mod_path: str) -> str:
    m = re.search(rf'^\s*{re.escape(mod_path)}\s+(v[^\s/]+)', text, re.MULTILINE)
    if not m:
        raise SystemExit(f"FAIL: missing {mod_path} in go.mod")
    return m.group(1).strip()


def parse_semver_tuple(ver: str) -> tuple[int, int, int]:
    cleaned = ver.lstrip("v")
    m = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", cleaned)
    if not m:
        return (0, 0, 0)
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)))


def github_headers() -> dict[str, str]:
    headers = {
        "User-Agent": "gpustack-autosync/1.0",
        "Accept": "application/vnd.github+json",
    }
    tok = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN") or ""
    if tok:
        headers["Authorization"] = f"Bearer {tok}"
    return headers


def fetch_json(url: str):
    req = urllib.request.Request(url, headers=github_headers())
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8"))


def load_releases(fixture_name: str, repo_slug: str) -> list[dict]:
    if fixture_dir:
        fp = os.path.join(fixture_dir, fixture_name)
        if not os.path.isfile(fp):
            raise SystemExit(f"FAIL: missing fixture file: {fp}")
        with open(fp, "r", encoding="utf-8") as f:
            raw = json.load(f)
        return raw if isinstance(raw, list) else [raw]
    raw = fetch_json(f"https://api.github.com/repos/{repo_slug}/releases?per_page=30")
    return raw if isinstance(raw, list) else []


def load_kustomize_tags() -> list[str]:
    if fixture_dir:
        txt_path = os.path.join(fixture_dir, "kustomize-tags.txt")
        json_path = os.path.join(fixture_dir, "kustomize-tags.json")
        if os.path.isfile(txt_path):
            return read_text(txt_path).splitlines()
        if os.path.isfile(json_path):
            with open(json_path, "r", encoding="utf-8") as f:
                data = json.load(f)
            lines = []
            for item in data:
                if isinstance(item, str):
                    lines.append(item)
                elif isinstance(item, dict) and item.get("ref"):
                    lines.append(f"0000000000000000000000000000000000000000\t{item['ref']}")
            return lines
        raise SystemExit(f"FAIL: missing kustomize-tags.txt or kustomize-tags.json in {fixture_dir}")
    out = subprocess.check_output(
        [
            "git",
            "ls-remote",
            "--tags",
            "--refs",
            "https://github.com/kubernetes-sigs/kustomize.git",
            "refs/tags/kustomize/v*",
        ],
        text=True,
    )
    return out.splitlines()


def select_releases(releases: list[dict], pattern: str, strip_v: bool = False) -> list[str]:
    rx = re.compile(pattern)
    seen = set()
    candidates = []
    for rel in releases:
        if not isinstance(rel, dict):
            continue
        if rel.get("prerelease") or rel.get("draft"):
            continue
        tag = str(rel.get("tag_name") or "").strip()
        if not tag:
            continue
        m = rx.fullmatch(tag)
        if not m:
            continue
        val = m.group(1) if strip_v and m.lastindex else (tag.lstrip("v") if strip_v else tag)
        if val not in seen:
            seen.add(val)
            candidates.append(val)
    candidates.sort(key=parse_semver_tuple, reverse=True)
    return candidates


def select_kustomize_versions(lines: list[str]) -> list[str]:
    rx = re.compile(r"refs/tags/kustomize/v(\d+\.\d+\.\d+)$")
    seen = set()
    candidates = []
    for line in lines:
        m = rx.search(line.strip())
        if not m:
            continue
        ver = m.group(1)
        if ver not in seen:
            seen.add(ver)
            candidates.append(ver)
    candidates.sort(key=parse_semver_tuple, reverse=True)
    return candidates


dockerfile_txt = read_text(os.path.join(repo_root, "pack/gpustack-operator/Dockerfile"))
gomod_txt = read_text(os.path.join(repo_root, "go.mod"))
helm_sh_txt = read_text(os.path.join(repo_root, "hack/lib/helm.sh"))

current_skopeo = extract_dockerfile_arg(dockerfile_txt, "SKOPEO_VERSION")
current_helm = extract_dockerfile_arg(dockerfile_txt, "HELM_VERSION")
current_vals = extract_dockerfile_arg(dockerfile_txt, "VALS_VERSION")
current_kustomize = extract_dockerfile_arg(dockerfile_txt, "KUSTOMIZE_VERSION")
current_ytt = extract_dockerfile_arg(dockerfile_txt, "YTT_VERSION")
current_kubectl = extract_dockerfile_arg(dockerfile_txt, "KUBECTL_VERSION")

current_k8s_api = extract_gomod_require(gomod_txt, "k8s.io/api")
k8s_m = re.fullmatch(r"v0\.(\d+)\.(\d+)", current_k8s_api)
if not k8s_m:
    raise SystemExit(f"FAIL: unexpected k8s.io/api version format in go.mod: {current_k8s_api}")
k8s_minor_num = k8s_m.group(1)
k8s_minor = f"0.{k8s_minor_num}"
k8s_release_minor = f"1.{k8s_minor_num}"

current_cr = extract_gomod_require(gomod_txt, "sigs.k8s.io/controller-runtime")
cr_m = re.fullmatch(r"v(0\.\d+)\.\d+", current_cr)
if not cr_m:
    raise SystemExit(f"FAIL: unexpected controller-runtime version in go.mod: {current_cr}")
cr_minor = cr_m.group(1)

current_kueue = extract_gomod_require(gomod_txt, "sigs.k8s.io/kueue")
kueue_m = re.fullmatch(r"v(0\.\d+)\.\d+", current_kueue)
if not kueue_m:
    raise SystemExit(f"FAIL: unexpected kueue version in go.mod: {current_kueue}")
kueue_minor = kueue_m.group(1)

current_helm_mod = extract_gomod_require(gomod_txt, "helm.sh/helm/v3")
helm_major = current_helm.split(".")[0]
if helm_major != "3":
    raise SystemExit(f"FAIL: HELM_VERSION in Dockerfile must stay on v3.*, got {current_helm}")

helm_sh_m = re.search(r'^helm_version=\$\{HELM_VERSION:-"(v3\.\d+\.\d+)"\}$', helm_sh_txt, re.MULTILINE)
if not helm_sh_m:
    raise SystemExit("FAIL: missing v3.* helm_version default in hack/lib/helm.sh")

k8s_candidates = select_releases(
    load_releases("kubernetes-releases.json", "kubernetes/kubernetes"),
    rf"^v1\.{re.escape(k8s_minor_num)}\.\d+$",
)
cr_candidates = select_releases(
    load_releases("controller-runtime-releases.json", "kubernetes-sigs/controller-runtime"),
    rf"^v{re.escape(cr_minor)}\.\d+$",
)
kueue_candidates = select_releases(
    load_releases("kueue-releases.json", "kubernetes-sigs/kueue"),
    rf"^v{re.escape(kueue_minor)}\.\d+$",
)
helm_candidates = select_releases(
    load_releases("helm-releases.json", "helm/helm"),
    r"^v3\.\d+\.\d+$",
)
skopeo_candidates = select_releases(
    load_releases("skopeo-releases.json", "podman-container-tools/skopeo"),
    r"^v?(\d+\.\d+\.\d+)$",
    strip_v=True,
)
vals_candidates = select_releases(
    load_releases("vals-releases.json", "helmfile/vals"),
    r"^v?(\d+\.\d+\.\d+)$",
    strip_v=True,
)
kustomize_candidates = select_kustomize_versions(load_kustomize_tags())
ytt_candidates = select_releases(
    load_releases("ytt-releases.json", "carvel-dev/ytt"),
    r"^v?(\d+\.\d+\.\d+)$",
    strip_v=True,
)


def fetch_text(url: str) -> str:
    req = urllib.request.Request(url, headers={"User-Agent": "gpustack-autosync/1.0"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        return resp.read().decode("utf-8")


def parse_helm_k8s_minor(gomod_text: str) -> tuple[int, int] | None:
    in_require_block = False
    found: list[tuple[int, int]] = []
    block_re = re.compile(r'^"?k8s\.io/(?:api|client-go)"?\s+v(\d+)\.(\d+)\.')
    single_re = re.compile(r'^require\s+"?k8s\.io/(?:api|client-go)"?\s+v(\d+)\.(\d+)\.')
    for raw_line in gomod_text.splitlines():
        line = raw_line.split("//", 1)[0].strip()
        if not line:
            continue
        if in_require_block:
            if line == ")":
                in_require_block = False
                continue
            m = block_re.match(line)
            if m:
                found.append((int(m.group(1)), int(m.group(2))))
            continue
        if re.match(r"^require\s*\($", line):
            in_require_block = True
            continue
        m_single = single_re.match(line)
        if m_single:
            found.append((int(m_single.group(1)), int(m_single.group(2))))
    return max(found) if found else None


def get_helm_k8s_minor(tag: str) -> tuple[bool, tuple[int, int] | None, str]:
    if fixture_dir:
        fp = os.path.join(fixture_dir, f"helm-{tag}-go.mod")
        if not os.path.isfile(fp):
            return True, None, ""
        parsed = parse_helm_k8s_minor(read_text(fp))
        if parsed is None:
            return False, None, f"k8s.io/api or k8s.io/client-go requirement not found in {fp}"
        return True, parsed, ""
    cache_dir = os.path.join(stage_dir, "discovery")
    os.makedirs(cache_dir, exist_ok=True)
    cache_path = os.path.join(cache_dir, f"helm-{tag}-go.mod")
    if os.path.isfile(cache_path) and os.path.getsize(cache_path) > 0:
        parsed = parse_helm_k8s_minor(read_text(cache_path))
        if parsed is None:
            return False, None, f"k8s.io/api or k8s.io/client-go requirement not found in {cache_path}"
        return True, parsed, ""
    url = f"https://raw.githubusercontent.com/helm/helm/{tag}/go.mod"
    try:
        txt = fetch_text(url)
    except Exception as exc:
        return False, None, str(exc)
    with open(cache_path, "w", encoding="utf-8") as f:
        f.write(txt)
    parsed = parse_helm_k8s_minor(txt)
    if parsed is None:
        return False, None, f"k8s.io/api or k8s.io/client-go requirement not found in {url}"
    return True, parsed, ""


def pick_compatible_helm(candidates: list[str], current_tag: str, max_k8s_minor: int) -> tuple[str, list[str]]:
    cur_tuple = parse_semver_tuple(current_tag)
    selected = current_tag
    incompatible = set()
    for cand in candidates:
        if parse_semver_tuple(cand) <= cur_tuple:
            break
        ok, helm_k8s_ver, err = get_helm_k8s_minor(cand)
        if not ok or (helm_k8s_ver is None and not fixture_dir):
            incompatible.add(cand)
            reason = err or "missing k8s.io requirement in go.mod"
            print(
                f"NOTICE: skipping Helm {cand} because its dependency manifest could not be verified ({reason})",
                file=sys.stderr,
            )
            continue
        if helm_k8s_ver is not None and helm_k8s_ver > (0, max_k8s_minor):
            incompatible.add(cand)
            print(
                f"NOTICE: skipping Helm {cand} because it requires k8s.io v{helm_k8s_ver[0]}.{helm_k8s_ver[1]}.x > repo v0.{max_k8s_minor}.x",
                file=sys.stderr,
            )
            continue
        selected = cand
        break
    compatible_recent = [c for c in candidates if c not in incompatible]
    return selected, compatible_recent


def pick_latest(candidates: list[str], fallback: str) -> str:
    if not candidates:
        return fallback
    if parse_semver_tuple(candidates[0]) > parse_semver_tuple(fallback):
        return candidates[0]
    return fallback


current_k8s_release = f"v{current_kubectl}"
target_k8s_release = pick_latest(k8s_candidates, current_k8s_release)
target_k8s_mod = f"v0.{target_k8s_release.lstrip('v').split('.', 1)[1]}"
target_kubectl = target_k8s_release.lstrip("v")

target_cr = pick_latest(cr_candidates, current_cr)
target_kueue = pick_latest(kueue_candidates, current_kueue)
target_helm_tag, compatible_helm_candidates = pick_compatible_helm(
    helm_candidates, f"v{current_helm}", int(k8s_minor_num)
)
target_helm_cli = target_helm_tag.lstrip("v")
target_skopeo = pick_latest(skopeo_candidates, current_skopeo)
target_vals = pick_latest(vals_candidates, current_vals)
target_kustomize = pick_latest(kustomize_candidates, current_kustomize)
target_ytt = pick_latest(ytt_candidates, current_ytt)

doc = {
    "k8s_minor": k8s_minor,
    "k8s_release_minor": k8s_release_minor,
    "helm_major": helm_major,
    "current": {
        "kubernetes": current_k8s_release,
        "k8s_modules": current_k8s_api,
        "controller_runtime": current_cr,
        "kueue_sdk": current_kueue,
        "helm_cli": current_helm,
        "helm_tag": f"v{current_helm}",
        "helm_module": current_helm_mod,
        "skopeo_cli": current_skopeo,
        "vals_cli": current_vals,
        "kustomize_cli": current_kustomize,
        "ytt_cli": current_ytt,
        "kubectl_cli": current_kubectl,
    },
    "target": {
        "kubernetes": target_k8s_release,
        "k8s_modules": target_k8s_mod,
        "controller_runtime": target_cr,
        "kueue_sdk": target_kueue,
        "helm_cli": target_helm_cli,
        "helm_tag": target_helm_tag,
        "helm_module": target_helm_tag,
        "skopeo_cli": target_skopeo,
        "vals_cli": target_vals,
        "kustomize_cli": target_kustomize,
        "ytt_cli": target_ytt,
        "kubectl_cli": target_kubectl,
    },
    "recent": {
        "kubernetes": k8s_candidates[:5],
        "controller_runtime": cr_candidates[:5],
        "kueue_sdk": kueue_candidates[:5],
        "helm_cli": compatible_helm_candidates[:5],
        "skopeo_cli": skopeo_candidates[:5],
        "vals_cli": vals_candidates[:5],
        "kustomize_cli": kustomize_candidates[:5],
        "ytt_cli": ytt_candidates[:5],
    },
}

with open(out_path, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY

read_discovery_field() {
  python3 - "${discovery_json}" "$1" <<'PY'
import json
import sys

doc = json.load(open(sys.argv[1], "r", encoding="utf-8"))
cur = doc
for part in sys.argv[2].split("."):
    cur = cur[part]
print(cur)
PY
}

cur_k8s_mod="$(read_discovery_field current.k8s_modules)"
tgt_k8s_mod="$(read_discovery_field target.k8s_modules)"
cur_cr="$(read_discovery_field current.controller_runtime)"
tgt_cr="$(read_discovery_field target.controller_runtime)"
cur_kueue="$(read_discovery_field current.kueue_sdk)"
tgt_kueue="$(read_discovery_field target.kueue_sdk)"
cur_helm_tag="$(read_discovery_field current.helm_tag)"
tgt_helm_tag="$(read_discovery_field target.helm_tag)"

mods_changed="false"
if [[ "${cur_k8s_mod}" != "${tgt_k8s_mod}" || \
      "${cur_cr}" != "${tgt_cr}" || \
      "${cur_kueue}" != "${tgt_kueue}" || \
      "${cur_helm_tag}" != "${tgt_helm_tag}" ]]; then
  mods_changed="true"
fi

source_revision="${base_sha}"
patch_clean="true"
staging_updated="false"
generate_ran="false"
staging_error=""
helm_fallback_applied="false"
staging_log="${stage_dir}/preflight-staging.log"

emit_sanitized_log_tail() {
  local log_file="$1"
  local label="$2"
  if [[ ! -s "${log_file}" ]]; then
    return 0
  fi
  python3 - "${log_file}" "${label}" <<'PY' >&2
import re
import sys

log_path, label = sys.argv[1:3]
with open(log_path, "r", encoding="utf-8", errors="replace") as f:
    lines = [ln.rstrip("\n") for ln in f]
tail = lines[-20:]
print(f"NOTICE: {label} (last {len(tail)} line(s) from {log_path}):", file=sys.stderr)
for ln in tail:
    sanitized = re.sub(r"\.go:", ".go@", ln)
    sanitized = re.sub(r"::error\b", "::notice", sanitized, flags=re.IGNORECASE)
    sanitized = re.sub(r"##\[error\]", "[error]", sanitized, flags=re.IGNORECASE)
    print(f"  {sanitized}", file=sys.stderr)
PY
}

stage_go_mods_and_generate() {
  local try_helm_tag="$1"
  patch_clean="true"
  staging_error=""
  generate_ran="false"

  if [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" ]] && command -v go >/dev/null 2>&1; then
    python3 - "${work_repo}/go.mod" "${tgt_k8s_mod}" "${cur_cr}" "${tgt_cr}" "${cur_kueue}" "${tgt_kueue}" "${cur_helm_tag}" "${try_helm_tag}" <<'PY'
import re
import sys

gomod_path, tgt_k8s, cur_cr, tgt_cr, cur_kueue, tgt_kueue, cur_helm, tgt_helm = sys.argv[1:9]
with open(gomod_path, "r", encoding="utf-8") as f:
    txt = f.read()

# Only rewrite explicitly lockstepped direct core k8s.io/* modules (never indirect/lagging modules
# such as k8s.io/dynamic-resource-allocation).
LOCKSTEP_K8S_MODS = (
    "k8s.io/api",
    "k8s.io/apiextensions-apiserver",
    "k8s.io/apimachinery",
    "k8s.io/apiserver",
    "k8s.io/cli-runtime",
    "k8s.io/client-go",
    "k8s.io/code-generator",
    "k8s.io/component-base",
    "k8s.io/component-helpers",
    "k8s.io/cri-streaming",
    "k8s.io/kube-aggregator",
    "k8s.io/kubectl",
    "k8s.io/kubelet",
)
k8s_minor = ".".join(tgt_k8s.split(".")[:2])
for mod in LOCKSTEP_K8S_MODS:
    txt = re.sub(
        rf'(^\s*{re.escape(mod)}\s+){re.escape(k8s_minor)}\.\d+\s*$',
        rf'\g<1>{tgt_k8s}',
        txt,
        flags=re.MULTILINE,
    )
if cur_cr != tgt_cr:
    txt = re.sub(
        r'(^\s*sigs\.k8s\.io/controller-runtime\s+)v\S+',
        rf'\g<1>{tgt_cr}',
        txt,
        flags=re.MULTILINE,
    )
if cur_kueue != tgt_kueue:
    txt = re.sub(
        r'(^\s*sigs\.k8s\.io/kueue\s+)v\S+',
        rf'\g<1>{tgt_kueue}',
        txt,
        flags=re.MULTILINE,
    )
if cur_helm != tgt_helm:
    txt = re.sub(
        r'(^\s*helm\.sh/helm/v3\s+)v\S+',
        rf'\g<1>{tgt_helm}',
        txt,
        flags=re.MULTILINE,
    )

with open(gomod_path, "w", encoding="utf-8") as f:
    f.write(txt)
PY
    if ! (
      cd "${work_repo}"
      GOMODCACHE="${stage_dir}/go-mod" \
      GOCACHE="${stage_dir}/go-cache" \
        go mod tidy
      GOMODCACHE="${stage_dir}/go-mod" \
      GOCACHE="${stage_dir}/go-cache" \
        go mod download
    ) >> "${staging_log}" 2>&1; then
      patch_clean="false"
      staging_updated="false"
      staging_error="go mod tidy/download failed while staging ${tgt_k8s_mod}; upstream release tags may still be propagating"
      printf 'WARN: %s\n' "${staging_error}" >&2
      emit_sanitized_log_tail "${staging_log}" "go mod tidy/download output"
      return 0
    fi
  fi

  if [[ "${patch_clean}" == "true" && "${AUTOSYNC_SKIP_GENERATE:-false}" != "true" ]] && command -v make >/dev/null 2>&1; then
    if [[ -d "${stage_dir}/.sbin" ]]; then
      mkdir -p "${work_repo}/.sbin"
      cp -RLp "${stage_dir}/.sbin/." "${work_repo}/.sbin/" 2>/dev/null || true
    fi
    if GOMODCACHE="${stage_dir}/go-mod" \
       GOCACHE="${stage_dir}/go-cache" \
       PATH="${stage_dir}/.sbin:${stage_dir}/.sbin/protoc/bin:${PATH}" \
         autosync::run_in_module_path_copy "${work_repo}" make generate >> "${staging_log}" 2>&1; then
      generate_ran="true"
    else
      patch_clean="false"
      staging_updated="false"
      generate_ran="false"
      staging_error="make generate failed inside staged work_repo"
      printf 'WARN: %s\n' "${staging_error}" >&2
      emit_sanitized_log_tail "${staging_log}" "make generate output"
      return 0
    fi
  fi
}

has_fixture_dir="false"
has_fixture_deps_cache="false"
if [[ -n "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR:-}" ]]; then
  has_fixture_dir="true"
  if [[ -d "${AUTOSYNC_K8S_DEPS_FIXTURE_DIR}/deps-cache" ]]; then
    has_fixture_deps_cache="true"
  fi
fi

can_stage_mods="false"
if [[ "${mods_changed}" == "true" && "${AUTOSYNC_SKIP_SOURCE_REVISION:-false}" != "true" ]]; then
  if [[ "${has_fixture_dir}" == "false" || "${has_fixture_deps_cache}" == "true" ]]; then
    can_stage_mods="true"
  fi
fi

if [[ "${can_stage_mods}" == "true" ]]; then
  work_base="$(mktemp -d "${TMPDIR:-/tmp}/autosync-k8s-deps-work.XXXXXXXX")"
  _PREFLIGHT_TMP_DIRS+=("${work_base}")
  work_repo="${work_base}/gpustack.ai/gpustack"
  mkdir -p "${work_repo}"
  tar --exclude='.git' -cf - -C "${repo_root}" . | tar -xf - -C "${work_repo}"

  if [[ "${cur_k8s_mod}" != "${tgt_k8s_mod}" ]]; then
    python3 - "${work_repo}/hack/deps.sh" "${tgt_k8s_mod}" <<'PY'
import re
import sys

deps_path, target_ver = sys.argv[1:3]
with open(deps_path, "r", encoding="utf-8") as f:
    txt = f.read()

mods = ("code-generator", "api", "apimachinery", "kube-aggregator", "apiextensions-apiserver")
for mod in mods:
    txt = re.sub(
        rf'(https://github\.com/kubernetes/{re.escape(mod)}\s+)v0\.\d+\.\d+(\s+\$\{{staging_dir\}}/k8s\.io/{re.escape(mod)})',
        rf'\g<1>{target_ver}\g<2>',
        txt,
    )

with open(deps_path, "w", encoding="utf-8") as f:
    f.write(txt)
PY

    deps_offline="false"
    if [[ "${has_fixture_dir}" == "true" ]]; then
      deps_offline="true"
    fi

    : > "${staging_log}"
    if ! GPUSTACK_DEPS_STAGE_ONLY=mods \
         GPUSTACK_DEPS_CACHE_DIR="${stage_dir}/deps-cache" \
         GPUSTACK_DEPS_OFFLINE="${deps_offline}" \
         bash "${work_repo}/hack/deps.sh" >> "${staging_log}" 2>&1; then
      patch_clean="false"
      staging_error="hack/deps.sh mod_staging failed for ${tgt_k8s_mod}"
      printf 'WARN: %s\n' "${staging_error}" >&2
      emit_sanitized_log_tail "${staging_log}" "hack/deps.sh mod_staging output"
    fi

    if [[ -n "$(find "${work_repo}/staging" -type f \( -name '*.rej' -o -name '*.orig' \) 2>/dev/null)" ]]; then
      patch_clean="false"
      staging_error="staging patches produced .rej or .orig files"
      printf 'WARN: %s\n' "${staging_error}" >&2
    fi
  fi

  if [[ "${patch_clean}" == "true" ]]; then
    : > "${staging_log}"
    stage_go_mods_and_generate "${tgt_helm_tag}"

    if [[ "${patch_clean}" != "true" && "${cur_helm_tag}" != "${tgt_helm_tag}" ]] && \
       [[ "${cur_k8s_mod}" != "${tgt_k8s_mod}" || "${cur_cr}" != "${tgt_cr}" || "${cur_kueue}" != "${tgt_kueue}" ]]; then
      printf 'NOTICE: staging failed with helm.sh/helm/v3 %s; retrying without helm bump (keeping %s)\n' \
        "${tgt_helm_tag}" "${cur_helm_tag}" >&2
      cp -f "${repo_root}/go.mod" "${work_repo}/go.mod"
      cp -f "${repo_root}/go.sum" "${work_repo}/go.sum"
      if [[ -d "${repo_root}/api" ]]; then
        rm -rf "${work_repo}/api"
        cp -Rp "${repo_root}/api" "${work_repo}/api"
      fi
      if [[ -d "${repo_root}/pkg" ]]; then
        rm -rf "${work_repo}/pkg"
        cp -Rp "${repo_root}/pkg" "${work_repo}/pkg"
      fi
      : > "${staging_log}"
      stage_go_mods_and_generate "${cur_helm_tag}"
      if [[ "${patch_clean}" == "true" ]]; then
        tgt_helm_tag="${cur_helm_tag}"
        helm_fallback_applied="true"
      fi
    fi

    if [[ "${patch_clean}" == "true" ]]; then
      stage_paths=(hack/deps.sh staging go.mod go.sum api pkg)
      source_revision="$(
        autosync::preflight::commit_staged_tree_revision \
          "${repo_root}" \
          "${base_sha}" \
          "chore(deps): stage kubernetes modules and generated outputs" \
          "${work_repo}" \
          "${stage_paths[@]}"
      )"
      if [[ "${source_revision}" != "${base_sha}" ]]; then
        staging_updated="true"
      fi
    fi
  fi
fi

report_json="${stage_dir}/k8s-deps-preflight-report.json"
upstream_records_json="${stage_dir}/upstream-records.json"

python3 - "${discovery_json}" "${report_json}" "${upstream_records_json}" \
  "${base_sha}" "${source_revision}" "${patch_clean}" "${staging_updated}" "${generate_ran}" \
  "${staging_error}" "${helm_fallback_applied}" "${tgt_helm_tag}" <<'PY'
import json
import sys

(
    discovery_path,
    report_path,
    records_path,
    base_sha,
    source_revision,
    patch_clean_str,
    staging_updated_str,
    generate_ran_str,
    staging_error,
    helm_fallback_applied,
    effective_helm_tag,
) = sys.argv[1:12]

with open(discovery_path, "r", encoding="utf-8") as f:
    disc = json.load(f)

cur = disc["current"]
tgt = disc["target"]
recent = disc["recent"]

if helm_fallback_applied == "true":
    tgt["helm_tag"] = effective_helm_tag
    tgt["helm_cli"] = effective_helm_tag.lstrip("v")
    tgt["helm_module"] = effective_helm_tag

patch_clean = patch_clean_str == "true"
go_mod_uids = {"kubernetes", "controller-runtime", "kueue-sdk", "helm-cli"}


def mk_rec(uid: str, cur_v: str, tgt_v: str, rec_list: list[str], note: str):
    changed = cur_v != tgt_v
    if changed and not patch_clean and uid in go_mod_uids:
        return {
            "upstreamId": uid,
            "status": "up_to_date",
            "version": cur_v,
            "recentVersions": rec_list if rec_list else [cur_v],
            "reason": f"held at {cur_v} because preflight staging failed ({staging_error})",
        }
    return {
        "upstreamId": uid,
        "status": "available" if changed else "up_to_date",
        "version": tgt_v if changed else cur_v,
        "recentVersions": rec_list if rec_list else [cur_v],
        "reason": note if changed else f"already at latest compatible version {cur_v}",
    }


records = [
    mk_rec(
        "kubernetes",
        cur["kubernetes"],
        tgt["kubernetes"],
        recent["kubernetes"],
        f"same-minor Kubernetes {disc['k8s_release_minor']}.x patch available ({cur['kubernetes']} -> {tgt['kubernetes']})",
    ),
    mk_rec(
        "controller-runtime",
        cur["controller_runtime"],
        tgt["controller_runtime"],
        recent["controller_runtime"],
        f"same-minor controller-runtime patch available ({cur['controller_runtime']} -> {tgt['controller_runtime']})",
    ),
    mk_rec(
        "kueue-sdk",
        cur["kueue_sdk"],
        tgt["kueue_sdk"],
        recent["kueue_sdk"],
        f"same-minor kueue patch available ({cur['kueue_sdk']} -> {tgt['kueue_sdk']})",
    ),
    mk_rec(
        "helm-cli",
        cur["helm_tag"],
        tgt["helm_tag"],
        recent["helm_cli"],
        f"Helm v{disc['helm_major']}.* patch available ({cur['helm_tag']} -> {tgt['helm_tag']})",
    ),
    mk_rec(
        "skopeo-cli",
        cur["skopeo_cli"],
        tgt["skopeo_cli"],
        recent["skopeo_cli"],
        f"skopeo release available ({cur['skopeo_cli']} -> {tgt['skopeo_cli']})",
    ),
    mk_rec(
        "vals-cli",
        cur["vals_cli"],
        tgt["vals_cli"],
        recent["vals_cli"],
        f"vals release available ({cur['vals_cli']} -> {tgt['vals_cli']})",
    ),
    mk_rec(
        "kustomize-cli",
        cur["kustomize_cli"],
        tgt["kustomize_cli"],
        recent["kustomize_cli"],
        f"kustomize release available ({cur['kustomize_cli']} -> {tgt['kustomize_cli']})",
    ),
    mk_rec(
        "ytt-cli",
        cur["ytt_cli"],
        tgt["ytt_cli"],
        recent["ytt_cli"],
        f"ytt release available ({cur['ytt_cli']} -> {tgt['ytt_cli']})",
    ),
]

failed_target = None
if not patch_clean:
    failed_target = dict(tgt)
    for k in (
        "kubernetes",
        "k8s_modules",
        "kubectl_cli",
        "controller_runtime",
        "kueue_sdk",
        "helm_cli",
        "helm_tag",
        "helm_module",
    ):
        tgt[k] = cur[k]

report = {
    "schemaVersion": 1,
    "base_sha": base_sha,
    "source_revision": source_revision,
    "k8s_minor": disc["k8s_minor"],
    "k8s_release_minor": disc["k8s_release_minor"],
    "helm_major": disc["helm_major"],
    "patch_clean": patch_clean_str == "true",
    "staging_updated": staging_updated_str == "true",
    "generate_ran": generate_ran_str == "true",
    "staging_error": staging_error,
    "current": cur,
    "target": tgt,
    "recent": recent,
}
if failed_target is not None:
    report["failed_target"] = failed_target

with open(report_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")

with open(records_path, "w", encoding="utf-8") as f:
    json.dump(records, f, indent=2)
    f.write("\n")
PY

rm -f "${discovery_json}"

compact_records="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",",":")))' "${upstream_records_json}")"
autosync::preflight::write_github_output "stage_dir" "${stage_dir}"
autosync::preflight::write_github_output "upstream_records" "${compact_records}"
autosync::preflight::write_github_output "source_revision" "${source_revision}"

printf 'OK: preflight-k8s-deps staged at %s (source_revision=%s)\n' "${stage_dir}" "${source_revision}"
