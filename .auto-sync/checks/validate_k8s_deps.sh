#!/usr/bin/env bash
# .auto-sync/checks/validate_k8s_deps.sh - command.v1 verification gate for k8s-deps-sync.
#
# Enforces shared-file diff guards (pack/gpustack-operator/Dockerfile CLI ARGs,
# hack/deps.sh mod_staging() heredoc, and codegen-only api/ outputs), Helm v3.*
# three-place lockstep, same-minor Kubernetes v0.36.x lockstep across go.mod and
# hack/deps.sh, upgrade-compat.sh modules, toolpins-selftest.sh, double
# make generate + make lint drift check, and RACE=false make test.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env
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

cand_dockerfile="${AUTOSYNC_CANDIDATE_ROOT}/pack/gpustack-operator/Dockerfile"
cand_deps_sh="${AUTOSYNC_CANDIDATE_ROOT}/hack/deps.sh"
cand_helm_sh="${AUTOSYNC_CANDIDATE_ROOT}/hack/lib/helm.sh"
cand_gomod="${AUTOSYNC_CANDIDATE_ROOT}/go.mod"
cand_api="${AUTOSYNC_CANDIDATE_ROOT}/api"
cand_staging="${AUTOSYNC_CANDIDATE_ROOT}/staging"
cand_proposal="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/k8s-deps.json"

# 1. Shared-file and API codegen-only diff guards.
assert_dockerfile_only_args \
  "${BASELINE_DIR}/pack/gpustack-operator/Dockerfile" \
  "${cand_dockerfile}" \
  "(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION"

assert_deps_sh_only_section \
  "${BASELINE_DIR}/hack/deps.sh" \
  "${cand_deps_sh}" \
  "mods"

assert_api_only_generated \
  "${BASELINE_DIR}/api" \
  "${cand_api}"

# 2. Validate Helm v3.* three-place lockstep and same-minor K8s lockstep.
if [[ ! -f "${BASELINE_DIR}/go.mod" ]]; then
  printf 'FAIL: missing baseline go.mod under %s\n' "${BASELINE_DIR}" >&2
  exit 1
fi

python3 - "${BASELINE_DIR}/go.mod" "${cand_dockerfile}" "${cand_helm_sh}" "${cand_deps_sh}" "${cand_gomod}" "${cand_proposal}" <<'PY'
import json
import os
import re
import sys

base_gomod_p, df_p, helm_sh_p, deps_sh_p, gomod_p, prop_p = sys.argv[1:7]


def read(p: str) -> str:
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


base_gomod = read(base_gomod_p)
df_txt = read(df_p)
helm_sh_txt = read(helm_sh_p)
deps_sh_txt = read(deps_sh_p)
gomod_txt = read(gomod_p)

# Determine baseline Kubernetes minor (e.g. 0.36).
m_base_k8s = re.search(r'^\s*k8s\.io/api\s+(v0\.(\d+)\.\d+)', base_gomod, re.MULTILINE)
if not m_base_k8s:
    raise SystemExit("FAIL: missing k8s.io/api in baseline go.mod")
base_k8s_minor_num = m_base_k8s.group(2)
expected_k8s_mod_prefix = f"v0.{base_k8s_minor_num}."
expected_kubectl_prefix = f"1.{base_k8s_minor_num}."

# Check HELM_VERSION three-place lockstep within v3.*.
m_df_helm = re.search(r'^ARG HELM_VERSION="([^"]+)"$', df_txt, re.MULTILINE)
if not m_df_helm:
    raise SystemExit("FAIL: missing ARG HELM_VERSION in pack/gpustack-operator/Dockerfile")
df_helm = m_df_helm.group(1).strip()
if not re.fullmatch(r"3\.\d+\.\d+", df_helm):
    raise SystemExit(f"FAIL: HELM_VERSION must stay within v3.* (got {df_helm!r})")

m_helm_comment = re.search(r'^#.*HELM_VERSION.*default is (v\d+\.\d+\.\d+)\.', helm_sh_txt, re.MULTILINE)
m_helm_var = re.search(r'^helm_version=\$\{HELM_VERSION:-"(v\d+\.\d+\.\d+)"\}$', helm_sh_txt, re.MULTILINE)
if not m_helm_comment or not m_helm_var:
    raise SystemExit("FAIL: hack/lib/helm.sh missing HELM_VERSION comment or helm_version variable")
if m_helm_comment.group(1) != f"v{df_helm}" or m_helm_var.group(1) != f"v{df_helm}":
    raise SystemExit(
        f"FAIL: Helm three-place lockstep mismatch: Dockerfile={df_helm}, "
        f"helm.sh comment={m_helm_comment.group(1)}, helm.sh var={m_helm_var.group(1)}"
    )

