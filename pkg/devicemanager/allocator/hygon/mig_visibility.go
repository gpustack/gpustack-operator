package hygon

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// The partition responder is reached by a type assertion on the server, so a signature that drifts
// from the interface would disable partition serving silently rather than failing to build.
var _ deviceplugin.PhysicalSlicedResponder = (*server)(nil)

// migVisibleDeviceEnv is what the vendor's own HSA runtime reads to select which bound instance a
// container uses. The value must carry the "MIG-" prefix: a bare UUID parses as nothing and the
// runtime silently falls back to the first instance it finds, which was measured.
const migVisibleDeviceEnv = "DMI_MIG_VISIBLE_DEVICE"

// ActuatePhysicalSliced materializes one partition for a container and returns the response that
// binds it.
//
// # Why exactly one accelerator
//
// The vendor's runtime exposes exactly ONE partition to a container, whatever it is given. Binding
// two instance files, passing a comma-separated list of prefixed identifiers, or passing "all" were
// all measured to yield a single visible device, on two driver generations. So a grant spanning two
// accelerators could not be delivered: the container would see one of them and silently run at a
// fraction of what it was admitted for.
//
// It is refused here rather than half-served. The alternative -- carve on every accelerator and let
// the runtime pick -- would consume quota on cards the workload can never reach, and would report
// success for a container that is not getting what it asked for.
func (s *server) ActuatePhysicalSliced(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	profile string,
) (*deviceplugin.PhysicalSlicedAllocation, error) {
	if s.mig == nil {
		return nil, fmt.Errorf("partition driver not configured")
	}

	cards, err := migAllocatedCards(devs, allocated)
	if err != nil {
		return nil, err
	}
	switch len(cards) {
	case 0:
		return nil, fmt.Errorf("no allocated card for partition container %q", ctr.Name)
	case 1:
	default:
		return nil, fmt.Errorf(
			"container %q was granted %d accelerators, but a hygon partition request can only be served by"+
				" one: the vendor runtime makes exactly one partition visible to a container, so the rest"+
				" would be carved and never reachable", ctr.Name, len(cards))
	}
	card := cards[0]

	accel, ok := migAllocatedAccelerator(devs, allocated, card.UUID)
	if !ok {
		return nil, fmt.Errorf("card %s: absent from the device record: fail closed", card.UUID)
	}

	release := lockMigCard(card.PciBusID)
	inst, outcome, err := reserveMigInstance(
		s.mig, deviceplugin.OperatorPodsDir, string(pod.UID), ctr.Name, card, profile)
	release()
	if err != nil {
		return nil, err
	}

	resp, err := s.migContainerResponse(accel, inst)
	if err != nil {
		s.rollbackMigInstance(card, inst, outcome, string(pod.UID), ctr.Name)
		return nil, err
	}

	res := deviceplugin.Resource{Group: accel.groupID, Device: card.UUID}
	return &deviceplugin.PhysicalSlicedAllocation{
		Profile: profile,
		Placements: deviceplugin.Placements{
			res: {{Start: inst.Placement.Start, Length: inst.Placement.Length}},
		},
		IDs:      map[deviceplugin.Resource]string{res: _MigInstanceIDPrefix + inst.UUID},
		Response: resp,
		Rollback: func() {
			s.rollbackMigInstance(card, inst, outcome, string(pod.UID), ctr.Name)
		},
	}, nil
}

// rollbackMigInstance undoes exactly what one reservation did, and nothing more.
//
// An adopted or rebound partition keeps its instance: it was not this call's to create, and
// destroying it would tear down a partition another allocation owns. In both cases the ownership
// record this call wrote is dropped, so the instance goes back to being adoptable rather than
// leaking as a partition nobody claims.
func (s *server) rollbackMigInstance(
	card migCard, inst migInstance, outcome migReserveOutcome, podUID, container string,
) {
	if outcome == migRebound {
		return
	}

	release := lockMigCard(card.PciBusID)
	defer release()

	if err := osx.Remove(migMarkerPath(deviceplugin.OperatorPodsDir, podUID, container, card.UUID)); err != nil {
		s.Logger.Error(err, "failed to drop the ownership record of a rolled-back partition",
			"card", card.UUID, "gpuInstance", inst.GiID)
	}
	if outcome != migCreated {
		return
	}
	if err := s.mig.DestroyInstance(card.PciBusID, inst); err != nil {
		s.Logger.Error(err, "failed to destroy a partition this allocation created; the orphan collector"+
			" will reclaim it", "card", card.UUID, "gpuInstance", inst.GiID)
	}
}

