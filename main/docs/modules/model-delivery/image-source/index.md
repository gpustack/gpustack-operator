# Model Image Source

A `ModelArtifact` can deliver weights from an OCI image pinned by digest. Kubernetes mounts the
image read-only for the workload; packaging and pulling it adds disk and registry costs.

## Contents

- [The source and its digest contract](#the-source-and-its-digest-contract)
- [Building the image](#building-the-image)
- [Delivery](#delivery)
- [Versions and prerequisites](#versions-and-prerequisites)
- [Disk cost](#disk-cost)
- [Image GC](#image-gc)
- [Registry mirrors and private registries](#registry-mirrors-and-private-registries)

## The source and its digest contract

```yaml
spec:
  source:
    image:
      reference: registry.example.com/team/qwen@sha256:669ed7b1...48   # digest-pinned, required
```

The reference **must pin a digest**. A tag is mutable, so one artifact could deliver different
weights on different pulls, and the identity a frozen reference pins would be nothing; admission
refuses a tag with that reason. Patterns are refused too: an image is mounted whole, so there is no
listing to select from.

> **Why** — the digest pins the image's manifest bytes. It is not the manifest digest a Hugging
> Face source resolves to, and `status.resolved` stays empty for an image source, as for a claim:
> the operator never reads the registry, so the reference in the immutable spec is the only record
> of what the artifact delivers, and the KV reuse identity falls back to the artifact's UID. Two
> artifacts naming one image never share KV blocks; that is safe, only not deduplicated.

Creation is **refused on an apiserver older than 1.35**, where the image-volume field would be
dropped from the Pod silently; the refusal names the floor and the alternatives. On a supported
apiserver the artifact resolves on its first pass with no network: `Resolved=True`, `resolved`
carrying only `resolvedTime`, no `nodes` aggregation, no revalidation.

## Building the image

The build's contract: **the weights live at the image's root**, because delivery mounts the root
whole and offers no sub-path (a sub-path would raise the containerd floor; see
[Versions](#versions-and-prerequisites)). A recipe measured end to end, from
`Qwen/Qwen2.5-7B-Instruct` at commit `a09a3545…`, one safetensors shard per layer:

```bash
for f in *.safetensors tokenizer.json tokenizer_config.json config.json generation_config.json; do
  oras push "$IMAGE" --artifact-type application/vnd.gpustack.weights "$f:application/octet-stream"
done
```

The mount is a read-only overlay of the layer snapshots, and every file the engine reads matches the
Hub copy byte for byte, measured sha256-equal to the LFS oids on Kubernetes 1.35.7 /
containerd 2.2.6. That equality is a property of the build, which this recipe produces; the operator
does not verify it.

## Delivery

An image source always delivers `Image`, whatever `model-artifact-delivery-mode` says; the Setting
governs hub sources (Hugging Face and ModelScope). A `ModelDeployment` renders one image volume per role, mounted
read-only at `/var/lib/gpustack/model`, and nothing else: no cache `emptyDir`, no `HF_*` or proxy
environment, no `--revision`, no ephemeral-storage raise.

The served path and the `--served-model-name` rule are the claim's, and the engine-argument
refusals apply unchanged. A take-over role gets the mount like a claim's.

An `Instance` model volume mounts the image read-only and whole, and needs no CSIDriver; the
node's plugin plays no part. An Instance pinned to a node checks that node before rendering: kubelet
below 1.35, containerd below 2.1, a runtime that is not containerd, or a version that cannot be read
holds the Pod with a phase message naming the node and the floor. An unpinned Instance cannot know,
and waits like a `ModelDeployment` does.

`WeightsReady` follows the claim path: `WeightsNotMounted` until every Pod has started. A failed or
slow pull keeps that reason; kubelet's own event on the Pod names the pull error, and that event is
the diagnostic path.

## Versions and prerequisites

| Component | Floor | Why |
| --- | --- | --- |
| apiserver | 1.35 | the `ImageVolume` feature gate is beta **on by default** from 1.35, GA in 1.36; below that the gate must be opened on the apiserver, which this version does not opt into, and a 1.32 or older apiserver refuses the source at admission |
| kubelet | 1.35 | it mounts the volume; the same gate line applies, checked per pinned node |
| containerd | 2.1 | the runtime that mounts image volumes; sub-path mounts would need 2.2, which is why none are offered |

PSA is not an obstacle: on an apiserver ≥ 1.33, Restricted admits `image` volumes
(kubernetes#130394, fixed in 1.33, not backported); on ≤ 1.32 Restricted refuses them, and
`enforce-version` makes no difference either way. Baseline admits wherever the apiserver knows the
field.

A private registry is reached with the role's or Instance's existing `imagePullSecrets`; the image
volume pull assembles credentials the same way a container image pull does.

## Disk cost

**A pulled image holds its bytes twice** while `discard_unpacked_layers` is `false` (the containerd
default): the compressed blobs in the content store and the unpacked snapshots. Measured on
containerd 2.2.6 with a 15,242,807,270-byte model:

| Image shape | Content store | Snapshots | Total | vs raw weights |
| --- | --- | --- | --- | --- |
| gzip-compressed layers | +12,074,816,089 B | +15,242,807,270 B | ~27.3 GB | **1.79×** |
| uncompressed layers | +15,242,807,270 B | +15,242,807,270 B | ~30.5 GB | **2.00×** |

Uncompressed layers save decompression time, not disk. **Kubelet's own accounting does not see the
doubling**: the CRI image size is the compressed size, ~12.07 GB where the disk really holds ~27.3
GB (~2.26× apart). Size a node's disk from the table, never from `kubectl`'s image sizes.

A node image that ships `discard_unpacked_layers = true` (the local kind images do) keeps only the
blobs and does not pay the snapshot copy; do not take that as the fleet default.

## Image GC

Three rules, verified on 1.35.7 / 2.2.6:

1. **In use, not collected.** While any *running* container mounts the image, kubelet's image GC
   passes over it (the protection needs kubelet ≥ 1.31 and containerd ≥ 2.1). A completed Pod
   counts for nothing.
2. **Released, collected in the next round.** After the last referencing Pod goes, the image can be
   collected on the next GC cycle (5 minutes; measured release→collection 4m2s). `imageMinimumGCAge`
   does not protect it: kubelet compares against the image's first-detected time, not its release.

   **Scaling a deployment to zero and back can re-pull a dozen GB** (a first pull measured 12m43s on
   a 2-vCPU node at ~20 MB/s).
3. **The 85% cascade.** A model image that pushes the disk past `imageGCHighThresholdPercent`
   (default 85) drags every other unused image on the node into the same rounds (measured: 18
   unrelated images collected alongside).

The only retention is another reference: a resident Pod mounting the image keeps it. A deployment's
replicas are themselves that reference while they run.

## Registry mirrors and private registries

The pull is a normal containerd pull, so it rides whatever the node's registry configuration
resolves: a Harbor pull-through cache, a Spegel peer-to-peer mirror, or Dragonfly as a configured
containerd mirror. None of these need operator support, and none are measured from here; Dragonfly
as an operator-integrated prefetcher is future work.

Two images sharing layer digests share snapshots and the download: measured, a second image whose
layers overlapped pulled in 112 ms and added 11.8 MB where a cold node paid 2m10s and 7.89 GB.

---

**See also** — [Model Artifact](/gpustack-operator/main/docs/modules/model-delivery/artifact/index.md) for the artifact contract the image
source joins · [Node Model Store](/gpustack-operator/main/docs/modules/model-delivery/node-store/index.md) for the plugin chain an image
source stays out of · [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md) for why there is nothing to
warm.

**Next** → [Model Prefetch](/gpustack-operator/main/docs/modules/model-delivery/prefetch/index.md)
