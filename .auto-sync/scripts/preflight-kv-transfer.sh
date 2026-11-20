#!/usr/bin/env bash
# .auto-sync/scripts/preflight-kv-transfer.sh - host-side preflight for kv-transfer-sync.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib-preflight.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib-preflight.sh"

REPO_ROOT="$(autosync::preflight::repo_root)"
STAGE_DIR="$(autosync::preflight::init_stage "kv-transfer" "${1:-${AUTOSYNC_STAGE_DIR:-}}")"
BASE_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

autosync::preflight::snapshot_baselines "${STAGE_DIR}" \
  "pkg/worker/settings/value.go" \
  "docs/reference/settings.md" \
  "docs/modules/model-delivery/remote.md" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md" \
  "pack/mirrored-mooncake/Dockerfile" \
  "pack/model-transfer/Dockerfile" \
  "pack/model-transfer/patches/hixl/001_channel_generation.patch" \
  "pack/mirrored-modelexpress-server/Dockerfile"

if [[ -f "${REPO_ROOT}/.auto-sync/setting-image-holds.json" ]]; then
  autosync::preflight::snapshot_baselines "${STAGE_DIR}" ".auto-sync/setting-image-holds.json"
fi

autosync::preflight::warm_caches "${STAGE_DIR}" --sbin

# Extract current repository pins and stage upstream data in Python.
PROBE_LIST_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-kv-probes.XXXXXXXX")"
KV_STAGE_TMP=""
cleanup_kv_preflight() {
  rm -f "${PROBE_LIST_FILE}"
  if [[ -n "${KV_STAGE_TMP:-}" && -d "${KV_STAGE_TMP}" ]]; then
    rm -rf "${KV_STAGE_TMP}"
  fi
}
trap cleanup_kv_preflight EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

DISCOVERY_JSON="${STAGE_DIR}/kv-transfer-discovery.json"

python3 - "${REPO_ROOT}" "${STAGE_DIR}" "${AUTOSYNC_KV_FIXTURE_DIR:-}" "${DISCOVERY_JSON}" "${PROBE_LIST_FILE}" "${BASE_SHA}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.request

repo_root, stage_dir, fixture_dir, discovery_path, probe_list_path, base_sha = sys.argv[1:7]
sys.path.insert(0, os.path.join(repo_root, ".auto-sync", "checks"))
from dockerfile_arg import has_dockerfile_arg, read_dockerfile_arg
from setting_image_holds import (
    domain_holds,
    load_setting_image_holds,
    resolve_holds_file_for_preflight,
    verify_kv_transfer_holds,
)


def read_text(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def extract_value_go_image(text: str, var_name: str) -> str:
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        text,
        re.DOTALL,
    )
    if not m:
        raise SystemExit(f"FAIL: missing {var_name} default in value.go")
    return m.group(1).strip()


def extract_values_yaml_mt_images(text: str) -> dict[str, str]:
    out = {}
    in_mt = False
    in_images = False
    for raw in text.splitlines():
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


