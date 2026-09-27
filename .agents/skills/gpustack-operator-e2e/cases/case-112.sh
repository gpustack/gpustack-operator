#!/usr/bin/env bash
#
# CASE 112 — Image source: a digest-pinned OCI image as a ModelArtifact's weights resolves
#            claim-shaped and mounts through a Kubernetes image volume (MUTATING, self-recovering)
#
#   case-112.sh <NS>
#
# Goal:        Prove the image source end to end: a tag-only reference is refused at admission,
#              naming the digest contract; an image artifact resolves on its first pass with no
#              network — claim-shaped status, no manifest digest, no revision; an Instance
#              carrying that artifact as a model volume runs a pod whose weights are the image,
#              read-only and whole, with the fixture file matching byte for byte; the node cache
#              plays no part (status.nodes never appears).
# Environment: Cluster nodes run image volumes — kubelet 1.35+ (beta default-on; 1.33/1.34 would
#              need the gate opened, which admission does not opt into) and containerd 2.1+. The
#              stock registry:2 and python:3.12-slim images are pullable, and python3 is on the
#              runner to generate and push the fixture. No GPU, no engine. There is no auto-skip
#              for a cluster that cannot serve image volumes: the admission-refusal rows still
#              run, and the mount row FAILs, which is the honest verdict for that cluster.
# Inputs:      MOCKED: the fixture image this case generates on the runner (one gzip layer, one
#              marker file, addressed by its own sha256) and pushes into an in-cluster registry
#              through a port-forward; the node reaches that registry over its own loopback — a
#              hostNetwork forwarder pod on the worker turns 127.0.0.1:35000 into the registry
#              service, and containerd treats localhost as insecure by default, so no containerd
#              configuration is touched.
# Expected:    - a tag-only reference is refused, the message naming why the digest is required;
#              - the image artifact resolves on its first pass: Resolved=True, resolved carrying
#                only resolvedTime — no manifestDigest, no revision;
#              - status.nodes never appears for the artifact;
#              - the Instance's pod mounts the image read-only at its model mount path and the
#                marker file's sha256 equals the fixture's;
#              - a ModelPrefetch naming the image artifact is refused: nothing to warm.
# Cleanup:     Trap deletes the case-labeled objects in the namespace, the in-cluster registry
#              and forwarder, and the scratch dir.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
CASES_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "${CASES_DIR}/_rows-lib.sh"

NS="${1:?usage: case-112.sh <NS>}"
P=c112
REG_PORT=35000            # the node's loopback port the forwarder serves the registry on
PUSH_PORT=5550            # the runner's loopback port port-forward pushes through
FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

cleanup() {
  echo
  echo "[case-112] cleanup"
  kubectl -n "$NS" delete modelartifacts.worker.gpustack.ai -l e2e.gpustack.ai/case=112 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete instances.worker.gpustack.ai -l e2e.gpustack.ai/case=112 --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl -n "$NS" delete pods,svc,cm -l e2e.gpustack.ai/case=112 --ignore-not-found --wait=false --force --grace-period=0 >/dev/null 2>&1
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null
  rm -rf "$SCRATCH"
}
SCRATCH="$(mktemp -d)"
trap cleanup EXIT

command -v python3 >/dev/null 2>&1 || { echo "[case-112] python3 is not on the runner; NOTHING WAS VERIFIED"; exit 2; }
# Workers are the nodes without the control-plane role — the shape the suite's kind clusters
# provision (a control-plane plus worker nodes); on a single-node kind cluster this exits 2.
read -r -a WORKERS <<<"$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}')"
[ "${#WORKERS[@]}" -ge 1 ] || { echo "[case-112] needs one schedulable worker, found none; NOTHING WAS VERIFIED"; exit 2; }
NODE="${WORKERS[0]}"
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "[case-112] no general InstanceType (run case-1 first); NOTHING WAS VERIFIED"; exit 2; }
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "[case-112] registry: in-cluster, reached by the node over its own loopback"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${P}-registry
  labels: {e2e.gpustack.ai/case: "112", app: ${P}-registry}
