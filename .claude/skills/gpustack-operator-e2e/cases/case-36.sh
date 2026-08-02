#!/usr/bin/env bash
#
# CASE 36 — Node-pinned Instance with additional volumes, and the host-access gates
#   (MUTATING, self-recovering; the SSH sub-check AUTO-SKIPS without the ssh client tools)
#
#   case-36.sh <NS>
#
# Goal:        An Instance pinned to a named node lands there through a node SELECTOR rather than a
#              direct pod assignment, and still passes through Kueue; the volumes it asks for beside
#              its workspace are mounted into the workload container with readOnly and subPath
#              honored; and the two host-boundary escapes — privileged mode and a hostPath volume —
#              are refused at creation while both Settings are off, yet never block an existing
#              Instance from being updated, edited while stopped, or restarted.
# Environment: Any cluster with a materialized general pool and a default StorageClass (the
#              persistent mount is provisioned through one). No GPU. At least two schedulable managed
#              nodes make the pin meaningful; with one the "pinned rather than merely available" node
#              sub-check records SKIP instead of a vacuous PASS. The SSH sub-check needs ssh and
#              ssh-keygen on the runner and SKIPs without them.
# Inputs:      All real, nothing mocked — sets the general InstanceType unit spec; creates a ConfigMap
#              with two keys, a Secret, and an InstancePersistentVolume; then one Instance pinned to a
#              chosen node carrying four additional volumes (persistent, configMap+subPath, secret,
#              hostPath read-only). Flips instance-privileged-allowed and
#              instance-host-path-volume-allowed to exercise the gates.
# Expected:    - the Pod carries exactly one nodeSelector entry, kubernetes.io/hostname=<node's own
#                hostname label>, and runs on the pinned node;
#              - its Kueue Workload reaches Admitted=True (the pin does not bypass Kueue);
#              - all four volumes are mounted in `main` at their paths, none of them in `sshd`, whose
#                spec is untouched by the feature;
#              - inside the container the persistent mount is writable, the subPath mount exposes only
#                the named key, the secret key is present, and a write to the read-only hostPath mount
#                is refused;
#              - a mount is visible over SSH without a sidecar change;
#              - with both Settings off, creating a privileged Instance and creating a hostPath
#                Instance are both rejected; allowing only instance-host-path-volume-allowed admits
#                the hostPath one and still rejects the privileged one;
#              - an Instance created while a gate was on still stops, edits and restarts after the
#                gate is turned back off;
#              - pinning to a node that does not exist is rejected at creation.
# Cleanup:     Trap kills the port-forward, deletes the test Instances, the InstancePersistentVolume,
#              the ConfigMap, the Secret and the SSH key Secret, and restores both Settings to the
#              values found at entry. Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

NS="${1:?usage: case-36.sh <NS>}"
INST=gpustack-e2e-pinned
INST_GATED=gpustack-e2e-gated
INST_PLAIN=gpustack-e2e-plain      # never holds an escape; the escalation-by-patch probe
CM=gpustack-e2e-case36-cm
SEC=gpustack-e2e-case36-secret
IPV=gpustack-e2e-case36-data
SSH_SECRET=gpustack-e2e-case36-key
PRIV_KEY=instance-privileged-allowed
HOST_KEY=instance-host-path-volume-allowed
HOST_DIR=/tmp/gpustack-e2e-case36
LOCAL_PORT=22036
KEYDIR="$(mktemp -d)"

IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }

# --- Setting helpers. The webhook resolves a Setting through a 30s cache, so a flip is not visible
#     immediately; every gate assertion below RETRIES until the expected verdict appears rather than
#     sleeping a fixed amount. ---
set_gate() { local b64; b64=$(printf '%s' "$2" | base64 | tr -d '\n')
  kubectl -n "$NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"$1\":\"${b64}\"}}" >/dev/null 2>&1; }
