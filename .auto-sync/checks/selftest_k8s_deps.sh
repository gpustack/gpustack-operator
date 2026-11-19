#!/usr/bin/env bash
# .auto-sync/checks/selftest_k8s_deps.sh - offline self-tests for k8s-deps-sync.
#
# Usage:
#   bash .auto-sync/checks/selftest_k8s_deps.sh [repo-root]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"
ROOT_DIR="${1:-${DEFAULT_ROOT}}"
ROOT_DIR="$(cd "${ROOT_DIR}" && pwd -P)"

FAILS=0
PASSES=0
SCRATCH=""

cleanup() {
  if [[ -n "${SCRATCH}" && -d "${SCRATCH}" ]]; then
    chmod -R u+w "${SCRATCH}" 2>/dev/null || true
    rm -rf "${SCRATCH}"
  fi
}
trap cleanup EXIT

pass() {
  printf 'PASS  [k8s-deps] %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  [k8s-deps] %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

SCRATCH="$(cd "$(mktemp -d)" && pwd -P)"

TASKPACK_FILE="${ROOT_DIR}/.auto-sync/k8s-deps-taskpack.yaml"
INSTRUCTIONS_FILE="${ROOT_DIR}/.auto-sync/instructions/k8s-deps.md"
PROPOSAL_FILE="${ROOT_DIR}/.auto-sync/proposals/k8s-deps.json"
PREFLIGHT_SCRIPT="${ROOT_DIR}/.auto-sync/scripts/preflight-k8s-deps.sh"
VALIDATE_SCRIPT="${ROOT_DIR}/.auto-sync/checks/validate_k8s_deps.sh"

for req in "${TASKPACK_FILE}" "${INSTRUCTIONS_FILE}" "${PROPOSAL_FILE}" "${PREFLIGHT_SCRIPT}" "${VALIDATE_SCRIPT}"; do
  if [[ ! -f "${req}" ]]; then
    fail "missing required k8s-deps-sync deliverable: ${req#"${ROOT_DIR}/"}"
  fi
done

if (( FAILS > 0 )); then
  printf 'K8S-DEPS SELFTEST FAILED (RED): %d missing deliverable(s)\n' "${FAILS}" >&2
  exit 1
fi

HOST_GOMODCACHE="$(go env GOMODCACHE)"
HOST_GOCACHE="$(go env GOCACHE)"

# Helper to copy the minimal repo subset needed by preflight and validation.
copy_k8s_deps_tree() {
  local src_root="$1"
  local dst_root="$2"
  local rel_paths=(
    "go.mod"
    "go.sum"
    "pack/gpustack-operator/Dockerfile"
    "hack/deps.sh"
    "hack/lib"
    "hack/staging"
    "hack/check/upgrade-compat.sh"
    "hack/check/toolpins-selftest.sh"
    "pkg/worker/settings/value.go"
    "docs/reference/settings.md"
    "api"
    "staging"
    ".auto-sync/scripts/lib-preflight.sh"
    ".auto-sync/checks/lib.sh"
    ".auto-sync/proposals/k8s-deps.json"
  )
  local rel
  for rel in "${rel_paths[@]}"; do
    if [[ -e "${src_root}/${rel}" ]]; then
      mkdir -p "${dst_root}/$(dirname "${rel}")"
      cp -R "${src_root}/${rel}" "${dst_root}/$(dirname "${rel}")/"
    fi
  done
  cp "${PREFLIGHT_SCRIPT}" "${dst_root}/.auto-sync/scripts/preflight-k8s-deps.sh"
  cp "${VALIDATE_SCRIPT}" "${dst_root}/.auto-sync/checks/validate_k8s_deps.sh"
  chmod +x "${dst_root}/.auto-sync/scripts/preflight-k8s-deps.sh" "${dst_root}/.auto-sync/checks/validate_k8s_deps.sh"
}

# 1. Offline preflight-k8s-deps.sh fixture discovery test.
PREFLIGHT_REPO="${SCRATCH}/preflight-repo"
mkdir -p "${PREFLIGHT_REPO}/.auto-sync/scripts" "${PREFLIGHT_REPO}/.auto-sync/checks" "${PREFLIGHT_REPO}/.auto-sync/proposals"
copy_k8s_deps_tree "${ROOT_DIR}" "${PREFLIGHT_REPO}"

git -C "${PREFLIGHT_REPO}" init -q -b main
git -C "${PREFLIGHT_REPO}" config user.name "k8s-deps-selftest"
git -C "${PREFLIGHT_REPO}" config user.email "k8s-deps-selftest@gpustack.local"
git -C "${PREFLIGHT_REPO}" config commit.gpgsign false
git -C "${PREFLIGHT_REPO}" add -A
git -C "${PREFLIGHT_REPO}" commit -q -m "baseline"
BASE_SHA="$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)"

FIXTURE_META="${SCRATCH}/fixture-meta.json"
python3 - "${ROOT_DIR}" "${FIXTURE_META}" <<'PY'
import json
import os
import re
import sys

root_dir, meta_out = sys.argv[1:3]


def read(rel: str) -> str:
    with open(os.path.join(root_dir, rel), "r", encoding="utf-8") as f:
        return f.read()


df_txt = read("pack/gpustack-operator/Dockerfile")
gomod_txt = read("go.mod")
deps_txt = read("hack/deps.sh")


def get_arg(name: str) -> str:
    m = re.search(rf'^ARG {re.escape(name)}="([^"]+)"$', df_txt, re.MULTILINE)
    assert m, f"missing ARG {name}"
    return m.group(1).strip()


def get_mod(mod: str) -> str:
    m = re.search(rf'^\s*{re.escape(mod)}\s+(v\d+\.\d+\.\d+)\b', gomod_txt, re.MULTILINE)
    assert m, f"missing {mod} in go.mod"
    return m.group(1).strip()


def bump_patch(ver: str) -> str:
    prefix = "v" if ver.startswith("v") else ""
    parts = ver.lstrip("v").split(".")
    assert len(parts) == 3, f"unexpected semver {ver}"
    return f"{prefix}{parts[0]}.{parts[1]}.{int(parts[2]) + 1}"


def bump_minor(ver: str) -> str:
    prefix = "v" if ver.startswith("v") else ""
    parts = ver.lstrip("v").split(".")
    assert len(parts) == 3, f"unexpected semver {ver}"
    return f"{prefix}{parts[0]}.{int(parts[1]) + 1}.0"


k8s_mod_cur = get_mod("k8s.io/api")
k8s_mod_next = bump_patch(k8s_mod_cur)
k8s_mod_cross_minor = bump_minor(k8s_mod_cur)
kubectl_cur = get_arg("KUBECTL_VERSION")
k8s_rel_cur = f"v{kubectl_cur}"
k8s_rel_next = bump_patch(k8s_rel_cur)
k8s_rel_cross_minor = bump_minor(k8s_rel_cur)

helm_cur = get_arg("HELM_VERSION")
helm_next = bump_patch(helm_cur)
vals_cur = get_arg("VALS_VERSION")
vals_next = bump_patch(vals_cur)
kust_cur = get_arg("KUSTOMIZE_VERSION")
kust_next = bump_patch(kust_cur)
skopeo_cur = get_arg("SKOPEO_VERSION")
ytt_cur = get_arg("YTT_VERSION")

cr_cur = get_mod("sigs.k8s.io/controller-runtime")
cr_next = bump_patch(cr_cur)
cr_cross_minor = bump_minor(cr_cur)
kueue_cur = get_mod("sigs.k8s.io/kueue")
kueue_cross_minor = bump_minor(kueue_cur)

m_chart = re.search(r"function chart_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF", deps_txt, re.DOTALL)
assert m_chart, "missing chart_staging() in hack/deps.sh"
first_chart_line = next(ln.strip() for ln in m_chart.group(1).splitlines() if ln.strip())
chart_url, chart_ver, chart_dest = first_chart_line.split()
chart_name = os.path.basename(chart_dest)
chart_ver_next = bump_patch(chart_ver)

with open(meta_out, "w", encoding="utf-8") as f:
    json.dump({
        "k8s_minor": ".".join(k8s_mod_cur.lstrip("v").split(".")[:2]),
        "k8s_mod_cur": k8s_mod_cur,
        "k8s_mod_next": k8s_mod_next,
        "k8s_mod_cross_minor": k8s_mod_cross_minor,
        "k8s_rel_cur": k8s_rel_cur,
        "k8s_rel_next": k8s_rel_next,
        "k8s_rel_cross_minor": k8s_rel_cross_minor,
        "helm_cur": helm_cur,
        "helm_next": helm_next,
        "vals_cur": vals_cur,
        "vals_next": vals_next,
        "kust_cur": kust_cur,
        "kust_next": kust_next,
        "skopeo_cur": skopeo_cur,
        "ytt_cur": ytt_cur,
        "cr_cur": cr_cur,
        "cr_next": cr_next,
        "cr_cross_minor": cr_cross_minor,
        "kueue_cur": kueue_cur,
        "kueue_cross_minor": kueue_cross_minor,
        "first_chart_name": chart_name,
        "first_chart_ver": chart_ver,
        "first_chart_ver_next": chart_ver_next,
    }, f, indent=2)
PY

read_meta() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "${FIXTURE_META}" "$1"
}

