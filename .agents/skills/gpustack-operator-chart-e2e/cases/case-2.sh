#!/usr/bin/env bash
#
# CASE 2 — Uninstall leaves zero leftovers   (MUTATING; run LAST)
#
#   case-2.sh <NS>
#
# Goal:        The shared teardown (helm uninstall + the leftovers helm does not manage) leaves the
#              cluster clean — no leftover releases, CRDs, apiservices, or clusterrolebindings —
#              while the gpustack-system namespace is intentionally kept.
# Environment: Any reachable cluster with the chart installed. No GPU. DELETES the whole release,
#              which now includes Kueue / NFD / the CSI drivers and Kueue's CRDs, and with them
#              every ClusterQueue and Workload in the cluster — run ONLY as the final step.
# Inputs:      All real, nothing mocked — runs teardown.sh (helm uninstall; the releases the chart
#              does not own; CRDs, finalizers, APIServices/webhooks, migration-hook leftovers).
# Expected:    After teardown, zero leftover: helm releases (gpustack/kueue/nfd/csi), every
#              gpustack.ai CRD, every CRD this release owned, gpustack apiservices, gpustack
#              clusterrolebindings, the gpustack-cpu-info NodeFeatureRule the worker applied, the
#              NodeFeatures this operator reported, and every Node label or annotation key in a
#              gpustack.ai domain but the two an administrator may set; a user's own NodeFeature and
#              label in those domains survive.
# NOT every kueue/nfd CRD by name. The teardown delegates to the chart's own
#              cleanup.sh, which leaves NFD's CRDs in place entirely (its subchart ships them
#              unannotated, so there is nothing to read ownership from) and removes Kueue's only when
# THIS release owns them — so a cluster running its own Kueue keeps it. Asserting on the
#              group names would fail a CORRECT teardown, and demanding their removal is what the
#              old private copy of that cleanup did wrong.
# Cleanup:     This case IS the cleanup (teardown is idempotent, safe to re-run); the
#              gpustack-system namespace is kept on purpose.
set -uo pipefail

NS="${1:?usage: case-2.sh <NS>}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"

# The one place this file names the release, and it is FIXED — the same literal teardown.sh and
# deploy.sh use. Parameterizing it here made this case check ownership for a name the teardown never
# uninstalled, and chasing that further meant deriving the worker Certificate's Secret name from the
# chart's own `worker.fullname` in shell. One agreed literal is smaller and cannot drift.
RELEASE=gpustack-operator

# Ask with the same pinned client that teardown uses, so a client incompatibility cannot appear as
# a leftover release.
HELM="$(bash "${LIB}/helm.sh")" || exit 1

