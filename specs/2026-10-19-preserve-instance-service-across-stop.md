# Spec: Preserve Instance Service Across Stop and Restart

Status: Shipped
Type: Feature

## Summary

Stopping an `Instance` (`spec.stop: true`) currently deletes its backing `Pod`, which triggers Kubernetes garbage collection on the `NodePort` `Service` because the `Service` is owned by the `Pod` rather than the `Instance` ([gpustack/gpustack#6462](https://github.com/gpustack/gpustack/issues/6462)). When the `Instance` starts again (`spec.stop: false`), the controller creates a new `Service` and receives a new random `NodePort`. This change binds `Service` ownership and label selection to the `Instance`, retains the `Service` while the `Instance` is stopped, aligns `Service` ports if `spec.ports` is edited while stopped, and deletes the `Service` only when the `Instance` is deleted or all ports are removed.

## Motivation

In [`pkg/worker/controllers/worker/instance.go`](../pkg/worker/controllers/worker/instance.go):

1. [`convertServiceFromPod`](../pkg/worker/controllers/worker/instance.go#L924-L952) sets the `Service` controller `OwnerReference` to the backing `Pod` (`core/v1` `Pod`) instead of the `Instance` (`worker.gpustack.ai/v1alpha1` `Instance`), and copies `pod.Labels` (including `kueue.x-k8s.io/queue-name`) into `Service.Spec.Selector`.
2. When `inst.Spec.Stop` is set to `true`, [`InstanceReconciler.Reconcile`](../pkg/worker/controllers/worker/instance.go#L192-L236) deletes the backing `Pod`. Kubernetes garbage collection then deletes the `Service` and releases its `NodePort` allocations.
3. When `inst.Spec.Stop` is set back to `false`, the controller creates a replacement `Service`, and the API server assigns different `NodePort` numbers. Users must update SSH configs and client endpoints after every stop/start cycle.

### Goals

- Bind the `Instance` backing `Service` (`OwnerReference` and `Selector`) to the `Instance` rather than the `Pod`.
- Retain the `Service` and its allocated `NodePort` values when `inst.Spec.Stop` transitions to `true` and back to `false`.
- Reconcile and align the retained `Service` on start when `spec.ports` or `spec.type` was modified while the `Instance` was stopped, preserving `NodePort` values for unchanged ports and deleting the `Service` if `spec.ports` becomes empty.
- Explicitly delete the `Service` alongside the `Pod` when the `Instance` is being deleted (`inst.DeletionTimestamp != nil`), while refusing to mutate or delete an unrelated same-name `Service` not controlled by the current `Instance`.

### Non-Goals

- In-place migration of `Pod`-owned `Service` objects created before an operator upgrade.
- Preserving `PodIPs` across stop/start cycles, or preserving `HostIPs` when an unpinned `Instance` schedules onto a different Node after restart.
- Changing the `Instance` CRD schema under `api/`.

## Proposal

Tie the lifecycle of `Service/<instance>` to `Instance/<instance>` instead of `Pod/<instance>`, keep the `Service` in place while `spec.stop` is `true`, align the `Service` specification against `inst.Spec.Ports` when `spec.stop` is `false`, and delete the `Service` when the `Instance` is deleted or has no ports.

### User Stories

#### Story 1

As an `Instance` user, I want the `Service` and its assigned `NodePort` values to stay unchanged after I stop (`spec.stop: true`) and restart (`spec.stop: false`) an `Instance`, so that I can reconnect with the same port configuration.

#### Story 2

As an `Instance` user, I want edits made to `spec.ports` or `spec.type` while the `Instance` is stopped to take effect cleanly when the `Instance` restarts, so that unchanged ports keep their existing `NodePort` values, newly added ports receive new `NodePort` values, and clearing all ports removes the unneeded `Service`.

#### Story 3

As a cluster administrator, I want the `Service` and its `NodePort` allocations to be deleted when the `Instance` itself is deleted, so that deleted instances never leak `Service` objects or exhaust the cluster `NodePort` range.

### Core Features & Acceptance Criteria

#### F1 — Bind `Service` ownership and selector to `Instance` and retain on Stop

- Replace `convertServiceFromPod` with `convertServiceFromInstance(inst *workercore.Instance) *core.Service` in [`pkg/worker/controllers/worker/instance.go`](../pkg/worker/controllers/worker/instance.go).
- Set the `Service` controller `OwnerReference` to `inst` (`workercore.SchemeGroupVersionKind("Instance")`) via `kubemeta.ControlOnWithoutBlock`.
- Set `Service.Spec.Selector` to `map[string]string{deviceplugin.InstancePartOfLabelKey: string(inst.UID)}` (`app.kubernetes.io/part-of: <inst.UID>`) so the selector matches the `Instance` UID label on the backing `Pod` regardless of changes to `spec.type` (`kueue.x-k8s.io/queue-name`).
- When `inst.Spec.Stop` is `true`, delete only the backing `Pod` and leave `Service/<instance>` in place.
- **Acceptance criteria:** Stopping a running `Instance` deletes its `Pod`, transitions `status.phase` to `Stopped`, and keeps `Service/<instance>` with its `spec.ports[*].nodePort` intact. Starting the `Instance` again creates a new `Pod` selected by the existing `Service` and populates `status.ports` with the same `NodePort` values.

#### F2 — Align or delete the retained `Service` when `spec.ports` changes while Stopped

- During active reconciliation (`!inst.Spec.Stop`) in [`pkg/worker/controllers/worker/instance.go`](../pkg/worker/controllers/worker/instance.go):
  - When `len(inst.Spec.Ports) == 0` and `svc != nil`: if `kubemeta.IsControlledBy(svc, inst)` and `svc.DeletionTimestamp == nil`, delete `svc` and requeue until `svc` is gone; if `svc` is not controlled by `inst`, leave the unrelated `Service` untouched.
  - When `len(inst.Spec.Ports) > 0` and `svc != nil`: verify `kubemeta.IsControlledBy(svc, inst)` (returning an error if an unrelated same-name `Service` exists), then align `svc.Spec.Selector`, `svc.Spec.Type` (`core.ServiceTypeNodePort`), and `svc.Spec.Ports` with the desired `Service` rendered from `inst`. For each desired port, if `svc.Spec.Ports` already has an entry with the same port name (`getPortName(p)`, which encodes `<protocol>-<port>`), carry forward its assigned `NodePort`; for newly added ports, leave `NodePort: 0` so the API server allocates one. Update `svc` only when a field differs.
- **Acceptance criteria:**
  1. Adding a port while stopped updates the existing `Service`, preserves the `NodePort` of every pre-existing port, and allocates a `NodePort` for the added port.
  2. Removing all ports while stopped deletes the existing `Service` when the `Instance` reconciles.
  3. Changing `spec.type` while stopped still routes `Service` traffic to the new `Pod` because `Service.Spec.Selector` keys on `app.kubernetes.io/part-of: <inst.UID>`.
  4. An unrelated same-name `Service` not controlled by the `Instance` is never overwritten or deleted.

#### F3 — Clean up `Service` on `Instance` deletion

- In the `inst.DeletionTimestamp != nil` branch of [`InstanceReconciler.Reconcile`](../pkg/worker/controllers/worker/instance.go#L115-L185), fetch `Service/<instance>` alongside `Pod/<instance>`; if present and controlled by `inst` (`kubemeta.IsControlledBy(svc, inst)`), delete it and wait until both `Pod` and `Service` are gone before unlocking the finalizer (`systemmeta.Unlock(inst)`).
- **Acceptance criteria:** Deleting a running or stopped `Instance` deletes both `Pod/<instance>` (if any) and `Service/<instance>` before removing the `Instance` finalizer, while leaving any unowned same-name `Service` untouched.

### Notes / Constraints / Caveats

- **Cluster `NodePort` pool usage:** A stopped `Instance` with exposed ports retains its `NodePort` allocation until the `Instance` is deleted or its `spec.ports` list is cleared.
- **End-to-end verification environment:** Provision a k3s cluster using [`testing/infra/clusters/k3s`](../testing/infra/clusters/k3s/README.md) against `server=frank@192.168.50.17` and validate the full create → stop → start → edit-ports → delete lifecycle with a CPU `Instance`.

### Boundaries

- **Always:** Run `make lint` and `make lint docs` after code and documentation changes; update [`docs/README.md`](../docs/README.md) module map (`GPU Instances`) and [`docs/modules/instances/_index.md`](../docs/modules/instances/_index.md) if user-visible behavior is described.
- **Ask first:** Modifying any API type under `api/` or webhook rule under `pkg/worker/webhooks/`.
- **Never:** Delete `Service/<instance>` on `spec.stop: true`; touch `staging/` or `binding/`.

### Risks and Mitigations

- **Risk:** `Service.Spec.Selector` previously included `kueue.x-k8s.io/queue-name`, so changing `spec.type` while stopped would break endpoint selection if the old selector remained. → **Mitigation:** Select pods by `app.kubernetes.io/part-of: <inst.UID>` (`deviceplugin.InstancePartOfLabelKey`) only and align `svc.Spec.Selector` on reconcile.
- **Risk:** Updating `svc.Spec.Ports` with `NodePort: 0` on existing ports could cause the API server or fake client to clear or reallocate `NodePort`. → **Mitigation:** Copy the existing `NodePort` from `svc.Spec.Ports` for matching port names (`<protocol>-<port>`) before calling `r.Client.Update(ctx, svc)`.
- **Risk:** Removing all ports from a stopped `Instance` and starting it would fail API validation if the controller tried to update `svc.Spec.Ports` to an empty slice. → **Mitigation:** Delete `svc` when `len(inst.Spec.Ports) == 0` and `svc != nil`.
- **Risk:** A user-created `Service` sharing the same `namespace/name` as an `Instance` could be overwritten or deleted. → **Mitigation:** Verify `kubemeta.IsControlledBy(svc, inst)` before updating or deleting `svc`.

## Design Details

### Commands

Execution environments:
- **Unit tests & lint:** Local worktree.
- **End-to-end verification:** Remote k3s cluster provisioned via [`testing/infra/clusters/k3s`](../testing/infra/clusters/k3s/README.md) with `server=frank@192.168.50.17` (SSH and `kubectl`).

- **Unit tests:**
  ```bash
  GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -v ./pkg/worker/controllers/worker -run TestInstanceReconciler
  ```
- **Code lint:**
  ```bash
  make lint
  ```
- **Docs lint:**
  ```bash
  make lint docs
  ```

### Project Structure

```text
pkg/worker/controllers/worker/
├── instance.go       # Bind Service to Instance, retain on Stop, align on Start, delete on Instance deletion
└── instance_test.go  # Reconcile tests for Service retention across Stop/Start, port edits, ownership, and deletion
docs/
├── README.md                     # Add spec link under GPU Instances module map
└── modules/instances/_index.md   # Note NodePort retention across stop and restart
specs/
└── 2026-10-19-preserve-instance-service-across-stop.md
```

### Code Style

Follow repository conventions in [`pkg/worker/controllers/worker/instance.go`](../pkg/worker/controllers/worker/instance.go): plain, concise comments with no task IDs or decorative symbols, and level-based reconciliation:

```go
func (r *InstanceReconciler) convertServiceFromInstance(
	inst *workercore.Instance,
) *core.Service {
	svc := &core.Service{
		ObjectMeta: kubemeta.SanitizeObjectMeta(inst.ObjectMeta),
		Spec: core.ServiceSpec{
			Selector: map[string]string{
				deviceplugin.InstancePartOfLabelKey: string(inst.UID),
			},
			Type: core.ServiceTypeNodePort,
		},
	}

	for i := range inst.Spec.Ports {
		p := inst.Spec.Ports[i]
		svc.Spec.Ports = append(svc.Spec.Ports, core.ServicePort{
			Name:       getPortName(p),
			Port:       p.Port,
			Protocol:   p.Protocol,
			TargetPort: intstr.FromInt32(p.Port),
		})
	}

	systemmeta.NoteResource(svc, "instances", nil)
	kubemeta.ControlOnWithoutBlock(svc, inst, workercore.SchemeGroupVersionKind("Instance"))

	return svc
}
```

### Implementation Plan

- [x] **T1 · Bind Instance Service to Instance, retain across Stop, align on Start, and delete on Instance deletion**
      Blocked by: None
      Owns: `pkg/worker/controllers/worker/instance.go`, `pkg/worker/controllers/worker/instance_test.go`
      Gate: review
      Acceptance: `convertServiceFromInstance` sets the `Service` controller `OwnerReference` to `Instance` and sets `Spec.Selector` to `app.kubernetes.io/part-of: <inst.UID>`; stopping an `Instance` deletes the `Pod` while keeping `Service/<instance>` and its `NodePort` allocations; starting an `Instance` reuses and aligns `Service/<instance>` (preserving `NodePort` values for unchanged ports, allocating new `NodePort` values for added ports, and deleting `Service/<instance>` when `spec.ports` is empty); deleting an `Instance` deletes both `Pod/<instance>` and `Service/<instance>` before unlocking the finalizer; unowned same-name `Service` objects are never overwritten or deleted; unit tests in `instance_test.go` cover all cases and `make lint` passes.
      Verify: `GOTOOLCHAIN=local GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -v ./pkg/worker/controllers/worker -run TestInstanceReconciler && make lint`

- [x] **T2 · Update documentation and verify CPU Instance Service lifecycle on k3s cluster**
      Blocked by: T1
      Owns: `docs/README.md`, `docs/modules/instances/_index.md`
      Gate: review
      Acceptance: `docs/README.md` links `../specs/2026-10-19-preserve-instance-service-across-stop.md` under `GPU Instances`; `docs/modules/instances/_index.md` documents `NodePort` preservation across stop and restart; `make lint docs` passes; a k3s cluster created via `testing/infra/clusters/k3s` on `server=frank@192.168.50.17` verifies that a CPU `Instance` retains its `Service` and `NodePort` across stop and restart, aligns edited ports on restart, and deletes its `Service` upon `Instance` deletion.
      Verify: `make lint docs`

### Test Plan

[x] I/we understand the owners of the involved components may require updates to existing tests to make this code solid enough prior to committing the changes necessary to implement this enhancement.

#### Prerequisite testing updates

None.

#### Unit tests

Coverage baseline for `pkg/worker/controllers/worker` measured on 2026-10-19: `84.3%` full package (`1.4%` on `TestInstanceReconciler` subset).

- `pkg/worker/controllers/worker`: `2026-10-19` - `84.3%`
  - `TestInstanceReconciler_ServicePreservedAcrossStopAndRestart`: verifies `Service` is owned by `Instance`, selects `app.kubernetes.io/part-of`, survives `spec.stop: true`, and re-populates `status.ports` with the same `NodePort` on restart.
  - `TestInstanceReconciler_ServiceAlignedOnPortEditsWhileStopped`: verifies that adding or removing ports while stopped preserves existing `NodePort` values for unchanged ports, and clearing `spec.ports` deletes the retained `Service`.
  - `TestInstanceReconciler_DeletingInstanceDeletesServiceAndPod`: verifies that an `Instance` with `DeletionTimestamp != nil` deletes both `Pod/<instance>` and `Service/<instance>` before removing the finalizer.
  - `TestInstanceReconciler_RefusesAServiceItDoesNotOwn`: verifies that an active ported `Instance` refuses to overwrite an unowned same-name `Service`, and an active portless or deleting `Instance` leaves an unowned same-name `Service` untouched.

#### Integration tests

Reconcile-level tests run against `sigs.k8s.io/controller-runtime/pkg/client/fake` in [`pkg/worker/controllers/worker/instance_test.go`](../pkg/worker/controllers/worker/instance_test.go) (listed under Unit tests above).

#### e2e tests

Provision a k3s cluster via [`testing/infra/clusters/k3s`](../testing/infra/clusters/k3s/README.md) (`server=frank@192.168.50.17`), deploy the operator image, and validate a CPU `Instance`:
1. Create a running CPU `Instance` exposing a TCP port; record the allocated `Service` UID and `NodePort`.
2. Patch `spec.stop: true`; verify the `Pod` is deleted, `Instance` phase is `Stopped`, and the `Service` UID and `NodePort` remain unchanged.
3. Patch `spec.stop: false`; verify a new `Pod` reaches `Running`, `Instance` phase reaches `Ready`, and `status.ports` reports the exact same `NodePort`.
4. Stop the `Instance`, edit `spec.ports` to add a second port, and restart; verify the original port keeps its `NodePort` and the added port receives a new `NodePort`.
5. Delete the `Instance`; verify both `Pod` and `Service` are deleted.

## Alternatives

- **Allocate static NodePorts and store them in `Instance.Status.Ports` or an annotation while deleting the `Service` on stop.**
  Rejected because another `Service` in the cluster could claim the released `NodePort` while the `Instance` is stopped, causing `Service` recreation on start to fail with a port-allocated conflict. Retaining the `Service` keeps the `NodePort` reserved in the API server's port allocator.

## Open Questions

None.
