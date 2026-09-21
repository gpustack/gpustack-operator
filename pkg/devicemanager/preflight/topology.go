package preflight

import (
	"fmt"
	"strings"
	"time"

	"gpustack.ai/gpustack/pkg/device"
)

// TopologyPolicyUnknown is what the report says when no readable kubelet configuration names a
// topology manager policy. It is deliberately not the kubelet's own default, none: the policy can
// also arrive on the kubelet's command line, in a place none of the sources below reads, and a
// default reported as if it had been read is a measurement nobody took.
const TopologyPolicyUnknown = "unknown"

type (
	// TopologyReport is what this pass established about this node's kubelet TopologyManager
	// policy: the placement guarantees the kubelet is configured to demand when it admits a
	// container asking for NUMA-affine devices.
	//
	// It is a section of its own rather than rows inside a manufacturer's group or a field on the
	// network section, for the same reason the network section is a section of its own: the
	// per-accelerator row type requires an accelerator id and an allocation mode, and a kubelet
	// policy has neither. And it is not a field on the network section because it is not a
	// statement about a link -- a node whose every link is up can run the most restrictive policy
	// there is, and a node with no RDMA at all still runs one.
	TopologyReport struct {
		// Timestamp is when the policy was read. A preflight reports mutable host state as it
		// stands, so the reading is only worth what its time claims.
		Timestamp time.Time `json:"timestamp" yaml:"timestamp"`
		// Policy is the TopologyManager policy this node's kubelet is configured with, in the
		// kubelet's own words, read out of its configuration files. It is
		// TopologyPolicyUnknown when nothing readable names one, and never the kubelet's
		// default: the policy may be set where none of the files shows, and an unread value
		// degrades to unknown rather than to one that asserts something.
		Policy string `json:"policy" yaml:"policy"`
		// Depth is how far the answer was taken, and it is always declared: the policy is read
		// out of the kubelet's own configuration, and nothing is run against the running kubelet
		// to confirm it. Carried rather than omitted so the answer cannot be read as having been
		// measured.
		Depth device.PreflightDepth `json:"depth" yaml:"depth"`
		// Note says why Policy is unknown, and is empty exactly when a source named the policy.
		// An unknown with no note reads as one fact when it is several -- none of the sources
		// carries the key, one could not be read, or several name different policies -- and only
		// the note says which.
		Note string `json:"note,omitempty" yaml:"note,omitempty"`
	}
)

// PreflightTopology reads this node's kubelet TopologyManager policy and returns the report's
// topology section.
//
// It takes no options and configures nothing: reading the kubelet's configuration is a pure read
// of files under the mounted host root, so there is no action for a dry run to withhold. The host
// root is validated here rather than trusted from the configuration, because a path that merely
// exists is not a host root, and a policy read through such a path would be read out of this
// container's own filesystem -- a policy this node's kubelet never saw. The answer degrades to
// unknown and says that nothing was looked for.
func (p *Preflighter) PreflightTopology() TopologyReport {
	if err := p.host.Validate(); err != nil {
		return unknownTopology(time.Now(),
			"the kubelet's configuration was never looked for, because the host root did not "+
				"validate: "+err.Error())
	}
	return topologyReport(p.host.root, time.Now())
}

// topologyReport reads the kubelet's TopologyManager policy out of the kubelet configuration
// under root and renders the report's topology section.
//
// It reads through readKubeletSetting, the one reader of this node's kubelet configuration, so
// that this reading and the CRI endpoint's cannot drift apart about where a kubelet keeps its
// configuration, what a repeated setting means, or when two configurations are a conflict rather
// than an override. What differs is the stakes, and that is what this function owns: the CRI read
// drives what preflight measures and so refuses a host it cannot interpret, while this one drives
// nothing and degrades to unknown, saying why in the note.
func topologyReport(root string, now time.Time) TopologyReport {
	reading := readKubeletSetting(root, topologyPolicySetting)

	switch {
	case reading.UnsearchableErr != nil:
		// Worded for what happened: no file was matched or opened, so calling this a
		// configuration that could not be read would point at a permissions problem that is
		// not there. It takes a host root that is itself a malformed pattern.
		return unknownTopology(now, fmt.Sprintf(
			"the kubelet configuration search under %s could not run, so which topology "+
				"manager policy its kubelet runs is unknown rather than defaulted: %s",
			reading.UnsearchablePattern, reading.UnsearchableErr))
	case reading.UnreadableErr != nil:
		// These patterns name the kubelet's own configuration paths, so a match that cannot be
		// read may be the one that decides. Reporting the generic unknown would claim every file
		// had been searched, so the reason names the file instead.
		return unknownTopology(now, fmt.Sprintf(
			"this host carries a kubelet configuration at %s that could not be read, so "+
				"which topology manager policy its kubelet runs is unknown rather than "+
				"defaulted: %s", reading.UnreadablePath, reading.UnreadableErr))

	case reading.Conflict != nil:
		// Two distribution trees on one machine are two configurations, and only one of them
		// belongs to the kubelet that is running. Taking either would publish a policy possibly
		// not in force, silently; the unknown names the conflict instead.
		return unknownTopology(now, fmt.Sprintf(
			"the kubelet configuration under %s names more than one topology manager policy "+
				"(%s), and only one of them belongs to the kubelet that is running, so no "+
				"policy is reported rather than a guessed one",
			reading.ConflictPattern, strings.Join(reading.Conflict, ", ")))

	case reading.Found:
		return TopologyReport{
			Timestamp: now, Policy: reading.Value, Depth: device.PreflightDepthDeclared,
		}

	default:
		patterns := make([]string, 0, len(kubeletConfigSources))
		for _, src := range kubeletConfigSources {
			patterns = append(patterns, src.pattern)
		}
		return unknownTopology(now, fmt.Sprintf(
			"none of the places this report reads the kubelet's configuration (%s, the last one "+
				"walked) names a topology manager policy; the kubelet may still run one set on "+
				"its command line, which none of them shows, so its default is not reported "+
				"either: that would be a value nobody read",
			strings.Join(patterns, ", ")))
	}
}

// unknownTopology is the section's answer whenever no policy can be named, whatever kept it from
// being named. The note is the only thing distinguishing the cases, so it is required.
func unknownTopology(now time.Time, note string) TopologyReport {
	return TopologyReport{
		Timestamp: now,
		Policy:    TopologyPolicyUnknown,
		Depth:     device.PreflightDepthDeclared,
		Note:      note,
	}
}
