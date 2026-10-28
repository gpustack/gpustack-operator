# Model Deployment Scaling Simulation

A deterministic local simulation compares the three `ModelDeploymentScaler` algorithms on identical
synthetic traffic. It records replica adjustments, request outcomes, *time to first token (TTFT)*,
*time per output token (TPOT)* and *inter-token latency (ITL)*. These results are not hardware
measurements and are not tuning or purchasing recommendations.

The measured hardware comparison — including a four-node H200 sensitivity wave scored against the
same true-SLO and goodput metrics — lives in
[Model Deployment Scaling Benchmark](scaling-benchmark.md#algorithm-sensitivity); the scaler
contract lives in [Model Deployment Scaling](scaling.md).

## Contents

- [Fixture and provenance](#fixture-and-provenance)
- [Results](#results)
- [Adjustment dynamics](#adjustment-dynamics)
- [Quota shortfall](#quota-shortfall)
- [Reading the numbers](#reading-the-numbers)
- [Limitations](#limitations)

## Fixture and provenance

Every configuration uses the same synthetic plant, fleet lifecycle and clock. Each receives the
same open-loop arrivals for a given scenario seed; only the scaler algorithm differs. Runs replay
identically for each seed. A separate fixed `Shadow` control observes the same traffic without
committing changes.

| Item | Value |
|---|---|
| Engine | vLLM exposition (synthetic plant counters and histograms) |
| Plant capacity | 300 tokens/s per replica, batch 4, batch gain 0.05, contention 0.02 |
| Request timeout | 30 s |
| SLO targets | *TTFT* 2 s; *ITL* 100 ms; no *TPOT* target |
| Fleet | 1 initial replica; quota 6 (4 in `quota-shortfall`); 10 s admission; 20 s boot; 30 s retirement |
| Scaler | 10 s sampling; *`maxReplicas`* `6`; scale-up step 1; 60 s scale-down stabilization |
| Configurations | `Proportional` on `QueueDepth` target 4 tolerance 0.1; `TokenThroughput` at 300 tokens/s; `Predictive` with the same capacity, 30 s horizon and 4-point warm-up; fixed `Shadow` control |
| Traffic | Open loop, 1 s steps, jittered request lengths, quantized to whole tokens per request; scenario seeds 20261026 through 20261034 |
| Token truth | Opt-in whole-token recording; TTFT, TPOT and ITL statistics derive from recorded token timestamps |

Scenarios: `idle` (no traffic), `step` (1 to 3 requests/s at 60 s), `burst-fault-reset` (burst, a
70–90 s scrape outage, a counter reset at 100 s and a controller restart at 120 s),
`holdout-cache-shift` (prefix-cache fraction rises from 0 to 0.5 at 120 s; holdout), and `noise`
(constant 2 requests/s with strongly jittered lengths).

The remaining scenarios are `ramp` (0.5 to 4 requests/s in five steps), `quota-ceiling` (10
requests/s against a 6-replica quota equal to the scaler maximum), `quota-shortfall` (8 requests/s
against a 4-replica quota below the maximum, then 0.5 requests/s), and `length-shift` (50/50-token
requests switch to 200/400-token requests at 120 s).

The fixture lives beside the scaler controller tests. Its comparison suite rewrites two deterministic
artifacts in the OS temp directory: a summary for each scenario and configuration, with provenance
notes, and a timeline with one sampled instant per line. The timeline records offered traffic,
signals read by each algorithm (null when missing), the raw recommendation, committed count,
ready replicas and reasons.

## Results

True SLO is the share of offered requests whose recorded *TTFT* and adjacent *ITL* gaps stayed
within the targets; failed and outstanding requests count as non-compliant. Goodput counts
compliant requests per simulated second.

| Scenario | Configuration | Offered | Done | Failed | Outstanding | True SLO | Goodput (req/s) | Changes | Peak committed |
|---|---|---|---|---|---|---|---|---|---|
| idle | Proportional | 0 | 0 | 0 | 0 | null | null | 0 | 1 |
| idle | TokenThroughput | 0 | 0 | 0 | 0 | null | null | 0 | 1 |
| idle | Predictive | 0 | 0 | 0 | 0 | null | null | 0 | 1 |
| step | Proportional | 600 | 600 | 0 | 0 | 63.2% | 1.58 | 5 | 6 |
| step | TokenThroughput | 600 | 600 | 0 | 0 | 61.7% | 1.54 | 6 | 6 |
| step | Predictive | 600 | 600 | 0 | 0 | 61.7% | 1.54 | 6 | 6 |
| burst-fault-reset | Proportional | 450 | 450 | 0 | 0 | 40.4% | 0.91 | 5 | 6 |
| burst-fault-reset | TokenThroughput | 450 | 450 | 0 | 0 | 38.9% | 0.88 | 4 | 4 |
| burst-fault-reset | Predictive | 450 | 450 | 0 | 0 | 38.9% | 0.88 | 4 | 4 |
| holdout-cache-shift | Proportional | 480 | 480 | 0 | 0 | 79.4% | 1.59 | 4 | 5 |
| holdout-cache-shift | TokenThroughput | 480 | 480 | 0 | 0 | 79.4% | 1.59 | 5 | 4 |
| holdout-cache-shift | Predictive | 480 | 480 | 0 | 0 | 79.4% | 1.59 | 5 | 4 |
| noise | Proportional | 360 | 360 | 0 | 0 | 71.1% | 1.42 | 5 | 6 |
| noise | TokenThroughput | 360 | 360 | 0 | 0 | 71.1% | 1.42 | 5 | 4 |
| noise | Predictive | 360 | 360 | 0 | 0 | 71.1% | 1.42 | 4 | 4 |
| ramp | Proportional | 532 | 532 | 0 | 0 | 76.1% | 1.69 | 5 | 6 |
| ramp | TokenThroughput | 532 | 532 | 0 | 0 | 76.1% | 1.69 | 4 | 5 |
| ramp | Predictive | 532 | 531 | 0 | 1 | 99.4% | 2.20 | 3 | 4 |
| quota-ceiling | Proportional | 1170 | 563 | 607 | 0 | 12.7% | 0.83 | 5 | 6 |
| quota-ceiling | TokenThroughput | 1170 | 435 | 735 | 0 | 12.7% | 0.83 | 5 | 6 |
| quota-ceiling | Predictive | 1170 | 435 | 735 | 0 | 12.7% | 0.83 | 5 | 6 |
| quota-shortfall | Proportional | 850 | 202 | 648 | 0 | 4.7% | 0.20 | 5 | 5 |
| quota-shortfall | TokenThroughput | 850 | 202 | 648 | 0 | 4.7% | 0.20 | 5 | 5 |
| quota-shortfall | Predictive | 850 | 202 | 648 | 0 | 4.7% | 0.20 | 5 | 5 |
| length-shift | Proportional | 480 | 410 | 59 | 11 | 51.0% | 1.02 | 5 | 6 |
| length-shift | TokenThroughput | 480 | 368 | 70 | 42 | 50.4% | 1.01 | 5 | 6 |
| length-shift | Predictive | 480 | 368 | 70 | 42 | 50.4% | 1.01 | 5 | 6 |

The fixed `Shadow` control never commits a change: it stays at the initial replica in every
scenario and therefore completes the least work under load — in `step`, 153 of 600 requests
finish and 357 time out. The completed-request columns for the three active configurations are
the effect of their scaling. Idle rows carry null fractions because no request was offered; the
absence of traffic is not a 0% or 100% compliance measurement.

## Adjustment dynamics

Selected instants from the `step` scenario timeline (raw recommendation, committed replicas, ready
replicas, throughput signal, observed processed tokens per second, observed waiting requests):

| Second | Configuration | Raw | Committed | Ready | Signal | Tokens/s observed | Waiting |
|---|---|---|---|---|---|---|---|
| 0 | all | 1 | 1 | 1 | Missing | null | 0 |
| 60 | all | 1 | 1 | 1 | Fresh | 200.4 | 0 |
| 70 | Proportional | 5 | 2 | 1 | Fresh | 270.8 | 16 |
| 70 | TokenThroughput | 2 | 2 | 1 | Fresh | 270.8 | 16 |
| 70 | Predictive | 2 | 2 | 1 | Fresh | 270.8 | 16 |
| 90 | Proportional | 13 | 4 | 1 | Fresh | 273.9 | 46 |
| 90 | TokenThroughput | 4 | 4 | 1 | Fresh | 273.9 | 46 |
| 90 | Predictive | 4 | 4 | 1 | Fresh | 273.9 | 46 |
| 110 | Proportional | 18 | 6 | 3 | Missing | null | 64 |
| 110 | TokenThroughput | 4 | 4 | 3 | Missing | null | 64 |
| 110 | Predictive | 4 | 4 | 3 | Missing | null | 64 |
| 130 | Proportional | 9 | 6 | 5 | Missing | null | 23 |
| 130 | TokenThroughput | 5 | 5 | 4 | Fresh | 1097.9 | 23 |
| 130 | Predictive | 7 | 5 | 4 | Fresh | 1097.9 | 23 |
| 150 | Proportional | 6 | 6 | 6 | Fresh | 629.2 | 0 |
| 150 | TokenThroughput | 7 | 6 | 4 | Fresh | 724.4 | 0 |
| 150 | Predictive | 7 | 6 | 4 | Fresh | 724.4 | 0 |
| 240 | Proportional | 6 | 6 | 6 | Fresh | 590.2 | 0 |
| 240 | TokenThroughput | 3 | 5 | 5 | Fresh | 590.2 | 0 |
| 240 | Predictive | 5 | 5 | 5 | Fresh | 590.2 | 0 |

Three behaviors are visible. First, `Proportional` overshoots its raw recommendation under a queue
backlog (18 at 110 s) because one instantaneous queue depth reflects the whole backlog, while the
windowed algorithms stay nearer capacity. Second, every configuration shows `Missing` throughput
windows while newly committed replicas boot (110–130 s for `Proportional`); the timeline records
those as missing readings, not zeros.

Third, all three configurations reach 6 replicas during 150–230 s, after which `TokenThroughput`
and `Predictive` scale down to 5 at 240 s while `Proportional` holds 6 — the source of their
differing change counts. In the `ramp` scenario the differences do change outcomes: `Predictive`
committed its first growth 15 s after the load event against 55 s for the reactive configurations
and completed 99.4% of requests within the true SLO against 76.1%.

## Quota shortfall

`quota-shortfall` caps the fleet at 4 replicas while the scaler maximum stays 6, so the overload
phase strands desired replicas in the pending cohort with admission pinned at the quota and the
block reason `ResourceExhausted`. When a cohort has waited *`pendingTimeoutSeconds`* (60 s), the
scaler rolls back to its last committed count of 4 while the raw recommendation still exceeds the
quota — 60 for `Proportional` at 100 s, 6 for the token configurations at 130 s.

When demand later drops to 0.5 requests/s, the recommendation falls to 4 or fewer and no new cohort
forms.

Clearance here is the pending timeout's rollback, not a quota change. The fixture contains no
quota raise, so recovery from an enlarged quota is not exercised and remains unverified.

## Reading the numbers

- True *TTFT*, *TPOT* and *ITL* statistics are computed from recorded whole-token timestamps.
  TTFT and TPOT percentiles weight completed requests; ITL percentiles pool every adjacent gap of
  completed requests, so longer responses contribute more samples. Failed partial traces and
  outstanding requests enter no percentile but stay non-compliant in the offered-based SLO
  denominator.
- The timeline distinguishes missing readings from true zeros. A gauge or latency field is null
  unless the sampler read it fresh; the observed token rate is null while the throughput signal is
  missing. A zero appears only where the sampler read a fresh zero.
- The plant's exposition histograms and per-request latency summaries are per-step approximations
  of the engine metrics the configurations consume. They are reported separately as observed
  values and are never relabeled as the timestamp-derived statistics above. In these scenarios the
  two coincide because per-request service rates stay constant within a request; they diverge when
  the rate changes between quanta, which the plant's own boundary tests cover.
- No *TPOT* SLO target is configured. TPOT is reported as a distribution only and is never scored.
- `Proportional` reads `QueueDepth` against its target. It does not read *TTFT*, *ITL* or *TPOT*;
  those appear in its rows only as evaluation metrics.

## Limitations

- The plant is a synthetic constant-rate service model with one capacity setting. It exercises the
  scaler's control loop, not engine or accelerator behavior; no hardware conclusion follows from it.
- The controller restart in `burst-fault-reset` reconstructs the scaler and sampler state; it is
  not a serving `Pod` restart.
- `noise` jitters correlated request lengths, not the arrival rate; `ramp` is a five-step rate ramp.
- Holdout scenarios are reported unmodified and no configuration is tuned on them within this
  fixture. That demonstrates independence from the current configuration only; it does not verify
  historical tuning.
- `quota-shortfall` clears its pending cohort through the pending timeout's rollback. No
  quota-raise or capacity-release event exists in the fixture.
- The comparison records measurements and does not declare a winning algorithm. Recommendation
  differences under one synthetic capacity do not establish general algorithm superiority.

---

**See also** — [Model Deployment Scaling](scaling.md) (the scaler contract, algorithms and guards) ·
[Model Deployment Scaling Benchmark](scaling-benchmark.md) (the measured hardware comparison) ·
[Model Deployment Scaling Walkthrough](../../walkthroughs/model-deployment/scaling.md) (configure
and observe the scaler on a cluster).

**Next** → [Model Deployment Scaling Benchmark](scaling-benchmark.md) — the measured hardware
comparison for the same three algorithms.
