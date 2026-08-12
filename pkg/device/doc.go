// Package device is this repository's vocabulary root. It holds the type aliases onto the
// worker API that describe what a node carries, plus the Detector and Allocator interfaces
// the Device Manager implements, and it states the layering the accelerator packages share.
//
// Read the diagram as one sentence: the Device Manager manages Devices; Kubernetes consumes
// them as Resources.
//
//	                    ┌────────────────────────────────────────────────┐
//	                    │  Device — what the node carries                │
//	  manages           │                                                │
//	DeviceManager ─────▶│    ├── Accelerator   GPU / TPU / XPU / NPU     │
//	(detector,          │    └── (future) IB port, Link port, …          │
//	 allocator)         │                                                │
//	                    └────────────────────────────────────────────────┘
//	                                       │
//	                                       │  maps onto
//	                                       ▼
//	                    ┌────────────────────────────────────────────────┐
//	  consumes          │  Resource — how Kubernetes sees a Device       │
//	DevicePlugin ──────▶│                                                │
//	Controllers         │    Resource{Group, Device}                     │
//	                    │    ResourceToken = Resource + Index            │
//	                    └────────────────────────────────────────────────┘
//
// The words, and the layer each belongs to:
//
//   - Device — anything the node carries that the Device Manager manages.
//   - Accelerator — a Device usable for compute acceleration: GPU, TPU, XPU or NPU. It is
//     the default word for the physical unit of accounting. Other kinds of Device
//     (InfiniBand ports, certain Link ports) may follow.
//   - Resource — the Kubernetes-side view of a Device: what the device plugin and the
//     controllers name when consuming one. It is correctly named for its layer, and the
//     hardware layer never speaks it.
//   - "card" — a manufacturer-hardware term, admissible only where a manufacturer SDK
//     models a card as something other than exactly one Accelerator: the Ascend DCMI card
//     that contains several devices, and the T-Head device-node ordinal.
//   - manufacturer — the company. Its native code is the manufacturer's library, SDK or
//     binding.
//
// docs/architecture/discovery.md carries the same diagram for the reader coming from the
// documentation rather than the code.
package device
