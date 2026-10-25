#!/usr/bin/env bash
#
# Disposable local cluster recipe for the aggregated API path.
#
# The commands create and remove only the resources this recipe owns: a kind
# cluster with a task-local name, a kubeconfig outside the one the user
# already had, a task-local kubectl, and a node image built locally and kept
# as a build cache.
#
# Every kubectl and kind invocation names the task-local kubeconfig and the
# task-local context, so the default kubeconfig and its current context are
# never read or written.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

# The exported value keeps every child process away from the default
# kubeconfig, and the explicit flag on each call keeps that true even when a
# command ignores the environment.
KUBECONFIG="$(lw_kubeconfig_path)"
export KUBECONFIG

# The controller that copies core Endpoints into EndpointSlices. The exclusion
# token is the alias the controller manager registers for it.
LW_MIRROR_CONTROLLER="endpointslicemirroring"
LW_CONTROLLER_ARG="*,-${LW_MIRROR_CONTROLLER}"

LW_LISTENER_PID=""
LW_LISTENER_FILES=()

usage() {
  cat <<'USAGE'
Usage: run.sh <command>

Commands:
  preflight   Check the host, the tools, the node image and the cluster name.
  prepare     Build or reuse the node image, create the cluster, prove its shape.
  network     Prove that a Mac address is reachable from the control plane.
  cleanup     Remove the cluster and the files this recipe created.
USAGE
}

lw_stop_listener() {
  if [[ -n "${LW_LISTENER_PID}" ]] && kill -0 "${LW_LISTENER_PID}" 2>/dev/null; then
    kill "${LW_LISTENER_PID}" 2>/dev/null || true
    wait "${LW_LISTENER_PID}" 2>/dev/null || true
  fi
  LW_LISTENER_PID=""
  local file
  for file in "${LW_LISTENER_FILES[@]+"${LW_LISTENER_FILES[@]}"}"; do
    rm -f "${file}"
  done
  LW_LISTENER_FILES=()
}

# Preflight

lw_check_host() {
  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"
  [[ "${os}" == "Darwin" ]] || lw_die "this recipe runs on macOS, this host reports ${os}"
  [[ "${arch}" == "arm64" ]] || lw_die "this recipe runs on native arm64, this host reports ${arch}"
  lw_log "host is ${os}/${arch}"
}

lw_check_docker() {
  local server_arch
  server_arch="$(docker version --format '{{.Server.Arch}}' 2>/dev/null)" \
    || lw_die "the Docker daemon is not reachable"
  [[ "${server_arch}" == "arm64" ]] \
    || lw_die "the Docker daemon serves ${server_arch}, the node image is built for arm64"
  lw_log "the Docker daemon serves linux/${server_arch}"
}

lw_check_kind() {
  local help
  help="$(kind build node-image --help 2>&1)"
  grep -q -- '--base-image' <<<"${help}" \
    || lw_die "this kind build has no --base-image flag, so the pinned base image cannot be named"
  grep -q -- '--type' <<<"${help}" \
    || lw_die "this kind build has no --type flag, so a release build cannot be requested"
  lw_log "kind $(kind version | awk '{print $2}') supports the release build form"
}

# The cluster name must be free. A name another run already used is refused
# rather than adopted.
lw_check_cluster_name() {
  lw_cluster_name_valid "${LW_CLUSTER_NAME}" \
    || lw_die "the cluster name ${LW_CLUSTER_NAME} is not a valid kind cluster name"
  local existing
  existing="$(kind get clusters 2>/dev/null || true)"
  if lw_cluster_name_is_free "${existing}" "${LW_CLUSTER_NAME}"; then
    lw_log "the cluster name ${LW_CLUSTER_NAME} is free"
    return 0
  fi
  if lw_ownership_is_readable \
    && [[ "$(lw_ownership_read CLUSTER_NAME || true)" == "${LW_CLUSTER_NAME}" ]]; then
    lw_die "the cluster ${LW_CLUSTER_NAME} already exists and belongs to this recipe; run cleanup first"
  fi
  lw_die "a cluster named ${LW_CLUSTER_NAME} already exists and this recipe does not own it"
}