# Two things a user owns, planted before the teardown so it can be seen to leave them alone: a
# NodeFeature in this namespace under another owner's part-of, and a label in the gpustack.ai
# domains of the kind an administrator sets for a nodeLabels TopologySource. cleanup.sh selects the
# NodeFeatures it deletes by this operator's markers and the Node labels it removes by exact key, so
# both must survive it. Either failing to plant is a FAIL further down, never a skip. The label key
# is one no real topology uses, and it is planted without --overwrite: a Node that already carries
# it is a FAIL, never an administrator's value this case clobbers and then removes.
USER_NF=case2-user-owned
USER_LABEL_KEY=topology.gpustack.ai/e2e-case2-user
USER_LABEL_VALUE=case2-user
USER_NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
USER_PLANTED=""
USER_LABEL_PLANTED=""
if [ -n "$USER_NODE" ] \
  && kubectl label node "$USER_NODE" "${USER_LABEL_KEY}=${USER_LABEL_VALUE}" >/dev/null 2>&1 \
  && USER_LABEL_PLANTED=yes \
  && kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${USER_NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${USER_NODE}
    app.kubernetes.io/part-of: case2-user
spec: {}
EOF
then
  USER_PLANTED=yes
fi

# Which Node keys NFD had written, read before the teardown removes NFD and the record with it. A
# key in the kept list below is excused only when NFD did NOT write it: that is the copy an
# administrator set. NFD writes the same keys itself in the default modes, and its copy has to be
# gone, so excusing the key by name alone would hide exactly the leftover this case looks for.
# One "<node><TAB><keys NFD lists as its own>" line per Node.
if ! NODES_BEFORE="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.nfd\.node\.kubernetes\.io/feature-labels}{"\n"}{end}' 2>&1)"; then
  NODES_BEFORE="CHECK-BROKEN: kubectl get nodes failed: $(printf '%s' "$NODES_BEFORE" | head -1)"
fi

# Tear everything down (delegates the cleanup to the chart's own files/cleanup.sh).
bash "$LIB/teardown.sh" "$NS"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
assert_empty() { # check  leftover-output
  if [ -z "$2" ]; then record PASS "$1" "none"; else record FAIL "$1" "$(echo "$2" | tr '\n' ' ' | cut -c1-60)"; fi
}

# EVERY leftover query goes through this, and that is the point. `cmd 2>/dev/null | grep` maps a
# transport error, an RBAC denial and a genuinely clean cluster onto the same empty string, and empty
# is exactly what assert_empty reads as PASS — so a failed question would report success over a wedged
# cluster. probe turns a failed query into a visible sentinel instead, which assert_empty then FAILs
# on. The ownership check below was written with sentinels for this reason; these four were not, which
# left two CRD assertions in one file disagreeing about whether a failed question counts as an answer.
probe() { # what-failed  grep-pattern  command...
  local what="$1" pattern="$2" out
  shift 2
  if ! out="$("$@" 2>&1)"; then
    echo "CHECK-BROKEN: ${what} failed: $(printf '%s' "$out" | head -1)"
    return 0
  fi
  printf '%s\n' "$out" | grep -E "$pattern" || true
}

# The releases THIS harness installs, by exact name — not a `gpustack|kueue|nfd|csi` pattern.
# That pattern also matches a standalone `kueue`, `nfd` or CSI release the cluster brought itself,
# which the teardown deliberately preserves (see the header). It would therefore FAIL a CORRECT
# teardown on the very "cluster already runs its own Kueue" topology this file promises keeps
# working — the same mistake as deciding CRD ownership by group name, one object kind up, and the
# mistake teardown.sh's own header records as the reason its private copy was deleted.
OWNED_RELEASES=(
  "$RELEASE"
  "${RELEASE}-device-manager"
  gpustack-kueue
  gpustack-node-feature-discovery
  gpustack-csi-driver-nfs
  gpustack-csi-driver-s3
)
assert_empty "no leftover releases" "$(
  if ! helm_out="$("$HELM" list --all -q -n "$NS" 2>&1)"; then
    echo "CHECK-BROKEN: helm list failed: $(printf '%s' "$helm_out" | head -1)"
  else
    printf '%s\n' "$helm_out" \
      | grep -Fxf <(printf '%s\n' "${OWNED_RELEASES[@]}") || true
  fi
)"
assert_empty "no leftover gpustack CRDs" \
  "$(probe 'kubectl get crd' 'gpustack\.ai' kubectl get crd)"
# Ownership, not name pattern: this is what still catches a Kueue CRD the release installed and
# failed to remove, without failing on one the cluster brought itself. Read from the Helm annotation
# rather than the managed-by label, because only the annotation names WHICH release.
# BOTH annotations, matched as a PAIR, exactly as cleanup.sh's own owned_crds() decides ownership.
# CRDs are cluster-scoped, so unlike every other ownership probe in this suite there is no `-n $NS` on
# the query to pin the namespace for us. release-name alone is satisfied by a co-located Kueue
# installed as its own release that happens to be called gpustack-operator in another namespace —
# cleanup.sh correctly leaves that one alone, and this check would then report it as a leftover and
# FAIL a CORRECT teardown. That is the very "cluster running its own Kueue" case this file's header
# says must keep working. The two checks have to decide ownership by the same rule or one of them is
# wrong by construction.
# Every failure path here reports a SENTINEL, never silence. An empty result is what this assertion
# reads as success, so a missing python3, an unreachable API server or a non-JSON body would all have
# passed the check they exist to make — the same "a failed question answered as the answer" shape the
# deploy guard above is built to avoid.
owned_crds="$(
  if ! command -v python3 >/dev/null 2>&1; then
    echo "CHECK-BROKEN: python3 is not on PATH, so ownership could not be read"
  elif ! crd_json="$(kubectl get crd -o json 2>&1)"; then
    echo "CHECK-BROKEN: kubectl get crd failed: $(echo "$crd_json" | head -1)"
  else
    printf '%s' "$crd_json" | NS="$NS" RELEASE="$RELEASE" python3 -c '
