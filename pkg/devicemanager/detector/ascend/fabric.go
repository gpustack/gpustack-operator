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
// rest is left absent.
func (in *ascend) readFabric(dev dcmi.Device, cardID, deviceID int32, logger klog.Logger) *device.Fabric {
	var spod *dcmi.SpodInfo
	if info, ret := dev.GetSuperPodInfo(); ret.IsSuccess() {
		spod = &info
	} else {
		logger.Info("skipping the fabric domain coordinates", "reason", ret.Error())
	}

	var productType productascend.Type
	if product, err := in.product.Resolve(cardID, deviceID); err != nil {
		logger.Info("skipping the fabric domain shape", "reason", err.Error())
	} else if productType = product.Type; productType == "" {
		logger.Info("the fabric domain has a shape this build has no name for", "productCode", product.Code)
	}

	return newFabric(spod, productType, readFabricEndpoints(dev, logger))
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
	if spod != nil {
		fabric.ID = strconvx.FormatUint(uint64(spod.Super_pod_id), 10)
		fabric.MemberCount = spod.Scale_type
		fabric.NodeIndex = strconvx.FormatUint(uint64(spod.Server_id), 10)
		fabric.RackID = strconvx.FormatUint(uint64(spod.Chassis_id), 10)
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