get_gate() { local v; v=$(kubectl -n "$NS" get secret gpustack-settings \
    -o jsonpath="{.data.$1}" 2>/dev/null | base64 -d 2>/dev/null); [ "$v" = "true" ] && echo true || echo false; }

ORIG_PRIV=$(get_gate "$PRIV_KEY")
ORIG_HOST=$(get_gate "$HOST_KEY")

PF_PID=""
restore() {
  echo
  echo "[case-36] cleanup: deleting test objects and restoring both gates"
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null
  kubectl -n default delete instance "$INST" "$INST_GATED" "$INST_PLAIN" --ignore-not-found --wait=false 2>/dev/null || true
  kubectl -n default delete instancepersistentvolume "$IPV" --ignore-not-found --wait=false 2>/dev/null || true
  kubectl -n default delete configmap "$CM" --ignore-not-found 2>/dev/null || true
  kubectl -n default delete secret "$SEC" "$SSH_SECRET" --ignore-not-found 2>/dev/null || true
  set_gate "$PRIV_KEY" "$ORIG_PRIV"
  set_gate "$HOST_KEY" "$ORIG_HOST"
  rm -rf "$KEYDIR" 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
print_and_exit() {
  echo
  echo "== CASE 36 — Node-pinned Instance with additional volumes, and the host-access gates =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
  if [ "$FAILS" -ne 0 ]; then
    echo
    echo "FAILED ${FAILS} check(s)."
    echo "Diagnose: kubectl -n default describe instance ${INST}; kubectl -n default get pod ${INST} -o yaml"
    exit 1
  fi
  echo "CASE 36 PASS"
  exit 0
}

# The general InstanceType carries no unit spec by default; the Instance webhook needs one to size
# the Pod. Set it and confirm it stuck (the validating webhook may be briefly unready after deploy).
unit_ram=""
for _ in $(seq 1 15); do
  kubectl patch instancetypes.worker.gpustack.ai "$IT" --type=merge \
    -p '{"spec":{"unitResources":{"cpu":"1","ram":"2Gi"},"localStorage":"10Gi"}}' >/dev/null 2>&1
  unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
  [ -n "$unit_ram" ] && break
  sleep 3
done
[ -n "$unit_ram" ] || { echo "no unit spec on ${IT} (validating webhook not ready?)"; exit 1; }

# --- Pick the node to pin to. Prefer the LAST schedulable managed node so the pin is unlikely to
#     coincide with the scheduler's own first choice; count them to decide whether the "pinned rather
#     than merely available" sub-check can say anything. ---
# A jsonpath filter on spec.unschedulable cannot express "absent or false" reliably, so cordoned
# nodes are dropped one by one instead — an empty selection here would silently look like an empty
# cluster.
NODES=""
for _n in $(kubectl get nodes -l gpustack.ai/managed=true \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  [ "$(kubectl get node "$_n" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)" = "true" ] && continue
  NODES="${NODES}${_n}
"
done
NODE_COUNT=$(printf '%s' "$NODES" | grep -c .)
PIN_NODE=$(printf '%s' "$NODES" | grep . | tail -1)
[ -n "$PIN_NODE" ] || { echo "no schedulable managed node found — run case-1 first"; exit 1; }
PIN_HOSTNAME=$(kubectl get node "$PIN_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}' 2>/dev/null)
[ -n "$PIN_HOSTNAME" ] || { echo "node ${PIN_NODE} carries no kubernetes.io/hostname label"; exit 1; }
echo "[case-36] pinning to node ${PIN_NODE} (hostname=${PIN_HOSTNAME}); ${NODE_COUNT} schedulable managed node(s)"

# --- Fixtures for the four volume sources. ---
kubectl -n default delete configmap "$CM" --ignore-not-found >/dev/null 2>&1
kubectl -n default create configmap "$CM" --from-literal=alpha=ALPHA_VALUE --from-literal=beta=BETA_VALUE >/dev/null
kubectl -n default delete secret "$SEC" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SEC" --from-literal=token=SECRET_VALUE >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1
kind: InstancePersistentVolume
metadata: { name: ${IPV}, namespace: default }
spec:
  capacity: 1Gi
  accessMode: ReadWriteOnce
EOF

HAS_SSH=1
for _tool in ssh ssh-keygen; do
  command -v "$_tool" >/dev/null 2>&1 || HAS_SSH=""
done
if [ -n "$HAS_SSH" ]; then
  ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
  kubectl -n default delete secret "$SSH_SECRET" --ignore-not-found >/dev/null 2>&1
  kubectl -n default create secret generic "$SSH_SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
fi

# --- 1. The pinned Instance with four additional volumes. The hostPath one needs its gate on for the
#     CREATE, so turn that gate on here and assert its OFF behavior later with a separate Instance. ---
set_gate "$HOST_KEY" true
echo "[case-36] creating pinned Instance ${INST} with four additional volumes"
created=""
for _ in $(seq 1 20); do
  if cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  nodeName: ${PIN_NODE}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
$([ -n "$HAS_SSH" ] && echo "  sshPublicKey: { name: ${SSH_SECRET} }")
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
  additionalVolumes:
    - mountPath: /mnt/data
      persistent: { name: ${IPV} }
    - mountPath: /mnt/cm/alpha
      subPath: alpha
      configMap: { name: ${CM} }
    - mountPath: /mnt/secret
      secret: { name: ${SEC} }
    - mountPath: /mnt/host
      readOnly: true
      hostPath: { path: ${HOST_DIR}, type: DirectoryOrCreate }
EOF
  then created=1; break; fi
  sleep 3
done
[ -n "$created" ] || { record FAIL "pinned Instance with four additional volumes created" \
  "create rejected — is instance-host-path-volume-allowed visible yet?"; print_and_exit; }
record PASS "pinned Instance with four additional volumes created" "${INST} accepted on ${IT}"

# --- 2. The pin survives Kueue admission as the ONE selector entry the operator contributed.
#
#     Kueue MERGES the admitted flavor's own nodeLabels into the Pod's nodeSelector rather than
#     overwriting it, so the admitted Pod legitimately carries more than one entry — asserting a
#     single entry would pass only while read before the admission patch lands, which is a race, not
#     a contract. The timing-independent statement is: exactly one kubernetes.io/hostname key, equal
#     to the pinned node's own hostname label, and every OTHER key accounted for by a flavor's
#     nodeLabels, so nothing unexplained was injected and the merge did not clobber the pin. ---
POD="$INST"
sel_pairs=""
for _ in $(seq 1 30); do
  sel_pairs=$(kubectl -n default get pod "$POD" \
    -o go-template='{{range $k, $v := .spec.nodeSelector}}{{$k}}={{$v}}{{"\n"}}{{end}}' 2>/dev/null | grep .)
  [ -n "$sel_pairs" ] && break
  sleep 3
done
flavor_pairs=$(kubectl get resourceflavor \
  -o go-template='{{range .items}}{{range $k, $v := .spec.nodeLabels}}{{$k}}={{$v}}{{"\n"}}{{end}}{{end}}' 2>/dev/null | grep .)
host_pairs=$(printf '%s\n' "$sel_pairs" | grep '^kubernetes\.io/hostname=')
unexplained=""
while IFS= read -r p; do
  [ -z "$p" ] && continue
  case "$p" in kubernetes.io/hostname=*) continue ;; esac
  printf '%s\n' "$flavor_pairs" | grep -qxF "$p" || unexplained="${unexplained}${p} "
done <<EOF
$sel_pairs
EOF
if [ "$host_pairs" = "kubernetes.io/hostname=${PIN_HOSTNAME}" ] && [ -z "$unexplained" ]; then
  record PASS "pin survives admission as the one operator-set selector" \
    "kubernetes.io/hostname=${PIN_HOSTNAME}; every other entry is a flavor nodeLabel"
else
  record FAIL "pin survives admission as the one operator-set selector" \
    "hostname entry '${host_pairs:-<none>}' (want kubernetes.io/hostname=${PIN_HOSTNAME}); unexplained: '${unexplained:-none}'"
fi

# --- 3. The Pod runs on the pinned node. With more than one candidate this also says the pin CHOSE
#     the node; with a single candidate it would be true regardless, so that reading records SKIP. ---
ran_on=""
for _ in $(seq 1 40); do
  ran_on=$(kubectl -n default get pod "$POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  [ -n "$ran_on" ] && break
  sleep 5
done
[ "$ran_on" = "$PIN_NODE" ] && record PASS "Pod lands on the pinned node" "spec.nodeName=${ran_on}" \
  || record FAIL "Pod lands on the pinned node" "expected ${PIN_NODE}, got '${ran_on:-<unscheduled>}'"
if [ "${NODE_COUNT:-0}" -ge 2 ]; then
  [ "$ran_on" = "$PIN_NODE" ] \
    && record PASS "the pin chose the node" "${NODE_COUNT} candidates, landed on the pinned one" \
    || record FAIL "the pin chose the node" "${NODE_COUNT} candidates, landed on '${ran_on}'"
else
  record SKIP "the pin chose the node" "only ${NODE_COUNT} schedulable managed node — landing there proves nothing"
fi

# --- 4. Kueue still gates the pinned workload. ---
admitted=""
for _ in $(seq 1 30); do
  # Kueue's pod integration owns the Workload by the Pod, which is what ties a Workload to THIS
  # Instance — the podSet template carries no name to match on.
  a=$(kubectl -n default get workloads.kueue.x-k8s.io \
        -o jsonpath='{range .items[*]}{.metadata.ownerReferences[0].name}{"|"}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
      | grep -m1 "^${POD}|True$")
  [ -n "$a" ] && { admitted=1; break; }
  sleep 3
done
[ -n "$admitted" ] && record PASS "pinned workload admitted by Kueue" "Admitted=True (the pin does not bypass Kueue)" \
  || record FAIL "pinned workload admitted by Kueue" "no Admitted Workload for ${POD} — a pin must not bypass admission"

# --- 5. Wait for the Pod to be Running before reading anything from inside it. ---
running=""
want_ready="true true "
[ -z "$HAS_SSH" ] && want_ready="true "
for _ in $(seq 1 60); do
  phase=$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  readies=$(kubectl -n default get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$readies" = "$want_ready" ]; then running=1; break; fi
  sleep 5
done
[ -n "$running" ] || { record FAIL "pinned Instance reaches Running" "phase='${phase:-<none>}' readies='${readies:-<none>}'"; print_and_exit; }
record PASS "pinned Instance reaches Running" "${POD} Running on ${ran_on}"

# --- 6. The four mounts are on `main`, at their paths, with readOnly and subPath as asked. ---
mounts=$(kubectl -n default get pod "$POD" \
  -o jsonpath='{range .spec.containers[?(@.name=="main")].volumeMounts[*]}{.name}:{.mountPath}:{.readOnly}:{.subPath}{"\n"}{end}' 2>/dev/null)
expect_mounts="additional-0:/mnt/data::
additional-1:/mnt/cm/alpha::alpha
additional-2:/mnt/secret::
additional-3:/mnt/host:true:"
got_mounts=$(printf '%s\n' "$mounts" | grep '^additional-')
if [ "$got_mounts" = "$expect_mounts" ]; then
  record PASS "four additional mounts on main" "additional-0..3 at their paths, readOnly/subPath honored"
else
  record FAIL "four additional mounts on main" "unexpected mounts: $(printf '%s' "$got_mounts" | tr '\n' ' ')"
fi

# The workspace mount must still be first and untouched by the feature.
ws=$(printf '%s\n' "$mounts" | grep -m1 '^workspace:')
[ "$ws" = "workspace:/workspace::" ] && record PASS "workspace mount unchanged" "$ws" \
  || record FAIL "workspace mount unchanged" "got '${ws:-<missing>}'"

# --- 7. Each Pod volume carries the source the Instance asked for. ---
vols=$(kubectl -n default get pod "$POD" -o go-template='{{range .spec.volumes}}{{.name}}={{if .persistentVolumeClaim}}pvc:{{.persistentVolumeClaim.claimName}}{{else if .configMap}}cm:{{.configMap.name}}{{else if .secret}}secret:{{.secret.secretName}}{{else if .hostPath}}host:{{.hostPath.path}}{{else}}other{{end}} {{end}}' 2>/dev/null)
for want in "additional-0=pvc:${IPV}" "additional-1=cm:${CM}" "additional-2=secret:${SEC}" "additional-3=host:${HOST_DIR}"; do
  case "$vols" in
    *"$want"*) record PASS "volume source rendered" "$want" ;;
    *)         record FAIL "volume source rendered" "missing ${want} — volumes: ${vols}" ;;
  esac
done

# --- 8. The sshd sidecar carries NONE of them: it enters main's mount namespace per session, so the
#     feature must not have to touch the sidecar at all. ---
if [ -n "$HAS_SSH" ]; then
  sshd_extra=$(kubectl -n default get pod "$POD" \
    -o jsonpath='{range .spec.containers[?(@.name=="sshd")].volumeMounts[*]}{.name}{"\n"}{end}' 2>/dev/null | grep -c '^additional-')
  [ "${sshd_extra:-0}" -eq 0 ] && record PASS "sidecar spec untouched" "sshd carries 0 additional-* mounts" \
    || record FAIL "sidecar spec untouched" "sshd carries ${sshd_extra} additional-* mount(s) — the sidecar should not need them"
else
  record SKIP "sidecar spec untouched" "no ssh client tools — Instance created without a sidecar"
fi

# --- 9. Runtime behavior inside the container. ---
exec_main() { kubectl -n default exec "$POD" -c main -- sh -c "$1" 2>/dev/null; }

w=$(exec_main 'touch /mnt/data/probe && echo OK')
[ "$w" = OK ] && record PASS "persistent mount is writable" "/mnt/data accepts a write" \
  || record FAIL "persistent mount is writable" "write to /mnt/data failed ('${w:-<no output>}')"

sub=$(exec_main 'cat /mnt/cm/alpha')
[ "$sub" = ALPHA_VALUE ] && record PASS "subPath exposes only the named key" "/mnt/cm/alpha == ALPHA_VALUE" \
  || record FAIL "subPath exposes only the named key" "/mnt/cm/alpha == '${sub:-<empty>}'"
beta=$(exec_main 'ls /mnt/cm 2>/dev/null | grep -c "^beta$"')
[ "${beta:-0}" -eq 0 ] && record PASS "subPath hides the other keys" "beta not present beside the mount" \
  || record FAIL "subPath hides the other keys" "beta is visible — subPath did not narrow the mount"

tok=$(exec_main 'cat /mnt/secret/token')
[ "$tok" = SECRET_VALUE ] && record PASS "secret mount readable" "/mnt/secret/token == SECRET_VALUE" \
  || record FAIL "secret mount readable" "/mnt/secret/token == '${tok:-<empty>}'"

ro=$(exec_main 'touch /mnt/host/probe >/dev/null 2>&1; echo $?')
[ -n "$ro" ] && [ "$ro" != "0" ] && record PASS "readOnly mount refuses a write" "touch /mnt/host/probe exited ${ro}" \
  || record FAIL "readOnly mount refuses a write" "touch /mnt/host/probe exited '${ro:-<no output>}' — a write to the read-only hostPath mount must fail"

# --- 10. A mount is visible over SSH, with no sidecar change. ---
if [ -n "$HAS_SSH" ]; then
  kubectl -n default port-forward "pod/$POD" "${LOCAL_PORT}:22" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 20); do (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null && { exec 3>&- 3<&-; break; }; sleep 1; done
  SSH_OPTS=(-p "$LOCAL_PORT" -i "$KEYDIR/id" -o IdentitiesOnly=yes
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15)
  over_ssh=$(ssh -T "${SSH_OPTS[@]}" root@127.0.0.1 'cat /mnt/cm/alpha' 2>/dev/null | tr -d '\r\n')
  [ "$over_ssh" = ALPHA_VALUE ] && record PASS "additional mount visible over SSH" "ssh 'cat /mnt/cm/alpha' == ALPHA_VALUE" \
    || record FAIL "additional mount visible over SSH" "got '${over_ssh:-<empty>}' — the sidecar must see main's mounts"
  kill "$PF_PID" 2>/dev/null; PF_PID=""
else
  record SKIP "additional mount visible over SSH" "no ssh client tools on the runner"
fi

# --- 11. The gates, on create. Retry each verdict: the webhook reads the Setting through a 30s cache,
#     so a flip is not visible at once, and the assertion is the STEADY-STATE verdict. ---
# gate_create <name> <privileged> <hostPath> -> "accepted" | "gated: <msg>" | "other: <msg>"
#
# The three verdicts are kept apart on purpose: an unrelated rejection — most easily the previous
# probe's object still terminating — would otherwise read as "the gate held" and PASS vacuously. Only
# a rejection naming the offending field counts as the gate.
gate_create() {
  local name="$1" priv="$2" host="$3" out
  out=$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${name}, namespace: default }
spec:
  type: ${IT}
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  privileged: ${priv}
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
$([ "$host" = true ] && printf '  additionalVolumes:\n    - mountPath: /mnt/host\n      hostPath: { path: %s, type: DirectoryOrCreate }\n' "$HOST_DIR")
EOF
)
  case "$out" in
    *created*|*configured*|*unchanged*)  echo accepted ;;
    *spec.privileged*|*spec.additionalVolumes*hostPath*|*"$PRIV_KEY"*|*"$HOST_KEY"*) echo "gated: $out" ;;
    *) echo "other: $out" ;;
  esac
}

