package ascend

import (
	"fmt"
	"sync"
)

// hcclTopoFilePathEnv names the file HCCL reads a node's fabric topology from. Only the A5
// generation carries it: the vendor's own device plugin returns early for every other card type,
// and the operator does the same, keyed on family950.
const hcclTopoFilePathEnv = "HCCL_TOPO_FILE_PATH"

// A5 product types, in the vendor's own numbering. They are established two ways: the two inference
// cards are recognized by the mainboard the chip is mounted on, and every other product is whatever
// the super pod reports about itself.
const (
	productTypeServer8P  uint32 = 0
	productType1DPod     uint32 = 1
	productType2DPod     uint32 = 2
	productTypeServer16P uint32 = 3
	productTypeServer32P uint32 = 4
	productTypeCard1P    uint32 = 5
	productTypeCard4P    uint32 = 6
)

// A5 carrier board ids: the mainboard a 300I inference card is mounted on, in its 1P and 4P
// variants. A5 mainboard ids are their own namespace -- the training baseboards are 0x44, 0x46 and
// 0x48 -- and these two are only ever read on a node whose family is already 950, so a match here
// means an inference card and not a coincidence with some other generation's numbering.
const (
	mainboardIDCard1P uint32 = 0x68
	mainboardIDCard4P uint32 = 0x6c
)

// hcclTopoFilePaths is the vendor's product-type-to-topology-file table, as ascend-device-plugin
// ships it.
//
// The directory holding these files is mounted by ascend-docker-runtime rather than by this
// allocator, so what is injected here is the env alone -- which is also why the file is never
// stat'ed: it exists in the container, not in the device-manager.
var hcclTopoFilePaths = map[uint32]string{
	productTypeServer8P:  "/usr/local/Ascend/driver/topo/950/atlas_850_1.json",
	productType1DPod:     "/usr/local/Ascend/driver/topo/950/atlas_950_1.json",
	productType2DPod:     "/usr/local/Ascend/driver/topo/950/atlas_950_2.json",
	productTypeServer16P: "/usr/local/Ascend/driver/topo/950/atlas_850_2.json",
	productTypeServer32P: "/usr/local/Ascend/driver/topo/950/atlas_850_3.json",
	productTypeCard1P:    "/usr/local/Ascend/driver/topo/950/atlas_350_1.json",
	productTypeCard4P:    "/usr/local/Ascend/driver/topo/950/atlas_350_3.json",
}

// topoDriver is the dcmi reader seam behind a _linux.go/_other.go build tag, the shape shareDriver
// has and for the same reason: the darwin stub errors, so the dcmi-free resolution below is
// table-tested with a fake. A device is addressed by the (card, device-in-card) pair dcmi names it
// by, which the detector already recorded on the accelerator.
type topoDriver interface {
	// MainboardID returns the id of the mainboard the device is mounted on.
	MainboardID(cardID, deviceID int32) (uint32, error)
	// SuperPodType returns the product type of the super pod the device sits in.
	SuperPodType(cardID, deviceID int32) (uint32, error)
}

// hcclTopoResolver resolves a node's A5 topology file once and remembers the answer.
//
// Both reads are node-level facts -- which mainboard the chips are mounted on, and what shape the
// super pod is -- so they cannot change while the device-manager runs, and every Allocate would
// otherwise repeat them. Only an answer is remembered: a failed read leaves the resolver unresolved,
// so a driver that was briefly unready is asked again on the next allocation rather than sinking the
// env for the lifetime of the process.
type hcclTopoResolver struct {
	driver topoDriver

	// mu guards the two fields below. One resolver is shared by the exclusive, shared, sliced and
	// visibility servers, each with its own gRPC listener, so two Allocate calls can arrive at once.
	mu          sync.Mutex
	resolved    bool
	path        string
	productType uint32
}

func newHcclTopoResolver(driver topoDriver) *hcclTopoResolver {
	return &hcclTopoResolver{driver: driver}
}

// Resolve returns the topology file this node's A5 accelerators describe their fabric with, along
// with the product type it was chosen by.
//
// The path is empty when the driver answered but named a product the vendor ships no file for. That
// is an answer rather than a failure -- there is no path to inject and no read left to retry -- so
// it is remembered like any other, and the product type comes back with it so the caller can say
// which product had none.
func (r *hcclTopoResolver) Resolve(cardID, deviceID int32) (path string, productType uint32, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.resolved {
		return r.path, r.productType, nil
	}

	if r.productType, err = r.readProductType(cardID, deviceID); err != nil {
		return "", 0, err
	}
	r.path, r.resolved = hcclTopoFilePaths[r.productType], true

	return r.path, r.productType, nil
}

// readProductType establishes which A5 product this node is, in the vendor's own order: the
// mainboard id recognizes the two inference cards, and anything else is what the super pod reports.
//
// The super pod is only consulted when the mainboard did not answer the question, which is what
// keeps a node that is neither from paying for a query it cannot use.
func (r *hcclTopoResolver) readProductType(cardID, deviceID int32) (uint32, error) {
	mainboardID, err := r.driver.MainboardID(cardID, deviceID)
	if err != nil {
		return 0, fmt.Errorf("read mainboard id of card %d device %d: %w", cardID, deviceID, err)
	}
	switch mainboardID {
	case mainboardIDCard1P:
		return productTypeCard1P, nil
	case mainboardIDCard4P:
		return productTypeCard4P, nil
	}

	productType, err := r.driver.SuperPodType(cardID, deviceID)
	if err != nil {
		return 0, fmt.Errorf("read super pod type of card %d device %d: %w", cardID, deviceID, err)
	}

	return productType, nil
}