lw_check_node_image() {
  if docker image inspect "${LW_NODE_IMAGE}" >/dev/null 2>&1; then
    lw_log "the node image ${LW_NODE_IMAGE} is present and is kept as a build cache"
  else
    lw_log "the node image ${LW_NODE_IMAGE} will be built from the ${LW_BASE_IMAGE} base image"
  fi
}

lw_check_kubeconfig_isolation() {
  lw_exported_kubeconfig_is_isolated \
    || lw_die "the exported KUBECONFIG points outside the recipe state directory"
}

cmd_preflight() {
  lw_require_cmd kind docker curl python3
  lw_check_host
  lw_check_docker
  lw_check_kind
  lw_check_cluster_name
  lw_check_kubeconfig_isolation
  lw_provision_kubectl
  lw_check_node_image
  lw_log "the release tarball for ${LW_KUBERNETES_VERSION} has checksum $(lw_release_tarball_checksum)"
  lw_log "the base image ${LW_BASE_IMAGE} resolves to $(lw_base_image_index_digest)"
  lw_log "the base image arm64 manifest is $(lw_base_image_arm64_digest)"
  lw_log "the recipe uses the kubeconfig $(lw_kubeconfig_path) and the context $(lw_context)"
  lw_log "the default kubeconfig $(lw_default_kubeconfig_path) has the fingerprint $(lw_default_kubeconfig_fingerprint)"
  lw_log "preflight finished"
}

# Prepare

lw_create_cluster() {
  lw_log "creating the cluster ${LW_CLUSTER_NAME}"
  if ! kind create cluster \
    --name "${LW_CLUSTER_NAME}" \
    --image "${LW_NODE_IMAGE}" \
    --kubeconfig "$(lw_kubeconfig_path)" \
    --config "${SCRIPT_DIR}/kind-config.yaml" \
    --wait 300s; then
    # kind handles its own default failure cleanup unless it is asked to
    # retain, and this wrapper issues no deletion of its own: a cluster that
    # still carries this name has unproven ownership, whether kind's cleanup
    # failed or another run took the name during the window, and a delete by
    # name cannot tell those apart.
    lw_log "the cluster creation failed; kind handles its own default failure cleanup and this run deletes nothing"
    lw_log "a cluster named ${LW_CLUSTER_NAME} that still exists has unproven ownership; inspect it first, and run kind delete cluster --name ${LW_CLUSTER_NAME} only once you have verified it is yours to remove"
    lw_die "the cluster ${LW_CLUSTER_NAME} could not be created"
  fi
}

lw_assert_server_version() {
  local version
  version="$(lw_server_version)"
  [[ "${version}" == "${LW_KUBERNETES_VERSION}" ]] \
    || lw_die "the cluster reports ${version}, the recipe builds ${LW_KUBERNETES_VERSION}"
  lw_log "the cluster reports ${version}"
}

lw_assert_node_architecture() {
  local node arch
  node="$(lw_kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  arch="$(lw_kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.architecture}')"
  [[ "${arch}" == "arm64" ]] || lw_die "the node ${node} reports architecture ${arch}, not arm64"
  lw_log "the node ${node} runs ${arch}"
}

# The arguments the kubeadm configuration asked for are read back from the
# rendered static Pod. The Pod must also be running: an argument the
# component rejects makes it exit, and a component that is not running
# mirrors nothing, which would make the behavioural check below pass for the
# wrong reason.
lw_assert_aggregator_routing() {
  local node pod command phase
  node="$(lw_kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  pod="kube-apiserver-${node}"
  phase="$(lw_static_pod_phase "${pod}")"
  [[ "${phase}" == "Running" ]] || lw_die "the ${pod} static Pod is ${phase}, not Running"
  command="$(lw_static_pod_command "${pod}")"
  lw_has_arg "${command}" "enable-aggregator-routing" "true" \
    || lw_die "the ${pod} static Pod does not carry enable-aggregator-routing=true"
  lw_log "the ${pod} static Pod runs with enable-aggregator-routing=true"
}

