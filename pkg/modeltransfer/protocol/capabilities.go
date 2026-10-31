package protocol

// Tensor ownership modes a backend may declare.
const (
	// OwnershipOwned means every read output has independent storage.
	OwnershipOwned = "owned"
	// OwnershipBorrowed means outputs borrow window storage; a consumer may
	// only use it after proving compatible retention. No backend of this
	// contract version is qualified for borrowed outputs.
	OwnershipBorrowed = "borrowed"
)

// RejectedOption is a requested transport option the backend refused, with
// the reason it gave. Rejected options are reported, never silently
// dropped, so a caller can see the requested path was not taken.
type RejectedOption struct {
	Option string `json:"option"`
	Reason string `json:"reason"`
}

// TransportSelection carries the control-plane and data-plane endpoints,
// which are selected separately: an authenticated reachable control path
// for manifests and identity, and payload endpoints chosen from granted
// devices and a qualified fabric domain.
type TransportSelection struct {
	Control string   `json:"control"`
	Payload []string `json:"payload"`
}

// BackendCapabilities reports what one backend can do after negotiation.
// Requested, Applied, and Effective keep the requested/applied/effective
// backend distinction; EffectiveObserved records whether the effective
// backend was independently observed or only assumed.
type BackendCapabilities struct {
	Requested         []string           `json:"requested"`
	Applied           string             `json:"applied"`
	Effective         string             `json:"effective"`
	EffectiveObserved bool               `json:"effective_observed"`
	Rejected          []RejectedOption   `json:"rejected"`
	Formats           []string           `json:"formats"`
	MemoryTypes       []string           `json:"memory_types"`
	MaxRequestBytes   int64              `json:"max_request_bytes"`
	AlignmentBytes    int64              `json:"alignment_bytes"`
	TensorOwnership   string             `json:"tensor_ownership"`
	DeviceVisibility  []string           `json:"device_visibility"`
	Transport         TransportSelection `json:"transport"`
}

// Validate checks the capability invariants: the applied option was
// requested and not rejected, an unobserved effective backend stays
// "unknown", alignment is a power of two, and no enumeration is empty.
func (c BackendCapabilities) Validate() error {
	if len(c.Requested) == 0 {
		return New(KindInvalidCapabilities, "requested",
			"at least one transport option must be requested")
	}
	requested := make(map[string]bool)
	for _, option := range c.Requested {
		if option == "" {
			return New(KindInvalidCapabilities, "requested",
				"transport options must be non-empty")
		}
		requested[option] = true
	}
	applied := false
	for _, option := range c.Requested {
		if option == c.Applied {
			applied = true
		}
	}
	if !applied {
		return New(KindInvalidCapabilities, "applied",
			"applied option %q was not requested", c.Applied)
	}
	rejected := make(map[string]bool)
	for _, option := range c.Rejected {
		if option.Option == "" || option.Reason == "" {
			return New(KindInvalidCapabilities, "rejected",
				"rejected options carry an option and a reason")
		}
		if !requested[option.Option] {
			return New(KindInvalidCapabilities, "rejected",
				"rejected option %q was not requested", option.Option)
		}
		rejected[option.Option] = true
	}
	if rejected[c.Applied] {
		return New(KindInvalidCapabilities, "applied",
			"option %q is both applied and rejected", c.Applied)
	}
	if c.EffectiveObserved {
		if c.Effective == "" || c.Effective == "unknown" {
			return New(KindInvalidCapabilities, "effective",
				"an observed effective backend must be named")
		}
	} else if c.Effective != "unknown" {
		return New(KindInvalidCapabilities, "effective",
			"an unobserved effective backend must stay unknown")
	}
	if len(c.Formats) == 0 {
		return New(KindInvalidCapabilities, "formats", "must not be empty")
	}
	if len(c.MemoryTypes) == 0 {
		return New(KindInvalidCapabilities, "memory_types", "must not be empty")
	}
	if len(c.DeviceVisibility) == 0 {
		return New(KindInvalidCapabilities, "device_visibility",
			"must not be empty")
	}
	if c.MaxRequestBytes < 1 {
		return New(KindInvalidCapabilities, "max_request_bytes",
			"must be a positive integer")
	}
	if c.AlignmentBytes < 1 || c.AlignmentBytes&(c.AlignmentBytes-1) != 0 {
		return New(KindInvalidCapabilities, "alignment_bytes",
			"must be a power of two")
	}
	if c.TensorOwnership != OwnershipOwned && c.TensorOwnership != OwnershipBorrowed {
		return New(KindInvalidCapabilities, "tensor_ownership",
			"unknown ownership %q", c.TensorOwnership)
	}
	if c.Transport.Control == "" {
		return New(KindInvalidCapabilities, "transport.control",
			"must not be empty")
	}
	if len(c.Transport.Payload) == 0 {
		return New(KindInvalidCapabilities, "transport.payload",
			"must not be empty")
	}
	return nil
}

// LocalBackendCapabilities returns the capabilities of the local
// verified-file reference reader: no vendor runtime, host memory only, one
// directly observed effective backend.
func LocalBackendCapabilities() BackendCapabilities {
	return BackendCapabilities{
		Requested:         []string{"local"},
		Applied:           "local",
		Effective:         "local",
		EffectiveObserved: true,
		Rejected:          nil,
		Formats:           []string{"safetensors"},
		MemoryTypes:       []string{"host"},
		MaxRequestBytes:   1 << 30,
		AlignmentBytes:    1,
		TensorOwnership:   OwnershipOwned,
		DeviceVisibility:  []string{"host"},
		Transport: TransportSelection{
			Control: "local://verified-manifest",
			Payload: []string{"local://file"},
		},
	}
}
