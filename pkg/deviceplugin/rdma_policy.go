// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// RDMAPolicyResolutionStatus is what the desired resolution concluded for one node.
type RDMAPolicyResolutionStatus string

const (
	// RDMAPolicyResolutionDefault says the cache is synced, no policy targets the node,
	// and every ordinary candidate stays eligible under the existing rules.
	RDMAPolicyResolutionDefault RDMAPolicyResolutionStatus = "Default"
	// RDMAPolicyResolutionPolicy says exactly one policy targets the node and its rules
	// form the desired candidate set.
	RDMAPolicyResolutionPolicy RDMAPolicyResolutionStatus = "Policy"
	// RDMAPolicyResolutionConflict says more than one policy targets the node, whatever
	// their rules say; there is no merge or priority rule to resolve them.
	RDMAPolicyResolutionConflict RDMAPolicyResolutionStatus = "Conflict"
	// RDMAPolicyResolutionInvalid says the targeting configuration cannot be applied on
	// this node, such as include names the complete inventory does not know.
	RDMAPolicyResolutionInvalid RDMAPolicyResolutionStatus = "Invalid"
	// RDMAPolicyResolutionUnknown says the inputs cannot decide: the cache never synced,
	// or the inventory is missing or incomplete.
	RDMAPolicyResolutionUnknown RDMAPolicyResolutionStatus = "Unknown"
)

// RDMAPolicyExclusionReason explains why an observed device is not in the selected set.
type RDMAPolicyExclusionReason string

const (
	// RDMAPolicyExclusionExcludedByPolicy marks a device the targeting policy's rules
	// leave out of the desired set.
	RDMAPolicyExclusionExcludedByPolicy RDMAPolicyExclusionReason = "ExcludedByPolicy"
	// RDMAPolicyExclusionExternallyOwned marks a device whose authoritative owner is
	// outside GPUStack; no include rule can override that ownership.
	RDMAPolicyExclusionExternallyOwned RDMAPolicyExclusionReason = "ExternallyOwned"
)

// RDMAPolicyNodeInput identifies the node whose labels feed selector matching.
type RDMAPolicyNodeInput struct {
	Name   string
	UID    string
	Labels map[string]string
}

// RDMAPolicyInputs is the complete input of one deterministic resolution.
type RDMAPolicyInputs struct {
	// Synced reports one successful initial policy list; an unsynced cache is unknown,
	// never an empty policy list.
	Synced bool

	// Node is the node being resolved.
	Node RDMAPolicyNodeInput

	// Policies is the policy list in cache order. Order never changes the result.
	Policies []workercore.DevicesRDMASelectionPolicy

	// Inventory is the node's internal verbs observation. A nil or incomplete snapshot
	// leaves the resolution unknown.
	Inventory *device.RDMASnapshot
}

// RDMAPolicyExclusion records one observed device outside the selected set, with why.
type RDMAPolicyExclusion struct {
	Name   string
	Reason RDMAPolicyExclusionReason
}

// RDMAPolicyResolution is the pure desired result for one node.
//
// It carries no applied claim: publishing references, conditions, and capacity stays with
// the activation owner. The result binds the policy UID and generation, the node, and the
// inventory revision it was computed from.
type RDMAPolicyResolution struct {
	// Status is the resolution outcome.
	Status RDMAPolicyResolutionStatus

	// PolicyRef names the one targeting policy for Policy and the offending policy for
	// Invalid; nil for every other status, so an absent reference plus a Default status
	// is the resolved default.
	PolicyRef *workercore.DevicesRDMASelectionPolicyReference

	// Selected are the desired HCA names in sorted order, for Policy and Default.
	Selected []string

	// Excluded records every observed device outside Selected in sorted name order, for
	// Policy and Default.
	Excluded []RDMAPolicyExclusion

	// Port is the 1-based physical port every selected HCA must carry, copied from the one
	// targeting policy. Nil for Default and for every non-applied status: no policy, no
	// port restriction.
	Port *int32

	// Message carries actionable diagnostics for Invalid and Unknown, and is empty
	// otherwise.
	Message string

	// NodeName and NodeUID bind the node this resolution was computed for.
	NodeName string
	NodeUID  string

	// InventoryRevision is the observation revision behind the result.
	InventoryRevision string
}

// rdmaSelectorIssue records one policy whose cached selector cannot be read. The policy
// can neither prove nor disprove that it targets the node, so it never counts as a match.
type rdmaSelectorIssue struct {
	Name       string
	UID        string
	Generation int64
	Reason     string
}