lw_assert_mirroring_controller_disabled() {
  local node pod command phase
  node="$(lw_kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  pod="kube-controller-manager-${node}"
  phase="$(lw_static_pod_phase "${pod}")"
  [[ "${phase}" == "Running" ]] || lw_die "the ${pod} static Pod is ${phase}, not Running"
  command="$(lw_static_pod_command "${pod}")"
  lw_has_arg "${command}" "controllers" "${LW_CONTROLLER_ARG}" \
    || lw_die "the ${pod} static Pod does not carry controllers=${LW_CONTROLLER_ARG}"
  lw_log "the ${pod} static Pod runs with controllers=${LW_CONTROLLER_ARG}"
}

# The fixture is a Service without a selector and a core Endpoints object
# without the skip-mirror label. The mirroring controller copies such an
# Endpoints into an EndpointSlice, so the fixture has to satisfy every
# condition the controller checks. A fixture that misses one would produce no
# slice even with the controller running.
#
# The namespace is not created here. The cleanup scope creates it before it
# arms the cleanup, so the namespace is deleted only when this recipe created
# it.
lw_create_mirror_fixture() {
  local node address
  node="$(lw_kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  address="$(lw_kubectl get node "${node}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
  lw_kubectl apply -f - <<FIXTURE
apiVersion: v1
kind: Service
metadata:
  name: mirror-check
  namespace: ${LW_NAMESPACE}
spec:
  ports:
    - name: https
      port: 443
      targetPort: 443
---
apiVersion: v1
kind: Endpoints
metadata:
  name: mirror-check
  namespace: ${LW_NAMESPACE}
subsets:
  - addresses:
      - ip: ${address}
    ports:
      - name: https
        port: 443
FIXTURE
}

# A second Service carries a selector, so the EndpointSlice controller, and
# not the mirroring controller, owns its slices. Its slice appearing in the
# same observation window shows that slices are created and visible here, so
# the absent slice below is the excluded controller and not a dead fixture.
lw_create_slice_control_fixture() {
  local node image
  node="$(lw_kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
  image="$(lw_node_pause_image)"
  lw_kubectl apply -f - <<FIXTURE
apiVersion: v1
kind: Service
metadata:
  name: slice-control
  namespace: ${LW_NAMESPACE}
spec:
  selector:
    app: slice-control
  ports:
    - name: http
      port: 80
      targetPort: 80
---
apiVersion: v1
kind: Pod
metadata:
  name: slice-control
  namespace: ${LW_NAMESPACE}
  labels:
    app: slice-control
spec:
  nodeSelector:
    kubernetes.io/hostname: ${node}
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/master
      operator: Exists
      effect: NoSchedule
  containers:
    - name: pause
      image: ${image}
      ports:
        - name: http
          containerPort: 80
FIXTURE
}

# The node image carries the pause image the kind build placed in it, so the
# control Pod needs no pull. When the node holds no pause image the public
# tag is used and the node pulls it.
lw_node_pause_image() {
  local node image
  node="$(lw_node_container_name)"
  image="$(docker exec "${node}" /usr/local/bin/crictl images 2>/dev/null |
    awk '$1 ~ /pause$/ {print $1 ":" $2; exit}')"
  if [[ -z "${image}" ]]; then
    image="registry.k8s.io/pause:3.10"
  fi
  printf '%s\n' "${image}"
}

lw_fixture_is_valid() {
  local selector skip_mirror leader addresses port_name
  selector="$(lw_kubectl -n "${LW_NAMESPACE}" get service mirror-check -o jsonpath='{.spec.selector}')"
  skip_mirror="$(lw_kubectl -n "${LW_NAMESPACE}" get endpoints mirror-check \
    -o jsonpath='{.metadata.labels.endpointslice\.kubernetes\.io/skip-mirror}')"
  leader="$(lw_kubectl -n "${LW_NAMESPACE}" get endpoints mirror-check \
    -o jsonpath='{.metadata.annotations.control-plane\.alpha\.kubernetes\.io/leader}')"
  addresses="$(lw_kubectl -n "${LW_NAMESPACE}" get endpoints mirror-check \
    -o jsonpath='{.subsets[*].addresses[*].ip}')"
  port_name="$(lw_kubectl -n "${LW_NAMESPACE}" get endpoints mirror-check \
    -o jsonpath='{.subsets[*].ports[*].name}')"
  lw_mirror_fixture_is_valid "${selector}" "${skip_mirror}" "${leader}" "${addresses}" "${port_name}"
}