import json, os, sys
try:
    doc = json.load(sys.stdin)
except Exception as e:
    print("CHECK-BROKEN: crd list is not valid json: %s" % e)
    sys.exit(0)
ns, release = os.environ["NS"], os.environ["RELEASE"]
for o in doc.get("items", []):
    meta = o.get("metadata", {})
    ann = meta.get("annotations") or {}
    if (ann.get("meta.helm.sh/release-name") == release
            and ann.get("meta.helm.sh/release-namespace") == ns):
        print(meta.get("name", ""))
'
  fi
)"
assert_empty "no CRDs still owned by this release" "$owned_crds"
assert_empty "no leftover apiservices" \
  "$(probe 'kubectl get apiservice' 'gpustack' kubectl get apiservice)"
assert_empty "no leftover rolebindings" \
  "$(probe 'kubectl get clusterrolebinding' 'gpustack' kubectl get clusterrolebinding)"
# The gpustack-cpu-info NodeFeatureRule belongs to no release: the worker applies it at boot, so
# helm uninstall never takes it, and the NFD CRD it lives under is kept on purpose. cleanup.sh deletes
# it by the operator's part-of label, and this reads it back by the same label. Missed, it keeps an
# external NFD labelling nodes, and it fails the install of any earlier version whose NFD release
# rendered the same rule.
# No NodeFeatureRule CRD means no rule can be left, which is not a failure; any OTHER failure to ask
# is, through the same sentinel every query here uses.
assert_empty "no leftover cpu-info NodeFeatureRule" "$(
  if ! nfr_crd="$(kubectl get crd nodefeaturerules.nfd.k8s-sigs.io -o name 2>&1)"; then
    case "$nfr_crd" in
      *NotFound*) ;;
      *) echo "CHECK-BROKEN: kubectl get crd failed: $(printf '%s' "$nfr_crd" | head -1)" ;;
    esac
  else
    probe 'kubectl get nodefeaturerule' 'gpustack-cpu-info' \
      kubectl get nodefeaturerules.nfd.k8s-sigs.io -l app.kubernetes.io/part-of=gpustack-operator -o name
  fi
)"
# The NodeFeatures the worker, the device-managers and the TopologySources report, by the same
# markers cleanup.sh selects them by. Left behind, an NFD that is still installed keeps applying
# them, and the next install replays them. A missing NodeFeature CRD is answered as for the rule.
for sel in app.kubernetes.io/part-of=gpustack-operator-worker \
  app.kubernetes.io/part-of=gpustack-operator-device-manager topology.gpustack.ai/source-uid; do
  assert_empty "no leftover NodeFeature ${sel}" "$(
    if ! nf_crd="$(kubectl get crd nodefeatures.nfd.k8s-sigs.io -o name 2>&1)"; then
      case "$nf_crd" in
        *NotFound*) ;;
        *) echo "CHECK-BROKEN: kubectl get crd failed: $(printf '%s' "$nf_crd" | head -1)" ;;
      esac
    else
      probe 'kubectl get nodefeature' '^nodefeature' \
        kubectl -n "$NS" get nodefeatures.nfd.k8s-sigs.io -l "$sel" -o name
    fi
  )"
