#!/usr/bin/env bash
#
# _quota-lib.sh — how many replicas a pool's quota can hold, summed over every flavor of its queue.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace. It reads; it creates,
# records and deletes nothing. The cases that make a pool short by occupying it (50, 95) source it so
# the one rule they size by is written once.
#
# A case uses it as:
#
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_quota-lib.sh"
#     CHARGE="$(workload_charge "$NS" "$WL")"            # one replica's Workload, already reserved
#     IFS='|' read -r BOUND FLAVORS DETAIL <<<"$(pool_replica_bound "$CQ" "$CHARGE")"
#     # BOUND is 0 when the numbers could not be read, and DETAIL then says why.
#
# ------------------------------------------------------------------------------------------------
# WHY EVERY FLAVOR, AND WHY WHAT KUEUE CHARGED
#
# Quota is scoped per flavor and every replica is a Workload of its own, so each replica can land on
# any flavor of the queue. Sizing from `resourceGroups[0].flavors[0]` describes one flavor of a pool
# that may have several -- a generic pool spans CPU nodes and the CPU of accelerated nodes, and one
# accelerator model on two node shapes is two flavors -- and the extra replicas then land on the
# flavor nobody counted.
#
# The replica's cost is read from what Kueue charged its Workload, not from the Pod. An accelerated
# queue covers only `credits.gpustack.ai/<manufacturer>`, which no Pod requests: a resource
# transformation derives it. The charge is the only place both kinds of pool state their cost in the
# unit the quota is written in.
#
# THE RESULT IS AN UPPER BOUND, NEVER THE CAPACITY. It ignores what other workloads already hold, a
# flavor whose node labels the replica cannot match, and the node room topology-aware scheduling
# checks before it reserves anything -- each of those admits fewer. A case that needs the pool's real
# capacity measures it by asking Kueue with more replicas than this bound, which is why a bound that
# is too high is harmless and one that is too low is not.
# ------------------------------------------------------------------------------------------------

# The jq definition that turns a Kubernetes quantity into integer milli-units, or null for a form it
# does not read. The shape is matched first for the reason case-50's `millis` gives: a numeric prefix
# of a value like "2e3" or "1Ki" is a confident wrong number. Milli-units keep CPU's `m` exact, and the
# division below is between integers, so 8 cores over 800m reads 10 and not 9.999.
_QUOTA_JQ_MILLI='
def milli:
  if type != "string" then null
  elif (test("^[0-9]+(\\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$") | not) then null
  else capture("^(?<n>[0-9.]+)(?<s>[A-Za-z]*)$")
    | (.n | tonumber) * ({"m": 1, "": 1e3, "k": 1e6, "M": 1e9, "G": 1e12, "T": 1e15, "P": 1e18,
        "E": 1e21, "Ki": 1024e3, "Mi": 1048576e3, "Gi": 1073741824e3, "Ti": 1099511627776e3,
        "Pi": 1125899906842624e3, "Ei": 1152921504606846976e3}[.s])
    | round
  end;
'

# workload_charge prints, as a JSON object of resource to milli-units, what Kueue charged one Workload
# across all of its PodSets. It prints {} for a Workload that holds no reservation, and a resource
# whose quantity cannot be read maps to null so the caller cannot mistake it for a small cost.
workload_charge() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" -o json 2>/dev/null \
    | jq -c "${_QUOTA_JQ_MILLI}"'
        [.status.admission.podSetAssignments[]?.resourceUsage // {} | to_entries[]]
        | group_by(.key)
        | map({key: .[0].key,
               value: (map(.value | milli) | if any(. == null) then null else add end)})
        | from_entries' 2>/dev/null
}

# pool_replica_bound prints "BOUND|FLAVORS|DETAIL" for ClusterQueue $1 and a charge from
# workload_charge. BOUND is, over every charged resource the queue covers, the least of that resource's
# per-flavor floor(nominalQuota / charge) summed across ALL flavors. Taking the least of the per-resource
# sums rather than summing per-flavor minimums keeps it an upper bound when the resources sit in
# different resource groups, where a replica picks a flavor per group. FLAVORS counts the queue's
# distinct flavors. BOUND is 0 when anything could not be read, and DETAIL then says what.
pool_replica_bound() {
  kubectl get clusterqueue "$1" -o json 2>/dev/null \
    | jq -r --argjson c "${2:-null}" "${_QUOTA_JQ_MILLI}"'
        ([.spec.resourceGroups[]?.flavors[]?.name] | unique | length) as $nf
        | [.spec.resourceGroups[]?.flavors[]? | .resources[]?
            | select(.name as $n | ($c // {}) | has($n))
            | {r: .name, q: (.nominalQuota | milli)}] as $rows
        | if ($c | type) != "object" or ($c | length) == 0 then
            "0|\($nf)|the replica holds no charged usage to size from"
          elif any($c[]; . == null) then
            "0|\($nf)|the replica is charged a quantity this cannot read: \($c | tojson)"
          elif ($rows | length) == 0 then
            "0|\($nf)|the queue covers none of the resources the replica is charged: \($c | keys | join(","))"
          elif any($rows[]; .q == null) then
            "0|\($nf)|the queue carries a nominalQuota this cannot read"
          else
            ($rows | map(select($c[.r] > 0)) | group_by(.r)
              | map(.[0].r as $r | {r: $r, n: (map(.q / $c[$r] | floor) | add)})) as $per
            | if ($per | length) == 0 then
                "0|\($nf)|the replica is charged nothing the queue covers"
              else
                "\($per | map(.n) | min)|\($nf)|\($per | map("\(.r) holds \(.n)") | join(", ")) across \($nf) flavor(s)"
              end
          end' 2>/dev/null
}