# Wait for the EndpointSlice of the control Service. The read discipline is
# the one lw_wait_for_no_slice keeps: a read that fails ends the wait at once
# with LW_SLICE_READ_FAILED, so a control that could not be observed is never
# reported as a control that produced nothing, and a window without room for
# the interval still performs its first read.
lw_wait_for_slice() {
  local service="$1" timeout="$2" waited=0 slice status
  while (( waited == 0 )) || (( waited < timeout )); do
    status=0
    slice="$(lw_read_slice_names "${service}")" || status=$?
    if (( status != 0 )); then
      lw_log "the EndpointSlice read for ${service} failed with status ${status}, the control cannot be observed"
      return "${LW_SLICE_READ_FAILED}"
    fi
    if [[ -n "${slice}" ]]; then
      return 0
    fi
    lw_sleep 2
    waited=$((waited + 2))
  done
  return 1
}

# The mirroring exclusion is proved by observation. The proof runs in the
# fixture cleanup scope, so the fixture namespace is removed when an assertion
# fails, and the exit status of the failure is preserved.
lw_prove_mirroring_disabled() {
  lw_with_fixture_cleanup lw_prove_mirroring_disabled_body
}

lw_prove_mirroring_disabled_body() {
  local window=60 observation=0
  lw_create_mirror_fixture
  lw_create_slice_control_fixture
  lw_kubectl -n "${LW_NAMESPACE}" wait --for=condition=Ready pod/slice-control --timeout=180s >/dev/null
  lw_fixture_is_valid \
    || lw_die "the mirror fixture does not satisfy the conditions the mirroring controller checks"
  lw_log "the mirror fixture is a selectorless Service with an Endpoints that carries no skip-mirror label"
  control=0
  lw_wait_for_slice "slice-control" 120 || control=$?
  case "${control}" in
    0)
      ;;
    "${LW_SLICE_READ_FAILED}")
      lw_die "the EndpointSlice read for the control Service failed, so the observation below cannot decide anything"
      ;;
    *)
      lw_die "the control Service produced no EndpointSlice, so the observation below cannot decide anything"
      ;;
  esac
  lw_log "the control Service produced an EndpointSlice, so EndpointSlice creation is working in this cluster"
  lw_wait_for_no_slice "mirror-check" "${window}" || observation=$?
  case "${observation}" in
    "${LW_SLICE_ABSENT}")
      lw_log "no mirrored EndpointSlice appeared for the selectorless Service in ${window} seconds"
      ;;
    "${LW_SLICE_PRESENT}")
      lw_die "a mirrored EndpointSlice appeared for the selectorless Service, the mirroring controller is running"
      ;;
    "${LW_SLICE_READ_FAILED}")
      lw_die "the EndpointSlice read failed, so the absence of a mirrored EndpointSlice is not proven"
      ;;
    *)
      lw_die "the EndpointSlice observation returned the unexpected status ${observation}"
      ;;
  esac
  # The success-path delete waits for the fixture namespace to be gone, so
  # the caller observes a cluster without it, and the wait itself is bounded,
  # so a namespace stuck in Terminating ends the proof instead of hanging it.
  lw_kubectl delete namespace "${LW_NAMESPACE}" --wait=true --timeout=120s >/dev/null
}

cmd_prepare() {
  lw_require_cmd kind docker curl python3
  lw_check_host
  lw_check_docker
  lw_check_kind
  lw_check_cluster_name
  lw_check_kubeconfig_isolation
  lw_provision_kubectl
  lw_ensure_node_image
  lw_create_cluster
  lw_ownership_write "$(lw_live_node_container_id)" "$(lw_node_image_id)"
  lw_assert_server_version
  lw_assert_node_architecture
  lw_assert_aggregator_routing
  lw_assert_mirroring_controller_disabled
  lw_prove_mirroring_disabled
  lw_default_kubeconfig_unchanged \
    || lw_die "the default kubeconfig changed during this run, the recipe must not touch it"
  lw_log "prepare finished"
}

# Network