// GetPhysicalSlicedVisibilityResponse names the partition an owner container already holds, for a
// container co-allocating the same accelerator.
//
// It resolves from the durable ownership record and verifies against the accelerator's live state,
// then returns the same response shape the owner's own allocation carried. It fails closed on
// anything it cannot prove: naming the parent accelerator instead would hand this container every
// partition carved on it, including other tenants'.
func (s *server) GetPhysicalSlicedVisibilityResponse(
	_ context.Context,
	pod *core.Pod,
	ctr *core.Container,
	devs *workercore.Devices,
	allocated map[deviceplugin.Resource]int32,
	owner string,
) (*deviceplugin.ContainerAllocateResponse, error) {
	if s.mig == nil {
		return nil, fmt.Errorf(
			"partition driver not configured; cannot address the partition container %q holds", owner)
	}

	cards, err := migAllocatedCards(devs, allocated)
	if err != nil {
		return nil, err
	}
	if len(cards) != 1 {
		return nil, fmt.Errorf(
			"container %q co-allocates %d accelerators, but a hygon partition is only ever held on one",
			ctr.Name, len(cards))
	}
	card := cards[0]

	accel, ok := migAllocatedAccelerator(devs, allocated, card.UUID)
	if !ok {
		return nil, fmt.Errorf("card %s: absent from the device record: fail closed", card.UUID)
	}

	inst, err := s.liveOwnedMigInstance(card, string(pod.UID), owner)
	if err != nil {
		return nil, err
	}
	return s.migContainerResponse(accel, inst)
}

// liveOwnedMigInstance returns the partition the owner container holds on one accelerator: read from
// its ownership record, then verified against the accelerator's live state.
//
// A missing, malformed, wrong-accelerator, dead or identity-changed record is an error, never a
// fallback. The identity check is complete here because this vendor issues a fresh identity on every
// create: a partition recreated at the same ids can never carry the recorded one.
func (s *server) liveOwnedMigInstance(card migCard, podUID, owner string) (migInstance, error) {
	path := migMarkerPath(deviceplugin.OperatorPodsDir, podUID, owner, card.UUID)
	m, err := parseMigMarker(path)
	if err != nil {
		return migInstance{}, fmt.Errorf(
			"read the partition record of container %q on card %s: %w", owner, card.UUID, err)
	}

	release := lockMigCard(card.PciBusID)
	defer release()

	state, err := s.mig.CardState(card.PciBusID, m.Profile)
	if err != nil {
		return migInstance{}, fmt.Errorf("read card %s state: %w", card.UUID, err)
	}
	live, ok := findLiveMigInstance(state, m.GiID)
	if !ok {
		return migInstance{}, fmt.Errorf(
			"the partition container %q holds on card %s (gpu instance %d) is gone: fail closed",
			owner, card.UUID, m.GiID)
	}
	if live.UUID != m.MigUUID {
		return migInstance{}, fmt.Errorf(
			"gpu instance %d on card %s now carries identity %q, not the %q container %q holds (id reused):"+
				" fail closed", m.GiID, card.UUID, live.UUID, m.MigUUID, owner)
	}
	return m.instance(), nil
}

// migContainerResponse builds the response that makes one partition usable inside a container.
//
// Four things are needed and all four were established by running a real workload against a bound
// instance: the node-level control nodes, the accelerator's own drm nodes, the vendor's user-space
// runtime, and the instance's own registry file bound at ITS OWN PATH read-only -- the vendor
// runtime scans that directory by absolute path, so a file mounted anywhere else is not found. The
// selection environment is set as well, although a container given exactly one file needs no
// selection: it costs nothing and makes the grant explicit to anyone reading the container.
//
// The registry file is required rather than best-effort. Without it the container has device nodes
// and no partition, which the vendor runtime reports as "no devices available" -- a failure at the
// workload rather than at the allocation, and much harder to attribute.
func (s *server) migContainerResponse(
	accel migAccelerator, inst migInstance,
) (*deviceplugin.ContainerAllocateResponse, error) {
	confMount := deviceplugin.NewROMount(inst.ConfPath)
	if confMount == nil {
		return nil, fmt.Errorf(
			"the registry file %q of partition %s is absent, so the container could not activate it",
			inst.ConfPath, inst.UUID)
	}

	resp := &deviceplugin.ContainerAllocateResponse{}

	// Control device nodes, from the same helper the whole-card and sliced paths use so the three
	// sets cannot drift apart.
	appendNodeDevices(resp, s.Logger)
	appendCardDevices(resp, accel.accel, s.Logger)

	if osx.Exists(hostHygonPath) {
		resp.Mounts = append(resp.Mounts,
			&deviceplugin.Mount{ContainerPath: ctrHygonDrvr, HostPath: hostHygonPath, ReadOnly: true})
	}
	if pMount := deviceplugin.NewROMount(hostHyhalDir); pMount != nil {
		resp.Mounts = append(resp.Mounts, pMount)
	}
	resp.Mounts = append(resp.Mounts, confMount)

	resp.Envs = map[string]string{migVisibleDeviceEnv: _MigInstanceIDPrefix + inst.UUID}

	return resp, nil
}
