#!/usr/bin/env python3
"""Offline report parser for one autoscaling benchmark arm (schema 1).

The input and output contract is frozen in
.claude/handoffs/scaling-build/T5-parser-seam.md. One arm directory in, one
JSON report out. The parser records validity instead of failing: an invalid
round is a report with reasons, and the later comparison gate reads that.

Measured facts only: a missing value is null with a reason, a zero is a zero,
quantiles come from raw observations with the same nearest-rank rule the load
driver prints, and no success verdict is computed anywhere.
"""

import argparse
import json
import math
import sys
from datetime import datetime, timezone
from pathlib import Path

SCHEMA_VERSION = "1"

# The bounded validity reasons. A reason may only be added from this set.
REASON_INTERRUPTED = "interrupted"
REASON_NONZERO_EXIT = "nonzero_exit"
REASON_MISSING_RUN = "missing_run"
REASON_UNSUPPORTED_SCHEMA = "unsupported_schema"
REASON_EMPTY_TIMELINE = "empty_timeline"
REASON_MALFORMED_TIMELINE = "malformed_timeline"
REASON_UID_MISMATCH = "uid_mismatch"
REASON_MISSING_OBJECTS = "missing_required_objects"
REASON_INCOMPLETE_COOLDOWN = "incomplete_cooldown"
REASON_MISSING_REQUESTS = "missing_requests"
REASON_MALFORMED_REQUESTS = "malformed_requests"
REASON_INVALID_RUN = "invalid_run"
REASON_SAMPLING_ERROR = "sampling_error"

NO_OBSERVATIONS = "no observations"
MISSING_REQUESTS_FILE = "requests file missing"

LATENCY_METRICS = ("ttft_ms", "itl_ms", "tpot_ms", "e2e_ms")

# The fields the load driver writes on every successful request. prompt_tokens
# may be null and served_model may be absent; the rest are always there.
REQUEST_REQUIRED = ("ttft_ms", "itl_ms", "tpot_ms", "e2e_ms", "output_tokens", "started_at")

ARM_FIELDS = (
    "arm", "repeat", "algorithm", "capacityMode", "seed",
    "startedAt", "finishedAt", "exitCode", "interrupted", "provenance",
    "baselineByRole",
)


def baseline_roles(run):
    """The baseline role counts of a run, or an empty map when the field is malformed.

    A MALFORMED BASELINE IS REPORTED AS invalid_run. It is never replaced by
    invented counts, so the analysis sees no role instead of a fabricated one,
    and the raw input stays visible in the arm metadata.
    """
    if not run:
        return {}
    baseline = run.get("baselineByRole")
    return baseline if isinstance(baseline, dict) else {}


def parse_ts(value):
    """Parse an RFC3339 timestamp into epoch seconds, or None."""
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed.timestamp()


def nearest_rank(values, pct):
    """Nearest-rank percentile over a sorted copy of values.

    IT IS THE DRIVER'S OWN RULE (bench_client.percentile), so a number in the
    report and the number the load summary printed are the same number.
    """
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = max(1, min(len(ordered), int(round(pct / 100.0 * len(ordered) + 0.5))))
    return ordered[rank - 1]


def quantile_block(values, reason=None):
    """Quantiles over raw observations, or null with a reason when missing."""
    if reason is None and not values:
        reason = NO_OBSERVATIONS
    if reason is not None:
        return {
            "count": len(values),
            "p50": None, "p95": None, "p99": None,
            "reason": reason,
        }
    return {
        "count": len(values),
        "p50": nearest_rank(values, 50),
        "p95": nearest_rank(values, 95),
        "p99": nearest_rank(values, 99),
        "reason": None,
    }


def load_json(path):
    try:
        return json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None