// rdmaPolicySelectorScan splits the policy list into policies whose selector provably
// matches the node and policies whose selector cannot be read. It reads every policy
// before returning, so the split never depends on list order.
func rdmaPolicySelectorScan(inputs RDMAPolicyInputs) ([]workercore.DevicesRDMASelectionPolicy, []rdmaSelectorIssue) {
	var matches []workercore.DevicesRDMASelectionPolicy
	var issues []rdmaSelectorIssue
	for i := range inputs.Policies {
		p := &inputs.Policies[i]
		switch p.Spec.NodeSelector {
		case nil:
			// Admission rejects a nil selector; a cached one still cannot prove that the
			// policy does not target this node, so it is a configuration error rather
			// than a silent default.
			issues = append(issues, rdmaSelectorIssue{
				Name:       p.Name,
				UID:        string(p.UID),
				Generation: p.Generation,
				Reason:     fmt.Sprintf("policy %s carries a nil nodeSelector", p.Name),
			})
		default:
			selector, err := metav1.LabelSelectorAsSelector(p.Spec.NodeSelector)
			if err != nil {
				// The upstream error names whichever match-label entry the map iteration
				// reaches first, so quoting it would make the report unstable; the
				// policy and field defect itself is stable.
				issues = append(issues, rdmaSelectorIssue{
					Name:       p.Name,
					UID:        string(p.UID),
					Generation: p.Generation,
					Reason:     fmt.Sprintf("policy %s carries an invalid nodeSelector", p.Name),
				})
				continue
			}
			if selector.Matches(labels.Set(inputs.Node.Labels)) {
				matches = append(matches, *p)
			}
		}
	}
	return matches, issues
}

