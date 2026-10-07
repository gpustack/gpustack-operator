# Elastic EP tensor parallel groups

Status: Shipped
Blocked on: None. Physical TP=2 acceptance follows merge and the CI image.
Type: Feature

## Motivation

The managed Elastic EP profile currently assigns one whole GPU to each DP member.
Static vLLM DP and EP layouts also support TP groups.
The operator needs to preserve DP membership while accounting for each member's entire TP group.

## Goals

- Support immutable TP sizes of one and two. Preserve omitted values as TP=1.
- Keep `elasticEp.width` as the total DP engine count, including the master.
- Require one complete, exclusive TP group per member Pod and Ray node.
- Preserve the master and its bootstrap width during scale-up.
- Hold uncertain allocation and placement observations instead of authorizing resize.

## Boundaries

PP remains one. Each member runs one local DP engine. The Ray head remains CPU-only.
Scale-down, generic Elastic DP, Router fault injection, and larger TP sizes are outside this change.
Non-elastic deployments retain their existing defaults and rendering.

## Design Details

Add optional `elasticEp.tensorParallelSize` with protobuf field number three.
The default is one. Admission accepts only one and two.
Effective values are immutable; omitted and explicit one are equivalent.
Removing an explicit two is an identity change and is refused.

Each GPU member requests exactly `tensorParallelSize` whole accelerators.
The webhook defaults an omitted accelerator count from the effective TP size.
Explicit resource quantities are preserved and validated.
Ray registration and the vLLM master both receive the effective TP size.
The existing host-resource sizing multiplies CPU and memory by the accelerator quantity.
Total GPU demand is `width * tensorParallelSize`; all four convergence layers continue to count DP members.

Allocation requires the entire TP group in the main container.
Every card must have distinct identity, exclusive mode, full units, and no slices or partition profiles.
Every card must agree with the same bookended node ledger.
A complete TP group contributes one member to allocated capacity.

Ray registration requires exactly TP GPUs on each captured member's unique live Ray node.
For TP=2, each DP placement group must contain two distinct unit-GPU bundles.
Both bundles must share the native engine actor's live node and captured Pod identity.
CPU-only bundles do not contribute to TP size.
Missing, fractional, repeated, or cross-node bundles make effective width unknown.
TP=1 retains its existing placement observation contract.

The collector reads placement-group bundle resources from GCS.
It does not call busy worker actors.
Native forward probes remain one per DP engine and retain their existing identity bookends.

## Evidence and assumptions

Pinned vLLM v0.29.0 admits TP greater than one for Elastic EP while rejecting PP greater than one.
Its static EP world is TP times DP.
Sources: [parallel configuration](https://github.com/vllm-project/vllm/blob/98dff2a81d747d1dba01a47f939f48c3526d4206/vllm/config/parallel.py),
[DP deployment](https://github.com/vllm-project/vllm/blob/98dff2a81d747d1dba01a47f939f48c3526d4206/docs/serving/data_parallel_deployment.md),
and [upstream elastic test](https://github.com/vllm-project/vllm/blob/98dff2a81d747d1dba01a47f939f48c3526d4206/tests/distributed/test_elastic_ep.py).

The documented static engine layout and previously verified TP=1 scale-up are accepted prerequisites.
They do not establish physical TP=2 acceptance.
The user-selected delivery sequence is implementation, PR review, merge, CI image, then physical validation.
Physical acceptance remains pending until the new image completes TP=2, DP=2 to DP=4 with real forwards.

## Implementation Plan

- [x] Task 1: Add the API, defaults, immutable admission, and rendering. Verify legacy defaults and invalid updates.
  Blocked by: None. Owns: API types, shared profile, webhook, renderer, generated output, and their tests.
- [x] Task 2: Validate complete TP allocation and Ray placement. Verify member counts and uncertain observations.
  Blocked by: Task 1. Owns: allocation, collector, observer, and their tests.
- [x] Task 3: Update the owning guide and index. Run required checks and prepare the reviewed PR.
  Blocked by: Task 2. Owns: model-deployment guide, shared index, and this spec.

## Test Plan

Use table-driven CPU tests for omitted and explicit TP, invalid sizes, wrong GPU quantities, and immutable updates.
Verify rendering for both TP sizes, CPU-only head resources, and stable master identity during scale-up.
Reject missing, duplicated, partial, sliced, and extra-container allocations.
Verify that two cards contribute one allocated member and that a disagreeing second card holds the layer.
Reject incorrect registered GPU quantities and incomplete or cross-node TP bundles.
Preserve existing TP=1 and non-elastic regressions.

After merge, use one eight-GPU preemptible node and one fresh deployment.
Use DeepSeek-V2-Lite-Chat revision `85864749cd611b4353ce1decdb286193298f64c7`.
Check the TP=2, DP=2 baseline, then increase width to four once.
Require complete allocation, Ray placement, effective DP=4, EP=8, stable master identity, and real forwards.
Record image digest and source revision. Preserve failed attempts and cleanup evidence.
Do not claim live fault recovery from CPU tests.

## Commands

Local validation uses the repository checkout and its pinned dependencies:

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -tags 'goccy netgo' ./pkg/worker/webhooks/worker ./pkg/worker/controllers/worker ./pkg/worker/elasticprofile
make generate
make lint
make lint docs
make test
git diff --check
```

## Code Style

Follow AGENTS.md, existing Go conventions, declarative test cases, and generated-file ownership.
Keep width semantics unchanged. Do not add a second resize state machine.