if re.search(r'helm\.sh/helm/v4\b', gomod_txt):
    raise SystemExit("FAIL: helm.sh/helm/v4 is forbidden in go.mod; must remain v3.*")
m_gomod_helm = re.search(r'^\s*helm\.sh/helm/v3\s+(v\d+\.\d+\.\d+)\b', gomod_txt, re.MULTILINE)
if not m_gomod_helm:
    raise SystemExit("FAIL: missing helm.sh/helm/v3 in go.mod")
if m_gomod_helm.group(1) != f"v{df_helm}":
    raise SystemExit(
        f"FAIL: Helm three-place lockstep mismatch: Dockerfile={df_helm}, go.mod helm.sh/helm/v3={m_gomod_helm.group(1)}"
    )

# Check KUBECTL_VERSION stays on same Kubernetes minor (1.<minor>.*).
m_df_kubectl = re.search(r'^ARG KUBECTL_VERSION="([^"]+)"$', df_txt, re.MULTILINE)
if not m_df_kubectl:
    raise SystemExit("FAIL: missing ARG KUBECTL_VERSION in pack/gpustack-operator/Dockerfile")
df_kubectl = m_df_kubectl.group(1).strip()
if not df_kubectl.startswith(expected_kubectl_prefix) or not re.fullmatch(r"\d+\.\d+\.\d+", df_kubectl):
    raise SystemExit(
        f"FAIL: KUBECTL_VERSION={df_kubectl!r} violates same-minor {expected_kubectl_prefix}* lockstep"
    )

# Check lockstep k8s.io/* modules in go.mod match baseline minor.
lockstep_mods = (
    "k8s.io/api",
    "k8s.io/apimachinery",
    "k8s.io/client-go",
    "k8s.io/code-generator",
    "k8s.io/kube-aggregator",
    "k8s.io/apiextensions-apiserver",
)
gomod_k8s_vers = {}
for mod in lockstep_mods:
    m_mod = re.search(rf'^\s*{re.escape(mod)}\s+(v\d+\.\d+\.\d+)\b', gomod_txt, re.MULTILINE)
    if not m_mod:
        raise SystemExit(f"FAIL: missing {mod} in go.mod")
    ver = m_mod.group(1)
    if not ver.startswith(expected_k8s_mod_prefix):
        raise SystemExit(
            f"FAIL: {mod} {ver} in go.mod violates same-minor {expected_k8s_mod_prefix}* lockstep"
        )
    gomod_k8s_vers[mod] = ver

# Check mod_staging() in hack/deps.sh stays on same Kubernetes minor and matches go.mod.
m_mod_fn = re.search(r'function mod_staging\(\)\s*\{.*?cat <<EOF\n(.*?)\nEOF', deps_sh_txt, re.DOTALL)
if not m_mod_fn:
    raise SystemExit("FAIL: missing mod_staging() heredoc in hack/deps.sh")

staged_k8s_vers = {}
for line in m_mod_fn.group(1).splitlines():
    parts = line.split()
    if len(parts) != 3:
        continue
    url, ver, _dest = parts
    m_repo = re.fullmatch(r'https://github\.com/kubernetes/(code-generator|api|apimachinery|kube-aggregator|apiextensions-apiserver)', url)
    if m_repo:
        mod_name = f"k8s.io/{m_repo.group(1)}"
        if not ver.startswith(expected_k8s_mod_prefix):
            raise SystemExit(
                f"FAIL: {mod_name} {ver} in hack/deps.sh violates same-minor {expected_k8s_mod_prefix}* lockstep"
            )
        if ver != gomod_k8s_vers[mod_name]:
            raise SystemExit(
                f"FAIL: {mod_name} version mismatch between hack/deps.sh ({ver}) and go.mod ({gomod_k8s_vers[mod_name]})"
            )
        staged_k8s_vers[mod_name] = ver

if len(staged_k8s_vers) != 5:
    raise SystemExit(f"FAIL: expected 5 k8s.io/* entries in mod_staging(), found {len(staged_k8s_vers)}")

# Validate proposal JSON structure.
if not os.path.isfile(prop_p):
    raise SystemExit(f"FAIL: missing proposal artifact {prop_p}")
with open(prop_p, "r", encoding="utf-8") as f:
    prop = json.load(f)
if not isinstance(prop, dict) or prop.get("schema_version") != 1:
    raise SystemExit("FAIL: k8s-deps.json must be a JSON object with schema_version=1")