def load_run(arm):
    """Return (run, reasons); malformed required fields are invalid_run."""
    reasons = []
    raw = load_json(arm / "run.json")
    if not isinstance(raw, dict):
        return None, [REASON_MISSING_RUN]
    if raw.get("schemaVersion") != SCHEMA_VERSION:
        reasons.append(REASON_UNSUPPORTED_SCHEMA)

    invalid = False
    for field in ("arm", "algorithm", "capacityMode", "deploymentUID",
                  "scalerUID", "startedAt", "finishedAt"):
        if not isinstance(raw.get(field), str) or not raw.get(field):
            invalid = True
    for field in ("repeat", "seed", "cooldownRequiredSeconds", "exitCode"):
        if not is_number(raw.get(field)):
            invalid = True
    if not isinstance(raw.get("interrupted"), bool):
        invalid = True

    baseline = raw.get("baselineByRole")
    if not isinstance(baseline, dict) or not baseline:
        invalid = True
    else:
        for role, count in baseline.items():
            if not isinstance(role, str) or not role or not is_number(count) or count <= 0:
                invalid = True
                break

    events = raw.get("phaseEvents")
    if not isinstance(events, list) or not events:
        invalid = True
    else:
        for event in events:
            if not isinstance(event, dict):
                invalid = True
                continue
            if not isinstance(event.get("name"), str) or not event["name"]:
                invalid = True
            if parse_ts(event.get("startedAt")) is None:
                invalid = True
            if not isinstance(event.get("loadActive"), bool):
                invalid = True

    if invalid and REASON_INVALID_RUN not in reasons:
        reasons.append(REASON_INVALID_RUN)
    return raw, reasons


def load_timeline(arm):
    """Return (rows, reasons); rows parsed and carried a ts and a phase.

    A ROW WITH A NON-EMPTY ERRORS MAP IS A SAMPLING ERROR, and primary
    evidence it belongs to is invalid even when the raw objects came back.
    """
    reasons = []
    path = arm / "timeline.jsonl"
    try:
        text = path.read_text()
    except OSError:
        return [], [REASON_EMPTY_TIMELINE]
    rows = []
    malformed = 0
    sampling_errors = 0
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            malformed += 1
            continue
        if not isinstance(row, dict) or not row.get("ts") or "phase" not in row:
            malformed += 1
            continue
        if parse_ts(row.get("ts")) is None:
            # A TIMESTAMP THAT DOES NOT PARSE CANNOT ORDER THE SAMPLES, so the
            # row is not evidence and the round does not integrate.
            malformed += 1
            continue
        if isinstance(row.get("errors"), dict) and row["errors"]:
            sampling_errors += 1
        rows.append(row)
    if malformed:
        reasons.append(REASON_MALFORMED_TIMELINE)
    if sampling_errors:
        reasons.append(REASON_SAMPLING_ERROR)
    if not rows:
        reasons.append(REASON_EMPTY_TIMELINE)
    return rows, reasons


def object_uid(obj):
    if not isinstance(obj, dict):
        return None
    metadata = obj.get("metadata")
    if not isinstance(metadata, dict):
        return None
    return metadata.get("uid")


def status_roles(obj):
    if not isinstance(obj, dict):
        return []
    status = obj.get("status")
    if not isinstance(status, dict):
        return []
    roles = status.get("roles")
    if not isinstance(roles, list):
        return []
    return [role for role in roles if isinstance(role, dict) and role.get("name")]


def status_field(obj, field):
    if not isinstance(obj, dict):
        return None
    status = obj.get("status")
    if not isinstance(status, dict):
        return None
    return status.get(field)


def spec_roles_of(deployment):
    """The requested replica per role, from deployment.spec.roles[].replicas.

    THE SPEC IS THE REQUESTED COUNT and status.desired is the delayed
    observation: both are carried, and the two are never conflated.
    """
    if not isinstance(deployment, dict):
        return {}
    spec = deployment.get("spec")
    if not isinstance(spec, dict):
        return {}
    roles = spec.get("roles")
    if not isinstance(roles, list):
        return {}
    requested = {}
    for role in roles:
        if isinstance(role, dict) and role.get("name"):
            requested[role["name"]] = role.get("replicas")
    return requested


def normalize_sample(row):
    """Turn one timeline row into the normalized sample the report reads."""
    deployment = row.get("deployment")
    scaler = row.get("scaler")
    return {
        "ts": row.get("ts"),
        "epoch": parse_ts(row.get("ts")),
        "phase": row.get("phase"),
        "deployment": deployment if isinstance(deployment, dict) else None,
        "scaler": scaler if isinstance(scaler, dict) else None,
        "errors": row.get("errors") if isinstance(row.get("errors"), dict) else {},
        "md_roles": {role["name"]: role for role in status_roles(deployment)},
        "spec_roles": spec_roles_of(deployment),
        "sc_roles": {role["name"]: role for role in status_roles(scaler)},
        "retirement": status_field(deployment, "retirement"),
        "deployment_uid": object_uid(deployment),
        "scaler_uid": object_uid(scaler),
        "complete": deployment is not None and scaler is not None,
    }


