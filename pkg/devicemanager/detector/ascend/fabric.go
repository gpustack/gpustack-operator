package ascend

import (
	"encoding/hex"

	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/binding/dcmi"
	"gpustack.ai/gpustack/pkg/device"
	productascend "gpustack.ai/gpustack/pkg/devicemanager/product/ascend"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

// fabricKindUB names the A5 interconnect in a fabric record, in the vendor's own word for it:
// unified bus. It is what makes the domain id beside it comparable to another A5 worker's and not to
// an NVLink or XGMI one.
const fabricKindUB = "ub"

// urmaDeviceCountMax bounds the endpoint enumeration below. It is the vendor's own limit for the
// same count; a driver reporting more than this is disbelieved rather than iterated.
const urmaDeviceCountMax = 128

// readFabric reads one A5 accelerator's place on the UB fabric: the super pod it sits in, the shape
// of that pod, and the accelerator's own endpoints.
//
// Every read is best-effort, and a failure is reported at Info rather than Error, because none of
// them is required for the accelerator to be detected or allocated -- they are what a consumer
// builds a communication plan from, and a worker whose driver answers none of them still profiles
// and still serves workloads that do not span machines.
//
// The three are independent on purpose: a standalone inference card names its shape from its
// mainboard and may sit in no super pod at all, so whichever of them answered is recorded and the
// rest is left absent. Two of them survive a pass that could not read them -- the shape because the
// resolver remembers it, the coordinates because rememberSuperPod does -- since a label withheld
// here is a label removed, and a read that did not happen cannot support that.
func (in *ascend) readFabric(dev dcmi.Device, cardID, deviceID int32, logger klog.Logger) *device.Fabric {
	var spod *dcmi.SpodInfo
	if info, ret := dev.GetSuperPodInfo(); ret.IsSuccess() {
		spod = &info
	} else {
		logger.Info("could not read the fabric domain coordinates",
			"reason", ret.Error(),
			"consequence", "reusing whichever this accelerator last answered with, if any")
	}
	spod = in.rememberSuperPod(superPodKey{cardID: cardID, deviceID: deviceID}, spod)

	var productType productascend.Type
	if product, err := in.product.Resolve(cardID, deviceID); err != nil {
		logger.Info("skipping the fabric domain shape", "reason", err.Error())
	} else if productType = product.Type; productType == "" {
		logger.Info("the fabric domain has a shape this build has no name for", "productCode", product.Code)
	}

	return newFabric(spod, productType, readFabricEndpoints(dev, logger))
}

// superPodKey addresses one accelerator by the (card, device-in-card) pair dcmi names it by, which
// is stable for the lifetime of the process.
type superPodKey struct {
	cardID   int32
	deviceID int32
}

// rememberSuperPod records the coordinates a successful read answered with, and returns the ones
// this pass should publish.
//
// A failed read publishes the last coordinates this accelerator answered with, if it ever answered
// any. The node-wide fabric label is removed when it is withheld, so reporting none on a pass the
// driver refused would take the node out of its super pod on one dcmi hiccup and only put it back at
// the next detect pass -- which the detect floor can hold off for minutes. This is the rule the
// interface inventory already follows: only a read that happened may replace what was recorded.
//
// A node that really left its super pod is a different answer rather than no answer -- the driver
// reports the invalid markers, which Type.InSuperPod refuses -- so the withdrawal still happens, and
// happens through a read that succeeded.
func (in *ascend) rememberSuperPod(key superPodKey, spod *dcmi.SpodInfo) *dcmi.SpodInfo {
	in.superPodsMu.Lock()
	defer in.superPodsMu.Unlock()

	if spod != nil {
		in.superPods[key] = *spod
		return spod
	}

	remembered, ok := in.superPods[key]
	if !ok {
		return nil
	}

	return &remembered
}

// readFabricEndpoints lists this accelerator's UB endpoint identifiers, across every urma device --
// the fabric's function entities -- that it exposes.
//
// One unreadable urma device does not sink the rest: the endpoints are a list a consumer filters, so
// a shorter list is a degraded answer while no list at all is none.
func readFabricEndpoints(dev dcmi.Device, logger klog.Logger) []string {
	count, ret := dev.GetUrmaDeviceCount()
	if !ret.IsSuccess() {
		logger.Info("skipping the fabric endpoints", "reason", ret.Error())
		return nil
	}
	if count > urmaDeviceCountMax {
		logger.Info("skipping the fabric endpoints, the urma device count is out of range",
			"count", count, "max", urmaDeviceCountMax)
		return nil
	}

	var endpoints []string
	for i := uint32(0); i < count; i++ {
		eids, ret := dev.GetEidList(i)
		if !ret.IsSuccess() {
			logger.Info("skipping one urma device's endpoints", "urmaDevice", i, "reason", ret.Error())
			continue
		}
		endpoints = append(endpoints, fabricEndpoints(eids)...)
	}

	return endpoints
}

// fabricEndpoints renders one urma device's endpoint identifiers the way the vendor publishes them:
// all 16 bytes, lowercase hex, unparsed.
//
// The encoding is load-bearing rather than cosmetic. A consumer derives the function entity, the
// die, the port and whether the endpoint carries device-to-device traffic by indexing into these
// characters, so a truncated value or a different case is not a cosmetic difference -- it is an
// endpoint that decodes to something else.
func fabricEndpoints(eids []dcmi.UrmaEidInfo) []string {
	endpoints := make([]string, 0, len(eids))
	for i := range eids {
		endpoints = append(endpoints, hex.EncodeToString(eids[i].Eid[:]))
	}

	return endpoints
}

// newFabric renders the published record from what the driver answered, and reports nil when it
// answered nothing at all.
//
// An empty ID beside a shape or a set of endpoints is a real outcome rather than a half-written
// record: it says this accelerator is on a UB fabric whose domain could not be identified, and the
// endpoints next to it are still what a communication plan is built from. Suppressing the whole
// record there would discard them.
func newFabric(spod *dcmi.SpodInfo, productType productascend.Type, endpoints []string) *device.Fabric {
	if spod == nil && productType == "" && len(endpoints) == 0 {
		return nil
	}

	fabric := &device.Fabric{
		Kind:      fabricKindUB,
		Type:      string(productType),
		Endpoints: endpoints,
	}
	// The coordinates are published only where the driver's answer is a membership. It answers the
	// query for a plain server too -- that answer is how Resolver established the shape -- so taking
	// it at face value would hand two machines with no interconnect one domain, while reading the
	// shape alone would deny a real 8P super server the domain it is in. Which shapes may carry one,
	// and which answers are the vendor's markers for "not in a super pod", is Type.InSuperPod.
	if spod != nil && productType.InSuperPod(spod.Super_pod_id, spod.Scale_type) {
		fabric.ID = strconvx.FormatUint(uint64(spod.Super_pod_id), 10)
		// Each of the other three carries its own check, because membership is established by the id
		// and says nothing about what else the driver managed to fill in. A pod shape is in its
		// domain whatever size it reports; the size, the server index and the chassis are separate
		// answers, and publishing the invalid marker for any of them advertises a domain of four
		// billion members, or this machine as its four-billionth. Absent is how each of them is
		// spelled when nobody reported it.
		if spod.Scale_type != productascend.InvalidSuperPodSize {
			fabric.MemberCount = spod.Scale_type
		}
		if spod.Server_id != productascend.InvalidSuperPodCoordinate {
			fabric.NodeIndex = strconvx.FormatUint(uint64(spod.Server_id), 10)
		}
		if spod.Chassis_id != productascend.InvalidSuperPodCoordinate {
			fabric.RackID = strconvx.FormatUint(uint64(spod.Chassis_id), 10)
		}
	}

	return fabric
}

// productDriver adapts the dcmi handle this detector already holds to the shared resolver's seam.
// No build tag is needed: unlike the allocator, this package does not link Go's plugin package, so
// cgo binding/dcmi loads in a darwin test binary here.
type productDriver struct {
	lib *dcmi.DCMI
}

func (d productDriver) MainboardID(cardID, deviceID int32) (uint32, error) {
	id, ret := d.lib.GetDeviceHandleByCardAndIndex(cardID, deviceID).GetMainboardId()
	if !ret.IsSuccess() {
		return 0, ret
	}
	return id, nil
}

func (d productDriver) SuperPodType(cardID, deviceID int32) (uint32, error) {
	info, ret := d.lib.GetDeviceHandleByCardAndIndex(cardID, deviceID).GetSuperPodInfo()
	if !ret.IsSuccess() {
		return 0, ret
	}
	return uint32(info.Super_pod_type), nil
}