K8S_MOD_CUR="$(read_meta k8s_mod_cur)"
K8S_MOD_NEXT="$(read_meta k8s_mod_next)"
K8S_MOD_CROSS_MINOR="$(read_meta k8s_mod_cross_minor)"
K8S_REL_CUR="$(read_meta k8s_rel_cur)"
K8S_REL_NEXT="$(read_meta k8s_rel_next)"
K8S_REL_CROSS_MINOR="$(read_meta k8s_rel_cross_minor)"
HELM_CUR="$(read_meta helm_cur)"
HELM_NEXT="$(read_meta helm_next)"
VALS_CUR="$(read_meta vals_cur)"
VALS_NEXT="$(read_meta vals_next)"
KUST_CUR="$(read_meta kust_cur)"
KUST_NEXT="$(read_meta kust_next)"
SKOPEO_CUR="$(read_meta skopeo_cur)"
YTT_CUR="$(read_meta ytt_cur)"
CR_CUR="$(read_meta cr_cur)"
CR_NEXT="$(read_meta cr_next)"
CR_CROSS_MINOR="$(read_meta cr_cross_minor)"
KUEUE_CUR="$(read_meta kueue_cur)"
KUEUE_CROSS_MINOR="$(read_meta kueue_cross_minor)"
FIRST_CHART_NAME="$(read_meta first_chart_name)"
FIRST_CHART_VER="$(read_meta first_chart_ver)"
FIRST_CHART_VER_NEXT="$(read_meta first_chart_ver_next)"

FIXTURE_DISCOVERY="${SCRATCH}/fixture-discovery"
mkdir -p "${FIXTURE_DISCOVERY}"

