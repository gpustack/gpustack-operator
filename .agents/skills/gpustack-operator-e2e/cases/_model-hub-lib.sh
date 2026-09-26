# shellcheck shell=bash
#
# Sourced by the node-delivery cases (101, 102, 103): the test model hub and forward proxy of
# _model-hub.py, Settings, consumer Pods and the plugin's readings. Not a case; never run it directly.
#
# Every function takes the namespace it acts in explicitly. The hub and the proxy run the stock
# MH_PYTHON image with the script mounted from a ConfigMap, so nothing is built for them.

MH_PYTHON="${E2E_MH_PYTHON_IMAGE:-python:3.12-alpine}"
MH_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"

# mh_deploy NS NAME REPOS_JSON [TLS_SECRET]: a hub named NAME serving REPOS_JSON, over HTTPS with
# the certificate in TLS_SECRET (keys tls.crt, tls.key) when given. Waits until it answers. Each
# deploy seeds new content, so no digest of an earlier run is already on a node.
mh_deploy() {
  local ns="$1" name="$2" repos="$3" tls="${4:-}" port=8080 scheme=http tlsenv="" tlsvol="" tlsmount=""
  if [ -n "$tls" ]; then
    port=8443 scheme=https
    tlsenv="
            - {name: TLS_CERT, value: /tls/tls.crt}
            - {name: TLS_KEY, value: /tls/tls.key}"
    tlsmount="
            - {name: tls, mountPath: /tls, readOnly: true}"
    tlsvol="
        - {name: tls, secret: {secretName: ${tls}}}"
  fi
  kubectl -n "$ns" create configmap "${name}-script" --from-file=hub.py="${MH_LIB_DIR}/_model-hub.py" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${name}, namespace: ${ns}, labels: {e2e.gpustack.ai/model-hub: "true"}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${name}}}
  template:
    metadata: {labels: {app: ${name}}}
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: hub
          image: ${MH_PYTHON}
          command: ["python3", "/script/hub.py", "hub"]
          env:
            - {name: PORT, value: "${port}"}
            - {name: REPOS, value: '${repos}'}
            - {name: SEED, value: "$(date +%s)-${RANDOM}"}${tlsenv}
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
          readinessProbe: {tcpSocket: {port: ${port}}, periodSeconds: 2}
          volumeMounts:
            - {name: script, mountPath: /script, readOnly: true}${tlsmount}
      volumes:
        - {name: script, configMap: {name: ${name}-script}}${tlsvol}
---
apiVersion: v1
kind: Service
metadata: {name: ${name}, namespace: ${ns}}
spec:
  selector: {app: ${name}}
  ports: [{port: ${port}, targetPort: ${port}}]
YAML
  kubectl -n "$ns" rollout status "deploy/${name}" --timeout=180s >/dev/null
  printf '%s://%s.%s.svc:%s' "$scheme" "$name" "$ns" "$port"
}

# mh_proxy NS NAME: a forward proxy named NAME on port 3128.
mh_proxy() {
  local ns="$1" name="$2"
  kubectl -n "$ns" create configmap "${name}-script" --from-file=hub.py="${MH_LIB_DIR}/_model-hub.py" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${name}, namespace: ${ns}, labels: {e2e.gpustack.ai/model-hub: "true"}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${name}}}
  template:
    metadata: {labels: {app: ${name}}}
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: proxy
          image: ${MH_PYTHON}
          command: ["python3", "/script/hub.py", "proxy"]
          env: [{name: PORT, value: "3128"}]
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
          readinessProbe: {tcpSocket: {port: 3128}, periodSeconds: 2}
          volumeMounts: [{name: script, mountPath: /script, readOnly: true}]
      volumes: [{name: script, configMap: {name: ${name}-script}}]
---
apiVersion: v1
kind: Service
metadata: {name: ${name}, namespace: ${ns}}
spec:
  selector: {app: ${name}}
  ports: [{port: 3128, targetPort: 3128}]
YAML
  kubectl -n "$ns" rollout status "deploy/${name}" --timeout=180s >/dev/null
  printf 'http://%s.%s.svc:3128' "$name" "$ns"
}

# mh_call NS NAME METHOD PATH: a request to the hub (or proxy) from inside its own Pod, printing the
# body. PATH is local to it, e.g. /_log or /_control?corrupt=t/a.
mh_call() {
  local ns="$1" name="$2" method="$3" path="$4" scheme=http port
  port="$(kubectl -n "$ns" get svc "$name" -o jsonpath='{.spec.ports[0].port}')"
  [ "$port" = 8443 ] && scheme=https
  kubectl -n "$ns" exec "deploy/${name}" -- python3 -c "
import ssl, urllib.request
ctx = ssl._create_unverified_context()
req = urllib.request.Request('${scheme}://127.0.0.1:${port}${path}', method='${method}')
print(urllib.request.urlopen(req, context=ctx).read().decode())" 2>/dev/null
}

