# Spec: Fabric Interface Count

Status: Shipped
Type: Feature

## Summary

KVCacheBackend member groups can declare how many distinct host-fabric interfaces each member requires. An omitted value keeps the existing one-interface behavior, while an RDMA value above one uses exclusive RDMA resources so a request cannot silently resolve to fewer adapters than declared.

## Motivation

### Goals

Backend operators need to declare the number of distinct RDMA or EFA interfaces available to a member, with one as the default. Rendered workloads must request the matching resource key and quantity, preserve existing manifests when the field is omitted, and prevent an RDMA request for multiple interfaces from silently receiving repeated shared claims on one endpoint.

Measurements on a single-node cluster with two whole-function RDMA endpoints showed that a shared RDMA request for two tokens returned one endpoint in 9 of 13 samples and two endpoints in 4 samples. A second token on one endpoint is a second claim on that same endpoint, and the allocator offers no preferred allocation, so Kubernetes may choose tokens from one endpoint without an error. A separate measurement established that when two endpoints are granted, the store discovers both and creates a transport context for each; it did not measure how bytes are distributed between adapters or any performance benefit.

### Non-Goals

- Add `instanceType` or accelerator resources to member groups.
- Change `memberResources`, detector behavior, EFA classification, or Device Manager shared-mode configuration.
- Derive or fix member REST ports.
- Change the existing `--no-shared` decision.

## Proposal

Add an optional per-member fabric-interface count. Omission and an explicit value of one request one shared RDMA resource for RDMA, or one EFA resource for EFA. For RDMA, a value above one requests that many exclusive RDMA resources, expressing a requirement for distinct endpoints. Other protocols request no fabric resource.

### User Stories

#### Story 1

As a backend operator, I want to configure how many distinct host-fabric interfaces each member gets, defaulting to one, so that a member can request the fabric shape its transport needs.

### Core Features & Acceptance Criteria

- `members[].fabricInterfaceCount` is optional, defaults to one, and accepts positive values only.
- An omitted count and a count of one render the shared RDMA resource with quantity one for RDMA.
- An RDMA count above one renders the exclusive RDMA resource with that quantity.
- EFA retains its protocol-derived resource key and renders the configured quantity; because the EFA
  plugin advertises one unit per node, an EFA count above one remains Pending on every EFA host.
- TCP and other non-host-fabric protocols render no fabric resource request.
- Unit tests assert the rendered resource key and quantity for omitted, one, above-one, and non-host-fabric cases.

### Notes / Constraints / Caveats

The count selects an RDMA allocation mode as well as a quantity. Shared RDMA tokens are capacity claims rather than distinct-endpoint guarantees: a second token can be granted on the same endpoint and is deduplicated by allocation, without an error. Exclusive RDMA resources are therefore required for a count above one. EFA has one advertised unit per node, so an EFA count above one is deliberately unsatisfiable and remains Pending.

The selected mode has a density cost. At one, a node can admit as many members as its shared-token pool permits. Above one, a node can admit at most its endpoint count divided by the requested count. The field is additive: existing objects with no count render exactly the existing shared-key quantity-one request.

### Boundaries

- **Always:** derive the resource key from the effective protocol and preserve the existing host-fabric Pod settings.
- **Always:** use exclusive RDMA resources for a count above one so the declared count means distinct endpoints.
- **Ask first:** request a design change to any stated non-goal or perform a hardware validation.
- **Never:** add InstanceType-based member scheduling, alter `memberResources`, change detector behavior, alter `--no-shared`, or fix port derivation in this change.

### Risks and Mitigations

- A multi-interface request reduces per-node density → use exclusive RDMA allocation only when the operator explicitly asks for more than one interface.
- Host-network members can contend for a REST port → retain the current port behavior and document the observed contention shape rather than changing it incidentally.
- EFA allocation behavior is supplied by its plugin → retain the protocol-derived EFA resource key and do not treat EFA as an RDMA endpoint.

## Design Details

### Commands

```sh
make generate
go build ./...
go test ./pkg/worker/... ./api/...
make lint
make lint docs
```

### Project Structure

- `api/worker/v1alpha1/kv_cache_backend.go` defines the KVCacheBackend API and validation metadata.
- `pkg/worker/kvcache/mooncake/member_workload.go` renders member DaemonSet resource requests.
- `pkg/worker/kvcache/mooncake/member_workload_test.go` verifies rendered member workloads.
- `api/worker/v1alpha1/zz_generated.*`, protobuf, and CRD outputs are generated from API source.

### Code Style

```go
if member.FabricInterfaceCount > 1 {
	return exclusiveRDMAResource
}
return sharedRDMAResource
```

Keep API comments explicit about behavior and consequences, use protocol-derived resource names, and regenerate rather than editing generated API outputs.

### Implementation Plan

> TODO — completed by `my-plan`.

### Test Plan

> TODO — completed by `my-plan`.

## Alternatives

- Keep all counts on the shared RDMA key: rejected because a count above one can silently resolve to one endpoint.
- Do not add a field: rejected because operators need to describe multi-interface members.
- Add InstanceType plus accelerator resources: rejected because member DaemonSets do not use Kueue admission and member node labels do not supply a manufacturer mapping.

## Open Questions

- A host-network member using the shared key can collide with another member on the same REST port; the second member fails with address-in-use and a DaemonSet presents it as CrashLoopBackOff. With exclusive RDMA resources exhausted first, the second member remains Pending and never reaches the port. Port derivation remains unfixed.
- Whether bytes are distributed between multiple granted adapters, and whether that produces a performance benefit, is unmeasured.
- Exclusive RDMA resources remain available when shared mode is disabled; the interaction with the existing shared-mode open question is not changed here.