cat > "${FIXTURE_DISCOVERY}/kubernetes-releases.json" <<JSON
[
  {"tag_name": "${K8S_REL_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${K8S_REL_NEXT}-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "${K8S_REL_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "${K8S_REL_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/helm-releases.json" <<JSON
[
  {"tag_name": "v4.0.0", "prerelease": false, "draft": false},
  {"tag_name": "v${HELM_NEXT}-rc.1", "prerelease": true, "draft": false},
  {"tag_name": "v${HELM_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "v${HELM_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kustomize-tags.txt" <<TXT
0000000000000000000000000000000000000001	refs/tags/kustomize/v${KUST_NEXT}
0000000000000000000000000000000000000002	refs/tags/kustomize/v${KUST_NEXT}-rc.1
0000000000000000000000000000000000000003	refs/tags/kustomize/v${KUST_CUR}
TXT

cat > "${FIXTURE_DISCOVERY}/skopeo-releases.json" <<JSON
[
  {"tag_name": "v${SKOPEO_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/vals-releases.json" <<JSON
[
  {"tag_name": "v${VALS_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "v${VALS_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/ytt-releases.json" <<JSON
[
  {"tag_name": "v${YTT_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/controller-runtime-releases.json" <<JSON
[
  {"tag_name": "${CR_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${CR_NEXT}", "prerelease": false, "draft": false},
  {"tag_name": "${CR_CUR}", "prerelease": false, "draft": false}
]
JSON

cat > "${FIXTURE_DISCOVERY}/kueue-releases.json" <<JSON
[
  {"tag_name": "${KUEUE_CROSS_MINOR}", "prerelease": false, "draft": false},
  {"tag_name": "${KUEUE_CUR}", "prerelease": false, "draft": false}
]
JSON

STAGE_DISC="${SCRATCH}/stage-disc/k8s-deps"
GHA_DISC="${SCRATCH}/gha-disc.txt"
: > "${GHA_DISC}"

if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_DISCOVERY}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_SOURCE_REVISION=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  GITHUB_OUTPUT="${GHA_DISC}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_DISC}" >/dev/null
) && \
python3 - "${STAGE_DISC}" "${GHA_DISC}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import sys

stage_dir, gha_path, base_sha, meta_path = sys.argv[1:5]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert isinstance(recs, list) and len(recs) == 8, recs
by_id = {r["upstreamId"]: r for r in recs}

assert by_id["kustomize-cli"]["version"] == meta["kust_next"], by_id["kustomize-cli"]
assert by_id["kustomize-cli"]["status"] == "available", by_id["kustomize-cli"]

assert by_id["helm-cli"]["version"] == f"v{meta['helm_next']}", by_id["helm-cli"]
assert by_id["helm-cli"]["status"] == "available", by_id["helm-cli"]

assert by_id["kubernetes"]["version"] == meta["k8s_rel_next"], by_id["kubernetes"]
assert by_id["kubernetes"]["status"] == "available", by_id["kubernetes"]

assert by_id["vals-cli"]["version"] == meta["vals_next"], by_id["vals-cli"]
assert by_id["vals-cli"]["status"] == "available", by_id["vals-cli"]

assert by_id["skopeo-cli"]["version"] == meta["skopeo_cur"], by_id["skopeo-cli"]
assert by_id["skopeo-cli"]["status"] == "up_to_date", by_id["skopeo-cli"]

assert by_id["ytt-cli"]["version"] == meta["ytt_cur"], by_id["ytt-cli"]
assert by_id["ytt-cli"]["status"] == "up_to_date", by_id["ytt-cli"]

assert by_id["controller-runtime"]["version"] == meta["cr_next"], by_id["controller-runtime"]
assert by_id["controller-runtime"]["status"] == "available", by_id["controller-runtime"]

assert by_id["kueue-sdk"]["version"] == meta["kueue_cur"], by_id["kueue-sdk"]
assert by_id["kueue-sdk"]["status"] == "up_to_date", by_id["kueue-sdk"]

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["k8s_minor"] == meta["k8s_minor"], report
assert report["helm_major"] == "3", report
assert report["base_sha"] == base_sha, report
assert report["source_revision"] == base_sha, report

for rel in (
    "pack/gpustack-operator/Dockerfile",
    "hack/deps.sh",
    "hack/lib/helm.sh",
    "go.mod",
    "api",
):
    assert os.path.exists(os.path.join(stage_dir, "baseline", rel)), f"missing baseline {rel}"

with open(gha_path, "r", encoding="utf-8") as f:
    gha_txt = f.read()
assert f"\n{stage_dir}\n" in gha_txt, gha_txt
assert f"\n{base_sha}\n" in gha_txt, gha_txt
assert "upstream_records<<" in gha_txt, gha_txt
PY
then
  if [[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
     [[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]]; then
    pass "preflight-k8s-deps.sh filters cross-minor k8s/helm v4, normalizes kustomize, and snapshots baselines"
  else
    fail "preflight-k8s-deps.sh discovery left dirty git tree"
  fi
else
  fail "preflight-k8s-deps.sh offline discovery fixture test"
fi

# 1b. Helm v3.22.1 (k8s.io v0.37.1) is skipped in favor of compatible Helm v3.21.5 (k8s.io v0.36.6).
HELM_LOCKSTEP_REPO="${SCRATCH}/helm-lockstep-repo"
cp -R "${PREFLIGHT_REPO}" "${HELM_LOCKSTEP_REPO}"
python3 - "${HELM_LOCKSTEP_REPO}" <<'PY'
import os
import re
import sys

repo = sys.argv[1]
df_p = os.path.join(repo, "pack/gpustack-operator/Dockerfile")
helm_sh_p = os.path.join(repo, "hack/lib/helm.sh")
gomod_p = os.path.join(repo, "go.mod")

with open(df_p, "r", encoding="utf-8") as f:
    df_txt = f.read()
df_txt = re.sub(r'^ARG HELM_VERSION="[^"]+"$', 'ARG HELM_VERSION="3.21.4"', df_txt, count=1, flags=re.MULTILINE)
with open(df_p, "w", encoding="utf-8") as f:
    f.write(df_txt)

with open(helm_sh_p, "r", encoding="utf-8") as f:
    h_txt = f.read()
h_txt = re.sub(r'default is v3\.\d+\.\d+\.', 'default is v3.21.4.', h_txt, count=1)
h_txt = re.sub(r'^helm_version=\$\{HELM_VERSION:-"v3\.\d+\.\d+"\}$', 'helm_version=${HELM_VERSION:-"v3.21.4"}', h_txt, count=1, flags=re.MULTILINE)
with open(helm_sh_p, "w", encoding="utf-8") as f:
    f.write(h_txt)

with open(gomod_p, "r", encoding="utf-8") as f:
    gm_txt = f.read()
gm_txt = re.sub(r'^(\s*helm\.sh/helm/v3\s+)v3\.\d+\.\d+', r'\g<1>v3.21.4', gm_txt, count=1, flags=re.MULTILINE)
with open(gomod_p, "w", encoding="utf-8") as f:
    f.write(gm_txt)
PY
git -C "${HELM_LOCKSTEP_REPO}" add -A
git -C "${HELM_LOCKSTEP_REPO}" commit -q --allow-empty -m "pin helm 3.21.4 for lockstep test"

FIXTURE_HELM_LOCKSTEP="${SCRATCH}/fixture-helm-lockstep"
cp -R "${FIXTURE_DISCOVERY}" "${FIXTURE_HELM_LOCKSTEP}"
cat > "${FIXTURE_HELM_LOCKSTEP}/helm-releases.json" <<'JSON'
[
  {"tag_name": "v4.0.0", "prerelease": false, "draft": false},
  {"tag_name": "v3.22.2", "prerelease": false, "draft": false},
  {"tag_name": "v3.22.1", "prerelease": false, "draft": false},
  {"tag_name": "v3.21.5", "prerelease": false, "draft": false},
  {"tag_name": "v3.21.4", "prerelease": false, "draft": false}
]
JSON
cat > "${FIXTURE_HELM_LOCKSTEP}/helm-v3.22.2-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

require k8s.io/api v1.0.0
GOMOD
cat > "${FIXTURE_HELM_LOCKSTEP}/helm-v3.22.1-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

require (
	k8s.io/api v0.35.0
	k8s.io/client-go v0.37.1 // max(api, client-go) must select 0.37
)
GOMOD
cat > "${FIXTURE_HELM_LOCKSTEP}/helm-v3.21.5-go.mod" <<'GOMOD'
module helm.sh/helm/v3

go 1.24.0

exclude (
	k8s.io/api v0.37.9 // ignored outside require
)

replace k8s.io/client-go => k8s.io/client-go v0.37.9 // ignored outside require

require k8s.io/api v0.36.6 // indirect
require (
	k8s.io/client-go v0.36.6
)
GOMOD

STAGE_HELM_LOCKSTEP="${SCRATCH}/stage-helm-lockstep/k8s-deps"
HELM_LOCKSTEP_ERR="${SCRATCH}/helm-lockstep.stderr"
if (
  cd "${HELM_LOCKSTEP_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_HELM_LOCKSTEP}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_SOURCE_REVISION=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${HELM_LOCKSTEP_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_HELM_LOCKSTEP}" >/dev/null 2>"${HELM_LOCKSTEP_ERR}"
) && \
python3 - "${STAGE_HELM_LOCKSTEP}" "${HELM_LOCKSTEP_ERR}" <<'PY'
import json
import os
import re
import sys

stage_dir, err_path = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["helm-cli"]["status"] == "available", by_id["helm-cli"]
assert by_id["helm-cli"]["version"] == "v3.21.5", by_id["helm-cli"]
assert "v3.22.1" not in by_id["helm-cli"]["recentVersions"], by_id["helm-cli"]

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
skipped = {item["tag"]: item for item in rep.get("skipped_helm_candidates", [])}
assert skipped.get("v3.22.2", {}).get("reason") == "incompatible_k8s_minor", skipped
assert skipped.get("v3.22.1", {}).get("reason") == "incompatible_k8s_minor", skipped

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert "skipping Helm v3.22.2" in err_txt, err_txt
assert "skipping Helm v3.22.1" in err_txt, err_txt
assert not re.search(r"\.go:", err_txt), err_txt
PY
then
  pass "preflight-k8s-deps.sh skips Helm v3.22.1 (k8s.io v0.37.1) and selects compatible Helm v3.21.5"
else
  fail "preflight-k8s-deps.sh Helm k8s.io minor lockstep filter"
fi

# 1c. Helm go.mod HTTP retry, fetch_error diagnostics, atomic cache write, and poisoned cache recovery.
STAGE_HELM_HTTP="${SCRATCH}/stage-helm-http/k8s-deps"
mkdir -p "${STAGE_HELM_HTTP}/discovery"
printf '<html>stale poisoned cache</html>\n' > "${STAGE_HELM_HTTP}/discovery/helm-v3.21.5-go.mod"
HELM_HTTP_ERR="${SCRATCH}/helm-http.stderr"
if python3 - "${HELM_LOCKSTEP_REPO}" "${FIXTURE_HELM_LOCKSTEP}" "${STAGE_HELM_HTTP}" "${HELM_HTTP_ERR}" "${HOST_GOMODCACHE}" "${HOST_GOCACHE}" <<'PY'
import http.server
import json
import os
import subprocess
import sys
import threading

repo_dir, fixture_dir, stage_dir, err_path, gomodcache, gocache = sys.argv[1:7]
counts = {}
lock = threading.Lock()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with lock:
            counts[self.path] = counts.get(self.path, 0) + 1
            n = counts[self.path]
        if self.path == "/v3.22.2/go.mod":
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b"transient outage")
        elif self.path == "/v3.22.1/go.mod":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"<html><body>502 Bad Gateway</body></html>")
        elif self.path == "/v3.21.5/go.mod":
            if n < 3:
                self.send_response(503)
                self.end_headers()
                self.wfile.write(b"retry me")
            else:
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"module helm.sh/helm/v3\n\nrequire k8s.io/client-go v0.36.6\n")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass


srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port = srv.server_address[1]
t = threading.Thread(target=srv.serve_forever, daemon=True)
t.start()
try:
    env = dict(os.environ)
    env["AUTOSYNC_K8S_DEPS_FIXTURE_DIR"] = fixture_dir
    env["AUTOSYNC_HELM_GOMOD_BASE_URL"] = f"http://127.0.0.1:{port}"
    env["AUTOSYNC_SKIP_GO_MOD_DOWNLOAD"] = "true"
    env["AUTOSYNC_SKIP_SOURCE_REVISION"] = "true"
    env["GOMODCACHE"] = gomodcache
    env["GOCACHE"] = gocache
    with open(err_path, "w", encoding="utf-8") as ef:
        res = subprocess.run(
            ["bash", os.path.join(repo_dir, ".auto-sync/scripts/preflight-k8s-deps.sh"), stage_dir],
            cwd=repo_dir,
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=ef,
        )
    assert res.returncode == 0, f"preflight exited {res.returncode}"
finally:
    srv.shutdown()
    srv.server_close()

assert counts.get("/v3.22.2/go.mod") == 3, counts
assert counts.get("/v3.21.5/go.mod") == 3, counts
assert not os.path.exists(os.path.join(stage_dir, "discovery", "helm-v3.22.1-go.mod"))
assert not os.path.exists(os.path.join(stage_dir, "discovery", "helm-v3.22.2-go.mod"))
with open(os.path.join(stage_dir, "discovery", "helm-v3.21.5-go.mod"), "r", encoding="utf-8") as f:
    assert "k8s.io/client-go v0.36.6" in f.read()

with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["target"]["helm_tag"] == "v3.21.5", rep["target"]
skipped = {item["tag"]: item for item in rep.get("skipped_helm_candidates", [])}
assert skipped.get("v3.22.2", {}).get("reason") == "fetch_error", skipped
assert skipped.get("v3.22.1", {}).get("reason") == "fetch_error", skipped

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert "failed to fetch Helm v3.22.2 go.mod after retries" in err_txt, err_txt
assert "failed to fetch Helm v3.22.1 go.mod after retries" in err_txt, err_txt
PY
then
  pass "preflight-k8s-deps.sh retries transient Helm go.mod fetches, avoids cache poisoning, and records fetch_error"
else
  fail "preflight-k8s-deps.sh Helm go.mod retry and atomic cache"
fi

# 2. Offline preflight-k8s-deps.sh detached source_revision test via cached mod_staging tarballs.
FIXTURE_STAGE="${SCRATCH}/fixture-stage"
cp -R "${FIXTURE_DISCOVERY}" "${FIXTURE_STAGE}"
mkdir -p "${FIXTURE_STAGE}/deps-cache/staging/k8s.io"

for mod_name in code-generator api apimachinery kube-aggregator apiextensions-apiserver; do
  tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/${mod_name}-${K8S_MOD_NEXT}.tar.gz" \
    --exclude='_VERSION_' \
    -C "${ROOT_DIR}/staging/k8s.io/${mod_name}" .
done
# Seed unpatched code-generator source so hack/staging/k8s.io/code-generator/001_enhanced.patch applies cleanly.
rm -f "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/code-generator-${K8S_MOD_NEXT}.tar.gz"
CG_TMP="${SCRATCH}/cg-unpatched"
mkdir -p "${CG_TMP}"
cp -R "${ROOT_DIR}/staging/k8s.io/code-generator/." "${CG_TMP}/"
rm -f "${CG_TMP}/_VERSION_"
(cd "${CG_TMP}" && patch -p1 -R --silent < "${ROOT_DIR}/hack/staging/k8s.io/code-generator/001_enhanced.patch")
tar -zcf "${FIXTURE_STAGE}/deps-cache/staging/k8s.io/code-generator-${K8S_MOD_NEXT}.tar.gz" -C "${CG_TMP}" .

STAGE_SRC_REV="${SCRATCH}/stage-srcrev/k8s-deps"
GHA_SRC_REV="${SCRATCH}/gha-srcrev.txt"
: > "${GHA_SRC_REV}"

if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_GENERATE=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  GITHUB_OUTPUT="${GHA_SRC_REV}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_SRC_REV}" >/dev/null
) && \
SRC_REV="$(python3 - "${STAGE_SRC_REV}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    report = json.load(f)
assert report["source_revision"] != base_sha, report
assert report["patch_clean"] is True, report
assert report["staging_updated"] is True, report
print(report["source_revision"])
PY
)" && \
[[ -n "${SRC_REV}" && "${SRC_REV}" != "${BASE_SHA}" ]] && \
grep -q "kubernetes/api ${K8S_MOD_NEXT}" <<<"$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:hack/deps.sh")" && \
[[ "$(git -C "${PREFLIGHT_REPO}" show "${SRC_REV}:staging/k8s.io/api/_VERSION_")" == "${K8S_MOD_NEXT}" ]] && \
grep -q '100644 blob .*\.auto-sync/\.stage/k8s-deps/k8s-deps-preflight-report\.json$' <<<"$(git -C "${PREFLIGHT_REPO}" ls-tree -r "${SRC_REV}" .auto-sync/.stage/k8s-deps)" && \
grep -q '100644 blob .*\.auto-sync/\.stage/k8s-deps/upstream-records\.json$' <<<"$(git -C "${PREFLIGHT_REPO}" ls-tree -r "${SRC_REV}" .auto-sync/.stage/k8s-deps)" && \
! grep -q '^120000 ' <<<"$(git -C "${PREFLIGHT_REPO}" ls-tree -r "${SRC_REV}" .auto-sync/.stage/k8s-deps)" && \
[[ -z "$(git -C "${PREFLIGHT_REPO}" status --porcelain)" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse HEAD)" == "${BASE_SHA}" ]] && \
[[ "$(git -C "${PREFLIGHT_REPO}" rev-parse refs/heads/main)" == "${BASE_SHA}" ]]; then
  pass "preflight-k8s-deps.sh stages mod_staging and .auto-sync/.stage/k8s-deps onto detached source_revision while preserving main"
else
  fail "preflight-k8s-deps.sh detached source_revision staging"
fi

# 2b. Uncommitted local edit in repo_root survives preflight-k8s-deps.sh untouched (I12).
printf '// local uncommitted edit\n' >> "${PREFLIGHT_REPO}/go.mod"
STAGE_DIRTY="${SCRATCH}/stage-dirty/k8s-deps"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  AUTOSYNC_SKIP_GENERATE=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_DIRTY}" >/dev/null
) && \
grep -q '// local uncommitted edit' "${PREFLIGHT_REPO}/go.mod"; then
  pass "preflight-k8s-deps.sh preserves uncommitted local edits in repo_root"
else
  fail "preflight-k8s-deps.sh preserves uncommitted local edits in repo_root"
fi
git -C "${PREFLIGHT_REPO}" checkout -- go.mod

# 2c. Partial upstream tag release / go mod tidy failure degrades gracefully without crashing (I11).
WRAP_GO_BIN="${SCRATCH}/wrap-go-bin"
mkdir -p "${WRAP_GO_BIN}"
cat > "${WRAP_GO_BIN}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "mod" && "${2:-}" == "tidy" ]]; then
  echo "api/v1/zz_generated.apiservice.go:17:2: simulated go mod tidy failure: unknown revision" >&2
  echo "open api/v1/doc.go: simulated error" >&2
  echo "::error::simulated workflow command" >&2
  echo "::warning file=x.go,line=1::simulated warning" >&2
  echo "::add-mask::secret" >&2
  echo "##[error]simulated azure matcher" >&2
  echo "##[warning]simulated azure warning" >&2
  echo "##[group]simulated azure group" >&2
  exit 1
fi
exec "${REAL_GO_BIN}" "$@"
SH
chmod +x "${WRAP_GO_BIN}/go"
REAL_GO_BIN="$(command -v go)"
STAGE_FAIL_TIDY="${SCRATCH}/stage-fail-tidy/k8s-deps"
FAIL_TIDY_ERR="${SCRATCH}/fail-tidy.stderr"
mkdir -p "${SCRATCH}/stage-fail-tidy/go-mod"
: > "${SCRATCH}/stage-fail-tidy/go-mod/.warmed"
if (
  cd "${PREFLIGHT_REPO}" && \
  REAL_GO_BIN="${REAL_GO_BIN}" \
  PATH="${WRAP_GO_BIN}:${PATH}" \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
  AUTOSYNC_SKIP_GENERATE=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_FAIL_TIDY}" >/dev/null 2>"${FAIL_TIDY_ERR}"
) && \
python3 - "${STAGE_FAIL_TIDY}" "${FAIL_TIDY_ERR}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import re
import sys

stage_dir, err_path, base_sha, meta_path = sys.argv[1:5]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["patch_clean"] is False, rep
assert rep["staging_updated"] is False, rep
assert rep["source_revision"] != base_sha, rep
assert "go mod tidy/download failed" in rep.get("staging_error", ""), rep
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
    assert rep["target"][k] == rep["current"][k], (k, rep["target"], rep["current"])
assert rep["target"]["vals_cli"] == meta["vals_next"], rep["target"]
assert rep.get("failed_target", {}).get("kubernetes") == meta["k8s_rel_next"], rep
held_back = rep.get("held_back_upstreams") or {}
assert held_back.get("kubernetes", {}).get("candidate") == meta["k8s_rel_next"], held_back
assert held_back["kubernetes"].get("reason"), held_back

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
for uid, cur_v in (
    ("kubernetes", meta["k8s_rel_cur"]),
    ("controller-runtime", meta["cr_cur"]),
    ("kueue-sdk", meta["kueue_cur"]),
    ("helm-cli", f"v{meta['helm_cur']}"),
):
    assert by_id[uid]["status"] == "up_to_date", by_id[uid]
    assert by_id[uid]["version"] == cur_v, by_id[uid]
assert by_id["vals-cli"]["status"] == "available", by_id["vals-cli"]
assert by_id["kustomize-cli"]["status"] == "available", by_id["kustomize-cli"]

with open(err_path, "r", encoding="utf-8") as f:
    err_txt = f.read()
assert not re.search(r"\.go:", err_txt), err_txt
for marker in ("::error", "::warning", "::add-mask", "##[error]", "##[warning]", "##[group]"):
    assert marker not in err_txt.lower(), (marker, err_txt)
assert ".go@17:2" in err_txt, err_txt
assert "open api/v1/doc.go@ simulated error" in err_txt, err_txt
PY
then
  pass "preflight-k8s-deps.sh sanitizes .go:<line> errors, holds Go modules up_to_date on tidy failure"
else
  fail "preflight-k8s-deps.sh degrades gracefully when go mod tidy fails"
fi

# 2d. When staging fails with both k8s and helm bumped, retry without helm bump resets work_repo completely and succeeds.
WRAP_GO_HELM_FAIL="${SCRATCH}/wrap-go-helm-fail"
mkdir -p "${WRAP_GO_HELM_FAIL}"
cat > "${WRAP_GO_HELM_FAIL}/go" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == "mod" && ( "${2:-}" == "tidy" || "${2:-}" == "download" ) ]]; then
  if [[ -f "go.mod" ]] && grep -q "helm.sh/helm/v3 v${FAIL_HELM_NEXT}" go.mod; then
    mkdir -p staging/k8s.io/api
    printf 'stale mutated state from attempt 1\n' > staging/k8s.io/api/dirty_from_attempt1.txt
    echo "simulated helm bump failure for v${FAIL_HELM_NEXT}" >&2
    exit 1
  fi
  exit 0
fi
exec "${REAL_GO_BIN}" "$@"
SH
chmod +x "${WRAP_GO_HELM_FAIL}/go"
STAGE_HELM_FB="${SCRATCH}/stage-helm-fb/k8s-deps"
mkdir -p "${SCRATCH}/stage-helm-fb/go-mod"
: > "${SCRATCH}/stage-helm-fb/go-mod/.warmed"
if (
  cd "${PREFLIGHT_REPO}" && \
  REAL_GO_BIN="${REAL_GO_BIN}" \
  FAIL_HELM_NEXT="${HELM_NEXT}" \
  PATH="${WRAP_GO_HELM_FAIL}:${PATH}" \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_STAGE}" \
  AUTOSYNC_SKIP_GENERATE=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_HELM_FB}" >/dev/null 2>&1
) && \
FB_REV="$(python3 - "${STAGE_HELM_FB}" "${BASE_SHA}" "${FIXTURE_META}" <<'PY'
import json
import os
import sys

stage_dir, base_sha, meta_path = sys.argv[1:4]
with open(meta_path, "r", encoding="utf-8") as f:
    meta = json.load(f)
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["patch_clean"] is True, rep
assert rep["staging_updated"] is True, rep
assert rep["source_revision"] != base_sha, rep
assert rep["target"]["helm_tag"] == f"v{meta['helm_cur']}", rep["target"]
held_back = rep.get("held_back_upstreams") or {}
assert held_back.get("helm-cli", {}).get("candidate") == f"v{meta['helm_next']}", held_back

with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
by_id = {r["upstreamId"]: r for r in recs}
assert by_id["kubernetes"]["status"] == "available", by_id["kubernetes"]
assert by_id["kubernetes"]["version"] == meta["k8s_rel_next"], by_id["kubernetes"]
assert by_id["helm-cli"]["status"] == "up_to_date", by_id["helm-cli"]
assert by_id["helm-cli"]["version"] == f"v{meta['helm_cur']}", by_id["helm-cli"]
print(rep["source_revision"])
PY
)" && \
grep -q "k8s.io/api ${K8S_MOD_NEXT}" <<<"$(git -C "${PREFLIGHT_REPO}" show "${FB_REV}:go.mod")" && \
grep -q "helm.sh/helm/v3 v${HELM_CUR}" <<<"$(git -C "${PREFLIGHT_REPO}" show "${FB_REV}:go.mod")" && \
! git -C "${PREFLIGHT_REPO}" cat-file -e "${FB_REV}:staging/k8s.io/api/dirty_from_attempt1.txt" 2>/dev/null; then
  pass "preflight-k8s-deps.sh retries without helm bump and stages k8s update cleanly"
else
  fail "preflight-k8s-deps.sh helm fallback retry"
fi

# 2e. Gitless tar-unpacked work_repo copy passes hack/check/agents-shell.sh after ensure_ephemeral_git and cleans up .git.
WORK_GITLESS="${SCRATCH}/work-repo-gitless/gpustack.ai/gpustack"
mkdir -p "${WORK_GITLESS}"
tar --exclude='.git' -cf - -C "${PREFLIGHT_REPO}" . | tar -xf - -C "${WORK_GITLESS}"
SC_FOR_WORK="${SHELLCHECK_BIN:-}"
if [[ -z "${SC_FOR_WORK}" && -x "${ROOT_DIR}/.sbin/shellcheck" ]]; then
  SC_FOR_WORK="${ROOT_DIR}/.sbin/shellcheck"
fi
if [[ -z "${SC_FOR_WORK}" ]]; then
  SC_FOR_WORK="$(type -P shellcheck 2>/dev/null || true)"
fi
if [[ -z "${SC_FOR_WORK}" ]]; then
  SC_FOR_WORK="${SCRATCH}/stub-shellcheck.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${SC_FOR_WORK}"
  chmod +x "${SC_FOR_WORK}"
fi
pre_rc=0
SHELLCHECK_BIN="${SC_FOR_WORK}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${WORK_GITLESS}" >/dev/null 2>&1 || pre_rc=$?
ensure_rc=0
(
  # shellcheck source=lib.sh
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
  autosync::ensure_ephemeral_git "${WORK_GITLESS}"
) || ensure_rc=$?
post_rc=0
SHELLCHECK_BIN="${SC_FOR_WORK}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${WORK_GITLESS}" >/dev/null 2>&1 || post_rc=$?
cleanup_rc=0
(
  # shellcheck source=lib.sh
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.auto-sync/checks/lib.sh"
  autosync::cleanup_ephemeral_git "${WORK_GITLESS}"
) || cleanup_rc=$?
# shellcheck disable=SC2016
if (( pre_rc == 2 && ensure_rc == 0 && post_rc == 0 && cleanup_rc == 0 )) && [[ ! -e "${WORK_GITLESS}/.git" ]] && \
   grep -qF 'autosync::ensure_ephemeral_git "${work_repo}"' "${PREFLIGHT_SCRIPT}" && \
   grep -qF 'autosync::cleanup_ephemeral_git "${work_repo}"' "${PREFLIGHT_SCRIPT}"; then
  pass "ensure_ephemeral_git on gitless work_repo copy lets agents-shell.sh succeed (rc=0) and cleans up .git"
else
  fail "ensure_ephemeral_git on gitless work_repo copy (pre_rc=${pre_rc}, ensure_rc=${ensure_rc}, post_rc=${post_rc}, cleanup_rc=${cleanup_rc})"
fi

# 3. Gitless env -i validate_k8s_deps.sh tests (AUTOSYNC_FAST_SELFTEST=true).
CAND_BASE="${SCRATCH}/cand-base"
mkdir -p "${CAND_BASE}/.auto-sync/scripts" "${CAND_BASE}/.auto-sync/checks" "${CAND_BASE}/.auto-sync/proposals"
copy_k8s_deps_tree "${ROOT_DIR}" "${CAND_BASE}"

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
    bash "${cand_dir}/.auto-sync/checks/validate_k8s_deps.sh"
}

# 3a. Positive case: bumping VALS_VERSION in pack/gpustack-operator/Dockerfile.
CAND_POS="${SCRATCH}/cand-pos"
cp -R "${CAND_BASE}" "${CAND_POS}"
python3 - "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${CAND_POS}/pack/gpustack-operator/Dockerfile" "${VALS_CUR}" "${VALS_NEXT}" <<'PY'
import re
import sys
src, dst, v_cur, v_next = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(rf'^ARG VALS_VERSION="{re.escape(v_cur)}"$', f'ARG VALS_VERSION="{v_next}"', txt, count=1, flags=re.MULTILINE)
assert out != txt, "failed to bump VALS_VERSION"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY
python3 - "${CAND_POS}/.auto-sync/proposals/k8s-deps.json" "${VALS_NEXT}" <<'PY'
import json
import sys

p, v_next = sys.argv[1:3]
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["vals-cli"] = v_next
with open(p, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY

if run_validator_env_i "${CAND_POS}" "${STAGE_DISC}" >/dev/null 2>&1 && \
   [[ ! -e "${CAND_POS}/.git" ]] && [[ ! -e "${CAND_POS}/.sbin" ]]; then
  pass "validate_k8s_deps.sh positive VALS_VERSION bump passes guards, upgrade-compat, and toolpins-selftest"
else
  fail "validate_k8s_deps.sh positive VALS_VERSION bump"
fi

# 3b. Negative case 1: mutating LIB_HAMI_CORE_COMMIT in pack/gpustack-operator/Dockerfile.
CAND_NEG1="${SCRATCH}/cand-neg1"
cp -R "${CAND_BASE}" "${CAND_NEG1}"
sed 's/^ARG LIB_HAMI_CORE_COMMIT="[^"]*"$/ARG LIB_HAMI_CORE_COMMIT="1111111111111111111111111111111111111111"/' \
  "${CAND_BASE}/pack/gpustack-operator/Dockerfile" > "${CAND_NEG1}/pack/gpustack-operator/Dockerfile"
if cmp -s "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${CAND_NEG1}/pack/gpustack-operator/Dockerfile"; then
  fail "negative test 3b failed to mutate LIB_HAMI_CORE_COMMIT"
fi

neg1_err=""
if ! neg1_err="$(run_validator_env_i "${CAND_NEG1}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG1}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation in Dockerfile"
else
  fail "validate_k8s_deps.sh fails closed on LIB_HAMI_CORE_COMMIT mutation (${neg1_err})"
fi

# 3c. Negative case 2: mutating chart_staging() in hack/deps.sh.
CAND_NEG2="${SCRATCH}/cand-neg2"
cp -R "${CAND_BASE}" "${CAND_NEG2}"
python3 - "${CAND_BASE}/hack/deps.sh" "${CAND_NEG2}/hack/deps.sh" "${FIRST_CHART_NAME}" "${FIRST_CHART_VER}" "${FIRST_CHART_VER_NEXT}" <<'PY'
import sys
src, dst, cname, cver, cnext = sys.argv[1:6]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
old_tok = f" {cver} ${{charts_dir}}/{cname}"
new_tok = f" {cnext} ${{charts_dir}}/{cname}"
out = txt.replace(old_tok, new_tok, 1)
assert out != txt, f"failed to mutate {old_tok} in hack/deps.sh"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY

neg2_err=""
if ! neg2_err="$(run_validator_env_i "${CAND_NEG2}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG2}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on chart_staging() mutation in hack/deps.sh"
else
  fail "validate_k8s_deps.sh fails closed on chart_staging() mutation (${neg2_err})"
