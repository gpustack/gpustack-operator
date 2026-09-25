# Upgrading to an Enforced Binding Dtype

> **Purpose** — what changes for pool-attached workloads once a `KVCachePoolBinding`'s `dtype` is
> handed to the engine, the check to run before upgrading, and the way out when a spelling is wrong.
> **Audience** operators with a KV cache pool · **Prerequisites** [KV Cache Pool](../kv-cache/pool.md) ·
> **Read time** ~5 min

From this release, a Binding's `spec.domain.dtype` stops being a declaration and becomes the engine's
`--kv-cache-dtype`. The rule itself is stated once, under
[The dtype is handed to the engine](../kv-cache/pool.md#the-dtype-is-handed-to-the-engine); this page
is what the switch does to objects that already exist.

## Contents

- [What changes on upgrade](#what-changes-on-upgrade)
- [Check every Binding first](#check-every-binding-first)
- [Find roles that pass the flag themselves](#find-roles-that-pass-the-flag-themselves)
- [If new Pods fail argument parsing](#if-new-pods-fail-argument-parsing)
- [Verify](#verify)

## What changes on upgrade

- **Every pool-attached `ModelDeployment` rolls once.** Its replicas gain an argument, which moves
  their spec hash, so each is replaced at the deployment's first reconcile after the upgrade.
- **The `dtype` reaches the engine verbatim.** A spelling that engine rejects — `bf16` on vLLM,
  `fp8` or `float16` on SGLang — makes every new Pod fail argument parsing, so each replica the
  rollout replaces stops serving.
- **A role's own `--kv-cache-dtype` is refused** while it attaches a pool. A deployment already
  stored with one keeps running on its own value, which wins because it comes later on the command
  line, until its next update is refused.
- **A Pod opting into injection is refused** when its container passes the flag itself, so its
  owner reports `FailedCreate` until the flag is removed.
- **A new Binding may not declare `auto`.** One stored with it keeps working and stays updatable.

## Check every Binding first

List every Binding's dtype, then compare each with the engines of the deployments that name it:

```bash
kubectl get kvcachepoolbindings -A \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,DTYPE:.spec.domain.dtype
```

Each engine's accepted spellings are listed once, under
[The dtype is handed to the engine](../kv-cache/pool.md#the-dtype-is-handed-to-the-engine). A
Binding whose deployments run both engines needs a value in both rows there. A spelling its engine
does not accept is the case the next sections recover from, and it is cheaper to fix before upgrading.

## Find roles that pass the flag themselves

These deployments keep their own value after the upgrade, and their next edit is refused until the
flag is gone from `extraArgs`:

```bash
kubectl get modeldeployments -A -o json | jq -r '.items[]
  | select(.spec.kvCache != null) | . as $md | .spec.roles[]
  | select((.command // []) == [] and ((.extraArgs // []) | any(test("^--kv[-_]cache[-_]dtype"))))
  | "\($md.metadata.namespace)/\($md.metadata.name) role \(.name)"'
```

Remove the flag once the Binding's dtype is the one the role should run.

## If new Pods fail argument parsing

The dtype is immutable, so the Binding cannot be corrected in place. Turn the Setting off first,
which renders and refuses nothing and recreates the replicas without the flag:

```bash
kubectl -n gpustack-system patch setting model-deployment-kv-cache-dtype-owned \
  --type merge -p '{"spec":{"value":"false"}}'
```

Then move the workloads to a Binding with a spelling the engine accepts, and turn the Setting back on:

1. Create a new Binding on the same pool with the corrected `dtype` and a new `domain.name` — the old
   name stays claimed until the old Binding is gone.
2. Recreate each deployment with `spec.kvCache.poolRef.name` naming the new Binding. `kvCache` is an
   identity field, so it is a new deployment rather than an edit.
3. Delete the old deployments; the old Binding's deletion completes once none of them holds it.
4. Patch the Setting back to `"true"`.

The cache under the old domain is not carried over. It was written without a binding dtype, so it
is not worth keeping.

## Verify

Every replica of a pool-attached role carries the Binding's value as two entries of its `command`,
because the operator owns that role's whole argv and folds every argument into it:

```bash
kubectl -n <namespace> get pods -l app.kubernetes.io/instance=<deployment> \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].command}{"\n"}{end}' \
  | grep -o -- '--kv-cache-dtype","[^"]*'
```

An injected Pod carries the same two entries at the end of its `args`, where the webhook appends.

---

**See also** — [KV Cache Pool](../kv-cache/pool.md) (the dtype rule) ·
[ModelDeployment](../reference/model-deployment.md#what-the-operator-owns) (what the operator owns) ·
[KV Cache Injection](../reference/kv-cache-injection.md) (the Pod path) ·
[Settings](../settings.md) (the escape switch)

**Next** → [Migration Troubleshooting](troubleshooting.md) — when an upgrade wedges for another reason.
