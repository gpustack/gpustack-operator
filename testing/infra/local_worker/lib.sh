#!/usr/bin/env bash
#
# Shared helpers for the disposable local cluster recipe.
#
# The functions split into two groups. The pure ones take their arguments and
# return a verdict, so the safety cases in test_recipe.sh can exercise them
# without a cluster. The rest reach Docker, kind and kubectl, and every one of
# them targets the task-owned resources named in the ownership record.

set -o errexit
set -o nounset
set -o pipefail

LW_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"

LW_KUBERNETES_VERSION="${LOCAL_WORKER_KUBERNETES_VERSION:-v1.36.5}"
LW_KUBECTL_VERSION="${LOCAL_WORKER_KUBECTL_VERSION:-v1.36.5}"
LW_NODE_IMAGE="${LOCAL_WORKER_NODE_IMAGE:-gpustack-upgrade-node:v1.36.5-arm64}"
LW_BASE_IMAGE="${LOCAL_WORKER_BASE_IMAGE:-kindest/base:v20260820-69b56db7}"
LW_CLUSTER_NAME="${LOCAL_WORKER_CLUSTER_NAME:-gpustack-local-worker}"
LW_NAMESPACE="${LOCAL_WORKER_NAMESPACE:-gpustack-local-worker-check}"

LW_OWNERSHIP_KEYS="CLUSTER_NAME CONTEXT NODE_CONTAINER_NAME NODE_CONTAINER_ID KUBECONFIG KUBECTL NODE_IMAGE NODE_IMAGE_ID KUBERNETES_VERSION DEFAULT_KUBECONFIG_SHA256 CREATED_AT"

# The outcomes of an EndpointSlice observation. A read that failed is an
# outcome of its own, so it is never read as an absence.
LW_SLICE_ABSENT=0
LW_SLICE_PRESENT=1
LW_SLICE_READ_FAILED=2

lw_log() {
  printf '[local-worker] %s\n' "$*"
}

lw_die() {
  printf '[local-worker] error: %s\n' "$*" >&2
  exit 1
}

lw_require_cmd() {
  local cmd
  for cmd in "$@"; do
    command -v "${cmd}" >/dev/null 2>&1 || lw_die "${cmd} is not on PATH"
  done
}

# The wait loops sleep through this helper, so a test can run them without
# waiting for the clock.
lw_sleep() {
  sleep "$@"
}

# Paths

lw_state_dir() {
  printf '%s\n' "${LOCAL_WORKER_STATE_DIR:-${LW_REPO_ROOT}/.dist/local_worker}"
}

lw_state_file() {
  printf '%s/ownership.env\n' "$(lw_state_dir)"
}

lw_kubeconfig_path() {
  printf '%s/kubeconfig\n' "$(lw_state_dir)"
}

lw_kubectl_path() {
  printf '%s/bin/kubectl\n' "$(lw_state_dir)"
}

lw_context() {
  printf 'kind-%s\n' "$LW_CLUSTER_NAME"
}

lw_node_container_name() {
  printf '%s-control-plane\n' "$LW_CLUSTER_NAME"
}

lw_default_kubeconfig_path() {
  printf '%s/.kube/config\n' "${HOME}"
}

# Digests and fingerprints

lw_sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    lw_die "neither shasum nor sha256sum is available on this host"
  fi
}

# The fingerprint of the kubeconfig the user already had. The recipe never
# writes to it, and the recorded value is compared again before cleanup so a
# change made elsewhere is reported rather than silently accepted.
lw_default_kubeconfig_fingerprint() {
  local path
  path="$(lw_default_kubeconfig_path)"
  if [[ -f "${path}" ]]; then
    lw_sha256 "${path}"
  else
    printf 'absent\n'
  fi
}

lw_default_kubeconfig_unchanged() {
  local recorded current
  recorded="$(lw_ownership_read DEFAULT_KUBECONFIG_SHA256)" || return 1
  current="$(lw_default_kubeconfig_fingerprint)"
  [[ "${recorded}" == "${current}" ]]
}

# Ownership record

