// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dmi

// Device is a handle to something the library will answer questions about: either a physical DCU or
// one MIG instance carved out of one. IsMigDeviceHandle tells the two apart, and the same queries --
// memory, utilization -- are served for both, which is what makes per-instance metrics possible.
type Device struct {
	handle dmiDevice
	lib    *DMI
}

// GpuInstance is a handle to a GPU instance: a card's memory and compute carved off as a unit, which
// compute instances are then created inside.
type GpuInstance struct {
	handle dmiGpuInstance
	lib    *DMI
}

// ComputeInstance is a handle to a compute instance, the unit a workload actually runs on. Each one
// corresponds to exactly one MIG device.
type ComputeInstance struct {
	handle dmiComputeInstance
	lib    *DMI
}

// maxInstancesPerQuery bounds the buffers handed to the two array-filling queries. A card carries
// four GPU slices, so neither its GPU instances of one profile nor the compute instances inside one
// of them can exceed that; the headroom is there so a future card with a finer split does not
// silently truncate.
const maxInstancesPerQuery = 32

// GetIndex reports the device's enumeration index.
func (d Device) GetIndex() (uint32, Return) {
	var index uint32
	ret := nvmlDeviceGetIndex(d.handle, &index)
	return index, ret
}

// GetMemoryInfo reports total, free and used memory.
//
// Asked of a MIG device handle it reports that instance's own memory rather than its card's, which
// is what makes it usable as a per-instance figure.
func (d Device) GetMemoryInfo() (Memory, Return) {
	var mem Memory
	ret := nvmlDeviceGetMemoryInfo(d.handle, &mem)
	return mem, ret
}

// GetUtilizationRates reports compute and memory utilization as percentages.
//
// This is the one entry point the vendor header does not declare, though the shared object exports
// it; the wrapper asserts NVML's signature for it. Asked of a MIG device handle it reports that
// instance's own utilization: on a card holding four instances where one ran a matmul loop, that one
// answered 95% while its three idle siblings answered 0%. It is therefore the source of the
// per-instance compute figure, and the only one -- nothing else on this API measures compute.
func (d Device) GetUtilizationRates() (Utilization, Return) {
	var util Utilization
	ret := nvmlDeviceGetUtilizationRates(d.handle, &util)
	return util, ret
}

// GetMigMode reports the device's current and pending Multi-Instance mode.
//
// The mode is set for the whole node, so every card of a host answers alike; see
// DMI.GetSystemMigMode.
func (d Device) GetMigMode() (current, pending uint32, ret Return) {
	ret = nvmlDeviceGetMigMode(d.handle, &current, &pending)
	return current, pending, ret
}

// GetGpuInstanceProfileInfoBySliceCount returns the GPU-instance profile occupying sliceCount GPU
// slices, if the card offers one.
//
// The argument is named for what it is because the vendor's is not: the underlying call's `profile`
// parameter is an index into a fixed slice-count enumeration -- 0 asks for the one-slice profile, 3
// for the four-slice one -- and is NOT the profile id, which comes back inside the answer and bears
// no relation to it. On a measured card, index 0 returned profile id 3, index 1 returned id 1, index
// 3 returned id 0, and index 2 returned INVALID_ARGUMENT because that card offers no three-slice
// profile. A caller that passed a profile id here would ask for the wrong profile or none.
//
// A card that offers no profile of this width answers with one of the absent codes; see
// Return.ReportsAbsent. That is routine, not a fault.
func (d Device) GetGpuInstanceProfileInfoBySliceCount(sliceCount uint32) (GpuInstanceProfileInfo, Return) {
	var info GpuInstanceProfileInfo
	if sliceCount == 0 || sliceCount > GPU_INSTANCE_PROFILE_COUNT {
		return info, ERROR_INVALID_ARGUMENT
	}
	ret := nvmlDeviceGetGpuInstanceProfileInfo(d.handle, sliceCount-1, &info)
	return info, ret
}

// GetGpuInstancePossiblePlacements returns every placement the profile may legally occupy on an
// empty card. The set is a property of the profile and the card, not of what is currently carved, so
// a caller deciding where to put a new instance has to subtract the occupied intervals itself.
func (d Device) GetGpuInstancePossiblePlacements(profileID uint32) ([]GpuInstancePlacement, Return) {
	placements := make([]GpuInstancePlacement, maxInstancesPerQuery)
	count := uint32(len(placements))
	ret := nvmlDeviceGetGpuInstancePossiblePlacements(d.handle, profileID, &placements[0], &count)
	if !ret.IsSuccess() {
		return nil, ret
	}
	if count > uint32(len(placements)) {
		return nil, ERROR_INSUFFICIENT_SIZE
	}
	return placements[:count], ret
}

