package deviceplugin

import (
	"fmt"

	core "k8s.io/api/core/v1"
)

// SlicedCoresPercent reads the per-card compute (SM) percent a sliced container asks
// for from its ".sliced.cores-percentage" request, defaulting to 100 (a whole card's
// compute) when absent or non-positive. Slices may oversubscribe compute, so each
// slice may request up to 100%. It drives HAMi-core CUDA_DEVICE_SM_LIMIT / vcann-rt
// aicore-quota directly (already a percent, no fraction conversion).
func SlicedCoresPercent(ctr *core.Container, coresResName core.ResourceName) int {
	if q, ok := ctr.Resources.Limits[coresResName]; ok {
		if v := q.Value(); v > 0 {
			return int(v)
		}
	}
	return 100
}

// SlicedMemoryMib derives the per-card VRAM limit (MiB) a sliced container asks for,
// given the card's total VRAM (cardVRAMMib). ".sliced.memory-percentage" takes
// precedence — floor(pct/100 * cardVRAMMib) — over the absolute ".sliced.memory-mib";
// the result is capped at the card's VRAM. It errors when neither is set, so a sliced
// allocate fails loudly rather than silently exposing the whole card's VRAM. Memory is
// the non-oversubscribable anchor, so percentage wins here exactly as the Pod webhook
// folds it into ".sliced.units" — keeping the credit accounting and the real limit
// consistent.
func SlicedMemoryMib(ctr *core.Container, memPctResName, memMibResName core.ResourceName, cardVRAMMib int64) (int64, error) {
	if q, ok := ctr.Resources.Limits[memPctResName]; ok {
		if pct := q.Value(); pct > 0 {
			return min(cardVRAMMib*pct/100, cardVRAMMib), nil
		}
	}
	if q, ok := ctr.Resources.Limits[memMibResName]; ok {
		if mib := q.Value(); mib > 0 {
			return min(mib, cardVRAMMib), nil
		}
	}
	return 0, fmt.Errorf("container %q has no %s or %s request", ctr.Name, memPctResName, memMibResName)
}

// ContainerEnvDeclared reports whether the container explicitly declares an env var named
// name in its Env list, so a caller can skip injecting a default and leave that value
// authoritative. Only explicit Env entries are checked; EnvFrom-sourced variables are not.
func ContainerEnvDeclared(ctr *core.Container, name string) bool {
	for i := range ctr.Env {
		if ctr.Env[i].Name == name {
			return true
		}
	}
	return false
}