lw_ownership_write() {
  local node_container_id="$1" node_image_id="$2" file tmp
  # A record with an empty identifier refuses every later cleanup, because no
  # live container can match it, so an unreadable identifier stops the write.
  [[ -n "${node_container_id}" ]] \
    || lw_die "the node container identifier could not be read, the ownership record is not written"
  [[ -n "${node_image_id}" ]] \
    || lw_die "the node image identifier could not be read, the ownership record is not written"
  file="$(lw_state_file)"
  mkdir -p "$(lw_state_dir)"
  tmp="${file}.tmp.$$"
  {
    printf 'CLUSTER_NAME=%s\n' "${LW_CLUSTER_NAME}"
    printf 'CONTEXT=%s\n' "$(lw_context)"
    printf 'NODE_CONTAINER_NAME=%s\n' "$(lw_node_container_name)"
    printf 'NODE_CONTAINER_ID=%s\n' "${node_container_id}"
    printf 'KUBECONFIG=%s\n' "$(lw_kubeconfig_path)"
    printf 'KUBECTL=%s\n' "$(lw_kubectl_path)"
    printf 'NODE_IMAGE=%s\n' "${LW_NODE_IMAGE}"
    printf 'NODE_IMAGE_ID=%s\n' "${node_image_id}"
    printf 'KUBERNETES_VERSION=%s\n' "${LW_KUBERNETES_VERSION}"
    printf 'DEFAULT_KUBECONFIG_SHA256=%s\n' "$(lw_default_kubeconfig_fingerprint)"
    printf 'CREATED_AT=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"${tmp}"
  mv "${tmp}" "${file}"
  lw_log "ownership record written to ${file}"
}

lw_ownership_read() {
  local key="$1" file line k v
  file="$(lw_state_file)"
  [[ -f "${file}" ]] || return 1
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ -z "${line}" || "${line}" == \#* ]] && continue
    [[ "${line}" == *=* ]] || lw_die "ownership record ${file} is corrupt: a line carries no key"
    k="${line%%=*}"
    v="${line#*=}"
    case " ${LW_OWNERSHIP_KEYS} " in
      *" ${k} "*) ;;
      *) lw_die "ownership record ${file} is corrupt: unknown key ${k}" ;;
    esac
    if [[ "${k}" == "${key}" ]]; then
      printf '%s\n' "${v}"
      return 0
    fi
  done <"${file}"
  return 1
}

# Report whether the record can be read. A record that is present but damaged
# is a hard failure for a caller that acts on it, and a plain refusal for a
# caller that only reports.
lw_ownership_is_readable() {
  local file line k
  file="$(lw_state_file)"
  [[ -f "${file}" ]] || return 1
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ -z "${line}" || "${line}" == \#* ]] && continue
    [[ "${line}" == *=* ]] || return 1
    k="${line%%=*}"
    case " ${LW_OWNERSHIP_KEYS} " in
      *" ${k} "*) ;;
      *) return 1 ;;
    esac
  done <"${file}"
  return 0
}

# Returns zero when the record is present and complete. A record that is
# present but damaged is a hard failure: a caller must not treat it as an
# absent record and delete a cluster it cannot prove it owns.
lw_ownership_validate() {
  local file key
  file="$(lw_state_file)"
  [[ -f "${file}" ]] || return 1
  lw_ownership_is_readable || lw_die "the ownership record ${file} is damaged"
  for key in ${LW_OWNERSHIP_KEYS}; do
    lw_ownership_read "${key}" >/dev/null || lw_die "the ownership record ${file} is missing ${key}"
  done
  return 0
}

lw_ownership_clear() {
  rm -f "$(lw_state_file)"
}

# Pure predicates

