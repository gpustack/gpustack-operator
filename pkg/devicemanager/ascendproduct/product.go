// Package ascendproduct resolves which A5 product a node is: a super pod of one shape or another, a
// server, or one of the two inference cards.
//
// It is the one Ascend fact both halves of the device manager need. The allocator names the HCCL
// fabric topology file from it, and the detector publishes it so a consumer can tell which shape of
// rank table this node belongs in -- and the rule that establishes it is the vendor's own two-step,
// which must not exist in two places and drift.
//
// It is its own package for a linkage reason rather than a taxonomic one. The allocator imports
// pkg/deviceplugin, which links Go's plugin package, and that is what makes cgo binding/dcmi abort at
// dyld load in a darwin test binary; the detector links dcmi there quite happily. So neither package
// can host the rule for the other: put it in the detector and the allocator drags dcmi in, put it in
// the allocator and the detector drags plugin in, and each breaks the other's darwin build. Hence a
// dcmi-free package that both compose, each over its own driver.
package ascendproduct

import (
	"fmt"
	"sync"
)

// Family is the device family the detector gives every A5 soc, and the gate every caller here is
// behind: A5 soc names are an open set (Ascend950PR, Ascend950DT) that the detector collapses onto
// one family by prefix, and no older generation declares any of the queries this package makes.
const Family = "950"

// Type is an A5 product's shape, as a word.
//
// A word rather than the vendor's bare number because "pod, server or card" is what both consumers
// actually ask: the allocator picks a topology file per shape, and a rank-table consumer picks its
// level-0 identity and its UB function-entity ids by whether the node is in a pod, in a plain server
// or is a standalone card.
type Type string

const (
	// TypeServer8P is an eight-chip server that is not part of a super pod.
	TypeServer8P Type = "server-8p"
	// TypePod1D is a super pod wired in one dimension.
	TypePod1D Type = "pod-1d"
	// TypePod2D is a super pod wired in two dimensions.
	TypePod2D Type = "pod-2d"
	// TypeServer16P is a sixteen-chip server that is not part of a super pod.
	TypeServer16P Type = "server-16p"
	// TypeServer32P is a thirty-two-chip server that is not part of a super pod.
	TypeServer32P Type = "server-32p"
	// TypeCard1P is a single-chip inference card, recognized by its mainboard.
	TypeCard1P Type = "card-1p"
	// TypeCard4P is a four-chip inference card, recognized by its mainboard.
	TypeCard4P Type = "card-4p"
)

// The vendor's own product-type numbering, which the super pod reports itself by.
const (
	codeServer8P  uint32 = 0
	codePod1D     uint32 = 1
	codePod2D     uint32 = 2
	codeServer16P uint32 = 3
	codeServer32P uint32 = 4
	codeCard1P    uint32 = 5
	codeCard4P    uint32 = 6
)

// A5 carrier board ids: the mainboard a 300I inference card is mounted on, in its 1P and 4P
// variants. A5 mainboard ids are their own namespace -- the training baseboards are 0x44, 0x46 and
// 0x48 -- and these two are only ever read on a node whose family is already 950, so a match here
// means an inference card and not a coincidence with some other generation's numbering.
const (
	mainboardIDCard1P uint32 = 0x68
	mainboardIDCard4P uint32 = 0x6c
)

// typeCodes names each of the vendor's product-type numbers. A number absent from it is a product
// this package has no word for, which Resolve reports as an empty Type beside the number itself.
var typeCodes = map[uint32]Type{
	codeServer8P:  TypeServer8P,
	codePod1D:     TypePod1D,
	codePod2D:     TypePod2D,
	codeServer16P: TypeServer16P,
	codeServer32P: TypeServer32P,
	codeCard1P:    TypeCard1P,
	codeCard4P:    TypeCard4P,
}

// Product is what a node answered about its own shape.
//
// Code travels beside Type so that a product this package has no word for is still diagnosable and
// still forward-compatible: a caller with nothing to do for an unnamed shape can say which number it
// was, rather than reporting an empty string and leaving its reader nothing to look up.
type Product struct {
	// Type is the product's shape, empty when the driver named a number this package does not know.
	Type Type
	// Code is the vendor's own product-type number, always exactly what the driver said.
	Code uint32
}

// Driver is the two node-level reads establishing a product, as a seam.
//
// A device is addressed by the (card, device-in-card) pair dcmi names it by, which both consumers
// already hold. The interface exists so this package stays dcmi-free and table-testable: the
// allocator composes it over a build-tagged dcmi reader, the detector over the handle it already
// has.
type Driver interface {
	// MainboardID returns the id of the mainboard the device is mounted on.
	MainboardID(cardID, deviceID int32) (uint32, error)
	// SuperPodType returns the product type of the super pod the device sits in.
	SuperPodType(cardID, deviceID int32) (uint32, error)
}

// Resolver establishes a node's A5 product once and remembers the answer.
//
// Both reads are node-level facts -- which mainboard the chips are mounted on, and what shape the
// super pod is -- so they cannot change while the device manager runs, and every caller would
// otherwise repeat them. Only an answer is remembered: a failed read leaves the resolver unresolved,
// so a driver that was briefly unready is asked again rather than sinking the answer for the
// lifetime of the process.
type Resolver struct {
	driver Driver

	// mu guards the two fields below. One resolver is shared by the allocator's exclusive, shared,
	// sliced and visibility servers, each with its own gRPC listener, so two callers can arrive at
	// once.
	mu       sync.Mutex
	resolved bool
	product  Product
}

func NewResolver(driver Driver) *Resolver {
	return &Resolver{driver: driver}
}

// Resolve returns the product this node is, reading the driver on the first call and answering from
// memory afterwards.
func (r *Resolver) Resolve(cardID, deviceID int32) (Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.resolved {
		return r.product, nil
	}

	product, err := r.read(cardID, deviceID)
	if err != nil {
		return Product{}, err
	}
	r.product, r.resolved = product, true

	return r.product, nil
}

// read applies the vendor's two-step in its own order: the mainboard id recognizes the two inference
// cards, and anything else is whatever the super pod reports about itself.
//
// The super pod is only consulted when the mainboard did not answer the question, which is what
// keeps a card from paying for a query it cannot use.
func (r *Resolver) read(cardID, deviceID int32) (Product, error) {
	mainboardID, err := r.driver.MainboardID(cardID, deviceID)
	if err != nil {
		return Product{}, fmt.Errorf("read mainboard id of card %d device %d: %w", cardID, deviceID, err)
	}
	switch mainboardID {
	case mainboardIDCard1P:
		return Product{Type: TypeCard1P, Code: codeCard1P}, nil
	case mainboardIDCard4P:
		return Product{Type: TypeCard4P, Code: codeCard4P}, nil
	}

	code, err := r.driver.SuperPodType(cardID, deviceID)
	if err != nil {
		return Product{}, fmt.Errorf("read super pod type of card %d device %d: %w", cardID, deviceID, err)
	}

	return Product{Type: typeCodes[code], Code: code}, nil
}