def is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def finite_number(value):
    """A real number: not a boolean, not NaN and not an infinity."""
    if not is_number(value):
        return False
    return not isinstance(value, float) or math.isfinite(value)


def request_record_ok(row):
    """Whether a success record carries the shape the load driver writes.

    A TRUNCATED RECORD, OR ONE THAT HOLDS A NON-FINITE OBSERVATION, IS
    malformed evidence: it is counted as failed and never as a success. A
    genuinely absent optional metric stays absent and its quantile stays null
    with a reason.
    """
    for field in REQUEST_REQUIRED:
        if field not in row:
            return False
    if not isinstance(row["itl_ms"], list):
        return False
    for field in ("ttft_ms", "tpot_ms", "e2e_ms", "output_tokens", "started_at"):
        if not finite_number(row[field]):
            return False
    for gap in row["itl_ms"]:
        if not finite_number(gap):
            return False
    if row.get("prompt_tokens") is not None and not finite_number(row["prompt_tokens"]):
        return False
    return True


def nonfinite_observation(row):
    """Whether a record carries a non-finite known numeric observation.

    THE CHECK RUNS BEFORE THE ERROR SPLIT, because an error record still
    carries measured evidence: a non-finite e2e_ms is malformed evidence, not
    a failure holding a number that never happened. Absent and null
    observations stay optional, and success-required keys are not demanded of
    error records.
    """
    for field in LATENCY_METRICS:
        value = row.get(field)
        if value is None:
            continue
        observations = value if isinstance(value, list) else [value]
        if any(not finite_number(item) for item in observations):
            return True
    return False


def role_stats(samples):
    """Per-role max counts and replica-seconds over observed samples only.

    THE INTERVAL IS THE TIME TO THE NEXT SAMPLE, and the last sample
    contributes nothing: nothing past the last observation was measured. A
    sample whose value is absent contributes nothing for that interval.
    """
    names = []
    seen = set()
    for sample in samples:
        for source in (sample["md_roles"], sample["sc_roles"]):
            for name in source:
                if name not in seen:
                    seen.add(name)
                    names.append(name)

    stats = {}
    for name in names:
        desired_max = None
        ready_max = None
        requested_max = None
        desired_seconds = 0.0
        ready_seconds = 0.0
        observed = 0
        for sample in samples:
            role = sample["md_roles"].get(name)
            if role is None:
                continue
            observed += 1
            desired = role.get("desired")
            ready = role.get("ready")
            if is_number(desired):
                desired_max = desired if desired_max is None else max(desired_max, desired)
            if is_number(ready):
                ready_max = ready if ready_max is None else max(ready_max, ready)
            requested = sample["spec_roles"].get(name)
            if is_number(requested):
                requested_max = requested if requested_max is None else max(requested_max, requested)
        for index, sample in enumerate(samples):
            if index + 1 >= len(samples):
                continue
            following = samples[index + 1]
            if sample["epoch"] is None or following["epoch"] is None:
                continue
            interval = following["epoch"] - sample["epoch"]
            if interval <= 0:
                continue
            role = sample["md_roles"].get(name)
            if role is None:
                continue
            desired = role.get("desired")
            ready = role.get("ready")
            if is_number(desired):
                desired_seconds += desired * interval
            if is_number(ready):
                ready_seconds += ready * interval
        stats[name] = {
            "desiredMax": desired_max,
            "readyMax": ready_max,
            "requestedMax": requested_max,
            "desiredReplicaSeconds": round(desired_seconds, 3),
            "readyReplicaSeconds": round(ready_seconds, 3),
            "samples": observed,
        }
    return stats


def load_phase_events(run):
    """The phaseEvents entries that carry load, in the order run.json wrote them."""
    events = run.get("phaseEvents") if run else None
    if not isinstance(events, list):
        return []
    return [event for event in events
            if isinstance(event, dict) and event.get("name") and event.get("loadActive") is True]