# A cluster name is task-owned when it is well formed and matches the name
# this run recorded. A name that another run created is refused.
lw_cluster_name_valid() {
  local name="$1"
  [[ "${name}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]
}

# A cluster name is free when no existing cluster carries it.
lw_cluster_name_is_free() {
  local existing="$1" name="$2"
  ! grep -Fx -- "${name}" <<<"${existing}" >/dev/null
}

# The recorded node container and the live one are the same cluster only when
# the immutable container identifier matches.
lw_ownership_matches() {
  local recorded="$1" live="$2"
  [[ -n "${recorded}" && -n "${live}" && "${recorded}" == "${live}" ]]
}

# The recipe runs against its own kubeconfig. A kubeconfig value that points
# anywhere else would send a command to a cluster this run does not own.
lw_kubeconfig_isolated() {
  local kubeconfig="$1" state_dir entry
  state_dir="$(lw_state_dir)"
  if [[ -z "${kubeconfig}" ]]; then
    return 0
  fi
  local old_ifs="${IFS}"
  IFS=':'
  for entry in ${kubeconfig}; do
    IFS="${old_ifs}"
    [[ "${entry}" == "${state_dir}"/* ]] || return 1
    IFS=':'
  done
  IFS="${old_ifs}"
  return 0
}

# The recipe exports its own kubeconfig, so every child process stays away
# from the default one.
lw_exported_kubeconfig_is_isolated() {
  lw_kubeconfig_isolated "${KUBECONFIG:-}"
}

# Normalize a rendered container command and args into name=value pairs.
# The rendering is a bracketed list of quoted strings, and a value may itself
# contain a comma, so the items are separated on the quote-comma-quote
# sequence that joins them rather than on the comma alone.
lw_arg_tokens() {
  printf '%s\n' "$1" |
    sed 's/","/\n/g' |
    tr -d '[]"' |
    awk '{
      for (i = 1; i <= NF; i++) {
        token = $i
        gsub(/^--+/, "", token)
        if (index(token, "=") > 0) print token
      }
    }'
}

lw_has_arg() {
  local text="$1" name="$2" value="$3"
  lw_arg_tokens "${text}" | grep -Fx -- "${name}=${value}" >/dev/null
}

# Every extraArgs entry of a kubeadm configuration must be a name and a value.
# The older map shape is a mapping of flag names to values, and a kubeadm
# version that expects the list shape rejects it.
lw_extra_args_use_name_value_lists() {
  local file="$1"
  awk '
    function indent_of(text,   matched) {
      match(text, /^[ \t]*/)
      return RLENGTH
    }
    {
      line = $0
      if (line ~ /^[ \t]*$/ || line ~ /^[ \t]*#/) next
      current_indent = indent_of(line)
      if (in_block) {
        if (current_indent <= block_indent) {
          if (has_name && !has_value) { malformed = 1 }
          in_block = 0
        } else {
          if (line ~ /^[ \t]*-[[:space:]]*name:/) {
            if (has_name && !has_value) { malformed = 1 }
            has_name = 1
            has_value = 0
            next
          }
          if (line ~ /^[ \t]*value:/) {
            if (!has_name) { malformed = 1 }
            has_value = 1
            next
          }
          malformed = 1
          next
        }
      }
      if (line ~ /^[ \t]*extraArgs:/) {
        in_block = 1
        block_indent = current_indent
        blocks++
        has_name = 0
        has_value = 0
        next
      }
    }
    END {
      if (in_block && has_name && !has_value) { malformed = 1 }
      exit (malformed || blocks == 0) ? 1 : 0
    }
  ' "${file}"
}

# The mirroring controller only copies Endpoints whose Service has no
# selector, and it skips Endpoints that carry the skip-mirror label or the
# leader-election annotation. A fixture that misses any of these conditions
# would produce no slice even with the controller running.
lw_mirror_fixture_is_valid() {
  local selector="$1" skip_mirror="$2" leader_annotation="$3" addresses="$4" port_name="$5"
  [[ -z "${selector}" ]] || return 1
  [[ -z "${skip_mirror}" ]] || return 1
  [[ -z "${leader_annotation}" ]] || return 1
  [[ -n "${addresses}" ]] || return 1
  [[ -n "${port_name}" ]] || return 1
  return 0
}

# Cluster and tool access

lw_kind() {
  kind "$@"
}

lw_kubectl() {
  "$(lw_kubectl_path)" --kubeconfig "$(lw_kubeconfig_path)" --context "$(lw_context)" "$@"
}

lw_node_container_id() {
  docker inspect --format '{{.Id}}' "$1" 2>/dev/null
}

lw_live_node_container_id() {
  lw_node_container_id "$(lw_node_container_name)"
}

lw_cluster_exists() {
  kind get clusters 2>/dev/null | grep -Fx -- "${LW_CLUSTER_NAME}" >/dev/null
}

# The rendered command and args of a static Pod, which is where the kubeadm
# arguments that were accepted at creation time can be read back.
lw_static_pod_command() {
  local pod="$1"
  lw_kubectl -n kube-system get pod "${pod}" \
    -o jsonpath='{range .spec.containers[*]}{.command}{" "}{.args}{"\n"}{end}'
}

lw_static_pod_phase() {
  local pod="$1"
  lw_kubectl -n kube-system get pod "${pod}" -o jsonpath='{.status.phase}'
}

# The version the cluster itself reports, read from the server rather than
# assumed from the image that was built. The server version endpoint is used
# because the client and the server versions are reported side by side.
lw_server_version() {
  lw_kubectl get --raw /version | awk -F'"' '/"gitVersion"/ {print $4; exit}'
}

# EndpointSlice observation

# The names of the EndpointSlices of a service. The status of the API read is
# kept, so a failed read is never read as an empty list, and the read carries
# its own request timeout, so an API that hangs ends as a failed read instead
# of stretching the observation window without bound. The error of the read
# reaches the log, because it names the reason the observation cannot decide.
lw_read_slice_names() {
  local service="$1" names status=0
  names="$(lw_kubectl --request-timeout=10s -n "${LW_NAMESPACE}" get endpointslices \
    -l "kubernetes.io/service-name=${service}" -o name)" || status=$?
  printf '%s\n' "${names}"
  return "${status}"
}

# Wait for the absence of an EndpointSlice of a service.
#
# The outcome is one of:
#   LW_SLICE_ABSENT      the window passed and every read succeeded
#   LW_SLICE_PRESENT     a read succeeded and the slice was there
#   LW_SLICE_READ_FAILED a read failed, so the absence is not proven
#
# A read that failed ends the window at once. It is never counted as an
# absence, and the window never reports an absence it did not observe, so a
# window without room for the interval still performs its first read.
lw_wait_for_no_slice() {
  local service="$1" timeout="$2" waited=0 names status interval=2
  while (( waited == 0 )) || (( waited < timeout )); do
    names=""
    status=0
    names="$(lw_read_slice_names "${service}")" || status=$?
    if (( status != 0 )); then
      lw_log "the EndpointSlice read for ${service} failed with status ${status}, the absence of a mirrored slice is not proven"
      return "${LW_SLICE_READ_FAILED}"
    fi
    if [[ -n "${names}" ]]; then
      return "${LW_SLICE_PRESENT}"
    fi
    lw_sleep "${interval}"
    waited=$((waited + interval))
  done
  return "${LW_SLICE_ABSENT}"
}

# Fixture cleanup

# The fixture namespace belongs to this run. The creation is the proof of that
# ownership: a namespace that already existed was not created here, and the
# recipe never deletes a namespace it did not create.
lw_create_fixture_namespace() {
  lw_kubectl create namespace "${LW_NAMESPACE}"
}

# The fixture namespace belongs to this run. The delete names the task
# kubeconfig and the task context, so it can only reach the cluster this
# recipe owns, and it never fails the caller.
lw_delete_fixture_namespace() {
  lw_kubectl delete namespace "${LW_NAMESPACE}" --wait=false >/dev/null 2>&1 || true
}

# Run a command with the fixture namespace created and its cleanup armed.
#
# The namespace is created first, and the cleanup is armed only after that
# creation succeeded, so a namespace that already existed is never deleted. The
# command runs in a subshell that carries its own exit trap, so the namespace is
# removed when the command fails, the exit status of the failure is the
# command's own, and the traps of the calling shell are left alone.
#
# The trap body keeps the status of the failure: a trap that fails under
# errexit would replace it with the status of the failure of the trap.
lw_with_fixture_cleanup() {
  (
    if ! lw_create_fixture_namespace; then
      lw_die "the fixture namespace ${LW_NAMESPACE} was not created, the recipe does not adopt a namespace it did not create"
    fi
    trap 'lw_delete_fixture_namespace || true' EXIT
    "$@"
  )
}

# Provisioning

lw_download_checksum() {
  curl -fsSL "$1" | awk '{print $1}'
}

# The task-local kubectl is pinned to the same version as the cluster and
# verified against the published checksum. The kubectl the user already has is
# never replaced.
lw_provision_kubectl() {
  local os arch base expected target tmp actual
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${arch}" in
    arm64 | aarch64) arch="arm64" ;;
    x86_64 | amd64) arch="amd64" ;;
    *) lw_die "unsupported host architecture ${arch}" ;;
  esac
  base="https://dl.k8s.io/release/${LW_KUBECTL_VERSION}/bin/${os}/${arch}"
  expected="$(lw_download_checksum "${base}/kubectl.sha256")" \
    || lw_die "cannot read the published checksum for kubectl ${LW_KUBECTL_VERSION}"
  target="$(lw_kubectl_path)"
  mkdir -p "$(dirname "${target}")"
  if [[ -x "${target}" ]]; then
    actual="$(lw_sha256 "${target}")"
    if [[ "${actual}" == "${expected}" ]]; then
      lw_log "task-local kubectl ${LW_KUBECTL_VERSION} is present and verified"
      return 0
    fi
    lw_log "the task-local kubectl does not match the published checksum; downloading it again"
  fi
  tmp="${target}.download.$$"
  curl -fsSL "${base}/kubectl" -o "${tmp}" || lw_die "cannot download kubectl ${LW_KUBECTL_VERSION}"
  actual="$(lw_sha256 "${tmp}")"
  [[ "${actual}" == "${expected}" ]] \
    || lw_die "the downloaded kubectl has checksum ${actual}, the published checksum is ${expected}"
  mv "${tmp}" "${target}"
  chmod +x "${target}"
  lw_log "task-local kubectl ${LW_KUBECTL_VERSION} installed and verified"
}