def fetch_json(url: str, headers: dict[str, str] | None = None):
    req = urllib.request.Request(url, headers=headers or {"User-Agent": "gpustack-autosync/1.0"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8"))


def parse_version_key(ver: str):
    parts = re.findall(r"\d+|[a-zA-Z]+", ver.lstrip("v"))
    key = []
    for p in parts:
        if p.isdigit():
            key.append((0, int(p), ""))
        else:
            key.append((1, 0, p.lower()))
    return key


def is_prerelease_version(ver: str) -> bool:
    v = ver.lstrip("v")
    if "-" in v:
        return True
    if re.search(r"(?:^|[0-9.])(a|b|rc|alpha|beta|dev)[0-9]*$", v, re.IGNORECASE):
        return True
    return False


def has_manylinux_arch(files: list[dict], arch: str) -> bool:
    pat = re.compile(rf"(?:manylinux|linux)[^/]*{re.escape(arch)}\.whl$")
    for f in files:
        if f.get("yanked"):
            continue
        if f.get("packagetype") != "bdist_wheel":
            continue
        fn = str(f.get("filename") or "")
        if pat.search(fn):
            return True
    return False


def has_meta_wheel(files: list[dict]) -> bool:
    for f in files:
        if f.get("yanked"):
            continue
        if f.get("packagetype") != "bdist_wheel":
            continue
        fn = str(f.get("filename") or "")
        if fn.endswith("-none-any.whl"):
            return True
    return has_manylinux_arch(files, "x86_64") and has_manylinux_arch(files, "aarch64")


def resolve_nixl_pypi(fixture_root: str) -> tuple[str, list[str]]:
    pkgs = {}
    for pkg in ("nixl", "nixl-cu12", "nixl-cu13"):
        if fixture_root:
            fp = os.path.join(fixture_root, "pypi", f"{pkg}.json")
            if not os.path.isfile(fp):
                raise SystemExit(f"FAIL: fixture dir set but missing {fp}")
            with open(fp, "r", encoding="utf-8") as f:
                pkgs[pkg] = json.load(f)
            continue
        pkgs[pkg] = fetch_json(f"https://pypi.org/pypi/{pkg}/json")

    common = set(pkgs["nixl"].get("releases", {}).keys())
    common &= set(pkgs["nixl-cu12"].get("releases", {}).keys())
    common &= set(pkgs["nixl-cu13"].get("releases", {}).keys())

    candidates = []
    for ver in common:
        if is_prerelease_version(ver):
            continue
        f_meta = pkgs["nixl"]["releases"].get(ver, [])
        f_cu12 = pkgs["nixl-cu12"]["releases"].get(ver, [])
        f_cu13 = pkgs["nixl-cu13"]["releases"].get(ver, [])
        if (
            has_meta_wheel(f_meta)
            and has_manylinux_arch(f_cu12, "x86_64")
            and has_manylinux_arch(f_cu12, "aarch64")
            and has_manylinux_arch(f_cu13, "x86_64")
            and has_manylinux_arch(f_cu13, "aarch64")
        ):
            candidates.append(ver)

    if not candidates:
        raise SystemExit("FAIL: no NIXL version with 3-package x86_64+aarch64 wheel parity found")
    candidates.sort(key=parse_version_key, reverse=True)
    return candidates[0], candidates[:5]


def resolve_mooncake_release(fixture_root: str) -> tuple[str, list[str]]:
    releases = None
    if fixture_root:
        fp = os.path.join(fixture_root, "mooncake-release.json")
        if os.path.isfile(fp):
            with open(fp, "r", encoding="utf-8") as f:
                raw = json.load(f)
            releases = raw if isinstance(raw, list) else [raw]
    else:
        headers = {
            "User-Agent": "gpustack-autosync/1.0",
            "Accept": "application/vnd.github+json",
        }
        tok = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN") or ""
        if tok:
            headers["Authorization"] = f"Bearer {tok}"
        releases = fetch_json(
            "https://api.github.com/repos/kvcache-ai/Mooncake/releases?per_page=20",
            headers=headers,
        )

    if not releases:
        return "", []

    stable = []
    for rel in releases:
        if not isinstance(rel, dict):
            continue
        if rel.get("prerelease") or rel.get("draft"):
            continue
        tag = str(rel.get("tag_name") or "").strip()
        if not tag:
            continue
        ver = tag[1:] if tag.startswith("v") else tag
        if is_prerelease_version(ver):
            continue
        stable.append(ver)

    if not stable:
        return "", []
    stable.sort(key=parse_version_key, reverse=True)
    return stable[0], stable[:5]


def sync_git_repo(url: str, dst: str, pin_commit: str = "") -> str:
    git_env = {**os.environ, "LC_ALL": "C", "GIT_LFS_SKIP_SMUDGE": "1"}
    lfs_opts = [
        "-c", "filter.lfs.smudge=",
        "-c", "filter.lfs.process=",
        "-c", "filter.lfs.clean=",
        "-c", "filter.lfs.required=false",
    ]
    if os.path.isdir(os.path.join(dst, ".git")):
        subprocess.run(["git", *lfs_opts, "-C", dst, "fetch", "--depth", "1", "origin"], check=True, env=git_env)
        subprocess.run(["git", *lfs_opts, "-C", dst, "checkout", "-f", "--detach", "FETCH_HEAD"], check=True, env=git_env)
    else:
        if os.path.exists(dst):
            shutil.rmtree(dst)
        subprocess.run(["git", *lfs_opts, "clone", "--depth", "1", url, dst], check=True, env=git_env)
    head_sha = subprocess.check_output(["git", "-C", dst, "rev-parse", "HEAD"], text=True, env=git_env).strip()
    if pin_commit and re.fullmatch(r"[0-9a-f]{40}", pin_commit):
        rc = subprocess.run(
            ["git", "-C", dst, "cat-file", "-e", f"{pin_commit}^{{commit}}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=git_env,
        ).returncode
        if rc != 0:
            # Fail fast on network/transport errors rather than escalating history fetches.
            transport_markers = (
                "unable to access",
                "could not resolve host",
                "failed to connect",
                "connection timed out",
                "connection refused",
                "operation timed out",
                "network is unreachable",
                "ssl certificate",
                "ssl_error",
                "tls handshake",
                "gnutls",
                "the remote end hung up unexpectedly",
                "early eof",
                "recv failure",
                "the requested url returned error",
                "authentication failed",
                "could not read username",
                "repository not found",
            )
            unadvertised_markers = (
                "unadvertised object",
                "not our ref",
                "no such remote ref",
                "couldn't find remote ref",
                "does not allow request for unadvertised object",
            )

            def raise_if_transport_error(stderr_txt: str, context: str) -> None:
                err_txt = (stderr_txt or "").strip()
                err_lower = err_txt.lower()
                is_transport_err = any(m in err_lower for m in transport_markers) and not any(
                    m in err_lower for m in unadvertised_markers
                )
                if is_transport_err:
                    raise SystemExit(f"{context}: {err_txt}")

            max_attempted_depth = 1
            fetch_res = subprocess.run(
                ["git", "-C", dst, "fetch", "--depth", "1", "origin", pin_commit],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                text=True,
                env=git_env,
            )
            if fetch_res.returncode != 0:
                raise_if_transport_error(
                    fetch_res.stderr or "",
                    f"FAIL: git fetch of pinned commit {pin_commit} from {url} failed with transport error",
                )

                for depth in (64, 256, 1024):
                    max_attempted_depth = depth
                    f_res = subprocess.run(
                        ["git", "-C", dst, "fetch", "--depth", str(depth), "origin"],
                        env=git_env,
                        capture_output=True,
                        text=True,
                    )
                    if f_res.returncode != 0:
                        raise_if_transport_error(
                            f_res.stderr or "",
                            f"FAIL: shallow git fetch (depth {depth}) from {url} failed with transport error",
                        )
                        continue
                    if (
                        subprocess.run(
                            ["git", "-C", dst, "cat-file", "-e", f"{pin_commit}^{{commit}}"],
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                            env=git_env,
                        ).returncode
                        == 0
                    ):
                        break

                still_missing = (
                    subprocess.run(
                        ["git", "-C", dst, "cat-file", "-e", f"{pin_commit}^{{commit}}"],
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        env=git_env,
                    ).returncode
                    != 0
                )
                allow_unshallow = os.environ.get("AUTOSYNC_ALLOW_UNSHALLOW", "false") == "true"
                if still_missing and allow_unshallow:
                    is_shallow = (
                        subprocess.check_output(
                            ["git", "-C", dst, "rev-parse", "--is-shallow-repository"],
                            text=True,
                            env=git_env,
                        ).strip()
                        == "true"
                    )
                    if is_shallow:
                        subprocess.run(
                            ["git", "-C", dst, "fetch", "--unshallow", "origin"],
                            check=False,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                            env=git_env,
                        )
                    else:
                        subprocess.run(
                            ["git", "-C", dst, "fetch", "origin"],
                            check=False,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                            env=git_env,
                        )

            recheck = subprocess.run(
                ["git", "-C", dst, "cat-file", "-e", f"{pin_commit}^{{commit}}"],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                env=git_env,
            ).returncode
            if recheck != 0:
                allow_unshallow = os.environ.get("AUTOSYNC_ALLOW_UNSHALLOW", "false") == "true"
                hint = (
                    "full history fetch (AUTOSYNC_ALLOW_UNSHALLOW=true) already attempted and pin still unreachable"
                    if allow_unshallow
                    else "set AUTOSYNC_ALLOW_UNSHALLOW=true to permit full history fetch"
                )
                raise SystemExit(
                    f"FAIL: pinned commit {pin_commit} not reachable in {url} after bounded shallow fetch (depth {max_attempted_depth}); {hint}"
                )
    return head_sha


def stage_fixture_repo(src: str, dst: str) -> str:
    if os.path.exists(dst):
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    if os.path.isdir(os.path.join(dst, ".git")):
        return subprocess.check_output(["git", "-C", dst, "rev-parse", "HEAD"], text=True).strip()
    return "fixture"


def ls_remote_stable_tags(
    repo_url: str, ref_glob: str, tag_re: re.Pattern[str], strip_prefix: str
) -> tuple[str, list[str]]:
    out = subprocess.check_output(
        ["git", "-c", "protocol.file.allow=always", "ls-remote", "--tags", "--refs", repo_url, ref_glob],
        text=True,
    )
    tags = []
    for raw in out.splitlines():
        raw = raw.strip()
        if not raw:
            continue
        parts = raw.split("\t", 1)
        if len(parts) != 2:
            continue
        ref = parts[1].strip()
        if not ref.startswith("refs/tags/"):
            continue
        tag = ref[len("refs/tags/"):]
        if tag_re.fullmatch(tag):
            tags.append(tag)

    if not tags:
        raise SystemExit(f"FAIL: no stable tags matching {tag_re.pattern} found in {repo_url} ({ref_glob})")

    def ver_key(t: str) -> tuple[int, ...]:
        num_part = t[len(strip_prefix):]
        return tuple(int(seg) for seg in num_part.split("."))

    tags = sorted(set(tags), key=ver_key, reverse=True)
    return tags[0], tags[:5]


def ensure_tag_fetched(dst: str, repo_url: str, tag: str, filter_blobless: bool = False) -> None:
    rc = subprocess.run(
        ["git", "-C", dst, "rev-parse", "--verify", "-q", f"refs/tags/{tag}^{{commit}}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode
    if rc == 0:
        return
    cmd = ["git", "-C", dst, "-c", "protocol.file.allow=always", "fetch", "--depth", "1"]
    if filter_blobless:
        cmd.append("--filter=blob:none")
    cmd.extend([repo_url, f"refs/tags/{tag}:refs/tags/{tag}"])
    subprocess.run(cmd, check=True)


def sync_tagged_repo(
    repo_url: str, dst: str, target_tag: str, pinned_tag: str = "", is_local: bool = False
) -> None:
    git_env = {**os.environ, "LC_ALL": "C", "GIT_LFS_SKIP_SMUDGE": "1"}
    lfs_opts = [
        "-c", "filter.lfs.smudge=",
        "-c", "filter.lfs.process=",
        "-c", "filter.lfs.clean=",
        "-c", "filter.lfs.required=false",
    ]
    if is_local:
        if os.path.isdir(os.path.join(dst, ".git")):
            subprocess.run(["git", "-C", dst, "remote", "set-url", "origin", repo_url], check=True, env=git_env)
            subprocess.run(
                ["git", *lfs_opts, "-C", dst, "-c", "protocol.file.allow=always", "fetch", "--tags", "--force", "--prune", "origin"],
                check=True,
                env=git_env,
            )
        else:
            if os.path.exists(dst):
                shutil.rmtree(dst)
            subprocess.run(
                ["git", *lfs_opts, "-c", "protocol.file.allow=always", "clone", "--no-single-branch", repo_url, dst],
                check=True,
                env=git_env,
            )
        ensure_tag_fetched(dst, repo_url, target_tag, filter_blobless=False)
    else:
        if not os.path.isdir(os.path.join(dst, ".git")):
            if os.path.exists(dst):
                shutil.rmtree(dst)
            subprocess.run(
                ["git", *lfs_opts, "-c", "protocol.file.allow=always", "clone", "--depth", "1", "--branch", target_tag, repo_url, dst],
                check=True,
                env=git_env,
            )
        else:
            subprocess.run(["git", "-C", dst, "remote", "set-url", "origin", repo_url], check=True, env=git_env)
            ensure_tag_fetched(dst, repo_url, target_tag, filter_blobless=False)

    if pinned_tag and pinned_tag != target_tag:
        try:
            ensure_tag_fetched(dst, repo_url, pinned_tag, filter_blobless=False)
        except subprocess.CalledProcessError:
            pass

    subprocess.run(["git", *lfs_opts, "-C", dst, "checkout", "-f", "-q", f"refs/tags/{target_tag}"], check=True, env=git_env)


def read_git_tag_file(repo_dir: str, tag: str, rel_path: str) -> str:
    if not os.path.isdir(os.path.join(repo_dir, ".git")):
        return ""
    res = subprocess.run(
        ["git", "-C", repo_dir, "show", f"refs/tags/{tag}:{rel_path}"],
        capture_output=True,
        text=True,
    )
    return res.stdout if res.returncode == 0 else ""


def parse_cargo_workspace_version(cargo_txt: str) -> str:
    if not cargo_txt:
        return ""
    try:
        import tomllib
        doc = tomllib.loads(cargo_txt)
        ws_ver = str(((doc.get("workspace") or {}).get("package") or {}).get("version") or "").strip()
        if ws_ver:
            return ws_ver
        return str((doc.get("package") or {}).get("version") or "").strip()
    except ImportError:
        section = ""
        ws_ver = ""
        pkg_ver = ""
        for raw_ln in cargo_txt.splitlines():
            st = raw_ln.strip()
            if not st or st.startswith("#"):
                continue
            if st.startswith("["):
                section = st
                continue
            m_v = re.match(r'^version\s*=\s*["\']([^"\']+)["\']', st)
            if m_v:
                if section == "[workspace.package]":
                    ws_ver = m_v.group(1).strip()
                elif section == "[package]":
                    pkg_ver = m_v.group(1).strip()
        return ws_ver or pkg_ver


mooncake_df = read_text(os.path.join(repo_root, "pack/mirrored-mooncake/Dockerfile"))
mt_df = read_text(os.path.join(repo_root, "pack/model-transfer/Dockerfile"))
mx_df = read_text(os.path.join(repo_root, "pack/mirrored-modelexpress-server/Dockerfile"))
value_go = read_text(os.path.join(repo_root, "pkg/worker/settings/value.go"))
values_yaml = read_text(os.path.join(repo_root, "deploy/gpustack-operator/chart/values.yaml"))
local_hixl_patch_path = os.path.join(repo_root, "pack/model-transfer/patches/hixl/001_channel_generation.patch")
with open(local_hixl_patch_path, "rb") as f:
    local_hixl_patch_bytes = f.read()

recipe = {
    "mooncake_version": read_dockerfile_arg(mooncake_df, "MOONCAKE_VERSION", "pack/mirrored-mooncake/Dockerfile"),
    "nixl_version": read_dockerfile_arg(mt_df, "NIXL_VERSION", "pack/model-transfer/Dockerfile"),
    "nixl_rocm_ref": read_dockerfile_arg(mt_df, "NIXL_ROCM_REF", "pack/model-transfer/Dockerfile"),
    "ucx_rocm_ref": read_dockerfile_arg(mt_df, "NIXL_ROCM_UCX_REF", "pack/model-transfer/Dockerfile"),
    "hixl_commit": read_dockerfile_arg(mt_df, "HIXL_COMMIT", "pack/model-transfer/Dockerfile"),
    "modelexpress_version": read_dockerfile_arg(mx_df, "MODELEXPRESS_VERSION", "pack/mirrored-modelexpress-server/Dockerfile"),
}
defaults = {
    "kv_cache_backend_image": extract_value_go_image(value_go, "KVCacheBackendImage"),
    "modelexpress_server_image": extract_value_go_image(value_go, "ModelDeploymentModelExpressServerImage"),
    "model_transfer_images": extract_values_yaml_mt_images(values_yaml),
}

holds_path = resolve_holds_file_for_preflight(repo_root, fixture_dir)
holds_label = holds_path or ".auto-sync/setting-image-holds.json"
kv_holds = domain_holds(load_setting_image_holds(holds_path), "kv-transfer")
verify_kv_transfer_holds(
    kv_holds,
    defaults["kv_cache_backend_image"],
    defaults["modelexpress_server_image"],
    defaults["model_transfer_images"],
    holds_label,
)

upstream_override = {}
if fixture_dir and os.path.isfile(os.path.join(fixture_dir, "upstream.json")):
    with open(os.path.join(fixture_dir, "upstream.json"), "r", encoding="utf-8") as f:
        upstream_override = json.load(f)

if "mooncake" in upstream_override:
    up_mooncake = str(upstream_override["mooncake"]).lstrip("v")
    up_mooncake_recent = [up_mooncake]
else:
    up_mooncake, up_mooncake_recent = resolve_mooncake_release(fixture_dir)
    if not up_mooncake:
        up_mooncake = recipe["mooncake_version"]
        up_mooncake_recent = [up_mooncake]

if "nixl_pypi" in upstream_override:
    up_nixl_pypi = str(upstream_override["nixl_pypi"])
    up_nixl_recent = [up_nixl_pypi]
else:
    up_nixl_pypi, up_nixl_recent = resolve_nixl_pypi(fixture_dir)
    if not up_nixl_pypi:
        up_nixl_pypi = recipe["nixl_version"]
        up_nixl_recent = [up_nixl_pypi]

# Stage gpustack/runner, hixl, nixl, ucx, modelexpress.
runner_stage = os.path.join(stage_dir, "gpustack-runner")
hixl_stage = os.path.join(stage_dir, "hixl")
nixl_stage = os.path.join(stage_dir, "nixl")
ucx_stage = os.path.join(stage_dir, "ucx")
mx_stage = os.path.join(stage_dir, "modelexpress")

mx_tag_re = re.compile(r"^v\d+\.\d+\.\d+$")
mx_url = os.environ.get("AUTOSYNC_KV_MODELEXPRESS_REPO_URL", "").strip() or "https://github.com/ai-dynamo/modelexpress.git"


def parse_mx_override(val: object) -> str:
    tag = str(val).strip()
    if not mx_tag_re.fullmatch(tag):
        raise SystemExit(
            f"FAIL: modelexpress upstream override must be a stable vX.Y.Z release tag, got: {tag!r}"
        )
    return tag


if fixture_dir:
    runner_fix = os.path.join(fixture_dir, "repos/gpustack-runner")
    if os.path.isdir(runner_fix):
        runner_head = stage_fixture_repo(runner_fix, runner_stage)
    else:
        runner_head = "aligned"

    hixl_fix = os.path.join(fixture_dir, "repos/hixl")
    up_hixl = stage_fixture_repo(hixl_fix, hixl_stage) if os.path.isdir(hixl_fix) else recipe["hixl_commit"]

    nixl_fix = os.path.join(fixture_dir, "repos/nixl")
    up_nixl_rocm = stage_fixture_repo(nixl_fix, nixl_stage) if os.path.isdir(nixl_fix) else recipe["nixl_rocm_ref"]

    ucx_fix = os.path.join(fixture_dir, "repos/ucx")
    up_ucx_rocm = stage_fixture_repo(ucx_fix, ucx_stage) if os.path.isdir(ucx_fix) else recipe["ucx_rocm_ref"]

    mx_fix = os.path.join(fixture_dir, "repos/modelexpress")
    if not os.path.isdir(mx_fix):
        raise SystemExit(f"FAIL: fixture missing required directory: {mx_fix}")
    if "modelexpress" in upstream_override:
        up_mx = parse_mx_override(upstream_override["modelexpress"])
        up_mx_recent = [up_mx]
    else:
        up_mx, up_mx_recent = ls_remote_stable_tags(mx_fix, "refs/tags/v*", mx_tag_re, "v")

    if os.path.isdir(os.path.join(mx_fix, ".git")) and subprocess.run(
        ["git", "-C", mx_fix, "rev-parse", "--verify", "-q", f"refs/tags/{up_mx}^{{commit}}"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0:
        sync_tagged_repo(mx_fix, mx_stage, up_mx, recipe["modelexpress_version"], is_local=True)
    else:
        stage_fixture_repo(mx_fix, mx_stage)
else:
    runner_head = sync_git_repo("https://github.com/gpustack/runner.git", runner_stage)
    up_hixl = sync_git_repo("https://github.com/hicann/hixl.git", hixl_stage, recipe["hixl_commit"])
    up_nixl_rocm = sync_git_repo("https://github.com/ai-dynamo/nixl.git", nixl_stage, recipe["nixl_rocm_ref"])
    up_ucx_rocm = sync_git_repo("https://github.com/openucx/ucx.git", ucx_stage, recipe["ucx_rocm_ref"])
    if "modelexpress" in upstream_override:
        up_mx = parse_mx_override(upstream_override["modelexpress"])
        up_mx_recent = [up_mx]
    else:
        up_mx, up_mx_recent = ls_remote_stable_tags(mx_url, "refs/tags/v*", mx_tag_re, "v")
    sync_tagged_repo(mx_url, mx_stage, up_mx, recipe["modelexpress_version"], is_local=False)

if "hixl" in upstream_override:
    up_hixl = str(upstream_override["hixl"])
if "nixl_rocm" in upstream_override:
    up_nixl_rocm = str(upstream_override["nixl_rocm"])
if "ucx_rocm" in upstream_override:
    up_ucx_rocm = str(upstream_override["ucx_rocm"])
if "gpustack_runner" in upstream_override:
    runner_head = str(upstream_override["gpustack_runner"])

mx_workspace_version = ""
if os.path.isdir(os.path.join(mx_stage, ".git")):
    mx_cargo_txt = read_git_tag_file(mx_stage, up_mx, "Cargo.toml")
    if mx_cargo_txt:
        mx_workspace_version = parse_cargo_workspace_version(mx_cargo_txt)
if not mx_workspace_version and os.path.isfile(os.path.join(mx_stage, "Cargo.toml")):
    mx_workspace_version = parse_cargo_workspace_version(read_text(os.path.join(mx_stage, "Cargo.toml")))

# Inspect gpustack/runner HIXL commit and patch parity.
runner_hixl_commit = ""
runner_patch_match = True
runner_cann_df = os.path.join(runner_stage, "pack/cann/Dockerfile.vllm")
if os.path.isfile(runner_cann_df):
    runner_cann_txt = read_text(runner_cann_df)
    if has_dockerfile_arg(runner_cann_txt, "VLLM_HIXL_COMMIT"):
        runner_hixl_commit = read_dockerfile_arg(runner_cann_txt, "VLLM_HIXL_COMMIT", runner_cann_df)
if not fixture_dir and re.fullmatch(r"[0-9a-f]{40}", runner_hixl_commit):
    has_runner_commit = (
        subprocess.run(
            ["git", "-C", hixl_stage, "cat-file", "-e", f"{runner_hixl_commit}^{{commit}}"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        ).returncode
        == 0
    )
    if not has_runner_commit:
        sync_git_repo("https://github.com/hicann/hixl.git", hixl_stage, runner_hixl_commit)
runner_patch_path = os.path.join(runner_stage, "pack/cann/patches/hixl/001_channel_generation.patch")
if os.path.isfile(runner_patch_path):
    with open(runner_patch_path, "rb") as f:
        runner_patch_match = f.read() == local_hixl_patch_bytes

# Extract nixl-rocm pyproject.toml [project].version at up_nixl_rocm when available.
nixl_rocm_project_version = ""
for cand_nixl_dir in (os.path.join(stage_dir, "nixl-rocm"), nixl_stage):
    if not os.path.isdir(os.path.join(cand_nixl_dir, ".git")):
        continue
    obj_spec = f"{up_nixl_rocm}:pyproject.toml"
    has_obj = subprocess.run(
        ["git", "-C", cand_nixl_dir, "cat-file", "-e", obj_spec],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0
    if has_obj:
        pyproject_raw = subprocess.check_output(
            ["git", "-C", cand_nixl_dir, "show", obj_spec],
            text=True,
        )
        try:
            import tomllib
            doc = tomllib.loads(pyproject_raw)
            nixl_rocm_project_version = str((doc.get("project") or {}).get("version") or "").strip()
        except ImportError:
            in_proj = False
            for raw_ln in pyproject_raw.splitlines():
                st = raw_ln.strip()
                if st.startswith("["):
                    in_proj = st == "[project]"
                    continue
                if in_proj:
                    m_v = re.match(r'^version\s*=\s*["\']([^"\']+)["\']', st)
                    if m_v:
                        nixl_rocm_project_version = m_v.group(1).strip()
                        break
        break

# Stage key Mooncake upstream files when Mooncake version drifted.
mooncake_stage = os.path.join(stage_dir, "mooncake")
mooncake_sparse_paths = [
    "CMakeLists.txt",
    "scripts/build_wheel.sh",
    "mooncake-transfer-engine/example/CMakeLists.txt",
    "mooncake-transfer-engine/src/transport/ascend_transport",
]
if up_mooncake != recipe["mooncake_version"]:
    if fixture_dir:
        mc_fix = os.path.join(fixture_dir, "repos/mooncake")
        if os.path.isdir(mc_fix):
            stage_fixture_repo(mc_fix, mooncake_stage)
    else:
        mc_tag = f"v{up_mooncake}"
        git_env = {**os.environ, "LC_ALL": "C"}
        try:
            if os.path.isdir(os.path.join(mooncake_stage, ".git")):
                subprocess.run(
                    ["git", "-C", mooncake_stage, "sparse-checkout", "set", *mooncake_sparse_paths],
                    check=True,
                    env=git_env,
                )
                subprocess.run(
                    ["git", "-C", mooncake_stage, "fetch", "--depth", "1", "--filter=blob:none",
                     "https://github.com/kvcache-ai/Mooncake.git", f"refs/tags/{mc_tag}:refs/tags/{mc_tag}"],
                    check=True,
                    env=git_env,
                )
                subprocess.run(
                    ["git", "-C", mooncake_stage, "checkout", "-f", "-q", f"refs/tags/{mc_tag}"],
                    check=True,
                    env=git_env,
                )
            else:
                if os.path.exists(mooncake_stage):
                    shutil.rmtree(mooncake_stage)
                subprocess.run(
                    ["git", "clone", "--depth", "1", "--filter=blob:none", "--sparse",
                     "--branch", mc_tag, "https://github.com/kvcache-ai/Mooncake.git", mooncake_stage],
                    check=True,
                    env=git_env,
                )
                subprocess.run(
                    ["git", "-C", mooncake_stage, "sparse-checkout", "set", *mooncake_sparse_paths],
                    check=True,
                    env=git_env,
                )
        except (OSError, subprocess.SubprocessError) as exc:
            print(f"WARN: unable to stage Mooncake {mc_tag} context: {exc}", file=sys.stderr)

def parse_generic_target_tags(raw_val: str) -> dict[str, str]:
    parsed: dict[str, str] = {}
    for item in (seg.strip() for seg in raw_val.split(",") if seg.strip()):
        if ":" in item:
            repo_part, tag_part = item.rsplit(":", 1)
            repo_name = repo_part.strip().split("/")[-1]
            tag_part = tag_part.strip()
            if not tag_part:
                continue
            if repo_name in ("mirrored-modelexpress-server", "modelexpress"):
                parsed["modelexpress"] = tag_part
            elif repo_name in ("mirrored-mooncake", "mooncake"):
                parsed["mooncake"] = tag_part
            elif repo_name == "model-transfer":
                if "-cuda" in tag_part:
                    parsed["mt_nvidia"] = tag_part
                elif "-rocm" in tag_part:
                    parsed["mt_amd"] = tag_part
                elif "-cann" in tag_part:
                    parsed["mt_ascend"] = tag_part
                else:
                    parsed["mt_ver"] = tag_part
        else:
            if item.endswith("-cpu"):
                parsed["mooncake"] = item
            elif "-cuda" in item:
                parsed["mt_nvidia"] = item
            elif "-rocm" in item:
                parsed["mt_amd"] = item
            elif "-cann" in item:
                parsed["mt_ascend"] = item
            elif re.fullmatch(r"v\d+\.\d+\.\d+", item):
                parsed["bare_v"] = item
    return parsed


def list_repo_tags(repo: str) -> list[str]:
    repo_tags = upstream_override.get("repo_tags")
    if isinstance(repo_tags, dict):
        for key in (repo, f"docker.io/{repo}"):
            if isinstance(repo_tags.get(key), list):
                return [str(t).strip() for t in repo_tags[key] if str(t).strip()]
    if fixture_dir:
        return []
    crane_bin = os.environ.get("AUTOSYNC_CRANE_BIN", "crane").strip() or "crane"
    try:
        res = subprocess.run(
            [crane_bin, "ls", f"docker.io/{repo}"],
            capture_output=True,
            text=True,
            timeout=30,
        )
        if res.returncode != 0:
            return []
        return [ln.strip() for ln in res.stdout.splitlines() if ln.strip()]
    except (OSError, subprocess.SubprocessError):
        return []


def extract_ref_tag(ref: str) -> str:
    last_seg = ref.split("/")[-1]
    if ":" not in last_seg:
        return ""
    return ref.rsplit(":", 1)[1].strip()


def discover_newer_mx_tag(current_image: str) -> str:
    cur_tag = extract_ref_tag(current_image)
    m_cur = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", cur_tag)
    if not m_cur:
        return ""
    cur_tuple = (int(m_cur.group(1)), int(m_cur.group(2)), int(m_cur.group(3)))
    best_tuple = cur_tuple
    best_tag = ""
    for tag in list_repo_tags("gpustack/mirrored-modelexpress-server"):
        m = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", tag)
        if not m:
            continue
        cand_tuple = (int(m.group(1)), int(m.group(2)), int(m.group(3)))
        if cand_tuple > best_tuple:
            best_tuple = cand_tuple
            best_tag = tag
    return best_tag


def discover_newer_mt_tags(current_images: dict[str, str], tags_list: list[str]) -> dict[str, str]:
    discovered: dict[str, str] = {}
    mt_tag_re = re.compile(r"^v(\d+)\.(\d+)\.(\d+)-([A-Za-z0-9._-]+)$")
    fam_re = re.compile(r"^(cuda\d+|rocm\d+|cann\d+)")
    for vendor in ("nvidia", "amd", "ascend"):
        cur_ref = current_images.get(vendor, "")
        cur_tag = extract_ref_tag(cur_ref)
        m_cur = mt_tag_re.fullmatch(cur_tag)
        if not m_cur:
            continue
        cur_tuple = (int(m_cur.group(1)), int(m_cur.group(2)), int(m_cur.group(3)))
        cur_flavor = m_cur.group(4)
        fam_m = fam_re.match(cur_flavor)
        cur_fam = fam_m.group(1) if fam_m else ""
        best_key = (cur_tuple, 0, "")
        best_tag = ""
        for tag in tags_list:
            m = mt_tag_re.fullmatch(tag)
            if not m:
                continue
            cand_tuple = (int(m.group(1)), int(m.group(2)), int(m.group(3)))
            cand_flavor = m.group(4)
            if cand_flavor != cur_flavor:
                # Non-numbered flavors (without a cudaXX/rocmXX/cannXX family) require an exact flavor match.
                if not cur_fam:
                    continue
                cand_fam_m = fam_re.match(cand_flavor)
                if not cand_fam_m or cand_fam_m.group(1) != cur_fam:
                    continue
            if cand_tuple <= cur_tuple:
                continue
            cand_key = (cand_tuple, 1 if cand_flavor == cur_flavor else 0, cand_flavor)
            if cand_key > best_key:
                best_key = cand_key
                best_tag = tag
        if best_tag:
            discovered[vendor] = best_tag
    return discovered


generic_targets = parse_generic_target_tags(os.environ.get("AUTOSYNC_TARGET_TAG", ""))

# Compute candidate target image tags for Stage B probing.
target_mooncake_tag = generic_targets.get("mooncake", "")
if "mooncake" in kv_holds:
    if target_mooncake_tag:
        raise SystemExit(
            f"FAIL: AUTOSYNC_TARGET_TAG targets mooncake ({target_mooncake_tag!r}), "
            f"which is held at {kv_holds['mooncake']['tag']!r} by {holds_label}; update {holds_label} first"
        )
    target_mooncake_image = defaults["kv_cache_backend_image"]
elif target_mooncake_tag:
    target_mooncake_image = f"gpustack/mirrored-mooncake:{target_mooncake_tag}"
else:
    target_mooncake_image = f"gpustack/mirrored-mooncake:{recipe['mooncake_version']}-cpu"

env_mx_tag = os.environ.get("AUTOSYNC_MODELEXPRESS_TARGET_TAG", "").strip()
if "modelexpress" in kv_holds:
    if env_mx_tag or generic_targets.get("modelexpress"):
        req_mx = env_mx_tag or generic_targets.get("modelexpress", "")
        raise SystemExit(
            f"FAIL: target tag targets modelexpress ({req_mx!r}), "
            f"which is held at {kv_holds['modelexpress']['tag']!r} by {holds_label}; update {holds_label} first"
        )
    if generic_targets.get("bare_v"):
        cur_mx_tag = extract_ref_tag(defaults["modelexpress_server_image"])
        if parse_version_key(generic_targets["bare_v"]) > parse_version_key(cur_mx_tag):
            raise SystemExit(
                f"FAIL: AUTOSYNC_TARGET_TAG={generic_targets['bare_v']!r} targets modelexpress, "
                f"which is held at {kv_holds['modelexpress']['tag']!r} by {holds_label}; update {holds_label} first"
            )
    target_mx_image = defaults["modelexpress_server_image"]
else:
    target_mx_tag = env_mx_tag
    if not target_mx_tag:
        target_mx_tag = generic_targets.get("modelexpress", "")
    if not target_mx_tag and isinstance(upstream_override.get("modelexpress_target_tag"), str):
        target_mx_tag = upstream_override["modelexpress_target_tag"].strip()
    if not target_mx_tag and generic_targets.get("bare_v"):
        cur_mx_tag = extract_ref_tag(defaults["modelexpress_server_image"])
        if parse_version_key(generic_targets["bare_v"]) > parse_version_key(cur_mx_tag):
            target_mx_tag = generic_targets["bare_v"]
    if not target_mx_tag:
        target_mx_tag = discover_newer_mx_tag(defaults["modelexpress_server_image"])
    target_mx_image = (
        f"gpustack/mirrored-modelexpress-server:{target_mx_tag}"
        if target_mx_tag
        else defaults["modelexpress_server_image"]
    )

mt_repo_tags = list_repo_tags("gpustack/model-transfer")
discovered_mt_tags = discover_newer_mt_tags(defaults["model_transfer_images"], mt_repo_tags)

explicit_mt_raw = os.environ.get("AUTOSYNC_MODEL_TRANSFER_TARGET_TAG", "").strip()
if not explicit_mt_raw and isinstance(upstream_override.get("model_transfer_target_tag"), str):
    explicit_mt_raw = upstream_override["model_transfer_target_tag"].strip()

explicit_mt_by_vendor: dict[str, str] = {}
explicit_mt_ver = generic_targets.get("mt_ver", "")
for vk in ("nvidia", "amd", "ascend"):
    if f"mt_{vk}" in generic_targets:
        explicit_mt_by_vendor[vk] = generic_targets[f"mt_{vk}"]
if explicit_mt_raw:
    for part in (seg.strip() for seg in explicit_mt_raw.split(",") if seg.strip()):
        if "-cuda" in part:
            explicit_mt_by_vendor["nvidia"] = part
        elif "-rocm" in part:
            explicit_mt_by_vendor["amd"] = part
        elif "-cann" in part:
            explicit_mt_by_vendor["ascend"] = part
        else:
            explicit_mt_ver = part

if "model-transfer" in kv_holds and (
    explicit_mt_raw or explicit_mt_ver or explicit_mt_by_vendor
):
    raise SystemExit(
        f"FAIL: target tag targets model-transfer, "
        f"which is held at {kv_holds['model-transfer']['tag']!r} by {holds_label}; update {holds_label} first"
    )

target_mt_images: dict[str, str] = {}
for vendor in ("nvidia", "amd", "ascend"):
    cur_ref = defaults["model_transfer_images"].get(vendor, "")
    cur_tag = extract_ref_tag(cur_ref)
    vk = f"model-transfer.{vendor}"
    hold_key = vk if vk in kv_holds else ("model-transfer" if "model-transfer" in kv_holds else "")
    if hold_key:
        if explicit_mt_by_vendor.get(vendor) or explicit_mt_ver:
            raise SystemExit(
                f"FAIL: target tag targets {vk}, "
                f"which is held at {kv_holds[hold_key]['tag']!r} by {holds_label}; update {holds_label} first"
            )
        if generic_targets.get("bare_v"):
            if parse_version_key(generic_targets["bare_v"]) > parse_version_key(cur_tag.split("-", 1)[0]):
                raise SystemExit(
                    f"FAIL: AUTOSYNC_TARGET_TAG={generic_targets['bare_v']!r} targets {vk}, "
                    f"which is held at {kv_holds[hold_key]['tag']!r} by {holds_label}; update {holds_label} first"
                )
        target_mt_images[vendor] = cur_ref
        continue
    m_cur = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)-([A-Za-z0-9._-]+)", cur_tag)
    cur_flavor = m_cur.group(4) if m_cur else ""
    chosen_tag = explicit_mt_by_vendor.get(vendor, "")
    if not chosen_tag and explicit_mt_ver and cur_flavor:
        chosen_tag = f"{explicit_mt_ver}-{cur_flavor}"
    if not chosen_tag and generic_targets.get("bare_v") and cur_flavor:
        if parse_version_key(generic_targets["bare_v"]) > parse_version_key(cur_tag.split("-", 1)[0]):
            chosen_tag = f"{generic_targets['bare_v']}-{cur_flavor}"
    if not chosen_tag:
        chosen_tag = discovered_mt_tags.get(vendor, "")
    target_mt_images[vendor] = (
        f"gpustack/model-transfer:{chosen_tag}"
        if chosen_tag
        else cur_ref
    )

probe_refs = []
for ref in (
    defaults["kv_cache_backend_image"],
    target_mooncake_image,
    f"gpustack/mirrored-mooncake:{up_mooncake}-cpu",
    defaults["modelexpress_server_image"],
    target_mx_image,
    defaults["model_transfer_images"].get("nvidia", ""),
    defaults["model_transfer_images"].get("amd", ""),
    defaults["model_transfer_images"].get("ascend", ""),
    target_mt_images.get("nvidia", ""),
    target_mt_images.get("amd", ""),
    target_mt_images.get("ascend", ""),
):
    if ref and ref not in probe_refs:
        probe_refs.append(ref)

extra_targets = upstream_override.get("target_images")
if isinstance(extra_targets, list):
    for ref in extra_targets:
        if isinstance(ref, str) and ref and ref not in probe_refs:
            probe_refs.append(ref)

with open(probe_list_path, "w", encoding="utf-8") as f:
    for ref in probe_refs:
        f.write(ref + "\n")

discovery = {
    "recipe": recipe,
    "defaults": defaults,
    "upstream": {
        "mooncake": up_mooncake,
        "mooncake_recent": up_mooncake_recent,
        "nixl_pypi": up_nixl_pypi,
        "nixl_pypi_recent": up_nixl_recent,
        "nixl_rocm": up_nixl_rocm,
        "ucx_rocm": up_ucx_rocm,
        "hixl": up_hixl,
        "modelexpress": up_mx,
        "modelexpress_recent": up_mx_recent,
        "gpustack_runner": runner_head,
    },
    "runner": {
        "hixl_commit": runner_hixl_commit,
        "patch_match": runner_patch_match,
    },
    "nixl_rocm_project_version": nixl_rocm_project_version,
    "modelexpress_workspace_version": mx_workspace_version,
    "base_sha": base_sha,
    "held_setting_images": kv_holds,
    "targets": {
        "mooncake_image": target_mooncake_image,
        "modelexpress_image": target_mx_image,
        "model_transfer_images": target_mt_images,
    },
}

with open(discovery_path, "w", encoding="utf-8") as f:
    json.dump(discovery, f, indent=2)
    f.write("\n")
PY

PROBE_REFS=()
while IFS= read -r ref; do
  [[ -n "${ref}" ]] && PROBE_REFS+=("${ref}")
done < "${PROBE_LIST_FILE}"

MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
if [[ -n "${AUTOSYNC_KV_FIXTURE_DIR:-}" && -f "${AUTOSYNC_KV_FIXTURE_DIR}/image-manifests.json" ]]; then
  AUTOSYNC_MOCK_MANIFESTS_JSON="${AUTOSYNC_KV_FIXTURE_DIR}/image-manifests.json" \
    autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
else
  autosync::preflight::record_image_probes "${MANIFESTS_JSON}" "${PROBE_REFS[@]}"
fi

UPSTREAM_RECORDS_JSON="${STAGE_DIR}/upstream-records.json"
PREFLIGHT_REPORT_JSON="${STAGE_DIR}/kv-transfer-preflight-report.json"

python3 - "${DISCOVERY_JSON}" "${MANIFESTS_JSON}" "${UPSTREAM_RECORDS_JSON}" "${PREFLIGHT_REPORT_JSON}" <<'PY'
import json
import re
import sys

disc_path, man_path, recs_path, rep_path = sys.argv[1:5]
with open(disc_path, "r", encoding="utf-8") as f:
    disc = json.load(f)
with open(man_path, "r", encoding="utf-8") as f:
    man = json.load(f)

images = man.get("images", {})
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


def get_image_entry(ref: str):
    entry = images.get(ref)
    if not isinstance(entry, dict):
        entry = images.get(f"docker.io/{ref}")
    return entry if isinstance(entry, dict) else None


def is_published(ref: str) -> bool:
    entry = get_image_entry(ref)
    return isinstance(entry, dict) and entry.get("published") is True


def is_rocm_amd64_published(ref: str) -> bool:
    entry = get_image_entry(ref)
    if not isinstance(entry, dict):
        return False
    amd64 = str(entry.get("amd64Digest") or "").strip()
    if not DIGEST_RE.fullmatch(amd64):
        return False
    arm64 = str(entry.get("arm64Digest") or "").strip()
    if arm64:
        return bool(DIGEST_RE.fullmatch(arm64) and arm64 != amd64 and entry.get("published") is True)
    if "amd64Published" in entry:
        return entry.get("amd64Published") is True
    return entry.get("published") is not False


def is_mt_vendor_published(vendor: str, ref: str) -> bool:
    if vendor == "amd" and "-rocm" in ref:
        return is_rocm_amd64_published(ref)
    return is_published(ref)


recipe = disc["recipe"]
defaults = disc["defaults"]
up = disc["upstream"]
runner = disc["runner"]
targets = disc["targets"]

runner_hixl_drift = bool(runner["hixl_commit"] and runner["hixl_commit"] != recipe["hixl_commit"])
runner_patch_drift = not bool(runner["patch_match"])
effective_hixl_target = runner["hixl_commit"] if runner["hixl_commit"] else up["hixl"]
hixl_upstream_held_by_runner = bool(runner["hixl_commit"] and up["hixl"] and up["hixl"] != runner["hixl_commit"])

stage_a_drifts = {
    "mooncake": up["mooncake"] != recipe["mooncake_version"],
    "nixl-pypi": up["nixl_pypi"] != recipe["nixl_version"],
    "nixl-rocm": up["nixl_rocm"] != recipe["nixl_rocm_ref"],
    "ucx-rocm": up["ucx_rocm"] != recipe["ucx_rocm_ref"],
    "hixl": effective_hixl_target != recipe["hixl_commit"],
    "modelexpress": up["modelexpress"] != recipe["modelexpress_version"],
    "gpustack-runner": runner_hixl_drift or runner_patch_drift,
}

has_stage_a = any(stage_a_drifts.values())
hixl_held_extra = (
    {
        "reason": (
            f"held at gpustack/runner VLLM_HIXL_COMMIT {recipe['hixl_commit'][:12]} "
            f"while upstream hicann/hixl is at {up['hixl'][:12]}"
        )
    }
    if not stage_a_drifts["hixl"] and hixl_upstream_held_by_runner
    else {}
)

mooncake_stage_b = (
    targets["mooncake_image"] != defaults["kv_cache_backend_image"]
    and is_published(targets["mooncake_image"])
)
mx_stage_b = (
    targets["modelexpress_image"] != defaults["modelexpress_server_image"]
    and is_published(targets["modelexpress_image"])
)

cand_mt = targets.get("model_transfer_images") or {}
def_mt = defaults.get("model_transfer_images") or {}
effective_mt = {}
mt_stage_b = False
for vendor in ("nvidia", "amd", "ascend"):
    cur_ref = def_mt.get(vendor, "")
    cand_ref = cand_mt.get(vendor, cur_ref)
    if cand_ref and cand_ref != cur_ref and is_mt_vendor_published(vendor, cand_ref):
        effective_mt[vendor] = cand_ref
        mt_stage_b = True
    else:
        effective_mt[vendor] = cur_ref

targets["model_transfer_images"] = effective_mt
has_stage_b = mooncake_stage_b or mx_stage_b or mt_stage_b

records = []
if has_stage_b:
    stage = "tag_promotion"
    image_published = True
    records = [
        {
            "upstreamId": "mooncake",
            "status": "available" if mooncake_stage_b else "up_to_date",
            "version": f"{recipe['mooncake_version']}+stage-b" if mooncake_stage_b else recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "up_to_date",
            "version": recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "up_to_date",
            "version": recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "up_to_date",
            "version": recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "up_to_date",
            "version": recipe["hixl_commit"],
            **hixl_held_extra,
        },
        {
            "upstreamId": "modelexpress",
            "status": "available" if mx_stage_b else "up_to_date",
            "version": f"{recipe['modelexpress_version']}+stage-b" if mx_stage_b else recipe["modelexpress_version"],
            "recentVersions": up["modelexpress_recent"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "available" if mt_stage_b else "up_to_date",
            "version": "aligned+stage-b" if mt_stage_b else "aligned",
        },
    ]
elif has_stage_a:
    stage = "recipe_sync"
    image_published = False
    records = [
        {
            "upstreamId": "mooncake",
            "status": "available" if stage_a_drifts["mooncake"] else "up_to_date",
            "version": up["mooncake"] if stage_a_drifts["mooncake"] else recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "available" if stage_a_drifts["nixl-pypi"] else "up_to_date",
            "version": up["nixl_pypi"] if stage_a_drifts["nixl-pypi"] else recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "available" if stage_a_drifts["nixl-rocm"] else "up_to_date",
            "version": up["nixl_rocm"] if stage_a_drifts["nixl-rocm"] else recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "available" if stage_a_drifts["ucx-rocm"] else "up_to_date",
            "version": up["ucx_rocm"] if stage_a_drifts["ucx-rocm"] else recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "available" if stage_a_drifts["hixl"] else "up_to_date",
            "version": effective_hixl_target if stage_a_drifts["hixl"] else recipe["hixl_commit"],
            **hixl_held_extra,
        },
        {
            "upstreamId": "modelexpress",
            "status": "available" if stage_a_drifts["modelexpress"] else "up_to_date",
            "version": up["modelexpress"] if stage_a_drifts["modelexpress"] else recipe["modelexpress_version"],
            "recentVersions": up["modelexpress_recent"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "available" if stage_a_drifts["gpustack-runner"] else "up_to_date",
            "version": up["gpustack_runner"] if stage_a_drifts["gpustack-runner"] else "aligned",
        },
    ]
else:
    stage = "no_change"
    image_published = (
        targets["mooncake_image"] == defaults["kv_cache_backend_image"]
        and is_published(targets["mooncake_image"])
    )
    records = [
        {
            "upstreamId": "mooncake",
            "status": "up_to_date",
            "version": recipe["mooncake_version"],
            "recentVersions": up["mooncake_recent"],
        },
        {
            "upstreamId": "nixl-pypi",
            "status": "up_to_date",
            "version": recipe["nixl_version"],
            "recentVersions": up["nixl_pypi_recent"],
        },
        {
            "upstreamId": "nixl-rocm",
            "status": "up_to_date",
            "version": recipe["nixl_rocm_ref"],
        },
        {
            "upstreamId": "ucx-rocm",
            "status": "up_to_date",
            "version": recipe["ucx_rocm_ref"],
        },
        {
            "upstreamId": "hixl",
            "status": "up_to_date",
            "version": recipe["hixl_commit"],
            **hixl_held_extra,
        },
        {
            "upstreamId": "modelexpress",
            "status": "up_to_date",
            "version": recipe["modelexpress_version"],
            "recentVersions": up["modelexpress_recent"],
        },
        {
            "upstreamId": "gpustack-runner",
            "status": "up_to_date",
            "version": "aligned",
        },
    ]

with open(recs_path, "w", encoding="utf-8") as f:
    json.dump(records, f, indent=2)
    f.write("\n")

report = {
    "schemaVersion": 1,
    "base_sha": disc.get("base_sha", ""),
    "source_revision": disc.get("base_sha", ""),
    "stage": stage,
    "image_published": image_published,
    "enforce_runner_hixl_parity": True,
    "hixl_upstream_head": up["hixl"],
    "hixl_upstream_held_by_runner": hixl_upstream_held_by_runner,
    "runner_patch_drift": runner_patch_drift,
    "has_stage_a": has_stage_a,
    "stage_a_drifts": stage_a_drifts,
    "stage_b_flags": {
        "mooncake": mooncake_stage_b,
        "modelexpress": mx_stage_b,
        "model_transfer": mt_stage_b,
    },
    "held_setting_images": disc.get("held_setting_images") or {},
    "recipe": recipe,
    "defaults": defaults,
    "targets": targets,
    "nixl_pypi": {
        "selected_version": up["nixl_pypi"],
        "recent_versions": up["nixl_pypi_recent"],
    },
    "nixl_rocm": {
        "target_ref": up["nixl_rocm"],
        "project_version": disc.get("nixl_rocm_project_version", ""),
    },
    "modelexpress": {
        "selected_version": up["modelexpress"],
        "recent_versions": up["modelexpress_recent"],
        "workspace_version": disc.get("modelexpress_workspace_version", ""),
    },
    "runner": runner,
}

with open(rep_path, "w", encoding="utf-8") as f:
    json.dump(report, f, indent=2)
    f.write("\n")
PY

autosync::preflight::apply_force_to_upstream_records "${UPSTREAM_RECORDS_JSON}"

if autosync::preflight::has_available_upstreams "${UPSTREAM_RECORDS_JSON}" || \
   [[ "${COMMENT_COMMAND:-}" == "revise" || \
      "${COMMENT_COMMAND:-}" == "retry" || \
      "${AUTOSYNC_WARM_TOOLS:-false}" == "true" ]]; then
  if [[ -n "${AUTOSYNC_KV_FIXTURE_DIR:-}" && "${AUTOSYNC_WARM_TOOLS:-false}" != "true" ]]; then
    autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go
  else
    autosync::preflight::warm_caches "${STAGE_DIR}" --sbin --go --tools shellcheck,helm,helm-docs,helm-schema,hugo
  fi
fi

SOURCE_REVISION="${BASE_SHA}"
if [[ "${AUTOSYNC_SKIP_SOURCE_REVISION:-false}" != "true" ]] && \
   autosync::preflight::has_available_upstreams "${UPSTREAM_RECORDS_JSON}"; then
  KV_STAGE_TMP="$(mktemp -d "${TMPDIR:-/tmp}/autosync-kv-stage.XXXXXXXX")"
  STAGED_TREE="${KV_STAGE_TMP}/staged-tree"
  mkdir -p "${STAGED_TREE}"

  BUNDLE_SPECS=(
    "${PREFLIGHT_REPORT_JSON}"
    "${UPSTREAM_RECORDS_JSON}"
    "${MANIFESTS_JSON}"
  )
  RUNNER_PATCH_FILE="${STAGE_DIR}/gpustack-runner/pack/cann/patches/hixl/001_channel_generation.patch"
  if [[ -f "${RUNNER_PATCH_FILE}" ]]; then
    BUNDLE_SPECS+=("${RUNNER_PATCH_FILE}::runner-001_channel_generation.patch")
  fi
  if [[ -d "${STAGE_DIR}/mooncake" ]]; then
    for mc_rel in \
      "CMakeLists.txt" \
      "scripts/build_wheel.sh" \
      "mooncake-transfer-engine/example/CMakeLists.txt" \
      "mooncake-transfer-engine/src/transport/ascend_transport"; do
      if [[ -e "${STAGE_DIR}/mooncake/${mc_rel}" ]]; then
        BUNDLE_SPECS+=("${STAGE_DIR}/mooncake/${mc_rel}::mooncake/${mc_rel}")
      fi
    done
  fi
  if [[ -d "${STAGE_DIR}/modelexpress" ]]; then
    for mx_rel in "Cargo.toml" "Cargo.lock"; do
      if [[ -f "${STAGE_DIR}/modelexpress/${mx_rel}" ]]; then
        BUNDLE_SPECS+=("${STAGE_DIR}/modelexpress/${mx_rel}::modelexpress/${mx_rel}")
      fi
    done
  fi
  autosync::preflight::populate_stage_bundle "${STAGED_TREE}" "kv-transfer" "${BUNDLE_SPECS[@]}"

  COMMIT_PATHS=(".auto-sync/.stage/kv-transfer")

  KV_STAGE_INFO="$(python3 - "${PREFLIGHT_REPORT_JSON}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    rep = json.load(f)
sb = rep.get("stage_b_flags") or {}
print(f"{rep.get('stage','')}|{str(bool(rep.get('runner_patch_drift'))).lower()}|{str(bool(sb.get('model_transfer'))).lower()}|{str(bool(sb.get('modelexpress'))).lower()}")
PY
)"
  IFS='|' read -r KV_STAGE_NAME RUNNER_PATCH_DRIFT MT_STAGE_B MX_STAGE_B <<<"${KV_STAGE_INFO}"

  if [[ "${KV_STAGE_NAME}" == "recipe_sync" && "${RUNNER_PATCH_DRIFT}" == "true" && -f "${RUNNER_PATCH_FILE}" ]]; then
    mkdir -p "${STAGED_TREE}/pack/model-transfer/patches/hixl"
    cp -f "${RUNNER_PATCH_FILE}" "${STAGED_TREE}/pack/model-transfer/patches/hixl/001_channel_generation.patch"
    COMMIT_PATHS+=("pack/model-transfer/patches/hixl/001_channel_generation.patch")
  elif [[ "${KV_STAGE_NAME}" == "tag_promotion" ]]; then
    mkdir -p \
      "${STAGED_TREE}/pkg/worker/settings" \
      "${STAGED_TREE}/pkg/worker/controllers/worker" \
      "${STAGED_TREE}/docs/reference" \
      "${STAGED_TREE}/docs/modules/model-delivery"
    cp -f "${REPO_ROOT}/pkg/worker/settings/value.go" "${STAGED_TREE}/pkg/worker/settings/value.go"
    if [[ -f "${REPO_ROOT}/pkg/worker/settings/value_test.go" ]]; then
      cp -f "${REPO_ROOT}/pkg/worker/settings/value_test.go" "${STAGED_TREE}/pkg/worker/settings/value_test.go"
    fi
    if [[ -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_transfer_test.go" ]]; then
      cp -f "${REPO_ROOT}/pkg/worker/controllers/worker/model_deployment_transfer_test.go" \
        "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_transfer_test.go"
    fi
    cp -f "${REPO_ROOT}/docs/reference/settings.md" "${STAGED_TREE}/docs/reference/settings.md"
    cp -f "${REPO_ROOT}/docs/modules/model-delivery/remote.md" "${STAGED_TREE}/docs/modules/model-delivery/remote.md"
    if [[ -d "${REPO_ROOT}/deploy/gpustack-operator/chart" ]]; then
      mkdir -p "${STAGED_TREE}/deploy/gpustack-operator"
      cp -R "${REPO_ROOT}/deploy/gpustack-operator/chart" "${STAGED_TREE}/deploy/gpustack-operator/chart"
    fi
    if [[ -d "${REPO_ROOT}/hack" ]]; then
      cp -R "${REPO_ROOT}/hack" "${STAGED_TREE}/hack"
    fi
    if [[ -f "${REPO_ROOT}/Makefile" ]]; then
      cp -f "${REPO_ROOT}/Makefile" "${STAGED_TREE}/Makefile"
    fi

    python3 - "${PREFLIGHT_REPORT_JSON}" \
      "${STAGED_TREE}/pkg/worker/settings/value.go" \
      "${STAGED_TREE}/docs/reference/settings.md" \
      "${STAGED_TREE}/docs/modules/model-delivery/remote.md" \
      "${STAGED_TREE}/deploy/gpustack-operator/chart/values.yaml" \
      "${STAGED_TREE}/deploy/gpustack-operator/chart/README.md.gotmpl" \
      "${STAGED_TREE}/pkg/worker/settings/value_test.go" \
      "${STAGED_TREE}/pkg/worker/controllers/worker/model_deployment_transfer_test.go" <<'PY'
import json
import os
import re
import sys

rep_p, vg_p, sm_p, rm_p, vy_p, rgt_p, vt_p, tt_p = sys.argv[1:9]
with open(rep_p, "r", encoding="utf-8") as f:
    rep = json.load(f)

sb = rep.get("stage_b_flags") or {}
defaults = rep.get("defaults") or {}
targets = rep.get("targets") or {}


def update_value_go(text: str, var_name: str, new_img: str) -> str:
    pat = re.compile(
        rf'({re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\(")([^"]+)("\))',
        re.DOTALL,
    )
    out, n = pat.subn(rf'\g<1>{new_img}\g<3>', text, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update {var_name} in value.go")
    return out


def update_settings_md(text: str, key: str, new_img: str) -> str:
    pat = re.compile(
        rf'^(\| `{re.escape(key)}`<br/>.*?<br/>Default: `)([^`]+)(`)',
        re.MULTILINE,
    )
    out, n = pat.subn(rf'\g<1>{new_img}\g<3>', text, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update {key} in settings.md")
    return out


def replace_exact_image_literal(path: str, old_img: str, new_img: str) -> None:
    if not os.path.isfile(path) or not new_img or old_img == new_img:
        return
    if not old_img:
        raise SystemExit(f"FAIL: missing default image while promoting {new_img!r} in {path}")
    with open(path, "r", encoding="utf-8") as f:
        txt = f.read()
    token = f'"{old_img}"'
    if token not in txt:
        raise SystemExit(f"FAIL: could not find {token} in {path} for Stage B target {new_img!r}")
    txt = txt.replace(token, f'"{new_img}"')
    with open(path, "w", encoding="utf-8") as f:
        f.write(txt)


def update_remote_md_mx(path: str, old_img: str, new_img: str) -> None:
    if not os.path.isfile(path) or not new_img or old_img == new_img:
        return
    if not old_img:
        raise SystemExit(f"FAIL: missing default image while promoting {new_img!r} in {path}")
    with open(path, "r", encoding="utf-8") as f:
        txt = f.read()
    pat = re.compile(
        rf'(name:\s*model-deployment-modelexpress-server-image\b.*?\n\s*value:\s*){re.escape(old_img)}(?=\s|#|$)',
        re.DOTALL,
    )
    out, n = pat.subn(rf'\g<1>{new_img}', txt, count=1)
    if n != 1:
        raise SystemExit(f"FAIL: could not update model-deployment-modelexpress-server-image in {path}")
    with open(path, "w", encoding="utf-8") as f:
        f.write(out)


def update_mt_images_yaml_file(path: str, mt_targets: dict[str, str]) -> None:
    if not os.path.isfile(path) or not mt_targets:
        return
    with open(path, "r", encoding="utf-8") as f:
        lines = f.readlines()
    in_mt = False
    in_images = False
    out_lines = []
    for raw in lines:
        st = raw.strip()
        if st and not st.startswith("#"):
            if not raw.startswith(" "):
                in_mt = raw.rstrip("\r\n") == "modelTransfer:" or raw.startswith("modelTransfer: ")
                in_images = False
            elif in_mt and re.match(r"^  [A-Za-z0-9_-]+:", raw):
                in_images = raw.rstrip("\r\n") == "  images:" or raw.startswith("  images: ")
            elif in_mt and in_images:
                m = re.match(r'^(    (nvidia|amd|ascend):\s*)(?:"([^"]+)"|\'([^\']+)\'|([^\s#]+))(.*)$', raw.rstrip("\r\n"))
                if m and m.group(2) in mt_targets and mt_targets[m.group(2)]:
                    raw = f"{m.group(1)}{mt_targets[m.group(2)]}{m.group(6)}\n"
        out_lines.append(raw)
    with open(path, "w", encoding="utf-8") as f:
        f.writelines(out_lines)


with open(vg_p, "r", encoding="utf-8") as f:
    vg_txt = f.read()
with open(sm_p, "r", encoding="utf-8") as f:
    sm_txt = f.read()

if sb.get("mooncake"):
    vg_txt = update_value_go(vg_txt, "KVCacheBackendImage", targets["mooncake_image"])
    sm_txt = update_settings_md(sm_txt, "kv-cache-backend-image", targets["mooncake_image"])
if sb.get("modelexpress"):
    old_mx = defaults.get("modelexpress_server_image") or defaults.get("modelexpress_image") or ""
    vg_txt = update_value_go(vg_txt, "ModelDeploymentModelExpressServerImage", targets["modelexpress_image"])
    sm_txt = update_settings_md(sm_txt, "model-deployment-modelexpress-server-image", targets["modelexpress_image"])
    update_remote_md_mx(rm_p, old_mx, targets["modelexpress_image"])
    replace_exact_image_literal(vt_p, old_mx, targets["modelexpress_image"])
    replace_exact_image_literal(tt_p, old_mx, targets["modelexpress_image"])

with open(vg_p, "w", encoding="utf-8") as f:
    f.write(vg_txt)
with open(sm_p, "w", encoding="utf-8") as f:
    f.write(sm_txt)

if sb.get("model_transfer"):
    mt_targets = targets.get("model_transfer_images") or {}
    update_mt_images_yaml_file(vy_p, mt_targets)
    update_mt_images_yaml_file(rgt_p, mt_targets)
PY

    if [[ "${MT_STAGE_B}" == "true" && "${AUTOSYNC_SKIP_GENERATE_CHART:-false}" != "true" ]] && \
       [[ -f "${STAGED_TREE}/hack/generate.sh" && -d "${STAGED_TREE}/deploy/gpustack-operator/chart" ]] && \
       PATH="${STAGE_DIR}/.sbin:${PATH}" command -v helm-docs >/dev/null 2>&1 && \
       PATH="${STAGE_DIR}/.sbin:${PATH}" command -v helm-schema >/dev/null 2>&1; then
      (
        cd "${STAGED_TREE}"
        PATH="${STAGE_DIR}/.sbin:${PATH}" bash "${STAGED_TREE}/hack/generate.sh" chart
      )
    fi

    COMMIT_PATHS+=(
      "pkg/worker/settings/value.go"
      "docs/reference/settings.md"
    )
    if [[ "${MX_STAGE_B}" == "true" && -f "${STAGED_TREE}/docs/modules/model-delivery/remote.md" ]]; then
      COMMIT_PATHS+=("docs/modules/model-delivery/remote.md")
    fi
    for extra_test_rel in \
      "pkg/worker/settings/value_test.go" \
      "pkg/worker/controllers/worker/model_deployment_transfer_test.go"; do
      if [[ -f "${STAGED_TREE}/${extra_test_rel}" ]]; then
        COMMIT_PATHS+=("${extra_test_rel}")
      fi
    done
    if [[ "${MT_STAGE_B}" == "true" && -d "${STAGED_TREE}/deploy/gpustack-operator/chart" ]]; then
      COMMIT_PATHS+=("deploy/gpustack-operator/chart/values.yaml")
      if [[ -f "${STAGED_TREE}/deploy/gpustack-operator/chart/values.schema.json" ]]; then
        COMMIT_PATHS+=("deploy/gpustack-operator/chart/values.schema.json")
      fi
      if [[ -f "${STAGED_TREE}/deploy/gpustack-operator/chart/README.md.gotmpl" ]]; then
        COMMIT_PATHS+=("deploy/gpustack-operator/chart/README.md.gotmpl")
      fi
      if [[ -f "${STAGED_TREE}/deploy/gpustack-operator/chart/README.md" ]]; then
        COMMIT_PATHS+=("deploy/gpustack-operator/chart/README.md")
      fi
    fi
  fi

  SOURCE_REVISION="$(
    autosync::preflight::commit_staged_tree_revision \
      "${REPO_ROOT}" "${BASE_SHA}" \
      "chore(kv-transfer): stage preflight bundle and deterministic updates" \
      "${STAGED_TREE}" \
      "${COMMIT_PATHS[@]}"
  )"
  if [[ "${SOURCE_REVISION}" != "${BASE_SHA}" ]]; then
    python3 - "${PREFLIGHT_REPORT_JSON}" "${SOURCE_REVISION}" <<'PY'
import json
import sys
p, src_rev = sys.argv[1:3]
with open(p, "r", encoding="utf-8") as f:
    rep = json.load(f)
rep["source_revision"] = src_rev
with open(p, "w", encoding="utf-8") as f:
    json.dump(rep, f, indent=2)
    f.write("\n")
PY
  fi
fi

rm -f "${DISCOVERY_JSON}"

COMPACT_RECORDS="$(python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])), separators=(",",":")))' "${UPSTREAM_RECORDS_JSON}")"
autosync::preflight::write_github_output "stage_dir" "${STAGE_DIR}"
autosync::preflight::write_github_output "upstream_records" "${COMPACT_RECORDS}"
autosync::preflight::write_github_output "source_revision" "${SOURCE_REVISION}"