def event_metrics(samples, events, baseline_by_role):
    """Load-event response metrics, anchored at the runner's startedAt.

    Use the last observed sample before the event as the baseline. Missing
    observations stay unknown; declared desired counts do not prove readiness.
    Genuine growth observed at the event timestamp can have zero delay.
    """
    out = {}
    for event in events:
        name = event["name"]
        started = parse_ts(event.get("startedAt"))
        preceding = [s for s in samples
                     if started is not None and s["epoch"] is not None and s["epoch"] < started]
        pre = preceding[-1] if preceding else None
        following = [s for s in samples
                     if started is not None and s["epoch"] is not None and s["epoch"] >= started]

        names = []
        seen = set()
        for source in ([pre] if pre is not None else []) + following:
            for role_map in (source["sc_roles"], source["spec_roles"], source["md_roles"]):
                for role in role_map:
                    if role not in seen:
                        seen.add(role)
                        names.append(role)
        for role in sorted(baseline_by_role or {}):
            if role not in seen:
                seen.add(role)
                names.append(role)

        role_results = {}
        for role in names:
            declared = (baseline_by_role or {}).get(role)
            if pre is not None:
                pre_sc = pre["sc_roles"].get(role)
                pre_rec = pre_sc.get("rawRecommendedReplicas") if pre_sc else None
                pre_spec = pre["spec_roles"].get(role)
                pre_md = pre["md_roles"].get(role)
                pre_ready = pre_md.get("ready") if pre_md else None
            else:
                pre_rec = pre_spec = pre_ready = None

            recommendation_at = None
            commit_at = None
            committed = None
            ready_at = None
            for sample in following:
                sc_role = sample["sc_roles"].get(role)
                if recommendation_at is None and is_number(pre_rec) and sc_role is not None:
                    recommended = sc_role.get("rawRecommendedReplicas")
                    if is_number(recommended) and recommended > pre_rec:
                        recommendation_at = sample
                requested = sample["spec_roles"].get(role)
                if commit_at is None and is_number(pre_spec) and is_number(requested) \
                        and requested > pre_spec:
                    commit_at = sample
                    committed = requested
                md_role = sample["md_roles"].get(role)
                ready = md_role.get("ready") if md_role else None
                if ready_at is None and is_number(pre_ready) and is_number(ready) \
                        and ready > pre_ready and committed is not None and ready >= committed:
                    ready_at = sample

            def metric(sample, reason):
                if sample is None:
                    return {"value": None, "reason": reason}
                if sample["epoch"] is None or started is None:
                    return {"value": None, "reason": "sample timestamps missing"}
                return {"value": round((sample["epoch"] - started) * 1000.0, 3)}

            rec_reason = ("no pre-event recommendation vector" if not is_number(pre_rec)
                          else "recommendation above the pre-event level never observed")
            commit_reason = ("no pre-event replica vector" if not is_number(pre_spec)
                             else "commit above the pre-event level never observed")
            ready_reason = ("no pre-event ready vector" if not is_number(pre_ready)
                            else ("no commit observed" if commit_at is None
                                  else "committed replicas never ready"))
            if declared is None:
                rec_reason = commit_reason = ready_reason = "role is not in baselineByRole"

            role_results[role] = {
                "recommendationResponseMs": metric(recommendation_at, rec_reason),
                "commitResponseMs": metric(commit_at, commit_reason),
                "growthReadyResponseMs": metric(ready_at, ready_reason),
            }
        out[name] = {"startedAt": event.get("startedAt"), "roles": role_results}
    return out


def retirement_block(samples):
    states = []
    for sample in samples:
        retirement = sample["retirement"]
        state = retirement.get("state") if isinstance(retirement, dict) else None
        if state and state not in states:
            states.append(state)
    final = samples[-1] if samples else None
    roles = {}
    if final and isinstance(final["retirement"], dict):
        name = final["retirement"].get("roleName")
        if name:
            roles[name] = final["retirement"].get("replicaOrdinal")
    return {
        "states": states,
        "presentAtEnd": bool(roles),
        "roles": roles,
    }


def evidence_block(samples):
    """The raw per-role evidence, preserved verbatim per sample.

    BOTH THE REQUESTED (spec) AND THE OBSERVED (status) VECTORS TRAVEL WITH
    THE SAMPLE, so a reader can tell a delayed status from a changed request
    without re-deriving either.
    """
    evidence = []
    for sample in samples:
        conditions = status_field(sample["scaler"], "conditions")
        priors = status_field(sample["scaler"], "resolvedCapacityPriors")
        evidence.append({
            "ts": sample["ts"],
            "phase": sample["phase"],
            "roles": sample["sc_roles"],
            "deploymentRoles": status_roles(sample["deployment"]),
            "specByRole": sample["spec_roles"],
            "conditions": conditions if isinstance(conditions, list) else None,
            "resolvedCapacityPriors": priors if isinstance(priors, list) else None,
        })
    return evidence