lw_release_tarball_checksum() {
  lw_download_checksum \
    "https://dl.k8s.io/release/${LW_KUBERNETES_VERSION}/kubernetes-server-linux-arm64.tar.gz.sha256"
}

lw_release_tarball_name() {
  printf 'kubernetes-server-linux-arm64.tar.gz\n'
}

# A release tarball that was already downloaded can stand in for the download
# kind performs, which carries a deadline a slow link can exceed. The copy is
# only used when its checksum matches the published one. It lives in a cache
# directory of its own so that removing the state directory keeps it.
lw_release_tarball_path() {
  printf '%s\n' "${LOCAL_WORKER_RELEASE_TARBALL:-${LW_REPO_ROOT}/.dist/local-worker-cache/$(lw_release_tarball_name)}"
}

lw_base_image_index_digest() {
  docker buildx imagetools inspect "${LW_BASE_IMAGE}" |
    awk '/^Digest:/ {print $2; exit}'
}

lw_base_image_arm64_digest() {
  docker buildx imagetools inspect "${LW_BASE_IMAGE}" |
    awk '/^ *Name: *.*@sha256:/ {digest = $NF} /^ *Platform: *linux\/arm64/ {print digest; exit}'
}

# The node image is a build cache: it is built once and kept, because a
# rebuild costs a full release download. The release build downloads the
# tarball itself; a verified local copy of the same tarball is used instead
# when one is present.
# Whether the cached release tarball still matches the published checksum.
# A cache that no longer matches is reported and ignored: the release build
# below downloads a tarball and verifies it itself, so a stale or partial
# cache never blocks a build.
lw_release_tarball_matches() {
  local path="$1" expected actual
  expected="$(lw_release_tarball_checksum)" || return 1
  actual="$(lw_sha256 "${path}")"
  if [[ "${actual}" != "${expected}" ]]; then
    lw_log "the cached release tarball ${path} has checksum ${actual}, the published checksum is ${expected}, the cache is ignored"
    return 1
  fi
  lw_log "the release tarball ${path} matches the published checksum ${expected}"
  return 0
}