fi

# 3d. Negative case 3: modifying a hand-written .go file under api/v1/.
CAND_NEG3="${SCRATCH}/cand-neg3"
cp -R "${CAND_BASE}" "${CAND_NEG3}"
hand_api_file="$(find "${CAND_NEG3}/api/v1" -maxdepth 1 -type f -name '*.go' ! -name 'zz_generated*' ! -name 'generated.*' | sort | head -n 1)"
if [[ -z "${hand_api_file}" ]]; then
  fail "unable to locate hand-written api/v1/*.go file for negative test"
else
  printf '\n// illegal hand-written api edit\n' >> "${hand_api_file}"
  neg3_err=""
  if ! neg3_err="$(run_validator_env_i "${CAND_NEG3}" "${STAGE_DISC}" 2>&1)" && \
     [[ ! -e "${CAND_NEG3}/.git" ]]; then
    pass "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit"
  else
    fail "validate_k8s_deps.sh fails closed on hand-written api/v1/*.go edit (${neg3_err})"
  fi
fi

# 3e. Negative case 4: bumping HELM_VERSION in Dockerfile without updating hack/lib/helm.sh.
CAND_NEG4="${SCRATCH}/cand-neg4"
cp -R "${CAND_BASE}" "${CAND_NEG4}"
python3 - "${CAND_BASE}/pack/gpustack-operator/Dockerfile" "${CAND_NEG4}/pack/gpustack-operator/Dockerfile" "${HELM_CUR}" "${HELM_NEXT}" <<'PY'
import re
import sys
src, dst, h_cur, h_next = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = re.sub(rf'^ARG HELM_VERSION="{re.escape(h_cur)}"$', f'ARG HELM_VERSION="{h_next}"', txt, count=1, flags=re.MULTILINE)
assert out != txt, f"failed to mutate HELM_VERSION from {h_cur}"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY

