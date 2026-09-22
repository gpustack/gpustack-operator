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

- [x] **T1 · Add the member interface-count API**
      Blocked by: None
      Owns: `api/worker/v1alpha1/kv_cache_backend.go`
      Gate: review
      Acceptance: The member field satisfies the Core Features: it is optional, defaults to one,
      accepts positive values, and documents the RDMA and EFA placement consequences.
      Verify: `go test ./api/worker/v1alpha1/...`

- [x] **T2 · Regenerate the API representations**
      Blocked by: T1
      Owns: `api/worker/v1alpha1/generated.pb.go`, `api/worker/v1alpha1/generated.proto`,
      `api/worker/v1alpha1/zz_generated.crds.go`, `api/worker/zz_generated.openapi.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendmember.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendmembertransport.go`,
      `pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendtransport.go`
      Gate: None
      Acceptance: The protobuf, CRD, OpenAPI, and apply-configuration representations carry the
      member field and preserve its wire number and validation metadata.
      Verify: `git diff --check eda33a4067dc245ef8e39d12d86d6e0ebb7cf655^ eda33a4067dc245ef8e39d12d86d6e0ebb7cf655 -- api/worker/v1alpha1/generated.pb.go api/worker/v1alpha1/generated.proto api/worker/v1alpha1/zz_generated.crds.go api/worker/zz_generated.openapi.go pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendmember.go pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendmembertransport.go pkg/kubeclients/applyconfiguration/worker/v1alpha1/kvcachebackendtransport.go`

- [x] **T3 · Render the effective fabric resource and quantity**
      Blocked by: T1, T2
      Owns: `pkg/worker/kvcache/mooncake/member_workload.go`
      Gate: None
      Acceptance: An omitted or one-value RDMA count uses the shared resource at quantity one; an
      RDMA value above one uses the exclusive resource at that quantity; EFA preserves its resource
      family and quantity; non-host-fabric protocols request neither.
      Verify: `go test ./pkg/worker/kvcache/mooncake/... -run '^TestFabricDeviceResource_NamesItsProtocols$'`

- [x] **T4 · Cover the rendered-workload interface-count matrix**
      Blocked by: T3
      Owns: `pkg/worker/kvcache/mooncake/member_workload_test.go`
      Gate: None
      Acceptance: The rendered workload asserts resource key and quantity for omitted, one,
      above-one, and non-host-fabric rows.
      Verify: `go test ./pkg/worker/kvcache/mooncake/... -run '^TestMemberWorkload_FabricGrantFollowsTheProtocol$'`; changing the RDMA exclusive threshold from `> 1` to `> 2` turns the above-one row red on resource-key and quantity assertions without a compile error.

- [x] **T5 · Document the interface-count behavior**
      Blocked by: T1, T3
      Owns: `docs/kv-cache/backend.md`
      Gate: None
      Acceptance: The backend documentation explains that a member's interface count selects its
      resource quantity and that an RDMA count above one uses exclusive resources.
      Verify: `make lint docs`

- [x] **T6 · Record the shipped design and its protocol dependency**
      Blocked by: T1, T2, T3, T4, T5
      Owns: `specs/2026-09-22-fabric-device-by-protocol.md`,
      `specs/2026-09-22-fabric-interface-count.md`
      Gate: None
      Acceptance: The specifications state the API, rendering, test coverage, and the preceding
      protocol design's updated count-dependent behavior without changing the shipped scope.
      Verify: `make lint docs`

### Test Plan

The rendered-workload matrix in `pkg/worker/kvcache/mooncake/member_workload_test.go` covers an
omitted count, one interface, an RDMA count above one, and non-host-fabric protocols. It asserts the
rendered resource key and quantity, so the renderer's defaulting and exclusive-RDMA threshold are
checked at the DaemonSet boundary rather than only through a helper.

The targeted matrix command is `go test ./pkg/worker/kvcache/mooncake/... -run
'^TestMemberWorkload_FabricGrantFollowsTheProtocol$'`. As a mutation check, changing the renderer's
exclusive threshold from `> 1` to `> 2` makes the above-one row fail on the resource-key and quantity
assertions; it does not fail to compile. The narrow resource-name test,
`go test ./pkg/worker/kvcache/mooncake/... -run '^TestFabricDeviceResource_NamesItsProtocols$'`,
also retains direct coverage of protocol and RDMA-mode selection.

## Alternatives

- Keep all counts on the shared RDMA key: rejected because a count above one can silently resolve to one endpoint.
- Do not add a field: rejected because operators need to describe multi-interface members.
- Add InstanceType plus accelerator resources: rejected because member DaemonSets do not use Kueue admission and member node labels do not supply a manufacturer mapping.

## Open Questions

- A host-network member using the shared key can collide with another member on the same REST port; the second member fails with address-in-use and a DaemonSet presents it as CrashLoopBackOff. With exclusive RDMA resources exhausted first, the second member remains Pending and never reaches the port. Port derivation remains unfixed.
- Whether bytes are distributed between multiple granted adapters, and whether that produces a performance benefit, is unmeasured.
- Exclusive RDMA resources remain available when shared mode is disabled; the interaction with the existing shared-mode open question is not changed here.