spec:
  containers:
    - name: registry
      image: registry:2
      ports: [{containerPort: 5000}]
---
apiVersion: v1
kind: Service
metadata:
  name: ${P}-registry
  labels: {e2e.gpustack.ai/case: "112"}
spec:
  selector: {app: ${P}-registry}
  ports: [{port: 5000, targetPort: 5000}]
EOF
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pod ${P}-registry -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && { REG_UP=1; break; }
  sleep 2
done
[ "${REG_UP:-0}" = 1 ] || { echo "[case-112] the registry pod never reached Running; NOTHING WAS VERIFIED"; exit 2; }

cat > "$SCRATCH/forward.py" <<'PY'
import socket, threading, sys
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError: pass
    finally:
        try: a.close(); b.close()
        except OSError: pass
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 35000)); srv.listen(64)
while True:
    c, _ = srv.accept()
    up = socket.create_connection((sys.argv[1], 5000))
    threading.Thread(target=pipe, args=(c, up), daemon=True).start()
    threading.Thread(target=pipe, args=(up, c), daemon=True).start()
PY
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${P}-forward
  labels: {e2e.gpustack.ai/case: "112"}
data:
  forward.py: |
$(sed 's/^/    /' "$SCRATCH/forward.py")
EOF
REGIP="$(kubectl -n "$NS" get svc ${P}-registry -o jsonpath='{.spec.clusterIP}')"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${P}-forward
  labels: {e2e.gpustack.ai/case: "112", app: ${P}-forward}
spec:
  hostNetwork: true
  dnsPolicy: ClusterFirstWithHostNet
  nodeName: $NODE
  tolerations: [{operator: Exists}]
  containers:
    - name: forward
      image: python:3.12-slim
      command: ["python", "/forward.py", "$REGIP"]
      volumeMounts: [{name: f, mountPath: /forward.py, subPath: forward.py}]
  volumes:
    - name: f
      configMap: {name: ${P}-forward}
EOF
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pod ${P}-forward -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && { FWD_UP=1; break; }
  sleep 2
done
[ "${FWD_UP:-0}" = 1 ] || { echo "[case-112] the forwarder pod never reached Running; NOTHING WAS VERIFIED"; exit 2; }

echo "[case-112] fixture: generate the marker image and push it"
printf 'gpustack e2e image-source fixture, case 112\n' > "$SCRATCH/marker.txt"
EXPECT="$(shasum -a 256 "$SCRATCH/marker.txt" | awk '{print $1}')"
# The port-forward is long-running, so it passes --request-timeout=0 explicitly: the kubectl shim
# would otherwise bound it at 30s and sever the push mid-flight. The local address is chosen with
# --address — docker resolves `localhost` to ::1 first, where nothing listens, and the IPv6
# connection times out instead of refusing.
kubectl -n "$NS" port-forward --request-timeout=0 --address 127.0.0.1 svc/${P}-registry "${PUSH_PORT}:5000" >"$SCRATCH/pf.log" 2>&1 &
PF_PID=$!
PF_UP=0
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${PUSH_PORT}/v2/" >/dev/null 2>&1; then PF_UP=1; break; fi
  sleep 1
done
[ "$PF_UP" = 1 ] || { echo "[case-112] the registry port-forward never came up; NOTHING WAS VERIFIED"; cat "$SCRATCH/pf.log"; exit 2; }
curl -sfi -X POST "http://127.0.0.1:${PUSH_PORT}/v2/${P}-fixture/blobs/uploads/" | head -3
# The fixture is generated and pushed in pure python through the port-forward: a docker push would
# run in the Docker Desktop daemon's VM, whose 127.0.0.1 is not the runner's. One gzip layer, one
# config, one manifest, each addressed by its own sha256; the manifest digest is what the artifact
# pins. Architecture matches the node's, so the platform check on pull is exact.
ARCH="$(kubectl get node "$NODE" -o jsonpath='{.status.nodeInfo.architecture}')"
DIGEST="$(python3 - "$SCRATCH/marker.txt" "127.0.0.1:${PUSH_PORT}" "$ARCH" <<'PY'
import gzip, hashlib, io, json, sys, tarfile, urllib.request