neg4_err=""
if ! neg4_err="$(run_validator_env_i "${CAND_NEG4}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG4}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump in Dockerfile"
else
  fail "validate_k8s_deps.sh fails closed on unaligned HELM_VERSION bump (${neg4_err})"
fi

# 3f. Negative case 5: cross-minor K8s jump (k8s.io/api in go.mod) fails closed.
CAND_NEG5="${SCRATCH}/cand-neg5"
cp -R "${CAND_BASE}" "${CAND_NEG5}"
python3 - "${CAND_BASE}/go.mod" "${CAND_NEG5}/go.mod" "${K8S_MOD_CUR}" "${K8S_MOD_CROSS_MINOR}" <<'PY'
import sys
src, dst, k_cur, k_cross = sys.argv[1:5]
with open(src, "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(f"k8s.io/api {k_cur}", f"k8s.io/api {k_cross}", 1)
assert out != txt, f"failed to mutate k8s.io/api {k_cur}"
with open(dst, "w", encoding="utf-8") as f:
    f.write(out)
PY

neg5_err=""
if ! neg5_err="$(run_validator_env_i "${CAND_NEG5}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG5}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api jump"
else
  fail "validate_k8s_deps.sh fails closed on cross-minor k8s.io/api jump (${neg5_err})"