# The host address the control plane reaches the Mac through. The name the
# container runtime provides is resolved inside the node, so the address is
# the one the container really uses.
lw_resolve_host_address() {
  local node address
  node="$(lw_node_container_name)"
  address="$(docker exec "${node}" getent ahostsv4 host.docker.internal 2>/dev/null |
    awk 'NR == 1 {print $1}')"
  if [[ -z "${address}" ]]; then
    address="$(docker exec "${node}" awk '/host\.docker\.internal/ {print $1; exit}' /etc/hosts 2>/dev/null)"
  fi
  [[ -n "${address}" ]] || lw_die "the node cannot resolve host.docker.internal"
  printf '%s\n' "${address}"
}

# The Mac address of the interface that leaves the host, used when the
# container runtime alias is not reachable.
lw_host_interface_address() {
  local interface
  interface="$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')"
  if [[ -n "${interface}" ]]; then
    ipconfig getifaddr "${interface}" 2>/dev/null || true
  fi
}

lw_can_bind() {
  python3 - "$1" <<'PY'
import socket
import sys

sock = socket.socket()
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    sock.bind((sys.argv[1], 0))
except OSError:
    sys.exit(1)
finally:
    sock.close()
PY
}

lw_write_listener() {
  local path="$1"
  cat >"${path}" <<'LISTENER'
import http.server
import socketserver
import sys
import time

bind_address, port_file, token = sys.argv[1], sys.argv[2], sys.argv[3]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = token.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        pass


class Server(socketserver.TCPServer):
    allow_reuse_address = True


server = Server((bind_address, 0), Handler)
with open(port_file, "w") as handle:
    handle.write(str(server.server_address[1]))
# One stray connection must not consume the probe, so requests are served
# until the deadline and the probe carries the token away on its own request.
deadline = time.monotonic() + 120
while time.monotonic() < deadline:
    server.timeout = deadline - time.monotonic()
    server.handle_request()
server.server_close()
LISTENER
}

lw_token() {
  if command -v uuidgen >/dev/null 2>&1; then
    printf 'gpustack-local-worker-%s\n' "$(uuidgen | tr '[:upper:]' '[:lower:]')"
  else
    printf 'gpustack-local-worker-%s-%s\n' "$(date +%s)" "$$"
  fi
}

# The probe runs inside the node container. The plain HTTP request carries no
# certificate and no identity, so nothing about the later authenticated worker
# is proved here.
lw_probe_from_node() {
  local node="$1" host="$2" port="$3" url
  url="http://${host}:${port}/"
  if docker exec "${node}" sh -c 'command -v curl' >/dev/null 2>&1; then
    docker exec "${node}" curl --fail --silent --show-error --max-time 15 "${url}"
    return $?
  fi
  if docker exec "${node}" sh -c 'command -v wget' >/dev/null 2>&1; then
    docker exec "${node}" wget --quiet --timeout=15 --output-document=- "${url}"
    return $?
  fi
  # The raw-socket fallback needs bash with /dev/tcp and coreutils timeout in
  # the node image. The pinned image carries both, so this guard is for an
  # overridden image: it names what is missing instead of failing mid-probe.
  if ! docker exec "${node}" sh -c 'command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1'; then
    lw_die "the node image carries no HTTP client and no bash with timeout for the raw-socket probe"
  fi
  # The raw-socket fallback has no deadline of its own, so the timeout
  # bounds the whole probe the way the curl and wget paths above are bounded.
  docker exec "${node}" timeout 15 bash -c '
    host="$1"
    port="$2"
    exec 3<>/dev/tcp/"$host"/"$port"
    printf "GET / HTTP/1.0\r\nHost: %s\r\n\r\n" "$host" >&3
    cat <&3
  ' _ "${host}" "${port}"
}

