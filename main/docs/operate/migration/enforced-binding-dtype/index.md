# Upgrading to an Enforced Binding Dtype

Starting in this release, `KVCachePoolBinding` *`spec.domain.dtype`* is rendered onto attached engines
as `--kv-cache-dtype` instead of acting only as metadata. Check existing Bindings and deployments
before upgrading; see [Engine cache precision](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md#engine-cache-precision) for the underlying
contract.

## Contents

- [Changes on upgrade](#changes-on-upgrade)
- [Step 1: Check binding precision](#step-1-check-binding-precision)
- [Step 2: Find conflicting dtype flags](#step-2-find-conflicting-dtype-flags)
- [Troubleshooting](#troubleshooting)
- [Verification](#verification)

## Changes on upgrade

- **One-time replica rollout**: every pool-attached `ModelDeployment` gains `--kv-cache-dtype`, changing
  its pod-spec hash and rolling replicas once.
- **Engine argument parsing**: *`spec.domain.dtype`* passes verbatim to the engine. Invalid engine
  spellings (`bf16` on vLLM; `fp8` or `float16` on SGLang) make replaced `Pod`s fail startup.
- **Owned role flag**: admission refuses `--kv-cache-dtype` in *`roles[].extraArgs`* on pool-attached
  `ModelDeployment`s. Existing stored deployments keep running with their explicit flag until next
  edited.
- **Owned `Pod` injection flag**: `Pod` injection refuses containers that pass `--kv-cache-dtype`,
  causing `FailedCreate` on the owning controller until removed.
- **No new `auto` Bindings**: admission rejects `dtype: auto` on new Bindings while allowing existing
  `auto` Bindings to keep running and updating.

## Step 1: Check binding precision

List every `KVCachePoolBinding` and compare *`spec.domain.dtype`* against the engines attached to it:

```bash
kubectl get kvcachepoolbindings -A \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,DTYPE:.spec.domain.dtype
```

Compare each value against the accepted spellings in
[Engine cache precision](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md#engine-cache-precision). If both vLLM and SGLang attach to one
Binding, *`dtype`* must be valid in both engines. Fix incompatible Bindings before upgrading to avoid
startup failures.

## Step 2: Find conflicting dtype flags

Find pool-attached managed roles that still declare `--kv-cache-dtype` in *`extraArgs`*:

```bash
kubectl get modeldeployments -A -o json | jq -r '.items[]
  | select(.spec.kvCache != null) | . as $md | .spec.roles[]
  | select((.command // []) == [] and ((.extraArgs // []) | any(test("^--kv[-_]cache[-_]dtype"))))
  | "\($md.metadata.namespace)/\($md.metadata.name) role \(.name)"'
```

Remove `--kv-cache-dtype` from *`extraArgs`* once the attached Binding declares the intended `dtype`.

## Troubleshooting

If upgraded `Pod`s fail argument parsing, you cannot edit *`spec.domain.dtype`* in place because
*`domain`* is immutable. Disable `model-deployment-kv-cache-dtype-owned` first to stop rendering and
restore serving immediately:

```bash
kubectl -n gpustack-system patch setting model-deployment-kv-cache-dtype-owned \
  --type merge -p '{"spec":{"value":"false"}}'
```

```diff
 apiVersion: gpustack.ai/v1
 kind: Setting
 metadata:
   name: model-deployment-kv-cache-dtype-owned
   namespace: gpustack-system
 spec:
-  value: "true"
+  value: "false"
```

Then migrate workloads to a valid Binding and re-enable enforcement:

1. Create a new `KVCachePoolBinding` on the same pool with a valid *`dtype`* and a new *`domain.name`*.
2. Recreate each `ModelDeployment` pointing *`spec.kvCache.poolRef.name`* at the new Binding
   (*`spec.kvCache`* is immutable on an existing deployment).
3. Delete the old `ModelDeployment`s, then delete the old `KVCachePoolBinding`.
4. Patch `Setting/model-deployment-kv-cache-dtype-owned` back to `"true"`.

Cached blocks under the old domain are discarded intentionally because they were written without an
enforced dtype.

## Verification

Confirm that every managed replica of a pool-attached `ModelDeployment` carries `--kv-cache-dtype` in
its synthesized *`command`*:

```bash
kubectl -n <namespace> get pods -l app.kubernetes.io/instance=<deployment> \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].command}{"\n"}{end}' \
  | grep -o -- '--kv-cache-dtype","[^"]*'
```

On webhook-injected standalone `Pod`s, the same two entries appear at the end of `containers[].args`.

---

**See also** — [KV Cache Pools and Bindings](/gpustack-operator/main/docs/modules/kv-cache/pool/index.md) (accepted engine dtype
spellings) · [Configuration](/gpustack-operator/main/docs/modules/model-deployment/configuration/index.md#operator-owned-keys)
(operator-owned engine flags) · [Installation Modes](/gpustack-operator/main/docs/operate/installation-modes/index.md#settings-and-precedence)
(runtime `Setting` startup precedence)

**Next** → [Migration Troubleshooting](/gpustack-operator/main/docs/operate/migration/troubleshooting/index.md) — recover wedged upgrades and stuck namespaces.