if not isinstance(prop.get("summary"), str) or not prop["summary"].strip():
    raise SystemExit("FAIL: k8s-deps.json summary must be a non-empty string")
if not isinstance(prop.get("versions"), dict) or not isinstance(prop.get("subscriptions"), list):
    raise SystemExit("FAIL: k8s-deps.json must contain versions object and subscriptions list")


def extract_df_arg(name: str) -> str:
    m_arg = re.search(rf'^ARG {re.escape(name)}="([^"]+)"$', df_txt, re.MULTILINE)
    if not m_arg:
        raise SystemExit(f"FAIL: missing ARG {name} in pack/gpustack-operator/Dockerfile")
    return m_arg.group(1).strip()


def extract_gomod_ver(mod_name: str) -> str:
    m_gm = re.search(rf'^\s*{re.escape(mod_name)}\s+(v\d+\.\d+\.\d+)\b', gomod_txt, re.MULTILINE)
    if not m_gm:
        raise SystemExit(f"FAIL: missing {mod_name} in go.mod")
    return m_gm.group(1).strip()


expected_versions = {
    "kubernetes": f"v{df_kubectl}",
    "k8s-modules": extract_gomod_ver("k8s.io/api"),
    "controller-runtime": extract_gomod_ver("sigs.k8s.io/controller-runtime"),
    "kueue-sdk": extract_gomod_ver("sigs.k8s.io/kueue"),
    "helm-cli": f"v{df_helm}",
    "skopeo-cli": extract_df_arg("SKOPEO_VERSION"),
    "vals-cli": extract_df_arg("VALS_VERSION"),
    "kustomize-cli": extract_df_arg("KUSTOMIZE_VERSION"),
    "ytt-cli": extract_df_arg("YTT_VERSION"),
    "kubectl-cli": df_kubectl,
    "helm-module": extract_gomod_ver("helm.sh/helm/v3"),
}
p_vers = prop["versions"]
if set(p_vers.keys()) != set(expected_versions.keys()):
    missing_k = sorted(set(expected_versions.keys()) - set(p_vers.keys()))
    extra_k = sorted(set(p_vers.keys()) - set(expected_versions.keys()))
    raise SystemExit(
        f"FAIL: proposals/k8s-deps.json versions keys mismatch (missing={missing_k}, extra={extra_k})"
    )
for uid, exp_v in expected_versions.items():
    if p_vers.get(uid) != exp_v:
        raise SystemExit(
            f"FAIL: proposals/k8s-deps.json versions.{uid} ({p_vers.get(uid)!r}) != pinned ({exp_v!r})"
        )
PY

# 3. Verify zero .rej/.orig artifacts under staging/.
if [[ -d "${cand_staging}" ]]; then
  bad_staging="$(find "${cand_staging}" -type f \( -name '*.rej' -o -name '*.orig' \) | sort)"
  if [[ -n "${bad_staging}" ]]; then
    printf 'FAIL: reject/orig files found under staging/: %s\n' "${bad_staging}" >&2
    exit 1
  fi
fi

# 4. Run upgrade-compat.sh modules and toolpins-selftest.sh.
bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/upgrade-compat.sh" modules
bash "${AUTOSYNC_CANDIDATE_ROOT}/hack/check/toolpins-selftest.sh"

if [[ "${AUTOSYNC_FAST_SELFTEST:-false}" == "true" ]]; then
  printf 'OK: validate_k8s_deps passed (fast selftest mode)\n'
  exit 0
fi

# 5. Double make generate + make lint drift check and RACE=false make test.
autosync::run_in_module_path_copy "${AUTOSYNC_CANDIDATE_ROOT}" make generate
lint_env=("AUTOSYNC_IN_VALIDATOR=true")
if [[ -z "${UV_CACHE_DIR:-}" || ! -d "${UV_CACHE_DIR}" ]]; then
  lint_env+=("GPUSTACK_SKIP_PYTHON_LINT=true")
fi
env "${lint_env[@]}" make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint
autosync::run_in_module_path_copy "${AUTOSYNC_CANDIDATE_ROOT}" make generate

if [[ -n "$(git -C "${AUTOSYNC_CANDIDATE_ROOT}" status --porcelain)" ]]; then
  printf 'FAIL: candidate tree has uncommitted drift after make generate -> make lint -> make generate\n' >&2
  git -C "${AUTOSYNC_CANDIDATE_ROOT}" status --porcelain >&2
  exit 1
fi

RACE=false make -C "${AUTOSYNC_CANDIDATE_ROOT}" test
printf 'OK: validate_k8s_deps passed\n'