# await_verdict <accepted|gated> <priv> <host> — retries to a STEADY state, because the webhook reads
# each Setting through a 30s cache, so the verdict right after a flip is still the old one. The delete
# WAITS: re-creating a name that is still terminating is exactly the unrelated rejection above.
await_verdict() {
  local want="$1" priv="$2" host="$3" v=""
  for _ in $(seq 1 20); do
    kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
    v=$(gate_create "$INST_GATED" "$priv" "$host")
    case "$v" in
      accepted) [ "$want" = accepted ] && break ;;
      gated:*)  [ "$want" = gated ] && break ;;
    esac
    sleep 3
  done
  kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
  echo "$v"
}

set_gate "$PRIV_KEY" false
set_gate "$HOST_KEY" false
v=$(await_verdict gated true false)
case "$v" in gated:*) record PASS "privileged create rejected while gated off" "${PRIV_KEY}=false rejects spec.privileged" ;;
             *)       record FAIL "privileged create rejected while gated off" "verdict '${v}' with ${PRIV_KEY}=false" ;; esac
v=$(await_verdict gated false true)
case "$v" in gated:*) record PASS "hostPath create rejected while gated off" "${HOST_KEY}=false rejects spec.additionalVolumes[*].hostPath" ;;
             *)       record FAIL "hostPath create rejected while gated off" "verdict '${v}' with ${HOST_KEY}=false" ;; esac

