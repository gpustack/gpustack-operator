package device

// AggregateAcceleratorSlicedDetail derives a group's slicing detail from its accelerators'
// per-accelerator statuses:
//
//   - the logical count is summed across accelerators, and the overcommit flag is taken from any
//     logically sliceable accelerator (uniform per model, meaningless when none is);
//   - the physical profiles are summed by name, and the physical ceiling is summed across
//     accelerators.
func AggregateAcceleratorSlicedDetail(accelerators []Accelerator) AcceleratorSlicedDetail {
	var detail AcceleratorSlicedDetail
	profileIndex := make(map[string]int)
	for i := range accelerators {
		st := accelerators[i].Status

		detail.Logical.Count += st.LogicalSliced.Count
		if st.LogicalSliced.Count > 0 && st.LogicalSliced.CoresPercentageOvercommit {
			detail.Logical.CoresPercentageOvercommit = true
		}

		detail.Physical.Count += st.PhysicalSliced.Count
		for _, p := range st.PhysicalSliced.Profiles {
			if idx, ok := profileIndex[p.Name]; ok {
				detail.Physical.Profiles[idx].Count += p.Count
				continue
			}
			profileIndex[p.Name] = len(detail.Physical.Profiles)
			detail.Physical.Profiles = append(detail.Physical.Profiles, AcceleratorSlicedPhysicalDetailProfile{
				Name:      p.Name,
				Count:     p.Count,
				MemoryMib: p.MemoryMib,
			})
		}
	}
	return detail
}

// SetGroupSlicedDetails fills each group's AcceleratorSlicedDetail from its accelerators'
// statuses.
// Detectors call it as the final step of accelerator detection.
func SetGroupSlicedDetails(groups DevicesGroupList) {
	for i := range groups {
		groups[i].AcceleratorSlicedDetail = AggregateAcceleratorSlicedDetail(groups[i].Accelerators)
	}
}
