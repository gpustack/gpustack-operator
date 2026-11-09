#!/usr/bin/env bash

# Static render tests for the model-transfer source runtime. Like
# test-topograph.sh, this never touches a cluster: everything is `helm
# template` plus assertions on the output, so the whole suite runs offline.

set -o errexit
set -o nounset
set -o pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
helm_bin="${HELM_BIN:-helm}"

kube_version="1.31.0"
image_nvidia="registry.example/model-transfer:v1-cuda12"
image_ascend="registry.example/model-transfer:v1-cann9"
budget_bytes=4294967296

function render() {
  "${helm_bin}" template gpustack-operator "${chart_dir}" --kube-version "${kube_version}" "$@"
}

function expect_failure() {
  local name="$1" expected="$2"
  shift 2

  local output
  if output="$(render "$@" 2>&1)"; then
    echo "${name}: expected render failure" >&2
    return 1
  fi
  if ! grep -Fq "${expected}" <<<"${output}"; then
    echo "${name}: unexpected render failure: ${output}" >&2
    return 1
  fi
}

# assert_order fails when the first pattern's first match does not precede the
# second's, which is how the env and args layering below is checked: layering
# is exactly an order claim, and "appears somewhere" would not prove it.
function assert_order() {
  local name="$1" first="$2" second="$3" rendered="$4"

  local first_line second_line
  first_line="$(grep -n -F -m1 -e "${first}" <<<"${rendered}" | cut -d: -f1)"
  second_line="$(grep -n -F -m1 -e "${second}" <<<"${rendered}" | cut -d: -f1)"
  if [[ -z "${first_line}" || -z "${second_line}" ]]; then
    echo "${name}: expected both '${first}' and '${second}' to render" >&2
    return 1
  fi
  if [[ "${first_line}" -ge "${second_line}" ]]; then
    echo "${name}: '${first}' must precede '${second}'" >&2
    return 1
  fi
}