// GetGpuInstanceRemainingCapacity reports how many more instances of the profile the card can still
// hold. It accounts for what other profiles already occupy: on a card whose two-slice instance held
// slices 0 and 1, the one-slice profile correctly reported two remaining.
func (d Device) GetGpuInstanceRemainingCapacity(profileID uint32) (uint32, Return) {
	var count uint32
	ret := nvmlDeviceGetGpuInstanceRemainingCapacity(d.handle, profileID, &count)
	return count, ret
}

// GetGpuInstances returns the GPU instances of ONE profile that currently exist on the card.
//
// It filters by profile id, so a caller wanting every instance on a card has to ask once per profile
// the card offers. Asking for one id and reading the answer as "the card's instances" would miss
// every instance of a different width.
func (d Device) GetGpuInstances(profileID uint32) ([]GpuInstance, Return) {
	handles := make([]dmiGpuInstance, maxInstancesPerQuery)
	count := uint32(len(handles))
	ret := nvmlDeviceGetGpuInstances(d.handle, profileID, &handles[0], &count)
	if !ret.IsSuccess() {
		return nil, ret
	}
	if count > uint32(len(handles)) {
		return nil, ERROR_INSUFFICIENT_SIZE
	}
	out := make([]GpuInstance, count)
	for i := range out {
		out[i] = GpuInstance{handle: handles[i], lib: d.lib}
	}
	return out, ret
}

// CreateGpuInstance creates an instance of the profile wherever the driver chooses to put it.
func (d Device) CreateGpuInstance(profileID uint32) (GpuInstance, Return) {
	var handle dmiGpuInstance
	ret := nvmlDeviceCreateGpuInstance(d.handle, profileID, &handle)
	return GpuInstance{handle: handle, lib: d.lib}, ret
}

// CreateGpuInstanceWithPlacement creates an instance of the profile at a chosen placement.
//
// This is the form the allocator uses. Letting the driver choose would make the instance's position
// unpredictable, and position is what a caller reconstructing ownership after a restart matches on.
func (d Device) CreateGpuInstanceWithPlacement(profileID uint32, placement GpuInstancePlacement) (GpuInstance, Return) {
	var handle dmiGpuInstance
	ret := nvmlDeviceCreateGpuInstanceWithPlacement(d.handle, profileID, &placement, &handle)
	return GpuInstance{handle: handle, lib: d.lib}, ret
}

// GetMaxMigDeviceCount reports how many MIG devices this card can hold at once.
func (d Device) GetMaxMigDeviceCount() (uint32, Return) {
	var count uint32
	ret := nvmlDeviceGetMaxMigDeviceCount(d.handle, &count)
	return count, ret
}

// IsMigDeviceHandle reports whether this handle names a MIG instance rather than a physical card.
func (d Device) IsMigDeviceHandle() (bool, Return) {
	var isMig uint32
	ret := nvmlDeviceIsMigDeviceHandle(d.handle, &isMig)
	return isMig != 0, ret
}

// GetGpuInstanceID reports which GPU instance a MIG device handle belongs to.
func (d Device) GetGpuInstanceID() (uint32, Return) {
	var id uint32
	ret := nvmlDeviceGetGpuInstanceId(d.handle, &id)
	return id, ret
}

// GetComputeInstanceID reports which compute instance a MIG device handle is.
func (d Device) GetComputeInstanceID() (uint32, Return) {
	var id uint32
	ret := nvmlDeviceGetComputeInstanceId(d.handle, &id)
	return id, ret
}

// GetMigDeviceHandleByIndex returns the MIG device at a GLOBAL index, if it belongs to this card.
//
// The index is not per-card, despite the call taking a card: it numbers every MIG device on the
// NODE. On a measured host the four instances of card 0 answered at indices 0 through 3, card 1's
// single instance at index 4 and card 2's at index 5, each only when asked of its own card and
// NOT_FOUND when asked of any other. So a caller sweeping 0..GetMaxMigDeviceCount on each card finds
// every instance of the first card and none of the rest. Prefer MigDevices, which sweeps the space
// this quirk actually requires.
func (d Device) GetMigDeviceHandleByIndex(globalIndex uint32) (Device, Return) {
	var handle dmiDevice
	ret := nvmlDeviceGetMigDeviceHandleByIndex(d.handle, globalIndex, &handle)
	return Device{handle: handle, lib: d.lib}, ret
}