fi

# 3g. Negative case 6: mismatched proposals/k8s-deps.json fails closed (missing/extra/mismatch across all 11 keys).
CAND_NEG6="${SCRATCH}/cand-neg6"
cp -R "${CAND_BASE}" "${CAND_NEG6}"
python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${CAND_NEG6}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
old_v = doc["versions"]["helm-cli"]
new_v = old_v.lstrip("v") if old_v.startswith("v") else f"v{old_v}"
assert new_v != old_v
doc["versions"]["helm-cli"] = new_v
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY

neg6_err=""
if ! neg6_err="$(run_validator_env_i "${CAND_NEG6}" "${STAGE_DISC}" 2>&1)" && \
   [[ ! -e "${CAND_NEG6}/.git" ]]; then
  pass "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json"
else
  fail "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json (${neg6_err})"
fi

# 3h. Negative case 7: mismatched k8s-modules / extra key in proposals/k8s-deps.json fails closed (I4).
CAND_NEG7="${SCRATCH}/cand-neg7"
cp -R "${CAND_BASE}" "${CAND_NEG7}"
python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${CAND_NEG7}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["k8s-modules"] = "v0.0.0"
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
neg7_err=""
if ! neg7_err="$(run_validator_env_i "${CAND_NEG7}" "${STAGE_DISC}" 2>&1)" && \
   grep -q "k8s-modules" <<<"${neg7_err}"; then
  pass "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json k8s-modules"