def timeline_phases(arm, samples, run, reasons):
    """Phases in first-seen order, the per-phase spans, and the reqs check."""
    phases = []
    for sample in samples:
        if sample["phase"] not in phases:
            phases.append(sample["phase"])

    for phase in phases:
        if not (arm / f"{phase}_reqs.jsonl").exists():
            reasons.append(REASON_MISSING_REQUESTS)

    block = {
        "samples": len(samples),
        "firstTs": samples[0]["ts"] if samples else None,
        "lastTs": samples[-1]["ts"] if samples else None,
        "phases": [
            {
                "phase": phase,
                "firstTs": next(s["ts"] for s in samples if s["phase"] == phase),
                "lastTs": next(s["ts"] for s in reversed(samples) if s["phase"] == phase),
                "samples": sum(1 for s in samples if s["phase"] == phase),
            }
            for phase in phases
        ],
        "roles": role_stats(samples),
        "events": event_metrics(samples, load_phase_events(run),
                                baseline_roles(run)),
        "retirement": retirement_block(samples),
        "evidence": evidence_block(samples),
    }
    return block, phases


def quiet_phases(run):
    """Phases whose phaseEvents entry explicitly declares loadActive false.

    A PREPARE OR COOLDOWN PHASE MEASURES NO TRAFFIC, so an existing but empty
    request file is the expected artifact there. A missing file, an empty file
    of an active phase, and an empty file of an undeclared phase stay invalid.
    A PHASE WITH CONTRADICTORY EVENTS IS NOT QUIET, because an event that
    declares the phase active is never relabelled as quiet.
    """
    events = run.get("phaseEvents") if run else None
    if not isinstance(events, list):
        return set()
    quiet = set()
    active = set()
    for event in events:
        if not isinstance(event, dict) or not event.get("name"):
            continue
        if event.get("loadActive") is False:
            quiet.add(event["name"])
        elif event.get("loadActive") is True:
            active.add(event["name"])
    return quiet - active


def phase_metrics(arm, phases, reasons, run=None):
    """Request counts and per-phase quantiles from the raw request records."""
    quiet = quiet_phases(run)
    out = {}
    for phase in phases:
        path = arm / f"{phase}_reqs.jsonl"
        try:
            text = path.read_text()
        except OSError:
            reasons.append(REASON_MISSING_REQUESTS)
            out[phase] = {
                "requests": {
                    "total": None, "succeeded": None, "failed": None,
                    "failureReasons": None, "reason": MISSING_REQUESTS_FILE,
                },
                "latencies": {
                    metric: quantile_block([], MISSING_REQUESTS_FILE)
                    for metric in LATENCY_METRICS
                },
            }
            continue

        total = succeeded = failed = malformed = 0
        failure_reasons = {}
        ttft, itl, tpot, e2e = [], [], [], []
        for line in text.splitlines():
            line = line.strip()
            if not line:
                continue
            total += 1
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                malformed += 1
                continue
            if not isinstance(row, dict):
                malformed += 1
                continue
            if nonfinite_observation(row):
                malformed += 1
                continue
            if "error" in row:
                failed += 1
                reason = str(row.get("error"))
                failure_reasons[reason] = failure_reasons.get(reason, 0) + 1
                continue
            if not request_record_ok(row):
                malformed += 1
                continue
            succeeded += 1
            ttft.append(row["ttft_ms"])
            tpot.append(row["tpot_ms"])
            e2e.append(row["e2e_ms"])
            itl.extend(row["itl_ms"])
        if malformed:
            reasons.append(REASON_MALFORMED_REQUESTS)
            failed += malformed
            failure_reasons["malformed_record"] = failure_reasons.get("malformed_record", 0) + malformed
        if total == 0 and phase not in quiet:
            reasons.append(REASON_MISSING_REQUESTS)

        out[phase] = {
            "requests": {
                "total": total,
                "succeeded": succeeded,
                "failed": failed,
                "failureReasons": failure_reasons,
            },
            "latencies": {
                "ttft_ms": quantile_block(ttft),
                "itl_ms": quantile_block(itl),
                "tpot_ms": quantile_block(tpot),
                "e2e_ms": quantile_block(e2e),
            },
        }
    return out