# Allowing only the hostPath gate must admit the hostPath one and still reject the privileged one —
# the point of splitting the two Settings.
set_gate "$HOST_KEY" true
v=$(await_verdict accepted false true)
[ "$v" = accepted ] && record PASS "hostPath create admitted once allowed" "${HOST_KEY}=true admits it" \
  || record FAIL "hostPath create admitted once allowed" "verdict '${v}'"
v=$(await_verdict gated true false)
case "$v" in gated:*) record PASS "each gate covers only its own field" "${HOST_KEY}=true does not allow privileged" ;;
             *)       record FAIL "each gate covers only its own field" "verdict '${v}' with only ${HOST_KEY}=true" ;; esac

# --- 12. A gate judges the escape a change TAKES, not the one it carries. An Instance that already
#     holds an escape keeps it when the gate goes off; an Instance that never held one cannot acquire
#     it by patch. ${INST} was created with the hostPath gate on; turn it off and stop/restart it. ---
set_gate "$HOST_KEY" false
sleep 35   # outlive the Setting cache, so the updates below are judged with the gate genuinely off

# A rejection here is only this contract's if it names a gate; report anything else as itself, so an
# unrelated refusal can never read as "the gate leaked into UPDATE".
gate_named() { case "$1" in *"$PRIV_KEY"*|*"$HOST_KEY"*|*privileged*not*allowed*|*host*path*not*allowed*) return 0 ;; esac; return 1; }
judge_update() {  # judge_update <check> <output>
  if gate_named "$2"; then
    record FAIL "$1" "rejected BY A GATE — an escape already held must never be re-judged: $2"
  else
    record FAIL "$1" "rejected, but not by a gate: $2"
  fi
}