// MigDevices returns every MIG device carved out of this card, resolving the global-index quirk
// documented on GetMigDeviceHandleByIndex.
//
// The sweep is bounded by the node's own capacity -- device count times per-card maximum -- because
// that is the widest the shared index space can be. It stops early once the card's own maximum has
// been found, so a full node costs no more than the instances it holds. An index belonging to
// another card answers with an absent code and is skipped; anything else stops the sweep and is
// returned, so a driver fault is never reported as a card with fewer instances than it has.
func (d Device) MigDevices() ([]Device, Return) {
	perCard, ret := d.GetMaxMigDeviceCount()
	if !ret.IsSuccess() {
		return nil, ret
	}
	if perCard == 0 {
		return nil, SUCCESS
	}

	cards, ret := d.lib.GetDeviceCount()
	if !ret.IsSuccess() {
		return nil, ret
	}

	out := make([]Device, 0, perCard)
	for idx := uint32(0); idx < perCard*cards && uint32(len(out)) < perCard; idx++ {
		md, r := d.GetMigDeviceHandleByIndex(idx)
		if r.IsSuccess() {
			out = append(out, md)
			continue
		}
		if r.ReportsAbsent() {
			continue
		}
		return nil, r
	}
	return out, SUCCESS
}

// GetInfo reports the instance's id, its profile id and where it sits on the card.
func (g GpuInstance) GetInfo() (GpuInstanceInfo, Return) {
	var info GpuInstanceInfo
	ret := nvmlGpuInstanceGetInfo(g.handle, &info)
	return info, ret
}

// GetComputeInstanceProfileInfoBySliceCount returns the compute-instance profile occupying
// sliceCount slices inside this GPU instance.
//
// The slice-count indexing is the same as its GPU-instance counterpart's, and so is the reason for
// naming it plainly; see GetGpuInstanceProfileInfoBySliceCount. engProfile selects the engine
// profile, of which the vendor defines exactly one, SHARED.
func (g GpuInstance) GetComputeInstanceProfileInfoBySliceCount(
	sliceCount, engProfile uint32,
) (ComputeInstanceProfileInfo, Return) {
	var info ComputeInstanceProfileInfo
	if sliceCount == 0 || sliceCount > COMPUTE_INSTANCE_PROFILE_COUNT {
		return info, ERROR_INVALID_ARGUMENT
	}
	ret := nvmlGpuInstanceGetComputeInstanceProfileInfo(g.handle, sliceCount-1, engProfile, &info)
	return info, ret
}

// GetComputeInstanceRemainingCapacity reports how many more compute instances of the profile this
// GPU instance can still hold.
func (g GpuInstance) GetComputeInstanceRemainingCapacity(profileID uint32) (uint32, Return) {
	var count uint32
	ret := nvmlGpuInstanceGetComputeInstanceRemainingCapacity(g.handle, profileID, &count)
	return count, ret
}

// GetComputeInstances returns the compute instances of ONE profile inside this GPU instance. Like
// its GPU-instance counterpart it filters by profile id and must be asked once per profile.
func (g GpuInstance) GetComputeInstances(profileID uint32) ([]ComputeInstance, Return) {
	handles := make([]dmiComputeInstance, maxInstancesPerQuery)
	count := uint32(len(handles))
	ret := nvmlGpuInstanceGetComputeInstances(g.handle, profileID, &handles[0], &count)
	if !ret.IsSuccess() {
		return nil, ret
	}
	if count > uint32(len(handles)) {
		return nil, ERROR_INSUFFICIENT_SIZE
	}
	out := make([]ComputeInstance, count)
	for i := range out {
		out[i] = ComputeInstance{handle: handles[i], lib: g.lib}
	}
	return out, ret
}

// CreateComputeInstance creates a compute instance of the profile inside this GPU instance.
func (g GpuInstance) CreateComputeInstance(profileID uint32) (ComputeInstance, Return) {
	var handle dmiComputeInstance
	ret := nvmlGpuInstanceCreateComputeInstance(g.handle, profileID, &handle)
	return ComputeInstance{handle: handle, lib: g.lib}, ret
}

// Destroy removes the GPU instance.
//
// It fails while the instance still holds a compute instance: the teardown order is forced, compute
// instance first. It also fails while a workload holds the instance, which is the driver refusing to
// pull a card out from under a running process rather than a fault to retry through.
func (g GpuInstance) Destroy() Return {
	return nvmlGpuInstanceDestroy(g.handle)
}

// GetInfo reports the compute instance's id, its profile id and its placement within its GPU
// instance's slice range.
func (c ComputeInstance) GetInfo() (ComputeInstanceInfo, Return) {
	var info ComputeInstanceInfo
	ret := nvmlComputeInstanceGetInfo(c.handle, &info)
	return info, ret
}

// Destroy removes the compute instance. It fails while a workload is using it.
func (c ComputeInstance) Destroy() Return {
	return nvmlComputeInstanceDestroy(c.handle)
}