// ResolveRDMAPolicy computes the desired RDMA candidate set for one node from the policy
// list, the node labels, and the internal verbs observation.
//
// A nonempty include restricts the candidates. Include and Exclude are mutually exclusive:
// a policy that sets both is invalid. Externally owned devices stay out of every selected
// set. Exact HCA names never bleed across prefixes: include mlx5_1 does not match mlx5_10.
//
// Resolution statuses, in the order they are decided:
//   - Unknown when the cache never synced or the inventory is missing or incomplete.
//   - Conflict when more than one policy with a readable selector targets the node, whatever
//     malformed selectors exist beside them.
//   - Invalid when any cached policy carries a nil or unreadable selector, or when the
//     one targeting policy names an unknown device, sets include and exclude together, or
//     pins a port that a selected device does not carry. A malformed selector never counts
//     as a match; the invalid report names every malformed policy in name and UID order,
//     and none of its diagnostics claims a match.
//   - Default when no policy targets the node.
//   - Policy when exactly one policy targets the node.
func ResolveRDMAPolicy(inputs RDMAPolicyInputs) RDMAPolicyResolution {
	res := RDMAPolicyResolution{
		NodeName: inputs.Node.Name,
		NodeUID:  inputs.Node.UID,
	}
	if !inputs.Synced {
		res.Status = RDMAPolicyResolutionUnknown
		res.Message = "the policy cache has not completed its initial list"
		return res
	}
	if inputs.Inventory == nil || !inputs.Inventory.Complete {
		res.Status = RDMAPolicyResolutionUnknown
		res.Message = "the verbs inventory is missing or incomplete"
		return res
	}
	res.InventoryRevision = inputs.Inventory.Revision

	matches, issues := rdmaPolicySelectorScan(inputs)
	switch {
	case len(matches) > 1:
		res.Status = RDMAPolicyResolutionConflict
		res.Message = fmt.Sprintf("%d policies target this node; there is no merge or priority rule", len(matches))
		return res
	case len(issues) > 0:
		// The report must not depend on cache order: sort by policy identity, bind the
		// first identity as the reference, and keep every policy's own reason verbatim.
		sort.Slice(issues, func(i, j int) bool {
			if issues[i].Name != issues[j].Name {
				return issues[i].Name < issues[j].Name
			}
			return issues[i].UID < issues[j].UID
		})
		reasons := make([]string, len(issues))
		for i := range issues {
			reasons[i] = issues[i].Reason
		}
		res.Status = RDMAPolicyResolutionInvalid
		res.PolicyRef = &workercore.DevicesRDMASelectionPolicyReference{
			Name:       issues[0].Name,
			UID:        issues[0].UID,
			Generation: issues[0].Generation,
		}
		res.Message = strings.Join(reasons, "; ")
		return res
	case len(matches) == 1:
		res.Status = RDMAPolicyResolutionPolicy
		res.PolicyRef = rdmaPolicyReference(&matches[0])
	default:
		res.Status = RDMAPolicyResolutionDefault
	}

	// Only one policy's rules apply; the default applies none.
	var policySpec *workercore.DevicesRDMASelectionPolicySpec
	if len(matches) == 1 {
		policySpec = &matches[0].Spec
	}

	known := make(map[string]device.RDMADevice, len(inputs.Inventory.Devices))
	for i := range inputs.Inventory.Devices {
		d := inputs.Inventory.Devices[i]
		known[d.Name] = d
	}

	// A complete inventory makes every unknown name a configuration error in both lists:
	// a misspelled exclude would otherwise silently keep the device it meant to remove.
	// An incomplete inventory never reaches this point.
	var unknownInclude, unknownExclude []string
	include := map[string]struct{}{}
	exclude := map[string]struct{}{}
	if policySpec != nil {
		for _, name := range policySpec.Include {
			include[name] = struct{}{}
		}
		for name := range include {
			if _, ok := known[name]; !ok {
				unknownInclude = append(unknownInclude, name)
			}
		}
		for _, name := range policySpec.Exclude {
			exclude[name] = struct{}{}
		}
		for name := range exclude {
			if _, ok := known[name]; !ok {
				unknownExclude = append(unknownExclude, name)
			}
		}
	}
	// Include and Exclude are mutually exclusive; a policy that sets both never applies.
	bothLists := policySpec != nil && len(policySpec.Include) > 0 && len(policySpec.Exclude) > 0
	badPort := policySpec != nil && policySpec.Port != nil && *policySpec.Port < 1
	if bothLists || badPort || len(unknownInclude) > 0 || len(unknownExclude) > 0 {
		sort.Strings(unknownInclude)
		sort.Strings(unknownExclude)
		parts := make([]string, 0, 4)
		if bothLists {
			parts = append(parts, "include and exclude are mutually exclusive")
		}
		if badPort {
			parts = append(parts, fmt.Sprintf("port %d must be at least 1", *policySpec.Port))
		}
		if len(unknownInclude) > 0 {
			parts = append(parts, fmt.Sprintf("include names the inventory does not know: %v", unknownInclude))
		}
		if len(unknownExclude) > 0 {
			parts = append(parts, fmt.Sprintf("exclude names the inventory does not know: %v", unknownExclude))
		}
		res.Status = RDMAPolicyResolutionInvalid
		res.Message = strings.Join(parts, "; ")
		return res
	}

	res.Selected = []string{}
	res.Excluded = []RDMAPolicyExclusion{}
	for name, dev := range known {
		// Authoritative ownership explains a device's absence no matter what the rules
		// say, so it is reported ahead of any policy reason.
		if dev.Owner == device.RDMADeviceOwnerExternal {
			res.Excluded = append(res.Excluded, RDMAPolicyExclusion{Name: name, Reason: RDMAPolicyExclusionExternallyOwned})
			continue
		}
		_, included := include[name]
		if policySpec != nil && len(policySpec.Include) > 0 && !included {
			res.Excluded = append(res.Excluded, RDMAPolicyExclusion{Name: name, Reason: RDMAPolicyExclusionExcludedByPolicy})
			continue
		}
		if _, excluded := exclude[name]; excluded {
			res.Excluded = append(res.Excluded, RDMAPolicyExclusion{Name: name, Reason: RDMAPolicyExclusionExcludedByPolicy})
			continue
		}
		res.Selected = append(res.Selected, name)
	}
	sort.Slice(res.Selected, func(i, j int) bool { return res.Selected[i] < res.Selected[j] })
	sort.Slice(res.Excluded, func(i, j int) bool { return res.Excluded[i].Name < res.Excluded[j].Name })

	// A port restriction must hold on every selected HCA: a selected device without that
	// physical port cannot serve the policy's fabric. Excluded and externally owned devices
	// owe nothing to the port, so their shape never invalidates the policy. The observation
	// is a complete inventory — the unknown-name check above proved it — so a port absent
	// from a device's ports is the device not carrying it, never the pass failing to read it.
	if policySpec != nil && policySpec.Port != nil {
		var missingPort []string
		for _, name := range res.Selected {
			dev := known[name]
			carried := false
			for i := range dev.Ports {
				if dev.Ports[i].Number == uint32(*policySpec.Port) {
					carried = true
					break
				}
			}
			if !carried {
				missingPort = append(missingPort, name)
			}
		}
		if len(missingPort) > 0 {
			res.Status = RDMAPolicyResolutionInvalid
			res.Selected = nil
			res.Excluded = nil
			res.Message = fmt.Sprintf("selected devices do not carry port %d: %v", *policySpec.Port, missingPort)
			return res
		}
		res.Port = ptr.To(*policySpec.Port)
	}
	return res
}

func rdmaPolicyReference(p *workercore.DevicesRDMASelectionPolicy) *workercore.DevicesRDMASelectionPolicyReference {
	return &workercore.DevicesRDMASelectionPolicyReference{
		Name:       p.Name,
		UID:        string(p.UID),
		Generation: p.Generation,
	}
}
