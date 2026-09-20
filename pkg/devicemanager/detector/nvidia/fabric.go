package nvidia

import (
	"encoding/hex"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/nvml"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// fabricKindNVLink names the NVLink interconnect in a fabric record. It is what makes the domain id
// beside it comparable to another NVLink worker's and not to an Ascend UB or AMD XGMI one.
const fabricKindNVLink = "nvlink"

// fabricInfo is the fabric registration this detector reads, in the one shape both of the driver's
// struct versions reduce to. Version 2 adds a health mask nothing here consumes, so the two versions
// share a renderer rather than each rendering a record of its own.
type fabricInfo struct {
	clusterUUID [nvml.GPU_FABRIC_UUID_LEN]uint8
	status      nvml.Return
	cliqueID    uint32
	state       uint8
}

// readFabric reads one accelerator's place on the NVLink fabric: the cluster it has registered with
// and the clique inside that cluster it can actually address.
//
// The read is best-effort and a failure is not reported as one, because an accelerator whose driver
// never answers still profiles and still serves workloads that do not span machines. A card whose
// generation has no fabric does not fail this read at all: the driver answers it, reporting the
// state as not-supported, which is why the gate below rather than the return code is what withholds
// the record on an ordinary card.
func (in *nvidia) readFabric(dev nvml.Device, uuid string, logger klog.Logger) *device.Fabric {
	read := readFabricInfo(dev, logger)
	info := in.rememberFabric(uuid, read)

	fabric := newFabric(info)
	// Only a reading this pass took is reported. What rememberFabric returns after a refused read is
	// the last answer the accelerator gave, and naming its state here would date-stamp an older
	// reading with this pass -- while a refused read has already reported itself above.
	if fabric == nil && read != nil {
		// Without this line, a card whose generation has no fabric and one stuck part-way through
		// registering are indistinguishable everywhere a consumer can look: the driver answers both
		// reads successfully, so the read path reports nothing, and both publish no record at all.
		// The published object carries no registration state and gains no field for one here, so
		// this is where that difference is readable.
		logger.V(3).Info("recorded no fabric domain for an accelerator that is not registered with a fabric",
			"registration", fabricStateName(read.state),
			"status", read.status.String(),
			"clusterIdentified", read.clusterUUID != [nvml.GPU_FABRIC_UUID_LEN]uint8{})
	}

	return fabric
}

// fabricStateName names a registration state for a log line. The number alone does not separate
// "this generation has no fabric" from "registration has not started", which is the one distinction
// the line exists to carry.
func fabricStateName(state uint8) string {
	switch state {
	case nvml.GPU_FABRIC_STATE_NOT_SUPPORTED:
		return "not-supported"
	case nvml.GPU_FABRIC_STATE_NOT_STARTED:
		return "not-started"
	case nvml.GPU_FABRIC_STATE_IN_PROGRESS:
		return "in-progress"
	case nvml.GPU_FABRIC_STATE_COMPLETED:
		return "completed"
	}
	// A value outside the vendor's enumeration is reported as what it is rather than folded into one
	// of the four, because a driver reporting one means this build's understanding is out of date.
	return "unrecognized-" + strconvx.FormatUint(uint64(state), 10)
}

// readFabricInfo asks the driver for one accelerator's fabric registration, newest struct version
// first.
//
// Version 1 is the one the vendor marks for removal, so it is the fallback rather than the entry
// point: a driver serving only the newer call still answers here, and one serving only the older
// call still does. What the fallback costs is on the cards that have no fabric at all -- two
// refusals per pass instead of one, neither of which is a failure.
func readFabricInfo(dev nvml.Device, logger klog.Logger) *fabricInfo {
	handler := dev.GetGpuFabricInfoV()

	newer, newerRet := handler.V2()
	if newerRet.IsSuccess() {
		return &fabricInfo{
			clusterUUID: newer.ClusterUuid,
			status:      nvml.Return(newer.Status),
			cliqueID:    newer.CliqueId,
			state:       newer.State,
		}
	}

	info, ret := handler.V1()
	if !ret.IsSuccess() {
		logger.V(3).Info("recorded no fabric domain for a card whose driver could not answer for it",
			"reason", ret.Error(), "newerEntryPoint", newerRet.Error())
		return nil
	}

	// The fallback answered where the newer call did not, and this is the only place that difference
	// is visible: both produce the same record, so nothing downstream can tell which one ran. On a
	// driver that exports both -- every driver this was written against does -- it means the newer
	// path stopped working rather than being absent, and a struct that no longer matches what the
	// driver writes is exactly the failure that would otherwise reach a consumer as silence.
	logger.V(3).Info("read the fabric registration through the older entry point",
		"reason", newerRet.Error())

	return &fabricInfo{
		clusterUUID: info.ClusterUuid,
		status:      nvml.Return(info.Status),
		cliqueID:    info.CliqueId,
		state:       info.State,
	}
}

// rememberFabric records the registration a successful read answered with, and returns the one this
// pass should publish.
//
// A failed read publishes the last registration this accelerator answered with, if it ever answered
// any. The node-wide fabric label is removed when it is withheld, so reporting none on a pass the
// driver refused would take the node out of its domain on one NVML hiccup and only put it back at
// the next detect pass -- which the detect floor can hold off for minutes. This is the rule the
// interface inventory already follows: only a read that happened may replace what was recorded.
//
// An accelerator that really left the fabric is a different answer rather than no answer -- the
// driver reports a registration state that is not complete, which newFabric refuses -- so the
// withdrawal still happens, and happens through a read that succeeded.
func (in *nvidia) rememberFabric(uuid string, info *fabricInfo) *fabricInfo {
	in.fabricsMu.Lock()
	defer in.fabricsMu.Unlock()

	if info != nil {
		in.fabrics[uuid] = *info
		return info
	}

	remembered, ok := in.fabrics[uuid]
	if !ok {
		return nil
	}

	return &remembered
}

// newFabric renders the published record from the driver's answer, and reports nil where that answer
// does not identify a domain.
//
// Unlike the UB record, which publishes a kind and a set of endpoints beside a domain it could not
// identify, there is nothing here to publish without one: this manufacturer reports no shape, no
// size and no endpoints, so a record carrying only the kind would say "this card is on some NVLink
// fabric" -- which every NVLink card is -- and a node-level domain would still be absent.
func newFabric(info *fabricInfo) *device.Fabric {
	if info == nil {
		return nil
	}

	// The registration state is the gate, and passing it is what makes the two identifiers mean
	// anything: the cluster uuid and the clique id are filled in by the fabric manager when the
	// accelerator finishes registering. A generation that has no fabric, one that has not started
	// registering and one still negotiating all leave whatever the driver put in those bytes, and
	// publishing that would put machines that share no interconnect into one domain -- while this id
	// is compared across workers, which is the only reason it is published at all.
	if info.state != nvml.GPU_FABRIC_STATE_COMPLETED {
		return nil
	}
	// The status beside the state reports whether the registration that completed succeeded, and the
	// vendor's own rule is to read it only once the state says it completed. A registration that
	// failed identifies no cluster.
	if info.status != nvml.SUCCESS {
		return nil
	}
	// An all-zero cluster uuid is the shape of a buffer nobody filled rather than the identity of a
	// cluster, and it is refused for the reason the states above are: every machine whose driver
	// leaves it zero would otherwise report one domain shared with all the others.
	if info.clusterUUID == ([nvml.GPU_FABRIC_UUID_LEN]uint8{}) {
		return nil
	}

	return &device.Fabric{
		Kind: fabricKindNVLink,
		// All 16 bytes, in order, lowercase hex, undelimited -- 32 characters. The encoding is
		// pinned for two reasons. The value is compared across workers, so two workers rendering the
		// same bytes differently are two domains; and it is a budget, because the node label built
		// from this is `<kind>-<id>-<clique>` and a rendering longer than 63 characters is not
		// truncated but withheld whole, which takes the node out of its domain silently. The budget:
		// 7 for `nvlink-`, 32 here, 1 separator and at most 10 for a uint32 clique is 50, leaving
		// 13. The vendor's own tooling renders these bytes as a dashed uuid instead, which would
		// spend 4 more of that margin and make the value's segment count manufacturer-specific.
		ID: hex.EncodeToString(info.clusterUUID[:]),
		// Zero is a real clique rather than an absent answer, so it is published like any other. Two
		// accelerators sharing the cluster uuid but not this number are on one fabric and still
		// cannot address each other.
		CliqueID: strconvx.FormatUint(uint64(info.cliqueID), 10),
	}
}