out=$(kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":true}}' 2>&1)
case "$out" in *patched*) record PASS "existing Instance stops with the gate off" "spec.stop=true accepted" ;;
               *) judge_update "existing Instance stops with the gate off" "$out" ;; esac

# Starting is gated on the OBSERVED phase — "can only start stopped instance" — not on spec.stop, so
# the controller must finish tearing the Pod down first. Restarting straight after the stop patch is
# refused while the phase is still Stopping, for a reason that has nothing to do with either Setting.
phase=""
for _ in $(seq 1 40); do
  phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$phase" = Stopped ] && break
  sleep 5
done
[ "$phase" = Stopped ] && record PASS "stopping settles to Stopped" "phase=Stopped (the restart below is judged on its own terms)" \
  || record FAIL "stopping settles to Stopped" "phase='${phase:-<none>}' — a restart cannot be judged from here"

out=$(kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"volumeMount":"/workspace2"}}' 2>&1)
case "$out" in *patched*) record PASS "stopped Instance still editable with the gate off" "spec.volumeMount edited while stopped" ;;
               *) judge_update "stopped Instance still editable with the gate off" "$out" ;; esac

# What a gate compares is the whole grant, not just the node path. ${INST} holds this host path
# READ-ONLY; the same path mounted writable is more than it had, so it faces the gate like any other
# new escape. Done here, while the Instance is stopped: running, additionalVolumes is immutable, and
# that refusal also names spec.additionalVolumes — it would read as the gate holding when it is not.
# For the same reason the verdict is judged on the SETTING's name, which only the gate emits.
out=$(kubectl -n default patch instance "$INST" --type=merge -p "{\"spec\":{\"additionalVolumes\":[
  {\"mountPath\":\"/mnt/host\",\"readOnly\":true,\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}},
  {\"mountPath\":\"/mnt/host-rw\",\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}}]}}" 2>&1)
case "$out" in
  *patched*|*configured*|*unchanged*)
    record FAIL "widening a held host path faces the gate" \
      "ACCEPTED — a read-only mount was widened to writable with the gate off"
    # Put the list back, so the restart below is judged on the Instance this case built.
    kubectl -n default patch instance "$INST" --type=merge -p "{\"spec\":{\"additionalVolumes\":[
      {\"mountPath\":\"/mnt/host\",\"readOnly\":true,\"hostPath\":{\"path\":\"${HOST_DIR}\",\"type\":\"DirectoryOrCreate\"}}]}}" >/dev/null 2>&1 ;;
  *"$HOST_KEY"*) record PASS "widening a held host path faces the gate" \
      "the same node path, writable, rejected by ${HOST_KEY}" ;;
  *) record FAIL "widening a held host path faces the gate" "rejected, but not by the gate: ${out}" ;;