# A macOS runner's urllib reads the system proxy settings, which cannot reach the runner's own
# loopback; the registry is loopback-only, so no proxy is ever wanted here.
urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))

marker_path, registry, arch = sys.argv[1], sys.argv[2], sys.argv[3]
marker = open(marker_path, "rb").read()

buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w", format=tarfile.USTAR_FORMAT) as tf:
    info = tarfile.TarInfo("marker.txt")
    info.size, info.mtime, info.uid, info.gid, info.mode = len(marker), 0, 0, 0, 0o644
    tf.addfile(info, io.BytesIO(marker))
uncompressed = buf.getvalue()
compressed = gzip.compress(uncompressed, mtime=0)
diff_id = "sha256:" + hashlib.sha256(uncompressed).hexdigest()
layer_digest = "sha256:" + hashlib.sha256(compressed).hexdigest()

config = json.dumps({
    "architecture": arch, "os": "linux",
    "config": {},
    "rootfs": {"type": "layers", "diff_ids": [diff_id]},
}).encode()
config_digest = "sha256:" + hashlib.sha256(config).hexdigest()

manifest = json.dumps({
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "size": len(config), "digest": config_digest},
    "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "size": len(compressed), "digest": layer_digest}],
}).encode()
manifest_digest = "sha256:" + hashlib.sha256(manifest).hexdigest()
repo = "c112-fixture"

def put_blob(blob, digest):
    req = urllib.request.Request(f"http://{registry}/v2/{repo}/blobs/uploads/", method="POST")
    with urllib.request.urlopen(req) as r:
        location = r.headers["Location"]
    # registry:2 echoes the Host header, so the Location can already be absolute.
    if not location.startswith("http"):
        location = f"http://{registry}{location}"
    sep = "&" if "?" in location else "?"
    put = urllib.request.Request(f"{location}{sep}digest={digest}", data=blob, method="PUT",
                                 headers={"Content-Type": "application/octet-stream"})
    urllib.request.urlopen(put)

put_blob(config, config_digest)
put_blob(compressed, layer_digest)
req = urllib.request.Request(f"http://{registry}/v2/{repo}/manifests/v1", data=manifest, method="PUT",
                             headers={"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
urllib.request.urlopen(req)
print(manifest_digest)
PY
)"
kill "$PF_PID" 2>/dev/null; PF_PID=""
[ -n "$DIGEST" ] || { echo "[case-112] could not pin the fixture digest; NOTHING WAS VERIFIED"; exit 2; }
REFERENCE="localhost:${REG_PORT}/${P}-fixture@${DIGEST}"
echo "[case-112] fixture digest: $DIGEST (node pulls localhost:${REG_PORT} over the loopback forwarder)"

echo "[case-112] admission: a tag-only reference is refused"
MSG="$(kubectl -n "$NS" create --dry-run=server -f - 2>&1 >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-tagged, namespace: $NS}
spec:
  source: {image: {reference: "localhost:${REG_PORT}/${P}-fixture:v1"}}
EOF
)"
if printf '%s' "$MSG" | grep -q "must pin the image to its digest"; then
  record PASS "tag-refused" "the refusal names the digest contract"
else
  record FAIL "tag-refused" "a tag-only reference was not refused with the digest contract (got: ${MSG:0:200})"
fi

echo "[case-112] artifact: the image source resolves claim-shaped"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata: {name: ${P}-weights, namespace: $NS, labels: {e2e.gpustack.ai/case: "112"}}
spec:
  source: {image: {reference: "$REFERENCE"}}