def cooldown_block(run, samples, phases, reasons):
    """Whether the final phase observed the required quiet cooldown.

    THE FINAL PHASE MUST EXPLICITLY BE COOLDOWN, a phase event must declare it
    quiet, and the final sample must carry the exact baseline role set with
    every role settled at its baseline
    (spec.replicas == status.desired == status.ready == baselineByRole), with
    no pending cohort and no retirement. A role still holding replicas above
    its baseline is a pending cohort, which is a measured outcome during
    growth but incomplete at the end of the run.
    """
    required = run.get("cooldownRequiredSeconds") if run else None
    final_phase = phases[-1] if phases else None
    epochs = [s["epoch"] for s in samples
              if final_phase is not None and s["phase"] == final_phase and s["epoch"] is not None]
    observed = round(epochs[-1] - epochs[0], 3) if epochs else None

    final = samples[-1] if samples else None
    baseline = baseline_roles(run)

    retirement = final["retirement"] if final else None
    retirement_active = isinstance(retirement, dict) and bool(retirement)
    retiring_role = retirement.get("roleName") if isinstance(retirement, dict) else None

    final_roles = {}
    pending_at_end = False
    pending_cohort = status_field(final["scaler"], "pendingCohort") if final else None
    unmeasurable_roles = []
    unsettled_roles = []
    for role_name in sorted(baseline):
        role = final["md_roles"].get(role_name) if final else None
        requested = final["spec_roles"].get(role_name) if final else None
        desired = role.get("desired") if role else None
        ready = role.get("ready") if role else None
        settled = (is_number(requested) and is_number(desired) and is_number(ready)
                   and requested == desired == ready == baseline.get(role_name))
        if not settled:
            unsettled_roles.append(role_name)
        pending = 0 if settled else None
        if pending is None and is_number(requested) and is_number(ready):
            pending = max(0, requested - ready)
        if pending is None:
            unmeasurable_roles.append(role_name)
        elif pending > 0:
            pending_at_end = True
        final_roles[role_name] = {
            "specReplicas": requested,
            "desired": desired,
            "ready": ready,
            "baseline": baseline.get(role_name),
            "pending": pending,
            "retiring": retiring_role == role_name,
        }

    observed_names = set()
    if final is not None:
        observed_names = set(final["spec_roles"]) | set(final["md_roles"])
    extra_roles = sorted(observed_names - set(baseline))

    cooldown_reasons = []
    measurable = observed is not None and is_number(required)
    if final_phase != "cooldown":
        cooldown_reasons.append("the final phase is not cooldown")
    if "cooldown" not in quiet_phases(run):
        cooldown_reasons.append("the cooldown phase has no quiet phase event")
    if not measurable:
        cooldown_reasons.append("cooldown duration not measurable")
    elif observed < required:
        cooldown_reasons.append(f"observed {observed}s is under the required {required}s")
    if pending_at_end:
        cooldown_reasons.append("pending replicas at the final sample")
    if pending_cohort is not None:
        cooldown_reasons.append("pending cohort at the final sample")
        pending_at_end = True
    if retirement_active:
        cooldown_reasons.append("retirement active at the final sample")
    for role_name in unmeasurable_roles:
        cooldown_reasons.append(f"baseline role {role_name} not measurable at the final sample")
    for role_name in unsettled_roles:
        cooldown_reasons.append(f"role {role_name} has not returned to its exact baseline")
    for role_name in extra_roles:
        cooldown_reasons.append(f"role {role_name} is outside the baseline role set")

    complete = (not cooldown_reasons) if (measurable and not unmeasurable_roles
                                          and final_phase == "cooldown") else None
    if complete is not True:
        reasons.append(REASON_INCOMPLETE_COOLDOWN)

    return {
        "requiredSeconds": required,
        "observedSeconds": observed,
        "complete": complete,
        "reasons": cooldown_reasons,
        "finalRoles": final_roles,
        "pendingAtEnd": pending_at_end,
        "retirementAtEnd": retirement_active,
    }


def request_stem(path):
    name = path.name
    if name.endswith("_reqs.jsonl"):
        return name[: -len("_reqs.jsonl")]
    return None