lw_ensure_node_image() {
  local arch tarball
  if docker image inspect "${LW_NODE_IMAGE}" >/dev/null 2>&1; then
    arch="$(docker image inspect --format '{{.Architecture}}' "${LW_NODE_IMAGE}")"
    [[ "${arch}" == "arm64" ]] \
      || lw_die "the node image ${LW_NODE_IMAGE} has architecture ${arch}, not arm64"
    lw_log "node image ${LW_NODE_IMAGE} is present"
    return 0
  fi
  tarball="$(lw_release_tarball_path)"
  if [[ -f "${tarball}" ]] && lw_release_tarball_matches "${tarball}"; then
    lw_log "building the node image from the verified release tarball"
    kind build node-image --type file --arch arm64 \
      --image "${LW_NODE_IMAGE}" \
      --base-image "${LW_BASE_IMAGE}" \
      "${tarball}"
    return 0
  fi
  lw_log "building the node image for Kubernetes ${LW_KUBERNETES_VERSION} on arm64"
  kind build node-image --type release --arch arm64 \
    --image "${LW_NODE_IMAGE}" \
    --base-image "${LW_BASE_IMAGE}" \
    "${LW_KUBERNETES_VERSION}"
}

lw_node_image_id() {
  docker image inspect --format '{{.Id}}' "${LW_NODE_IMAGE}" 2>/dev/null
}
