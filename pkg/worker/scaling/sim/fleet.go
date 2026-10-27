package sim

import "sort"

// FleetConfig fixes synthetic admission, readiness and retirement delays.
type FleetConfig struct {
	Initial           int
	Quota             int
	AdmissionSeconds  float64
	BootSeconds       float64
	RetirementSeconds float64
}

// Replica describes a fake lifecycle observation, not a scheduler or drain guarantee.
type Replica struct {
	Ordinal    int
	CreatedAt  float64
	Admitted   bool
	AdmittedAt float64
	Ready      bool
	Retiring   bool
	RetireAt   float64
}

// Fleet tracks replica lifetimes independently of planning policy.
type Fleet struct {
	config   FleetConfig
	replicas map[int]Replica
	Retired  int
	Aborted  int
}

// NewFleet starts with the configured replicas already admitted and ready.
func NewFleet(config FleetConfig) *Fleet {
	f := &Fleet{config: config, replicas: map[int]Replica{}}
	for i := 0; i < config.Initial; i++ {
		f.replicas[i] = Replica{Ordinal: i, Admitted: true, Ready: true, AdmittedAt: -config.BootSeconds}
	}
	return f
}

// SetDesired begins creation or retirement without making new capacity immediately ready.
// Never-admitted replicas are canceled immediately without graceful retirement.
func (f *Fleet) SetDesired(count int, now float64) {
	for i := 0; i < count; i++ {
		r, ok := f.replicas[i]
		if !ok {
			r = Replica{Ordinal: i, CreatedAt: now}
		}
		if r.Retiring {
			f.Aborted++
			r.Retiring = false
		}
		f.replicas[i] = r
	}
	for i, r := range f.replicas {
		if i >= count && !r.Retiring {
			if !r.Admitted {
				delete(f.replicas, i)
				continue
			}
			r.Retiring = true
			r.RetireAt = now + f.config.RetirementSeconds
			f.replicas[i] = r
		}
	}
}

// Advance applies deterministic lifecycle events in ordinal order within quota.
func (f *Fleet) Advance(now float64) {
	for i, r := range f.replicas {
		if r.Retiring && now >= r.RetireAt {
			delete(f.replicas, i)
			f.Retired++
		}
	}
	admitted := 0
	for _, r := range f.replicas {
		if r.Admitted {
			admitted++
		}
	}
	for _, r := range f.Replicas() {
		if !r.Admitted && !r.Retiring && admitted < f.config.Quota && now >= r.CreatedAt+f.config.AdmissionSeconds {
			r.Admitted = true
			r.AdmittedAt = now
			admitted++
		}
		if r.Admitted && now >= r.AdmittedAt+f.config.BootSeconds {
			r.Ready = true
		}
		f.replicas[r.Ordinal] = r
	}
}

// Replicas returns a stable ordered snapshot, including retiring replicas.
func (f *Fleet) Replicas() []Replica {
	result := make([]Replica, 0, len(f.replicas))
	for _, r := range f.replicas {
		result = append(result, r)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ordinal < result[j].Ordinal })
	return result
}

// Ready returns the ordinals accepting new work; retiring replicas are excluded.
func (f *Fleet) Ready() []int {
	var result []int
	for _, r := range f.Replicas() {
		if r.Ready && !r.Retiring {
			result = append(result, r.Ordinal)
		}
	}
	return result
}

// Draining returns replicas that may finish existing requests during retirement.
func (f *Fleet) Draining() []int {
	var result []int
	for _, r := range f.Replicas() {
		if r.Ready && r.Retiring {
			result = append(result, r.Ordinal)
		}
	}
	return result
}

// Ordinals returns all existing replicas, including pending and retiring replicas.
func (f *Fleet) Ordinals() []int {
	result := make([]int, 0, len(f.replicas))
	for _, r := range f.Replicas() {
		result = append(result, r.Ordinal)
	}
	return result
}