# mh_log NS NAME: the hub's (or proxy's) request record, one JSON object a line.
mh_log() { mh_call "$1" "$2" GET /_log; }

# mh_pod_ip NS NAME: the IP of the Pod behind deployment NAME.
mh_pod_ip() { kubectl -n "$1" get pods -l "app=$2" -o jsonpath='{.items[0].status.podIP}'; }

# mh_manifest_sha NS NAME REPO PATH: the SHA-256 of a file as the hub serves it.
mh_manifest_sha() {
  mh_call "$1" "$2" GET /_manifest | python3 -c "import json,sys; print(json.load(sys.stdin)['$3']['files']['$4']['sha256'])"
}

# setting_get KEY / setting_set KEY VALUE: a Setting in the store, bypassing admission the way a
# seed from the environment does. setting_set prints the previous value.
setting_get() {
  kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.$1}" 2>/dev/null | base64 -d 2>/dev/null
}
setting_set() {
  local b64
  b64="$(printf '%s' "$2" | base64 | tr -d '\n')"
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"$1\":\"${b64}\"}}" >/dev/null
}

# setting_unset KEY: removes KEY from the store, so the Setting reads its default again.
setting_unset() {
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge -p "{\"data\":{\"$1\":null}}" >/dev/null
}

# settings_settle: waits out the worker's thirty-second Settings read cache, so its next read of a
# Setting written through the store sees the new value. The plugin needs no wait: it reads its
# NodeModelStore's spec, which the worker rewrites on the Secret's change.
settings_settle() { sleep 35; }

# artifact NS NAME REPOSITORY [SECRET] [LABEL] [SPEC_YAML]: a Hugging Face ModelArtifact at main,
# with SPEC_YAML (such as allowPatterns) appended to its spec.
artifact() {
  local secret=""
  [ -n "${4:-}" ] && secret="
      secretRef: {name: $4}"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelArtifact
metadata:
  name: $2
  namespace: $1
  labels: {e2e.gpustack.ai/case: "${5:-model-hub}"}
spec:
  source:
    huggingFace:
      repository: $3
      revision: main${secret}${6:-}
YAML
}

# wait_resolved NS NAME BOUND: waits for Resolved=True, printing the digest ("" on timeout).
wait_resolved() {
  local digest=""
  for _ in $(seq 1 $(( $3 / 2 ))); do
    if [ "$(kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" \
      -o jsonpath="{.status.conditions[?(@.type=='Resolved')].status}" 2>/dev/null)" = True ]; then
      digest="$(kubectl -n "$1" get modelartifacts.worker.gpustack.ai "$2" -o jsonpath='{.status.resolved.manifestDigest}')"
      break
    fi
    sleep 2
  done
  printf '%s' "$digest"
}

# consumer NS POD NODE ARTIFACT UID DIGEST [SECRET] [EXTRA_ATTRS_YAML]: a bare Pod pinned to NODE
# mounting the artifact through the plugin, restricted-compliant, that lists the SHA-256 of every
# file it can read and then sleeps. EXTRA_ATTRS_YAML is appended to its volumeAttributes.
consumer() {
  local secret=""
  [ -n "${7:-}" ] && secret="
        nodePublishSecretRef: {name: $7}"
  kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1, labels: {e2e.gpustack.ai/consumer: "true"}}
spec:
  nodeName: $3
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 65534, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: c
      image: ${MH_PYTHON}
      command: ["python3", "-c", "import hashlib,os,time\nfor r,_,fs in os.walk('/model'):\n  for f in sorted(fs):\n    p=os.path.join(r,f); print(hashlib.sha256(open(p,'rb').read()).hexdigest(), os.path.relpath(p,'/model'), flush=True)\nwhile True: time.sleep(3600)"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: model, mountPath: /model, readOnly: true}]
  volumes:
    - name: model
      csi:
        driver: model.csi.gpustack.ai
        readOnly: true
        volumeAttributes:
          artifact: "$4"
          artifactUID: "$5"
          manifestDigest: "$6"${8:-}${secret}
YAML
}

