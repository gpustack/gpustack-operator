# Model Deployment Routing Reference

> **Purpose** — which replica each managed router sends a request to by default, and how to change
> that choice through `spec.router.extraArgs`.
> **Audience** users, operators · **Prerequisites** [Model Deployment
> Reference](model-deployment.md) · **Read time** ~5 min

A router only chooses when a role has more than one replica. Everything measured here ran on a
server role; on a prefill/decode pair, where each half is chosen separately, none of it has been run.

## Contents

- [Each router's default](#each-routers-default)
- [Shared prefixes land on one replica](#shared-prefixes-land-on-one-replica)
- [Switching to round robin](#switching-to-round-robin)
- [llm-d-router takes no policy flag](#llm-d-router-takes-no-policy-flag)

## Each router's default

**`vllm-router` and `sglang-gateway` route by `cache_aware`, and `llm-d-router` scores every
candidate for each request.** The operator renders no policy flag for the first two, so each runs its
upstream default:

| `spec.router.name` | Default | What picks the replica |
|---|---|---|
| `vllm-router` | `cache_aware` | The replica whose past requests share the longest prefix with this one, when that share is above `--cache-threshold` (`0.3`); below it, the replica with the smallest record. Once the busiest replica has more than `--balance-abs-threshold` (`64`) requests over the idlest and more than `--balance-rel-threshold` (`1.5`) times as many, the least-loaded replica instead |
| `sglang-gateway` | `cache_aware` | The same algorithm, with the same three defaults |
| `llm-d-router` | A fixed scoring profile | The highest total of prefix-cache match (weight `3`, not scored on a decode half), queue depth (`2`) and KV-cache utilization (`2`) |

Both `cache_aware` routers log the policy once at startup, as `policy: CacheAware { cache_threshold:
0.3, … }`. Those two rows are read from `vllm-project/router@v0.1.15` and
`sgl-project/sglang@gateway-v0.3.1`, the versions `pack/llm-router/Dockerfile` builds; the profile is
what `pkg/worker/kvcache/router/router.go` renders.

## Shared prefixes land on one replica

**Under `cache_aware`, requests that share a long prefix and arrive one after another all go to one
replica, and the others stay idle.** That is the policy working, not a fault: the replica that served
the prefix holds it in its prefix cache, so the next request sharing it does not recompute it.

Such a request only goes elsewhere once the load gap above opens, and traffic that sends one
request at a time never has more than one in flight to open it.

Measured on two server replicas, each on its own GPU, with serial chat requests that open with the
same text of about 500 characters and end in a question of their own, streaming and non-streaming in
turn. No request failed:

| Router | Engine | Requests on each replica |
|---|---|---|
| `vllm-router` | vLLM `0.29.0` | 354 and 0 |
| `sglang-gateway` | SGLang `0.5.18` | 406 and 0 |
| `llm-d-router` | vLLM `0.29.0` | 187 and 170 |
| `llm-d-router` | SGLang `0.5.18` | 375 and 0 |

How any of the three spreads concurrent traffic is not measured.

## Switching to round robin

**Add `--policy round_robin` to `spec.router.extraArgs` on `vllm-router` or `sglang-gateway`** to
send each request to the next replica in turn, whatever it shares with earlier ones:

```yaml
spec:
  router:
    name: vllm-router                    # or sglang-gateway
    extraArgs:
      - --policy
      - round_robin
```

`--policy` is in neither router's [refused list](model-deployment.md#prefill-and-decode), so admission
passes it and the operator appends it to the command line it renders. The router then logs
`Starting router … | policy: RoundRobin`.

Measured on the same two replicas and the same kind of traffic, with the flag set at creation:
under each router, 120 requests with none failing, 60 on each replica. `vllm-router` counted them
in `vllm_router_policy_decisions_total{policy="round_robin"}`, 60 per replica, and `sglang-gateway`
in `smg_worker_selection_total{policy="round_robin"}`, 120 in all.

`router.extraArgs` is [editable](model-deployment.md#which-fields-are-the-deployments-identity), so a
running deployment can switch as well. The router Pod is then replaced, and the new one starts with
no record of earlier prefixes. Only a flag set at creation has been run.

**The value is each project's own, and the router checks it, not admission**, so a misspelled one
stops the router at startup:

| `spec.router.name` | Values `--policy` takes |
|---|---|
| `vllm-router` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `consistent_hash`, `rendezvous_hash` |
| `sglang-gateway` | `random`, `round_robin`, `cache_aware`, `power_of_two`, `prefix_hash`, `manual` |

Only `round_robin` has been run. The three `cache_aware` thresholds pass through `extraArgs` the same
way, as do both routers' `--prefill-policy` and `--decode-policy` for a prefill/decode pair; none of
those has been run either.

## llm-d-router takes no policy flag

**`llm-d-router` cannot be switched through `extraArgs`.** Its scorers and their weights are in the
configuration document the operator renders and mounts, and `--config-file`, which would point the
router at another one, is refused there. No field sets them either.

---

**See also** — [Model Deployment Reference](model-deployment.md) for `spec.router` and the flags each
router refuses · [Model Deployment Metrics Reference](model-deployment-metrics.md) for the router
counters a deployment's snapshot reads.

**Next** → [Model Deployment Status Reference](model-deployment-status.md)
