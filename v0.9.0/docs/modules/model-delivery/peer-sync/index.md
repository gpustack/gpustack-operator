# Node-to-Node Sync

A fleet's nodes mostly need the same weights. When the hub serves every node separately, egress
grows with node count and a hub outage stalls every cold node.

Node-to-node sync lets the plugins serve each other from the per-node trees the cache already
publishes: the **hub delivers one copy's worth of bytes for any number of nodes**, and a cold
node can materialize a resolved artifact while the hub is unreachable.

## Contents

- [The switch and the port](#the-switch-and-the-port)
- [Serving cached weights](#serving-cached-weights)
- [Peer authentication](#peer-authentication)
- [Cold-node pull](#cold-node-pull)
- [Status and metrics](#status-and-metrics)
- [Cost model](#cost-model)

## The switch and the port

The Helm chart enables node-to-node sync on port `32445` when `modelManager.enabled` is `true`.
Set `modelManager.port` to `0` to disable peer serving and pulling while keeping node delivery
and the model cache enabled. The standalone `model-manager` command keeps peer sync disabled
until its port and peer authentication are configured.

| Layer | Field | Default | Meaning |
|---|---|---|---|
| chart (`L1`) | `modelManager.port` | `32445` | the TCP port the peer endpoints answer on; `0` is off and is the master switch |
| chart (`L1`) | `modelManager.peerSync.maxServingStreams` / `streamsPerSource` | `8` / `4` | the serving and pulling concurrency limits |
| Settings (`L2`) | `model-store-peer-sync` | `true` | whether a node's plugin may pull from peers; `false` keeps the listener but pulls from the hub only |

## Serving cached weights

A serving plugin answers a published tree's manifest (its digest and every file's path, size and
digest, stored by the publish itself) and answers one file per request by HTTP byte ranges. Trees
published before manifests were stored are not listing sources. At most `maxServingStreams` file
answers run at once.

## Peer authentication

A request carries the pulling plugin's projected ServiceAccount token (audience
`gpustack-model-peer`); the serving plugin checks it with a TokenReview and admits only the
plugins' ServiceAccount. Accepted tokens are cached briefly, never past the token's own expiry;
refusals are never cached.

Peer clients do not verify the serving plugin's TLS certificate. Use peer sync on a trusted Pod
network: TokenReview authenticates requesters, and digest checks detect altered model bytes, but
neither authenticates the server. Set `modelManager.port` to `0` if that network trust cannot be met.

The NetworkPolicy the chart ships (default on) drops every other source: a tenant Pod's
connection to the port times out rather than being answered. On CNIs where a `hostNetwork`
tenant bypasses podSelector policies, the token check remains the gate.

If a monitoring service scrapes the secure port, add its Pod and namespace selectors to
`modelManager.networkPolicy.scrapers`. The default policy admits the worker and peer plugins;
other ingress needs an explicit rule. The policy takes effect only on a CNI that enforces it.

## Cold-node pull

Discovery lists the ready nodes holding the digest (and skips plugins without the peer port);
the candidate's stored manifest is reassembled and bound to the artifact's resolved root digest
before any byte is pulled, so a forged or truncated listing is refused and the next candidate
takes over.

Bytes are hashed in the download stream and checkpointed every 64 MiB, so a peer dying
mid-file resumes from the last checkpoint, and the hub fallback re-pulls only the bytes after it.
A pull uses one peer at a time; it does not stripe a file across several peers.

## Status and metrics

`NodeModelStore.status.models[].source` names where the bytes came from (`Hub` or `Peer`, the
majority kind), and the plugin's `download_bytes_total` metric splits `hub` from `peer`. On a
shared filesystem, kubelet's eviction thresholds still cap the cache's high watermark as
[Node Model Store](/gpustack-operator/v0.9.0/docs/modules/model-delivery/node-store/index.md) describes; a threshold kubelet can never fire
floors that cap, because the cache's own collection is then the only space reclaimer.

## Cost model

The feature buys hub egress and hub-independence, not wall clock: on networks where the
node-to-node link is slower than each node's own hub path, all-hub finishes a simultaneous
fan-out sooner. Serving costs CPU on the seed node (measured ≈ 0.25 vCPU per concurrent puller
on 2-vCPU nodes, plus the link's bandwidth); on GPU nodes this shares headroom with inference.

---

**See also** — [Node Model Store](/gpustack-operator/v0.9.0/docs/modules/model-delivery/node-store/index.md) for the cache the trees live in, and
[Model Artifact](/gpustack-operator/v0.9.0/docs/modules/model-delivery/artifact/index.md) for the manifest digest the listings are bound to.


**Next** → [Model Store Operations](/gpustack-operator/v0.9.0/docs/modules/model-delivery/operations/index.md)