def build_report(arm):
    reasons = []
    run, run_reasons = load_run(arm)
    reasons.extend(run_reasons)

    rows, timeline_reasons = load_timeline(arm)
    reasons.extend(timeline_reasons)
    samples = [normalize_sample(row) for row in rows]

    # OUT-OF-ORDER OR DUPLICATE TIMESTAMPS DO NOT INTEGRATE: replica-seconds
    # and response times are only defined over a strictly forward timeline.
    epochs = [sample["epoch"] for sample in samples if sample["epoch"] is not None]
    if any(later <= earlier for earlier, later in zip(epochs, epochs[1:])):
        reasons.append(REASON_MALFORMED_TIMELINE)

    if any(not sample["complete"] for sample in samples):
        reasons.append(REASON_MISSING_OBJECTS)

    declared_deployment = run.get("deploymentUID") if run else None
    declared_scaler = run.get("scalerUID") if run else None
    observed_deployment = []
    observed_scaler = []
    uid_missing = False
    for sample in samples:
        deployment_uid = sample["deployment_uid"]
        scaler_uid = sample["scaler_uid"]
        if not deployment_uid or not scaler_uid:
            # A SAMPLE THAT CARRIES NO UID HAS NO IDENTITY, so the round cannot
            # be tied to the objects it claims to observe.
            uid_missing = True
        if deployment_uid and deployment_uid not in observed_deployment:
            observed_deployment.append(deployment_uid)
        if scaler_uid and scaler_uid not in observed_scaler:
            observed_scaler.append(scaler_uid)
    if uid_missing:
        reasons.append(REASON_UID_MISMATCH)
    if (declared_deployment and any(uid != declared_deployment for uid in observed_deployment)) or \
            (declared_scaler and any(uid != declared_scaler for uid in observed_scaler)):
        reasons.append(REASON_UID_MISMATCH)

    timeline, phases = timeline_phases(arm, samples, run, reasons)

    phase_names = list(phases)
    for path in sorted(arm.glob("*_reqs.jsonl")):
        stem = request_stem(path)
        if stem and stem not in phase_names:
            phase_names.append(stem)
    metrics = phase_metrics(arm, phase_names, reasons, run)

    cooldown = cooldown_block(run, samples, phases, reasons)

    exit_code = run.get("exitCode") if run else None
    if run and run.get("interrupted"):
        reasons.append(REASON_INTERRUPTED)
    if run and exit_code is not None and exit_code != 0:
        reasons.append(REASON_NONZERO_EXIT)

    # ONE MISSING FILE IS FLAGGED BY TWO CHECKS (the timeline pass and the
    # phase-metrics pass); the report names each reason once, in first order.
    reasons = list(dict.fromkeys(reasons))

    return {
        "schemaVersion": SCHEMA_VERSION,
        "generatedAt": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "arm": {field: run.get(field) for field in ARM_FIELDS} if run else None,
        "validity": {"valid": not reasons, "reasons": reasons},
        "uids": {
            "deploymentUID": declared_deployment,
            "scalerUID": declared_scaler,
            "observedDeploymentUIDs": observed_deployment,
            "observedScalerUIDs": observed_scaler,
        },
        "timeline": timeline,
        "phases": metrics,
        "cooldown": cooldown,
    }


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Parse one autoscaling benchmark arm directory into a schema 1 report.")
    parser.add_argument(
        "arm_dir",
        help="the arm directory holding run.json, timeline.jsonl and <phase>_reqs.jsonl")
    parser.add_argument("--out", required=True, help="path of the JSON report to write")
    args = parser.parse_args(argv)

    arm = Path(args.arm_dir)
    if not arm.is_dir():
        print(f"arm directory {arm} does not exist", file=sys.stderr)
        return 2

    report = build_report(arm)
    out = Path(args.out)
    try:
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(json.dumps(report, indent=2) + "\n")
    except OSError as exc:
        print(f"write report {out}: {exc}", file=sys.stderr)
        return 2
    # FAIL-CLOSED EXIT CODES: the harness may only treat a round as passed
    # when the report was written AND the round is valid. An invalid round
    # still writes its diagnostic report and exits 1.
    return 0 if report["validity"]["valid"] else 1


if __name__ == "__main__":
    sys.exit(main())