else
  fail "validate_k8s_deps.sh fails closed on mismatched proposals/k8s-deps.json k8s-modules (${neg7_err})"
fi

CAND_NEG8="${SCRATCH}/cand-neg8"
cp -R "${CAND_BASE}" "${CAND_NEG8}"
python3 - "${CAND_BASE}/.auto-sync/proposals/k8s-deps.json" "${CAND_NEG8}/.auto-sync/proposals/k8s-deps.json" <<'PY'
import json
import sys
src, dst = sys.argv[1:3]
with open(src, "r", encoding="utf-8") as f:
    doc = json.load(f)
doc["versions"]["extra-key"] = "1.0.0"
with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
PY
neg8_err=""
if ! neg8_err="$(run_validator_env_i "${CAND_NEG8}" "${STAGE_DISC}" 2>&1)" && \
   grep -q "extra-key" <<<"${neg8_err}"; then
  pass "validate_k8s_deps.sh fails closed on extra unexpected key in proposals/k8s-deps.json"
else
  fail "validate_k8s_deps.sh fails closed on extra unexpected key in proposals/k8s-deps.json (${neg8_err})"
fi

# 3i. no_change fixture (all upstreams up_to_date without AUTOSYNC_SKIP_SOURCE_REVISION) keeps source_revision == base_sha.
FIXTURE_NOCHANGE="${SCRATCH}/fixture-nochange"
mkdir -p "${FIXTURE_NOCHANGE}"
cat > "${FIXTURE_NOCHANGE}/kubernetes-releases.json" <<JSON
[{"tag_name": "${K8S_REL_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/helm-releases.json" <<JSON
[{"tag_name": "v${HELM_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/kustomize-tags.txt" <<TXT
0000000000000000000000000000000000000003	refs/tags/kustomize/v${KUST_CUR}
TXT
cat > "${FIXTURE_NOCHANGE}/skopeo-releases.json" <<JSON
[{"tag_name": "v${SKOPEO_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/vals-releases.json" <<JSON
[{"tag_name": "v${VALS_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/ytt-releases.json" <<JSON
[{"tag_name": "v${YTT_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/controller-runtime-releases.json" <<JSON
[{"tag_name": "${CR_CUR}", "prerelease": false, "draft": false}]
JSON
cat > "${FIXTURE_NOCHANGE}/kueue-releases.json" <<JSON
[{"tag_name": "${KUEUE_CUR}", "prerelease": false, "draft": false}]
JSON
STAGE_NOCHANGE="${SCRATCH}/stage-nochange/k8s-deps"
if (
  cd "${PREFLIGHT_REPO}" && \
  AUTOSYNC_K8S_DEPS_FIXTURE_DIR="${FIXTURE_NOCHANGE}" \
  AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true \
  GOMODCACHE="${HOST_GOMODCACHE}" \
  GOCACHE="${HOST_GOCACHE}" \
  bash "${PREFLIGHT_REPO}/.auto-sync/scripts/preflight-k8s-deps.sh" "${STAGE_NOCHANGE}" >/dev/null
) && \
python3 - "${STAGE_NOCHANGE}" "${BASE_SHA}" <<'PY'
import json
import os
import sys

stage_dir, base_sha = sys.argv[1:3]
with open(os.path.join(stage_dir, "upstream-records.json"), "r", encoding="utf-8") as f:
    recs = json.load(f)
assert all(r["status"] == "up_to_date" for r in recs), recs
with open(os.path.join(stage_dir, "k8s-deps-preflight-report.json"), "r", encoding="utf-8") as f:
    rep = json.load(f)
assert rep["source_revision"] == base_sha, rep
PY
then
  pass "preflight-k8s-deps.sh keeps source_revision == base_sha when all upstreams are up_to_date"
else
  fail "preflight-k8s-deps.sh keeps source_revision == base_sha when all upstreams are up_to_date"
fi

echo
if (( FAILS > 0 )); then
  printf 'K8S-DEPS SELFTEST FAILED: %d failure(s), %d passed\n' "${FAILS}" "${PASSES}" >&2
  exit 1
fi
printf 'K8S-DEPS SELFTEST PASSED: %d check(s) passed\n' "${PASSES}"