# Read document identities rather than counting repeated kind strings: duplicate
# root keys in one document can otherwise hide a lost vendor resource.
function assert_vendor_documents() {
  local kind="$1" rendered="$2" suffix="${3:-}"
  awk -v wanted="${kind}" -v suffix="${suffix}" '
    function finish() {
      if (kinds > 1) { print "duplicate kind keys in a YAML document" > "/dev/stderr"; bad = 1 }
      if (kind == wanted) {
        if (seen[name]++) { print "duplicate resource identity" > "/dev/stderr"; bad = 1 }
        count++
      }
      kind = name = ""; kinds = metadata = 0
    }
    /^---[[:space:]]*$/ { finish(); next }
    /^kind:/ { kinds++; kind = $2 }
    /^metadata:/ { metadata = 1; next }
    /^[^[:space:]#]/ { metadata = 0 }
    metadata && /^  name:/ { name = $2 }
    END {
      finish()
      if (bad || count != 2 ||
          !seen["gpustack-operator-model-transfer-ascend" suffix] ||
          !seen["gpustack-operator-model-transfer-nvidia" suffix]) {
        print "expected separate, uniquely named vendor " wanted " documents" > "/dev/stderr"
        exit 1
      }
    }
  ' <<<"${rendered}"
}

enabled=(--set modelTransfer.images.amd=null --set modelTransfer.enabled=true --set "modelTransfer.source.maxMemoryBytes=${budget_bytes}" --set "modelTransfer.images.nvidia=${image_nvidia}" --set "modelTransfer.images.ascend=${image_ascend}")

# Off by default, and nothing else in the release changes: not a Pod, not a
# policy variable, not a mount on the model-manager.
disabled="$(render)"
if grep -q 'model-transfer' <<<"${disabled}"; then
  echo 'disabled: model-transfer resources rendered' >&2
  exit 1
fi

# The published defaults render all three source fleets when explicitly enabled.
default_fleets="$(render --set modelTransfer.enabled=true --show-only templates/model-transfer/daemonset.yaml)"
if [[ "$(grep -c '^kind: DaemonSet$' <<<"${default_fleets}")" -ne 3 ]]; then
  echo 'defaults: expected AMD, Ascend and NVIDIA source DaemonSets' >&2
  exit 1
fi
for manufacturer in amd ascend nvidia; do
  if ! grep -q "^  name: gpustack-operator-model-transfer-${manufacturer}$" <<<"${default_fleets}"; then
    echo "defaults: missing ${manufacturer} source DaemonSet" >&2
    exit 1
  fi
done
amd_doc="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-amd$/{on=1} on' <<<"${default_fleets}")"
if ! grep -Fq 'image: "gpustack/model-transfer:v0.1.0-rocm7.2"' <<<"${amd_doc}" \
  || ! grep -q '^            - name: AMD_VISIBLE_DEVICES$' <<<"${amd_doc}"; then
  echo 'amd: the default fleet must use the published ROCm image and AMD device environment' >&2
  exit 1
fi

# One DaemonSet per named manufacturer, each with exactly the two containers:
# the operator image runs the broker, the named vendor image runs the source
# module.
daemonsets="$(render "${enabled[@]}" --show-only templates/model-transfer/daemonset.yaml)"
assert_vendor_documents DaemonSet "${daemonsets}"
if [[ "$(grep -c '^kind: DaemonSet$' <<<"${daemonsets}")" -ne 2 ]]; then
  echo "enabled: expected 2 source DaemonSets" >&2
  exit 1
fi
if [[ "$(grep -c '^        - name: stage-libs$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - /etc/gpustack/lib/model-transfer$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^            path: /var/lib/gpustack/operator/lib/model-transfer$' <<<"${daemonsets}")" -ne 2 ]]; then
  echo 'enabled: each source DaemonSet must stage the model-transfer wheel onto the host' >&2
  exit 1
fi
if [[ "$(grep -c '^        - name: broker$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^        - name: worker$' <<<"${daemonsets}")" -ne 2 ]]; then
  echo 'enabled: each source DaemonSet must carry exactly a broker and a worker container' >&2
  exit 1
fi
if [[ "$(grep -c '^            - python3$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - gpustack_model_transfer.source$' <<<"${daemonsets}")" -ne 2 ]]; then
  echo 'enabled: the worker container must run python3 -m gpustack_model_transfer.source' >&2
  exit 1
fi

# Both vendors use privileged workers with host /dev and /sys. NVIDIA uses
# its RuntimeClass; Ascend mounts driver and DCMI libraries separately from
# the image's CANN toolkit. The cache is writable for
# the broker and read-only for the worker; the ledger and the bootstrap file
# have exactly one writer each.
nvidia_doc="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-nvidia$/{on=1} on' <<<"${daemonsets}")"
ascend_doc="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-ascend$/{on=1} on' <<<"${daemonsets}")"

if ! grep -q '^      runtimeClassName: nvidia$' <<<"${nvidia_doc}"; then
  echo 'nvidia: the source Pod must run under the nvidia RuntimeClass' >&2
  exit 1
fi
for vendor_doc in "${nvidia_doc}" "${ascend_doc}"; do
  if ! grep -q '^            privileged: true$' <<<"${vendor_doc}"; then
    echo 'source worker: privileged device access is required for both vendors' >&2
    exit 1
  fi
  for host_path in 'path: /dev' 'path: /sys'; do
    if [[ "$(grep -cF "${host_path}" <<<"${vendor_doc}")" -ne 1 ]]; then
      echo "source worker: expected exactly one ${host_path} mount" >&2
      exit 1
    fi
  done
done
# Kubernetes expands the Pod UID when starting the container.
# shellcheck disable=SC2016
if [[ "$(grep -cF -- '--worker-id=$(KUBERNETES_POD_UID)' <<<"${daemonsets}")" -ne 2 ]]; then
  echo 'source worker: broker identity must bind to the actual Pod UID' >&2
  exit 1
fi
for host_path in '/usr/local/Ascend/driver' '/usr/local/dcmi'; do
  if ! grep -qF "path: ${host_path}" <<<"${ascend_doc}"; then
    echo "ascend: expected the host ${host_path} mount" >&2
    exit 1
  fi
done
if grep -q 'mountPath: /usr/local/Ascend$' <<<"${ascend_doc}"; then
  echo 'ascend: the whole host Ascend tree must not shadow the image CANN' >&2
  exit 1
fi

if [[ "$(grep -c 'path: /var/lib/gpustack/model-transfer/nvidia$' <<<"${nvidia_doc}")" -ne 1 ]] \
  || [[ "$(grep -c 'path: /var/lib/gpustack/model-transfer/ascend$' <<<"${ascend_doc}")" -ne 1 ]]; then
  echo 'enabled: each vendor needs its own durable ledger root' >&2
  exit 1
fi
if grep -q '/var/lib/gpustack/model-transfer' <<<"$(grep -A3 '^        - name: worker$' <<<"${nvidia_doc}")"; then
  echo 'worker: the broker ledger is not the worker business' >&2
  exit 1
fi

cache_mounts="$(grep -A2 '^            - name: cache$' <<<"${nvidia_doc}")"
if [[ "$(grep -c 'mountPath: /var/lib/gpustack/models$' <<<"${cache_mounts}")" -ne 2 ]] \
  || [[ "$(grep -c 'readOnly: true$' <<<"${cache_mounts}")" -ne 1 ]]; then
  echo 'cache: the broker writes it and the worker reads it read-only, once each' >&2
  exit 1
fi

# The loopback control wire and the TLS-only session listener, on Pod
# networking: no hostNetwork anywhere, and the TLS pair passed unconditionally.
if grep -q 'hostNetwork' <<<"${daemonsets}"; then
  echo 'enabled: source Pods use Pod networking, not the host network' >&2
  exit 1
fi
if [[ "$(grep -c '^            - --control-addr=127.0.0.1:9250$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --session-addr=:9251$' <<<"${daemonsets}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --session-tls-cert=' <<<"${daemonsets}")" -ne 2 ]]; then
  echo 'enabled: every broker needs its control, session and TLS arguments' >&2
  exit 1
fi

# The broker's memory window and session cap follow the source configuration:
# the ceiling on both of its own flags, the floor and the session cap passed
# through.
for arg in '--physical-bytes=4294967296' '--max-memory-bytes=4294967296' '--min-memory-bytes=1073741824' '--max-sessions=64'; do
  if [[ "$(grep -c -- "${arg}\$" <<<"${daemonsets}")" -ne 2 ]]; then
    echo "enabled: every broker needs ${arg}" >&2
    exit 1
  fi
done

# By default workerResources is empty and no resources block is rendered on the
# worker container; the configured ceiling and floor reach the worker as its own
# window policy.
for vendor_doc in "${nvidia_doc}" "${ascend_doc}"; do
  worker_block="$(sed -n '/^        - name: worker$/,/^      volumes:/p' <<<"${vendor_doc}")"
  if grep -q '^          resources:$' <<<"${worker_block}"; then
    echo 'source worker: an empty workerResources must render no resources block' >&2
    exit 1
  fi
  if ! grep -q '^            - name: GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES$' <<<"${worker_block}" \
    || ! grep -q '^              value: "4294967296"$' <<<"${worker_block}"; then
    echo 'source worker: the ceiling must reach the worker as its window policy' >&2
    exit 1
  fi
  if ! grep -q '^            - name: GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES$' <<<"${worker_block}" \
    || ! grep -q '^              value: "1073741824"$' <<<"${worker_block}"; then
    echo 'source worker: the floor must reach the worker as its window policy' >&2
    exit 1
  fi
done

# An explicit workerResources renders as given, requests and limits included.
explicit="$(render "${enabled[@]}" \
  --set 'modelTransfer.workerResources.requests.cpu=250m' \
  --set 'modelTransfer.workerResources.requests.memory=1Gi' \
  --set 'modelTransfer.workerResources.limits.memory=16Gi' \
  --show-only templates/model-transfer/daemonset.yaml)"
nvidia_explicit="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-nvidia$/{on=1} on' <<<"${explicit}")"
worker_explicit="$(sed -n '/^        - name: worker$/,/^      volumes:/p' <<<"${nvidia_explicit}")"
if ! grep -q '^              memory: 16Gi$' <<<"${worker_explicit}" \
  || ! grep -q '^              memory: 1Gi$' <<<"${worker_explicit}"; then
  echo 'worker resources: an explicit map is used as given, requests included' >&2
  exit 1
fi

# A zero ceiling follows the node's RAM: the broker derives the ceiling at
# start, the floor and session cap pass through at their defaults, and the
# worker renders no resources block at all.
zero="$(render --set modelTransfer.images.amd=null --set modelTransfer.enabled=true \
  --set "modelTransfer.images.nvidia=${image_nvidia}" \
  --set "modelTransfer.images.ascend=${image_ascend}" \
  --show-only templates/model-transfer/daemonset.yaml)"
if [[ "$(grep -c -- '--physical-bytes=0$' <<<"${zero}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --min-memory-bytes=1073741824$' <<<"${zero}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --max-sessions=64$' <<<"${zero}")" -ne 2 ]]; then
  echo 'zero ceiling: the broker must receive the follow-the-node figure' >&2
  exit 1
fi
nvidia_zero="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-nvidia$/{on=1} on' <<<"${zero}")"
worker_zero="$(sed -n '/^        - name: worker$/,/^      volumes:/p' <<<"${nvidia_zero}")"
if grep -q '^          resources:$' <<<"${worker_zero}"; then
  echo 'zero ceiling: the worker must render no resources block at all' >&2
  exit 1
fi

# A tuned source block passes every figure through.
tuned="$(render "${enabled[@]}" \
  --set modelTransfer.source.minMemoryBytes=2147483648 \
  --set modelTransfer.source.maxSessions=32 \
  --set modelTransfer.source.maxMemoryBytes=8589934592 \
  --show-only templates/model-transfer/daemonset.yaml)"
if [[ "$(grep -c '^            - --physical-bytes=8589934592$' <<<"${tuned}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --min-memory-bytes=2147483648$' <<<"${tuned}")" -ne 2 ]] \
  || [[ "$(grep -c '^            - --max-sessions=32$' <<<"${tuned}")" -ne 2 ]]; then
  echo 'tuned source: the broker must receive every configured figure' >&2
  exit 1
fi
nvidia_tuned="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-nvidia$/{on=1} on' <<<"${tuned}")"
worker_tuned="$(sed -n '/^        - name: worker$/,/^      volumes:/p' <<<"${nvidia_tuned}")"
if ! grep -q '^              value: "8589934592"$' <<<"${worker_tuned}" \
  || ! grep -q '^              value: "2147483648"$' <<<"${worker_tuned}"; then
  echo 'tuned source: the window policy must follow' >&2
  exit 1
fi

# The mandatory PCI selector outranks a conflicting global and component entry,
# and both extra entries still land.
placement="$(render "${enabled[@]}" \
  --set-string 'global.nodeSelector.feature\.node\.kubernetes\.io/pci-10de\.present=false' \
  --set-string 'modelTransfer.nodeSelector.feature\.node\.kubernetes\.io/pci-10de\.present=false' \
  --set 'modelTransfer.nodeSelector.zone=team-a' \
  --set 'global.nodeSelector.tier=edge' \
  --show-only templates/model-transfer/daemonset.yaml)"
if ! grep -Fq 'feature.node.kubernetes.io/pci-10de.present: "true"' <<<"${placement}"; then
  echo 'placement: the PCI label is a requirement no override may invert' >&2
  exit 1
fi
if ! grep -Fq 'zone: team-a' <<<"${placement}" || ! grep -Fq 'tier: edge' <<<"${placement}"; then
  echo 'placement: the component and global selectors must merge under it' >&2
  exit 1
fi

# Env layering on both containers (user env before the managed identity), the
# bootstrap credential handed to the worker, and the extra broker arguments
# appended after the chart's own. The ordering claims are scoped to one
# container each: a whole-document match would prove nothing about which
# container carries what.
layered="$(render "${enabled[@]}" \
  --set 'modelTransfer.env.HTTP_PROXY=http://proxy:8080' \
  --set 'modelTransfer.extraArgs[0]=--max-generations=4' \
  --show-only templates/model-transfer/daemonset.yaml)"
ascend_doc="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-ascend$/{on=1} on' <<<"${layered}")"
broker_block="$(sed -n '/^        - name: broker$/,/^        - name: worker$/p' <<<"${ascend_doc}")"
worker_block="$(sed -n '/^        - name: worker$/,$p' <<<"${ascend_doc}")"
assert_order 'broker env' 'name: HTTP_PROXY' 'name: KUBERNETES_POD_UID' "${broker_block}"
assert_order 'worker env' 'name: HTTP_PROXY' 'name: KUBERNETES_POD_UID' "${worker_block}"
vendor_env="$(render "${enabled[@]}" \
  --set 'modelTransfer.env.NVIDIA_VISIBLE_DEVICES=none' \
  --show-only templates/model-transfer/daemonset.yaml)"
nvidia_env="$(awk '/^---$/{on=0} /^  name: gpustack-operator-model-transfer-nvidia$/{on=1} on' <<<"${vendor_env}")"
nvidia_worker="$(sed -n '/^        - name: worker$/,$p' <<<"${nvidia_env}")"
assert_order 'managed vendor env' 'value: "none"' 'value: "all"' "${nvidia_worker}"
assert_order 'worker identity' 'name: KUBERNETES_NODE_NAME' 'name: GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS' "${worker_block}"
assert_order 'worker contract' 'name: GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE' 'name: GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE' "${worker_block}"
if [[ "$(grep -c '^            - name: HTTP_PROXY$' <<<"${layered}")" -ne 4 ]]; then
  echo 'env: the user env must reach both containers of both vendors' >&2
  exit 1
fi
assert_order 'broker args' '--session-tls-key=' '--max-generations=4' "${broker_block}"
if [[ "$(grep -c '^    type: OnDelete$' <<<"${layered}")" -ne 2 ]]; then
  echo 'source workers require acknowledged drain before replacement' >&2
  exit 1
fi
if [[ "$(grep -c 'fieldPath: status.podIP' <<<"${layered}")" -ne 4 ]]; then
  echo 'every broker and worker needs its managed Pod address' >&2
  exit 1
fi

# Release, namespace and image overrides: the operator half follows the global
# image settings, the vendor reference is used as supplied.
overridden="$(render --namespace ops-ns --set namespaceOverride=real-ops \
  "${enabled[@]}" \
  --set global.imageRegistry=reg.local --set global.imageNamespace=mirror --set global.imagePullPolicy=Always \
  --show-only templates/model-transfer/daemonset.yaml)"
if ! grep -Fq 'namespace: real-ops' <<<"${overridden}"; then
  echo 'overrides: the namespace override must reach the source DaemonSets' >&2
  exit 1
fi
if ! grep -Eq '^          image: "reg\.local/mirror/gpustack-operator:' <<<"${overridden}"; then
  echo 'overrides: the broker image must follow the global image settings' >&2
  exit 1
fi
if [[ "$(grep -cF "image: \"${image_nvidia}\"" <<<"${overridden}")" -ne 2 ]] \
  || [[ "$(grep -cF "image: \"${image_ascend}\"" <<<"${overridden}")" -ne 2 ]]; then
  echo 'overrides: vendor image references are used as supplied on stage-libs and worker, never rewritten' >&2
  exit 1
fi
if [[ "$(grep -c '^          imagePullPolicy: Always$' <<<"${overridden}")" -ne 6 ]]; then
  echo 'overrides: the global pull policy reaches every initContainer and container' >&2
  exit 1
fi

pull_secrets="$(render "${enabled[@]}" --set global.imagePullSecrets[0].name=mirror-pull-secret --show-only templates/model-transfer/daemonset.yaml)"
if [[ "$(grep -c '^      imagePullSecrets:$' <<<"${pull_secrets}")" -ne 2 ]] \
  || [[ "$(grep -c '^        - name: mirror-pull-secret$' <<<"${pull_secrets}")" -ne 2 ]]; then
  echo 'pull secrets: every source DaemonSet carries the global secret' >&2
  exit 1
fi

# The memory figures may be zero — the follow-the-node answer — or any byte
# count; the only failures are a negative figure and one that is not a byte
# count at all, plus a worker limit the cluster cannot schedule. A string
# lands on the schema's integer check before the template sees it.
expect_failure negative-budget \
  'is negative' \
  --set modelTransfer.enabled=true --set modelTransfer.source.maxMemoryBytes=-4 \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure unparseable-budget \
  'got string, want integer' \
  --set modelTransfer.enabled=true --set modelTransfer.source.maxMemoryBytes=huge \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure suffixed-budget-without-schema \
  'is not a plain byte count' \
  --skip-schema-validation \
  --set modelTransfer.enabled=true --set modelTransfer.source.maxMemoryBytes=16Gi \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure suffixed-minimum-without-schema \
  'is not a plain byte count' \
  --skip-schema-validation \
  --set modelTransfer.enabled=true --set modelTransfer.source.minMemoryBytes=2Gi \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure negative-source-block \
  'is negative' \
  --set modelTransfer.enabled=true --set modelTransfer.source.minMemoryBytes=-1073741824 \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure unparseable-limit \
  'is not a plain Kubernetes memory quantity' \
  --set modelTransfer.enabled=true --set modelTransfer.source.maxMemoryBytes="${budget_bytes}" \
  --set modelTransfer.images.nvidia="${image_nvidia}" \
  --set modelTransfer.workerResources.limits.memory=huge
expect_failure bad-sessions \
  'got string, want integer' \
  --set modelTransfer.enabled=true --set modelTransfer.source.maxSessions=many \
  --set modelTransfer.images.nvidia="${image_nvidia}"
expect_failure unknown-manufacturer \
  'which global.manufacturers does not carry' \
  --set modelTransfer.enabled=true \
  --set modelTransfer.images.noknown=vendor/image:v1
expect_failure unsupported-manufacturer \
  'only amd, ascend, and nvidia are supported by model-transfer' \
  --set modelTransfer.enabled=true \
  --set modelTransfer.images.hygon=vendor/image:v1

# Trust is always provisioned. Without cert-manager the chart renders the pair
# itself, with the public half beside it; with cert-manager it renders the
# Certificate instead and the Secret is cert-manager's.
tls_fallback="$(render "${enabled[@]}" --show-only templates/model-transfer/tls.yaml)"
assert_vendor_documents Secret "${tls_fallback}" -cert
if [[ "$(grep -c '^kind: Secret$' <<<"${tls_fallback}")" -ne 2 ]] \
  || [[ "$(grep -c '^type: kubernetes.io/tls$' <<<"${tls_fallback}")" -ne 2 ]] \
  || [[ "$(grep -c '^  ca.crt:' <<<"${tls_fallback}")" -ne 2 ]]; then
  echo 'tls: each vendor needs a serving Secret carrying its trust anchor' >&2
  exit 1
fi
tls_certmanager="$(render "${enabled[@]}" --set global.certmanager.enabled=true --show-only templates/model-transfer/tls.yaml)"
assert_vendor_documents Certificate "${tls_certmanager}" -cert
tls_external="$(render "${enabled[@]}" --set global.certmanager.enabled=true \
  --set global.certmanager.issuer.name=cluster-ca --show-only templates/model-transfer/tls.yaml)"
assert_vendor_documents Certificate "${tls_external}" -cert
for tls_managed in "${tls_certmanager}" "${tls_external}"; do
  if [[ "$(grep -c '^  secretTemplate:$' <<<"${tls_managed}")" -ne 2 ]] \
    || [[ "$(grep -c '^      app.kubernetes.io/component: model-transfer$' <<<"${tls_managed}")" -ne 2 ]]; then
    echo 'tls: cert-manager Secrets must carry the component label used by discovery watches' >&2
    exit 1
  fi
done
if [[ "$(grep -c '^kind: Issuer$' <<<"${tls_certmanager}")" -ne 2 ]] \
  || [[ "$(grep -c '^kind: Certificate$' <<<"${tls_certmanager}")" -ne 2 ]] \
  || [[ "$(grep -c '^kind: Secret$' <<<"${tls_certmanager}")" -ne 0 ]]; then
  echo 'tls: with cert-manager on, the Certificate replaces the chart Secret' >&2
  exit 1
fi
if [[ "$(grep -cF '*.' <<<"${tls_certmanager}")" -eq 0 ]] \
  || ! grep -q 'svc.cluster.local' <<<"${tls_certmanager}"; then
  echo 'tls: the certificate must name the headless Service and its Pod wildcard' >&2
  exit 1
fi

# One headless Service per manufacturer, so an engine picks an exact endpoint
# and verifies it by the Service name the certificate was issued for.
assert_vendor_documents Service "$(render "${enabled[@]}" --show-only templates/model-transfer/service.yaml)"
services="$(render "${enabled[@]}" --show-only templates/model-transfer/service.yaml)"
if [[ "$(grep -c '^kind: Service$' <<<"${services}")" -ne 2 ]] \
  || [[ "$(grep -c '^  clusterIP: None$' <<<"${services}")" -ne 2 ]] \
  || [[ "$(grep -c '^      port: 9251$' <<<"${services}")" -ne 2 ]]; then
  echo 'services: each vendor needs its headless Service on the session port' >&2
  exit 1
fi

# The identity is what a broker runs under, and no more: TokenReview, the read
# surface, the one Binding write, and the authority ConfigMaps scoped to the
# operator namespace. No Secret, no configmap outside it.
rbac="$(render "${enabled[@]}" --show-only templates/model-transfer/serviceaccount.yaml)"
for resource in 'tokenreviews' '"pods", "nodes"' '"settings"' 'modelprefetches' 'modelstorebindings' '"configmaps"'; do
  if ! grep -qF "${resource}" <<<"${rbac}"; then
    echo "rbac: expected ${resource} in the source identity" >&2
    exit 1
  fi
done
if grep -Eiq 'resources: \["secrets"\]|resources: \["secrets",|^\s+- secrets$' <<<"${rbac}"; then
  echo 'rbac: the source identity must not read Secrets' >&2
  exit 1
fi
authority_rules="$(sed -n '/resources: \["configmaps"\]/,/verbs:/p' <<<"${rbac}")"
if ! grep -q '"delete"' <<<"${authority_rules}"; then
  echo "rbac: source broker lacks authority ConfigMap delete permission" >&2
  exit 1
fi

binding_rules="$(sed -n '/modelstorebindings/,/verbs:/p' <<<"${rbac}")"
for verb in '"get"' '"list"' '"watch"' '"patch"' '"update"'; do
  if ! grep -qF "${verb}" <<<"${binding_rules}"; then
    echo "rbac: the Binding write set is incomplete, missing ${verb}" >&2
    exit 1
  fi
done

# The model-manager reads the vendors' ledgers read-only, only while the
# feature is on.
manager_on="$(render "${enabled[@]}" --show-only templates/model-manager/daemonset.yaml)"
if ! grep -q -- '--model-transfer-state-root=/var/lib/gpustack/model-transfer' <<<"${manager_on}" \
  || ! grep -q '^        - name: model-transfer-state$' <<<"${manager_on}"; then
  echo 'handoff: the model-manager must read the ledger parent read-only' >&2
  exit 1
fi
manager_off="$(render --show-only templates/model-manager/daemonset.yaml)"
if grep -q 'model-transfer' <<<"${manager_off}"; then
  echo 'handoff: with the feature off, the model-manager renders untouched' >&2
  exit 1
fi

echo 'model-transfer: all static renders pass'