esac

out=$(kubectl -n default patch instance "$INST" --type=merge -p '{"spec":{"stop":false}}' 2>&1)
case "$out" in *patched*) record PASS "existing Instance restarts with the gate off" "spec.stop=false accepted (hostPath volume kept)" ;;
               *) judge_update "existing Instance restarts with the gate off" "$out" ;; esac

# --- 12b. The other half of the same rule, and the one that decides whether the gates protect
#     anything: an Instance that has NEVER held an escape must not acquire one by patch. A gate that
#     fired only on create would stop nothing — an Instance created stopped and empty asks for
#     nothing, so a single patch would hand it both escapes while both settings are off. ---
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_PLAIN}, namespace: default }
spec:
  type: ${IT}
  stop: true
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF
if ! kubectl -n default get instance "$INST_PLAIN" >/dev/null 2>&1; then
  record SKIP "escalation by patch is refused" "the plain stopped Instance could not be created"
else
  # Judge each escalation on the field it names, so an unrelated refusal is never read as the gate
  # holding, and an acceptance is never excused.
  judge_escalation() {  # judge_escalation <check> <field-marker> <output>
    case "$3" in
      *patched*|*configured*|*unchanged*)
        record FAIL "$1" "ACCEPTED — an escape was acquired by patch with the gate off" ;;
      *"$2"*) record PASS "$1" "rejected on $2" ;;
      *) record FAIL "$1" "rejected, but not on $2: $3" ;;
    esac
  }

  out=$(kubectl -n default patch instance "$INST_PLAIN" --type=merge \
    -p '{"spec":{"privileged":true}}' 2>&1)
  judge_escalation "privileged cannot be acquired by patch" "spec.privileged" "$out"

  out=$(kubectl -n default patch instance "$INST_PLAIN" --type=merge \
    -p '{"spec":{"additionalVolumes":[{"mountPath":"/mnt/escalate","hostPath":{"path":"/"}}]}}' 2>&1)
  judge_escalation "a hostPath cannot be acquired by patch" "spec.additionalVolumes" "$out"

  out=$(kubectl -n default patch instance "$INST_PLAIN" --type=merge \
    -p '{"spec":{"stop":false,"privileged":true,"additionalVolumes":[{"mountPath":"/mnt/escalate","hostPath":{"path":"/"}}]}}' 2>&1)
  judge_escalation "both cannot be acquired in the patch that restarts it" "spec.privileged" "$out"
fi

# --- 13. A pin to a node that does not exist is rejected at creation. ---
out=$(cat <<EOF | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST_GATED}, namespace: default }
spec:
  type: ${IT}
  nodeName: gpustack-e2e-no-such-node
  image: ubuntu:24.04
  command: ["sleep", "infinity"]
  resources: { cpu: "1", ram: "2Gi", localStorage: "10Gi" }
  volume: { ephemeral: { capacity: 1Gi } }
EOF
)
case "$out" in
  *created*|*configured*|*unchanged*)
    kubectl -n default delete instance "$INST_GATED" --ignore-not-found >/dev/null 2>&1
    record FAIL "unknown pinned node rejected" "create was ACCEPTED for a node that does not exist" ;;
  # Only a rejection that names the field is this rule; anything else is an unrelated failure.
  *spec.nodeName*) record PASS "unknown pinned node rejected" "create rejected on spec.nodeName" ;;
  *) record FAIL "unknown pinned node rejected" "rejected, but not on spec.nodeName: ${out}" ;;
esac

print_and_exit
