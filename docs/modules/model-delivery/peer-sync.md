# Node-to-Node Sync

> **Purpose** — how a node's plugin serves its published trees to the other nodes' plugins, how a cold node pulls from them, and what bounds and protects that path.
> **Audience** users, operators · **Prerequisites** [Node Model Store](node-store.md)
> **Read time** ~6 min

A fleet's nodes need the same weights far more often than they need different ones. When the
hub serves every node separately, egress grows with node count and a hub outage stalls every
cold node.

Node-to-node sync lets the plugins serve each other from the per-node trees the cache already
publishes: the **hub delivers one copy's worth of bytes for any number of nodes**, and a cold
node can materialize a resolved artifact while the hub is unreachable.

## Contents

- [The switch and the port](#the-switch-and-the-port)
- [What a peer serves](#what-a-peer-serves)
- [Who may ask](#who-may-ask)
- [How a cold node pulls](#how-a-cold-node-pulls)
- [What the status and metrics say](#what-the-status-and-metrics-say)
- [Cost model](#cost-model)

## The switch and the port

| Layer | Field | Default | Meaning |
|---|---|---|---|
| chart (`L1`) | `modelManager.port` | `0` | the TCP port the peer endpoints answer on; `0` is off and is the master switch |
| chart (`L1`) | `modelManager.peerSync.maxServingStreams` / `streamsPerSource` | `8` / `4` | the serving and pulling concurrency limits |
| Settings (`L2`) | `model-store-peer-sync` | `true` | whether a node's plugin may pull from peers; `false` keeps the listener but pulls from the hub only |

## What a peer serves

`GET /peer/v1/trees/{hex}` answers a published tree's manifest (its digest and every file's
path, size and digest, stored by the publish itself); `GET /peer/v1/trees/{hex}/files/{path}`
answers one file by HTTP byte range. Trees published before manifests were stored are not
listing sources. At most `maxServingStreams` file answers run at once.

## Who may ask

A request carries the pulling plugin's projected ServiceAccount token (audience
`gpustack-model-peer`); the serving plugin checks it with a TokenReview and admits only the
plugins' ServiceAccount, caching positives until the earlier of a short lifetime and the
token's own expiry — never caching refusals.

The NetworkPolicy the chart ships (default on) drops every other source: a tenant Pod's
connection to the port times out rather than being answered. On CNIs where a `hostNetwork`
tenant bypasses podSelector policies, the token check remains the gate.

## How a cold node pulls

Discovery lists the ready nodes holding the digest (and skips plugins without the peer port);
the candidate's stored manifest is reassembled and bound to the artifact's resolved root digest
before any byte is pulled, so a forged or truncated listing is refused and the next candidate
takes over.

Bytes are hashed in the download stream and checkpointed every 64 MiB, so a peer dying
mid-file resumes from the last checkpoint — the hub fallback re-pulls only the bytes after it.
One source at a time; scheduling segments across several peers is a deliberate extension point.

## What the status and metrics say

`NodeModelStore.status.models[].source` names where the bytes came from (`Hub` or `Peer`, the
majority kind), and the plugin's `download_bytes_total` metric splits `hub` from `peer`. On a
shared filesystem, kubelet's eviction thresholds still cap the cache's high watermark as
[Node Model Store](node-store.md) describes; a threshold kubelet can never fire
floors that cap, because the cache's own collection is then the only space reclaimer.

## Cost model

The feature buys hub egress and hub-independence, not wall clock: on networks where the
node-to-node link is slower than each node's own hub path, all-hub finishes a simultaneous
fan-out sooner. Serving costs CPU on the seed node (measured ≈ 0.25 vCPU per concurrent puller
on 2-vCPU nodes, plus the link's bandwidth) — on GPU nodes this shares headroom with inference.

**See also** — [Node Model Store](node-store.md) for the cache the trees live in, and
[Model Artifact](artifact.md) for the manifest digest the listings are bound to.