done
# Every label and annotation key in a gpustack.ai domain (gpustack.ai/ or <anything>.gpustack.ai/)
# on every Node, not a list of the keys known today: the next key the worker writes is covered the
# day it is added. The only keys excused are the two an administrator may set, and only where NFD
# had not written them (see NODES_BEFORE):
#   - gpustack.ai/managed: onboards a Node under manual node management
#     (GPUSTACK_NODE_MANAGEMENT_MANUAL=true); in the default mode it is NFD's, and NFD removes it;
#   - topology.gpustack.ai/<level>, any level except profile: what a TopologySource in nodeLabels
#     mode reads; a snapshot-mode source writes it through NFD instead, and NFD removes it.
# topology.gpustack.ai/profile is never excused: the worker writes it on every Node itself.
assert_empty "no gpustack.ai-domain key left on any Node" "$(
  if ! nodes_after="$(kubectl get nodes -o json 2>&1)"; then
    echo "CHECK-BROKEN: kubectl get nodes failed: $(printf '%s' "$nodes_after" | head -1)"
  else
    printf '%s' "$nodes_after" | NODES_BEFORE="$NODES_BEFORE" python3 -c '
import json, os, sys
before = os.environ["NODES_BEFORE"]
if before.startswith("CHECK-BROKEN"):
    print(before)
    sys.exit(0)
try:
    after = json.load(sys.stdin)
except Exception as e:
    print("CHECK-BROKEN: node list is not valid json: %s" % e)
    sys.exit(0)
def ours(key):
    domain = key.split("/", 1)[0] if "/" in key else ""
    return domain == "gpustack.ai" or domain.endswith(".gpustack.ai")
def kept(key):
    return key == "gpustack.ai/managed" or (
        key.startswith("topology.gpustack.ai/") and key != "topology.gpustack.ai/profile")
written = {}
for line in before.splitlines():
    node, _, keys = line.partition("\t")
    written[node] = set(k for k in keys.split(",") if k)
for o in after.get("items", []):
    meta = o.get("metadata", {})
    node = meta.get("name", "")
    for k in sorted(meta.get("labels") or {}):
        if ours(k) and not (kept(k) and k not in written.get(node, set())):
            print("%s:%s" % (node, k))
    for k in sorted(meta.get("annotations") or {}):
        if ours(k):
            print("%s:annotation %s" % (node, k))
'
  fi
)"
# The two things planted before the teardown, read back and then removed, so this case leaves them
# no more than the teardown should have left anything of its own.
if [ -z "$USER_PLANTED" ]; then
  record FAIL "a user's NodeFeature and label survive" "CHECK-BROKEN: could not plant them before the teardown"
else
  user_nf="$(kubectl -n "$NS" get nodefeatures.nfd.k8s-sigs.io "$USER_NF" -o name 2>&1)"
  user_label="$(kubectl get node "$USER_NODE" \
    -o jsonpath="{.metadata.labels.${USER_LABEL_KEY//./\\.}}" 2>&1)"
  if [ "$user_nf" = "nodefeature.nfd.k8s-sigs.io/${USER_NF}" ] && [ "$user_label" = "$USER_LABEL_VALUE" ]; then
    record PASS "a user's NodeFeature and label survive" "${USER_NF}, ${USER_NODE}:${USER_LABEL_KEY}"
  else
    record FAIL "a user's NodeFeature and label survive" \
      "$(printf 'nodefeature=%s label=%s' "$user_nf" "$user_label" | tr '\n' ' ' | cut -c1-60)"
  fi
fi
kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$USER_NF" --ignore-not-found >/dev/null 2>&1 || true
[ -z "$USER_LABEL_PLANTED" ] || kubectl label node "$USER_NODE" "${USER_LABEL_KEY}-" >/dev/null 2>&1 || true

echo
echo "== CASE 2 — Uninstall leaves zero leftovers =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s) — re-run teardown.sh (idempotent), or see ../_e2e-lib/references/troubleshooting.md"
  echo "for stuck CRDs/finalizers."
  exit 1
fi
echo "CASE 2 PASS"