# Start a listener on the Mac, ask the control plane for its token and stop
# the listener again. The listener binds the address itself when the Mac owns
# it, and every Mac interface otherwise.
lw_probe_address() {
  local node="$1" address="$2" label="$3"
  local bind_address token workdir listener port_file log_file response port waited=0
  workdir="$(lw_state_dir)/network"
  mkdir -p "${workdir}"
  listener="${workdir}/listener.py"
  port_file="${workdir}/port"
  log_file="${workdir}/listener.log"
  rm -f "${port_file}" "${log_file}"
  LW_LISTENER_FILES=("${listener}" "${port_file}" "${log_file}")

  if lw_can_bind "${address}"; then
    bind_address="${address}"
  else
    bind_address="0.0.0.0"
    lw_log "the Mac cannot bind ${address}, the listener binds every Mac interface instead"
  fi

  token="$(lw_token)"
  lw_write_listener "${listener}"
  python3 "${listener}" "${bind_address}" "${port_file}" "${token}" >"${log_file}" 2>&1 &
  LW_LISTENER_PID=$!

  while [[ ! -s "${port_file}" ]]; do
    if ! kill -0 "${LW_LISTENER_PID}" 2>/dev/null; then
      lw_stop_listener
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
    if (( waited > 30 )); then
      lw_stop_listener
      return 1
    fi
  done
  port="$(cat "${port_file}")"

  response="$(lw_probe_from_node "${node}" "${address}" "${port}")" || {
    lw_stop_listener
    return 1
  }
  if ! grep -Fq -- "${token}" <<<"${response}"; then
    lw_stop_listener
    return 1
  fi

  lw_stop_listener
  lw_log "the control plane reached ${address}:${port} over plain HTTP and received the expected token"
  lw_log "the ${label} probe used http://${address}:${port}/ with the listener bound to ${bind_address}"
  return 0
}

cmd_network() {
  lw_require_cmd docker python3
  lw_ownership_validate || lw_die "the cluster was not created by this recipe, there is nothing to probe"
  local node address interface_address
  node="$(lw_node_container_name)"
  address="$(lw_resolve_host_address)"
  lw_log "the node resolves host.docker.internal to ${address}"

  lw_probe_address "${node}" "${address}" "container host alias" \
    || lw_die "the control plane could not reach ${address} from the Mac"

  # The container runtime alias names the gateway the container uses to reach
  # the host. The address of a Mac interface is probed as well, with the
  # listener bound to that address, so the proof names an address the Mac
  # itself owns.
  interface_address="$(lw_host_interface_address)"
  if [[ -n "${interface_address}" && "${interface_address}" != "${address}" ]]; then
    if lw_probe_address "${node}" "${interface_address}" "Mac interface"; then
      lw_log "the Mac interface address ${interface_address} is reachable from the control plane"
    else
      lw_log "the Mac interface address ${interface_address} is not reachable from the control plane"
    fi
  fi
  lw_log "network finished"
}

# Cleanup

cmd_cleanup() {
  lw_require_cmd kind docker
  if ! lw_ownership_validate; then
    lw_log "no ownership record was found, so no cluster is proven to belong to this recipe"
    lw_log "the cluster ${LW_CLUSTER_NAME} and the files under $(lw_state_dir) were left untouched"
    return 0
  fi
  local recorded live
  if [[ "$(lw_ownership_read CLUSTER_NAME)" != "${LW_CLUSTER_NAME}" ]]; then
    lw_die "the ownership record names cluster $(lw_ownership_read CLUSTER_NAME), not ${LW_CLUSTER_NAME}, so the record is kept: the cluster it names may still be running"
  fi
  recorded="$(lw_ownership_read NODE_CONTAINER_ID)"
  live="$(lw_live_node_container_id)"
  if [[ -z "${live}" ]]; then
    lw_log "the node container is already gone, only the recipe files are removed"
    rm -rf "$(lw_state_dir)"
    return 0
  fi
  if ! lw_ownership_matches "${recorded}" "${live}"; then
    lw_die "the live node container ${live} is not the one this recipe created, the cluster is not removed"
  fi
  lw_log "removing the cluster ${LW_CLUSTER_NAME}"
  kind delete cluster --name "${LW_CLUSTER_NAME}" --kubeconfig "$(lw_kubeconfig_path)"
  lw_default_kubeconfig_unchanged \
    || lw_die "the default kubeconfig changed during this cleanup, the recipe must not touch it"
  rm -rf "$(lw_state_dir)"
  lw_log "the node image ${LW_NODE_IMAGE} is kept as a build cache"
  lw_log "cleanup finished"
}

case "${1:-}" in
  preflight)
    cmd_preflight
    ;;
  prepare)
    cmd_prepare
    ;;
  network)
    cmd_network
    ;;
  cleanup)
    cmd_cleanup
    ;;
  verify)
    lw_die "the verify command is not part of this recipe, it starts the authenticated worker and belongs to a later stage"
    ;;
  *)
    usage
    exit 1
    ;;
esac
