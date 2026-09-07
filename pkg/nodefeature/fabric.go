package nodefeature

import (
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

const (
	// FabricFeatureLabelPrefix prefixes every label this file writes, and is what makes the set
	// identifiable as a set.
	FabricFeatureLabelPrefix = FeatureLabelPrefix + "fabric."

	// NodeFabricDomainLabelKey names the scale-up interconnect domain every accelerator on this
	// node belongs to, as `<kind>-<id>`.
	//
	// The kind is part of the VALUE and not merely of the key, because a domain id is only
	// comparable within its own interconnect: an Ascend super pod id is a small integer and an AMD
	// XGMI hive id is a 64-bit number, so `7` could name both and a selector matching on the id
	// alone would co-locate a job across two unrelated fabrics.
	//
	// Emitted only when EVERY accelerator on the node reports the SAME domain, which is what makes
	// it usable as an equality selector: a node whose accelerators sit in different domains has no
	// single answer, and publishing one of them would advertise co-location the hardware does not
	// offer. Such a node carries no key, and a consumer needing the per-accelerator truth reads the
	// `Devices` object, which carries it.
	NodeFabricDomainLabelKey = FabricFeatureLabelPrefix + "domain"

	// NodeFabricMembersLabelKey is how many members the domain reports having.
	//
	// Informational only. The selector is equality-matched, so "at least 128 members" cannot be
	// asked through it and only "members == 384" can. Publishing it answers an operator's question
	// about the node's blast radius; it cannot answer a scheduler's.
	NodeFabricMembersLabelKey = FabricFeatureLabelPrefix + "members"
)

// ConstructFabricNodeLabels constructs the scale-up fabric labels from one detect pass's inventory.
//
// Both keys describe the node AS A WHOLE, so both are withheld unless the node has one unambiguous
// answer. See NodeFabricDomainLabelKey for why a partial answer is worse than none here.
//
// It returns an empty map rather than nil for a node with no such fabric, and an empty result is
// MEANINGFUL at the object these are written to: the writer removes the keys under this prefix that
// a pass did not report, so a node taken out of its super pod loses the label rather than keeping a
// domain it has left. That is what makes withholding a decision instead of a silence — and it is why
// the reduction below insists on the WHOLE node, since the removal is only sound when every writer
// touching the object computes the same answer.
func ConstructFabricNodeLabels(groups device.DevicesGroupList) map[string]string {
	labels := map[string]string{}

	domain, members, ok := soleFabricDomain(groups)
	if !ok {
		return labels
	}

	// The domain value is a COMPOSITE, so the sanitizer is not a safety net for it: it caps a label
	// value at 63 characters, and a truncated `<kind>-<id>` publishes a DIFFERENT domain that other
	// nodes may genuinely be in. A domain whose rendering does not survive sanitization therefore
	// gets no key at all, which is the same rule NodeRDMANumaLabelKey applies to its own set value.
	if kubemeta.SanitizeLabelValue(domain) != domain {
		return labels
	}
	labels[NodeFabricDomainLabelKey] = domain

	// Zero means the manufacturer does not report a size, never a domain with no members.
	if members > 0 {
		labels[NodeFabricMembersLabelKey] = kubemeta.SanitizeLabelValue(strconvx.FormatUint(uint64(members), 10))
	}

	return labels
}

// soleFabricDomain reports the one domain every accelerator on this node belongs to, the member
// count that domain reports, and whether there was a single unambiguous answer at all.
//
// An accelerator carrying NO fabric record disagrees just as loudly as one carrying a different
// domain: a node where half the accelerators are on a fabric and half are not has no node-level
// domain, and treating the silent half as agreement would publish a key that promises the whole
// node.
func soleFabricDomain(groups device.DevicesGroupList) (domain string, members uint32, ok bool) {
	for gi := range groups {
		accels := groups[gi].Accelerators
		for ai := range accels {
			fabric := accels[ai].Topology.Fabric
			if fabric == nil || fabric.Kind == "" || fabric.ID == "" {
				return "", 0, false
			}

			seen := fabric.Kind + "-" + fabric.ID
			if domain == "" {
				domain, members = seen, fabric.MemberCount
				continue
			}
			if seen != domain {
				return "", 0, false
			}
		}
	}

	return domain, members, domain != ""
}