EOF
RESOLVED=""
for _ in $(seq 1 12); do
  RESOLVED="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai ${P}-weights \
    -o jsonpath="{.status.conditions[?(@.type=='Resolved')].status}" 2>/dev/null)"
  [ "$RESOLVED" = "True" ] && break
  sleep 5
done
if [ "$RESOLVED" = "True" ]; then
  record PASS "resolved" "the image artifact resolved on its first pass"
else
  record FAIL "resolved" "the image artifact never reached Resolved=True"
fi
DIGEST_ECHO="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.resolved.manifestDigest}' 2>/dev/null)"
REVISION_ECHO="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.resolved.revision}' 2>/dev/null)"
if [ -z "$DIGEST_ECHO" ] && [ -z "$REVISION_ECHO" ]; then
  record PASS "claim-shaped" "resolved carries neither a manifest digest nor a revision"
else
  record FAIL "claim-shaped" "resolved echoes digest='$DIGEST_ECHO' revision='$REVISION_ECHO', want both absent"
fi
NODES_ECHO="$(kubectl -n "$NS" get modelartifacts.worker.gpustack.ai ${P}-weights -o jsonpath='{.status.nodes}' 2>/dev/null)"
if [ -z "$NODES_ECHO" ]; then
  record PASS "no-nodes" "status.nodes never appeared: the node cache plays no part"
else
  record FAIL "no-nodes" "status.nodes appeared ($NODES_ECHO)"
fi

echo "[case-112] instance: the operator renders the image volume and the pod reads the weights"
kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: {name: ${P}-inst, namespace: $NS, labels: {e2e.gpustack.ai/case: "112"}}
spec:
  type: $IT
  nodeName: $NODE
  image: busybox:1.36
  command: ["sleep", "86400"]
  volume: {ephemeral: {capacity: 1Gi}}
  additionalVolumes:
    - mountPath: /weights
      model: {artifactRef: {name: ${P}-weights}}
EOF
POD="${P}-inst"
for _ in $(seq 1 60); do
  [ "$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] && break
  sleep 5
done
if [ -z "$POD" ]; then
  record FAIL "mount" "no Instance pod reached Running; the weights never mounted"
else
  GOT="$(kubectl -n "$NS" exec "$POD" -- sh -c "sha256sum /weights/marker.txt" 2>/dev/null | awk '{print $1}')"
  if [ "$GOT" = "$EXPECT" ]; then
    record PASS "mount" "pod $POD reads the image's marker file with the fixture's sha256"
  else
    record FAIL "mount" "the marker sha256 read back '$GOT', want $EXPECT"
  fi
  MOUNT_RO="$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.containers[0].volumeMounts[?(@.mountPath=="/weights")].readOnly}' 2>/dev/null)"
  VOL_IMAGE="$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.volumes[*].image.reference}' 2>/dev/null)"
  if [ "$MOUNT_RO" = "true" ] && [ "$VOL_IMAGE" = "$REFERENCE" ]; then
    record PASS "volume-shape" "the pod's weights are one image volume, read-only, the pinned reference"
  else
    record FAIL "volume-shape" "volumeMount readOnly='$MOUNT_RO', volume image reference='$VOL_IMAGE'"
  fi
fi

echo "[case-112] prefetch: an image artifact is refused"
if kubectl -n "$NS" apply -f - >/dev/null 2>&1 <<EOF
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelPrefetch
metadata: {name: ${P}-warm, namespace: $NS}
spec:
  artifactRef: {name: ${P}-weights}
  bindingRef: {name: nonexistent}
EOF
then
  record FAIL "prefetch-refused" "a ModelPrefetch naming an image artifact was admitted"
else
  record PASS "prefetch-refused" "the prefetch was refused: nothing to warm"
fi

echo
echo "[case-112] rows"
for r in "${ROWS[@]}"; do echo "  $r"; done
[ "$FAILS" -eq 0 ] || exit 1