# pod_ready NS POD BOUND: waits for the Pod to run, returning 0 once it does.
pod_ready() {
  for _ in $(seq 1 $(( $3 / 2 ))); do
    [ "$(kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && return 0
    sleep 2
  done
  return 1
}

# pod_mount_events NS POD: the messages of the Pod's FailedMount events.
pod_mount_events() {
  kubectl -n "$1" get events --field-selector "involvedObject.name=$2,reason=FailedMount" \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null
}

# plugin_pod NODE: the model-manager Pod on NODE.
plugin_pod() {
  kubectl -n "$SYSTEM_NS" get pods -l app.kubernetes.io/component=model-manager \
    --field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# plugin_metric NODE METRIC: the plugin's metric on NODE, summed over its labels.
plugin_metric() {
  local pod
  pod="$(plugin_pod "$1")"
  kubectl get --raw "/api/v1/namespaces/${SYSTEM_NS}/pods/https:${pod}:32444/proxy/metrics" 2>/dev/null \
    | awk -v m="$2" '$1 ~ "^"m"({|$)" {s += $2} END {printf "%d", s}'
}

# store_state NODE DIGEST: the node's NodeModelStore entry for DIGEST, "state|reason".
store_state() {
  kubectl get nodemodelstores.worker.gpustack.ai "$1" \
    -o jsonpath="{range .status.models[?(@.digest=='$2')]}{.state}|{.reason}{end}" 2>/dev/null
}

# model_workers: the nodes a consumer may land on, the kind workers.
model_workers() {
  kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}'
}

# nms_watch NODE OUT: records every event of NODE's NodeModelStore into OUT, one JSON line each (the
# wall time, the event type, the resourceVersion and the status), until it is killed; prints its PID.
# It reads the raw watch, so a write is counted once however the object is read afterwards.
nms_watch() {
  python3 - "$1" "$2" >/dev/null 2>&1 <<'PY' &
import json, subprocess, sys, time
node, out = sys.argv[1], sys.argv[2]
rv = ""
with open(out, "a") as f:
    while True:
        path = f"/apis/worker.gpustack.ai/v1alpha1/nodemodelstores?watch=1&fieldSelector=metadata.name={node}"
        if rv:
            path += f"&resourceVersion={rv}"
        p = subprocess.Popen(["kubectl", "get", "--raw", path], stdout=subprocess.PIPE, text=True)
        for line in p.stdout:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            if ev.get("type") == "ERROR":
                rv = ""
                break
            o = ev.get("object", {})
            rv = o.get("metadata", {}).get("resourceVersion", rv)
            f.write(json.dumps({"t": time.time(), "type": ev.get("type"), "rv": rv, "status": o.get("status", {})}) + "\n")
            f.flush()
        p.wait()
        time.sleep(1)
PY
  echo $!
}

# nms_write_stats OUT DIGEST: the writes OUT recorded, as "progress=N mingap=S intermediate=K last=T":
# N writes that changed only progress fields (an entry's downloadedBytes, capacity.storedBytes), the
# least gap in seconds between two of them, K distinct downloadedBytes values of DIGEST strictly
# between 0 and its size, and T the wall time of the last write.
nms_write_stats() {
  python3 - "$1" "$2" <<'PY'
import json, sys
out, digest = sys.argv[1], sys.argv[2]
evs = [json.loads(l) for l in open(out) if l.strip()]
writes = [e for e in evs if e["type"] == "MODIFIED"]
def held(st, old):
    st = json.loads(json.dumps(st))
    written = {m["digest"]: m.get("downloadedBytes", 0) for m in old.get("models", [])}
    for m in st.get("models", []):
        if m["digest"] in written:
            m["downloadedBytes"] = written[m["digest"]]
        elif "downloadedBytes" in m:
            m["downloadedBytes"] = 0
    if st.get("capacity") and old.get("capacity"):
        st["capacity"]["storedBytes"] = old["capacity"]["storedBytes"]
    return st
progress, times, seen = 0, [], set()
for a, b in zip(evs, evs[1:]):
    if b["type"] != "MODIFIED":
        continue
    if held(b["status"], a["status"]) == held(a["status"], a["status"]):
        progress += 1
        times.append(b["t"])
for e in writes:
    for m in e["status"].get("models", []):
        if m["digest"] == digest and 0 < m.get("downloadedBytes", 0) < m.get("sizeBytes", 0):
            seen.add(m["downloadedBytes"])
gaps = [y - x for x, y in zip(times, times[1:])]
print(f"progress={progress} mingap={int(min(gaps)) if gaps else -1} intermediate={len(seen)} last={int(writes[-1]['t']) if writes else 0}")
PY
}

# progress_as NS NAME SUBJECT: the artifact's v1 progress read as SUBJECT (impersonated), or the
# error kubectl prints; the exit status is kubectl's.
progress_as() {
  kubectl get --raw "/apis/worker.gpustack.ai/v1/namespaces/$1/modelartifacts/$2/progress" --as="$3" 2>&1
}
