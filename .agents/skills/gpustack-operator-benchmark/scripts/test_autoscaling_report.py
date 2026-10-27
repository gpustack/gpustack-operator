"""Fixture tests for the offline autoscaling arm report parser (schema 1 rev 2).

The contract is frozen in .claude/handoffs/scaling-build/T5-parser-seam.md
(revision 2: fail-closed exit codes, spec-replicas commits, phaseEvents
anchors, strict cooldown equality). Every case runs the real CLI over a built
arm directory and asserts on the written report and the exit code.
"""

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))

import autoscaling_report  # noqa: E402

SCRIPT = SCRIPTS / "autoscaling_report.py"


# ----------------------------------------------------------------- builders

def write_json(path, payload):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2) + "\n")


def write_run(arm, **over):
    run = {
        "schemaVersion": "1",
        "arm": "catalog-a",
        "repeat": 1,
        "algorithm": "TokenThroughput",
        "capacityMode": "Catalog",
        "seed": 7,
        "deploymentUID": "md-uid-1",
        "scalerUID": "sc-uid-1",
        "baselineByRole": {"server": 2},
        "cooldownRequiredSeconds": 300,
        "startedAt": "2026-10-26T09:59:40Z",
        "finishedAt": "2026-10-26T10:10:00Z",
        "exitCode": 0,
        "interrupted": False,
        "phaseEvents": [
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
            {"name": "cooldown", "startedAt": "2026-10-26T10:01:00Z", "loadActive": False},
        ],
    }
    run.update(over)
    for key, value in list(run.items()):
        if value is ...:
            del run[key]
    write_json(arm / "run.json", run)
    return run


def md_roles(*roles):
    return {"roles": [{"name": n, "desired": d, "ready": r} for n, d, r in roles]}


def scaler_roles(*roles):
    return {"roles": [dict({"name": n}, **extra) for n, extra in roles]}


def sample(ts, phase, md=None, scaler=None, errors=None):
    row = {"ts": ts, "phase": phase, "errors": errors or {}}
    if md is not None:
        row["deployment"] = {
            "metadata": {"uid": md.get("uid", "md-uid-1")},
            "spec": md.get("spec", {}),
            "status": md["status"],
        }
    else:
        row["deployment"] = None
    if scaler is not None:
        row["scaler"] = {"metadata": {"uid": scaler.get("uid", "sc-uid-1")}, "status": scaler["status"]}
    else:
        row["scaler"] = None
    return row


def req(ttft=100.0, gaps=(19.0, 21.0), tpot=20.0, e2e=500.0, out_tokens=None, **over):
    gaps = list(gaps)
    record = {
        "ttft_ms": ttft,
        "itl_ms": gaps,
        "tpot_ms": tpot,
        "e2e_ms": e2e,
        "prompt_tokens": 100,
        "output_tokens": out_tokens if out_tokens is not None else len(gaps) + 1,
        "started_at": 1.0,
    }
    record.update(over)
    return record


def bad_req(error="timeout", e2e=900.0, **over):
    record = {"error": error, "e2e_ms": e2e}
    record.update(over)
    return record


def write_timeline(arm, rows):
    path = arm / "timeline.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))


def write_reqs(arm, phase, rows):
    path = arm / f"{phase}_reqs.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))


def valid_md(uid="md-uid-1", roles=None, retirement=None, spec=None):
    """A raw-shaped deployment: spec.replicas defaults to the status desired."""
    role_tuples = roles if roles is not None else [("server", 2, 2)]
    status = md_roles(*role_tuples)
    spec_list = spec if spec is not None else [
        {"name": name, "replicas": desired} for name, desired, _ in role_tuples
    ]
    if retirement is not None:
        status["retirement"] = retirement
    return {"uid": uid, "spec": {"roles": spec_list}, "status": status}


def valid_scaler(uid="sc-uid-1", roles=None, priors=None, conditions=None):
    status = scaler_roles(*(roles if roles is not None else [
        ("server", {"rawRecommendedReplicas": 2, "constrainedReplicas": 2,
                    "runningRequests": 0, "signalState": "Fresh"}),
    ]))
    if priors is not None:
        status["resolvedCapacityPriors"] = priors
    if conditions is not None:
        status["conditions"] = conditions
    return {"uid": uid, "status": status}


def base_timeline():
    """A complete two-phase arm: load ramps 2 -> 4, cooldown settles back to 2."""
    return [
        sample("2026-10-26T09:59:48Z", "load", md=valid_md(), scaler=valid_scaler()),
        sample("2026-10-26T10:00:00Z", "load", md=valid_md(roles=[("server", 2, 2)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2, "runningRequests": 4})])),
        sample("2026-10-26T10:00:10Z", "load", md=valid_md(roles=[("server", 2, 2)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 4, "runningRequests": 8})])),
        sample("2026-10-26T10:00:12Z", "load", md=valid_md(roles=[("server", 4, 2)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 4, "runningRequests": 8})])),
        sample("2026-10-26T10:00:30Z", "load", md=valid_md(roles=[("server", 4, 4)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 4, "runningRequests": 1})])),
        sample("2026-10-26T10:01:00Z", "cooldown", md=valid_md(roles=[("server", 4, 4)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2, "runningRequests": 0})])),
        sample("2026-10-26T10:06:30Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
               scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2, "runningRequests": 0})])),
    ]


def duplicate_timestamp_timeline():
    """A timeline where two samples carry the same timestamp."""
    rows = base_timeline()
    rows[2] = sample("2026-10-26T10:00:00Z", "load", md=valid_md(), scaler=valid_scaler())
    return rows


def invalid_timestamp_timeline():
    """A timeline where one sample carries a timestamp that does not parse."""
    rows = copy.deepcopy(base_timeline())
    rows[2]["ts"] = "not-a-timestamp"
    return rows


def without_uids(rows, index=None):
    """Rows whose deployment and scaler carry no metadata.uid.

    Without an index every row loses its uid; with one only that row does.
    """
    cleared = []
    for position, row in enumerate(rows):
        row = copy.deepcopy(row)
        if index is None or position == index:
            row["deployment"]["metadata"]["uid"] = ""
            row["scaler"]["metadata"]["uid"] = ""
        cleared.append(row)
    return cleared


LOAD_REQUESTS = [
    req(ttft=100.0, gaps=(19.0, 21.0), tpot=20.0, e2e=500.0),
    req(ttft=200.0, gaps=(30.0,), tpot=30.0, e2e=800.0),
    req(ttft=300.0, gaps=(40.0, 41.0), tpot=40.0, e2e=900.0),
]
COOLDOWN_REQUESTS = [req(ttft=90.0, gaps=(15.0,), tpot=15.0, e2e=300.0)]


def build_complete(arm):
    write_run(arm, provenance={"engineImage": "registry.example/engine:1.2.3", "workloadSeed": 11})
    write_timeline(arm, base_timeline())
    write_reqs(arm, "load", LOAD_REQUESTS)
    write_reqs(arm, "cooldown", COOLDOWN_REQUESTS)


# The case table is declarative data: inputs and expected report values only.
# build_arm and test_fixture_cases are the one shared executor that turns a
# case into an arm directory, runs the real CLI, and applies the assertions.
#
# Fields: run (write_run overrides), omit_run (no run manifest at all),
# timeline (rows, or absent when the file itself is missing), requests (phase
# -> record list), empty_requests (phases whose file exists but measures
# nothing), expect.reasons (exact), expect.reasons_include,
# expect.values (dotted report path -> expected), expect.absent_strings.
CASES = [
    {
        "name": "complete valid round",
        "valid": True,
        "run": {"provenance": {"engineImage": "registry.example/engine:1.2.3", "workloadSeed": 11}},
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons": [],
            "phase_names": ["load", "cooldown"],
            "values": {
                "arm.algorithm": "TokenThroughput",
                "arm.provenance.engineImage": "registry.example/engine:1.2.3",
                "uids.observedDeploymentUIDs": ["md-uid-1"],
                "uids.observedScalerUIDs": ["sc-uid-1"],
                "timeline.samples": 7,
                "timeline.phases.0.phase": "load",
                "timeline.phases.1.phase": "cooldown",
                "timeline.roles.server.desiredMax": 4,
                "timeline.roles.server.requestedMax": 4,
                # Twelve observed baseline seconds precede the previous six samples.
                "timeline.roles.server.desiredReplicaSeconds": 1560,
                "timeline.roles.server.readyReplicaSeconds": 1524,
                # Responses use the event at 09:59:50 and its observed baseline.
                "timeline.events.load.roles.server.recommendationResponseMs.value": 20000,
                "timeline.events.load.roles.server.commitResponseMs.value": 22000,
                "timeline.events.load.roles.server.growthReadyResponseMs.value": 40000,
                "timeline.retirement.presentAtEnd": False,
                "phases.load.requests.succeeded": 3,
                "phases.load.latencies.ttft_ms.p50": 200.0,
                # itl observations: 19,21,30,40,41 -> p95 nearest rank over 5 = 41.0
                "phases.load.latencies.itl_ms.p95": 41.0,
                "phases.load.latencies.tpot_ms.count": 3,
                "cooldown.observedSeconds": 330,
                "cooldown.finalRoles.server.pending": 0,
                "cooldown.complete": True,
            },
        },
    },
    {
        "name": "absent optional metrics are null with reason and zero stays zero",
        "valid": True,
        "run": {"provenance": ..., "cooldownRequiredSeconds": 30},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load",
                   md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler(roles=[("server", {})])),
            sample("2026-10-26T10:01:00Z", "cooldown",
                   md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2})])),
            sample("2026-10-26T10:01:30Z", "cooldown",
                   md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2})])),
        ],
        "requests": {
            "load": [req(ttft=0.0, gaps=(), tpot=0.0, e2e=100.0, out_tokens=1, prompt_tokens=None)],
            "cooldown": [req()],
        },
        "expect": {
            "reasons": [],
            "values": {
                "arm.provenance": None,
                "timeline.roles.server.desiredMax": 2,
                "phases.load.latencies.tpot_ms.p50": 0.0,
                "phases.load.latencies.itl_ms.count": 0,
                "phases.load.latencies.itl_ms.p95": None,
                "phases.load.latencies.itl_ms.reason": "no observations",
            },
        },
    },
    {
        "name": "all missing timeline",
        "valid": False,
        "requests": {"load": [req()]},
        "expect": {
            "reasons_include": ["empty_timeline"],
            "values": {
                "timeline.samples": 0,
                # the load phase still reports its request metrics from the reqs file
                "phases.load.requests.succeeded": 1,
            },
        },
    },
    {
        "name": "empty quiet-phase request files are valid",
        "valid": True,
        "run": {"cooldownRequiredSeconds": 30, "phaseEvents": [
            {"name": "prepare", "startedAt": "2026-10-26T09:59:00Z", "loadActive": False},
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
            {"name": "cooldown", "startedAt": "2026-10-26T10:01:00Z", "loadActive": False},
        ]},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "prepare", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:00:30Z", "load", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:01:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:30Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req()]},
        "empty_requests": ["prepare", "cooldown"],
        "expect": {
            "reasons": [],
            "values": {
                # a quiet phase measured no requests: zero is the measured outcome
                "phases.prepare.requests.total": 0,
                "phases.prepare.requests.succeeded": 0,
                "phases.cooldown.latencies.ttft_ms.count": 0,
            },
        },
    },
    {
        "name": "empty active and undeclared phase request files stay invalid",
        "valid": False,
        "run": {"phaseEvents": [
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
            {"name": "cooldown", "startedAt": "2026-10-26T10:01:00Z", "loadActive": False},
        ]},
        "timeline": [
            sample("2026-10-26T10:00:30Z", "load", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:01:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:30Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "empty_requests": ["load", "extra"],
        "requests": {"cooldown": [req()]},
        "expect": {
            "reasons_include": ["missing_requests"],
            "values": {
                "phases.cooldown.requests.succeeded": 1,
            },
        },
    },
    {
        "name": "missing requests file invalidates without discarding evidence",
        "valid": False,
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:06:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req()]},
        "expect": {
            "reasons_include": ["missing_requests"],
            "values": {
                "phases.load.requests.succeeded": 1,
                "phases.cooldown.requests.total": None,
                "phases.cooldown.requests.reason": "requests file missing",
                "phases.cooldown.latencies.ttft_ms.reason": "requests file missing",
            },
        },
    },
    {
        "name": "interrupted run never reaches a cooldown phase",
        "valid": False,
        "run": {"interrupted": True, "exitCode": 143},
        "timeline": base_timeline()[:4],
        "requests": {"load": [req()]},
        "expect": {
            "reasons_include": ["interrupted", "nonzero_exit", "incomplete_cooldown"],
            "values": {"arm.interrupted": True},
        },
    },
    {
        "name": "mismatched uids",
        "valid": False,
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(), scaler=valid_scaler(uid="sc-other")),
        ],
        "requests": {"load": [req()]},
        "expect": {
            "reasons_include": ["uid_mismatch"],
            "values": {
                "uids.observedScalerUIDs": ["sc-other"],
                "uids.scalerUID": "sc-uid-1",
            },
        },
    },
    {
        "name": "partial role readiness at cooldown",
        "valid": False,
        "run": {"baselineByRole": {"server": 2, "decode": 1}},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load",
                   md=valid_md(roles=[("server", 2, 2), ("decode", 1, 0)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2}),
                                              ("decode", {"rawRecommendedReplicas": 1})])),
            sample("2026-10-26T10:06:00Z", "cooldown",
                   md=valid_md(roles=[("server", 2, 2), ("decode", 1, 0)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 2}),
                                              ("decode", {"rawRecommendedReplicas": 1})])),
        ],
        "requests": {"load": [req()], "cooldown": [req()]},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {
                "cooldown.finalRoles.decode.ready": 0,
                "cooldown.finalRoles.decode.pending": 1,
            },
        },
    },
    {
        "name": "pending quota at cooldown",
        "valid": False,
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:06:00Z", "cooldown", md=valid_md(roles=[("server", 4, 2)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 4})])),
        ],
        "requests": {"load": [req()], "cooldown": [req()]},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {
                "cooldown.pendingAtEnd": True,
                "cooldown.finalRoles.server.pending": 2,
            },
        },
    },
    {
        "name": "insufficient cooldown",
        "valid": False,
        "run": {"cooldownRequiredSeconds": 600},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:06:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req()], "cooldown": [req()]},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {
                "cooldown.observedSeconds": 0,
                "cooldown.complete": False,
            },
        },
    },
    {
        "name": "failed requests counted with reasons, quantiles from successes",
        "valid": False,
        "timeline": base_timeline()[:1],
        "requests": {"load": [req(ttft=100.0, e2e=400.0),
                              bad_req(error="timeout"),
                              bad_req(error="http 500")]},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {
                "phases.load.requests.total": 3,
                "phases.load.requests.failed": 2,
                "phases.load.requests.failureReasons": {"timeout": 1, "http 500": 1},
                "phases.load.latencies.ttft_ms.count": 1,
                "phases.load.latencies.ttft_ms.p50": 100.0,
            },
        },
    },
    {
        "name": "a nonfinite error observation is malformed evidence",
        "valid": False,
        "timeline": base_timeline()[:1],
        "requests": {"load": [req(ttft=100.0, e2e=400.0),
                              {"error": "timeout", "e2e_ms": float("nan")},
                              {"error": "timeout", "e2e_ms": float("inf")}]},
        "expect": {
            "reasons_include": ["malformed_requests", "incomplete_cooldown"],
            "values": {
                "phases.load.requests.total": 3,
                "phases.load.requests.failed": 2,
                "phases.load.requests.succeeded": 1,
                "phases.load.requests.failureReasons": {"malformed_record": 2},
                "phases.load.latencies.ttft_ms.count": 1,
                "phases.load.latencies.e2e_ms.count": 1,
            },
        },
    },
    {
        "name": "a finite error stays a counted failure",
        "valid": True,
        "run": {"cooldownRequiredSeconds": 30},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:30Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req(ttft=100.0, e2e=400.0),
                              {"error": "timeout", "e2e_ms": 900.0},
                              {"error": "", "e2e_ms": 800.0},
                              {"error": "timeout"}],
                     "cooldown": [req()]},
        "expect": {
            "reasons": [],
            "values": {
                "phases.load.requests.total": 4,
                "phases.load.requests.failed": 3,
                "phases.load.requests.succeeded": 1,
                "phases.load.requests.failureReasons": {"timeout": 2, "": 1},
                "phases.load.latencies.ttft_ms.count": 1,
                "phases.load.latencies.e2e_ms.count": 1,
            },
        },
    },
    {
        "name": "a nonfinite success observation is malformed evidence",
        "valid": False,
        "run": {"cooldownRequiredSeconds": 30},
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:01:30Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req(ttft=float("nan"), e2e=400.0),
                              req(ttft=100.0, gaps=(19.0, float("inf")), tpot=20.0, e2e=500.0)],
                     "cooldown": [req()]},
        "expect": {
            "reasons_include": ["malformed_requests"],
            "values": {
                "phases.load.requests.succeeded": 0,
                "phases.load.requests.failed": 2,
                "phases.load.requests.failureReasons": {"malformed_record": 2},
                "phases.load.latencies.ttft_ms.count": 0,
            },
        },
    },
    {
        "name": "zero and negative improvement reported as raw quantities",
        "valid": True,
        "timeline": [
            sample("2026-10-26T10:00:00Z", "load", md=valid_md(), scaler=valid_scaler()),
            sample("2026-10-26T10:05:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
            sample("2026-10-26T10:10:00Z", "cooldown", md=valid_md(roles=[("server", 2, 2)]),
                   scaler=valid_scaler()),
        ],
        "requests": {"load": [req(ttft=100.0, e2e=400.0)],
                     "cooldown": [req(ttft=150.0, e2e=600.0)]},
        "expect": {
            "reasons": [],
            # the later phase is slower; the parser reports raw numbers and no verdict key
            "values": {
                "phases.load.latencies.ttft_ms.p50": 100.0,
                "phases.cooldown.latencies.ttft_ms.p50": 150.0,
            },
            "absent_strings": ["improvement", "better"],
        },
    },
    {
        "name": "malformed baselineByRole is an invalid run",
        "valid": False,
        "run": {"baselineByRole": {}},
        "timeline": base_timeline()[:1],
        "requests": {"load": [req()]},
        "expect": {"reasons_include": ["invalid_run"]},
    },
    {
        "name": "sampler errors invalidate primary evidence",
        "valid": False,
        "timeline": base_timeline()[:2] + [
            sample("2026-10-26T10:00:30Z", "load",
                   md=valid_md(roles=[("server", 4, 4)]),
                   scaler=valid_scaler(roles=[("server", {"rawRecommendedReplicas": 4})]),
                   errors={"scaler get": "the server was unable to complete the request"}),
        ],
        "requests": {"load": [req()]},
        "expect": {"reasons_include": ["sampling_error"]},
    },
    {
        "name": "unordered timestamps do not integrate",
        "valid": False,
        "timeline": list(reversed(base_timeline()[:3])),
        "requests": {"load": [req()]},
        "expect": {"reasons_include": ["malformed_timeline"]},
    },
    {
        "name": "duplicate timestamps do not integrate",
        "valid": False,
        "timeline": duplicate_timestamp_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {"reasons_include": ["malformed_timeline"]},
    },
    {
        "name": "missing run manifest is reported",
        "valid": False,
        "omit_run": True,
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {"reasons_include": ["missing_run"]},
    },
    {
        "name": "malformed baselineByRole is an invalid run with diagnostics",
        "valid": False,
        "run": {"baselineByRole": ["server"]},
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["invalid_run"],
            # the raw input stays visible: nothing is invented in its place
            "values": {"arm.baselineByRole": ["server"]},
        },
    },
    {
        "name": "a cooldown declared active is not a quiet cooldown",
        "valid": False,
        "run": {"phaseEvents": [
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
            {"name": "cooldown", "startedAt": "2026-10-26T10:01:00Z", "loadActive": True},
        ]},
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {"cooldown.complete": False},
        },
    },
    {
        "name": "a cooldown without a phase event is not a quiet cooldown",
        "valid": False,
        "run": {"phaseEvents": [
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
        ]},
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {"cooldown.complete": False},
        },
    },
    {
        "name": "a cooldown with contradictory events is not a quiet cooldown",
        "valid": False,
        "run": {"phaseEvents": [
            {"name": "load", "startedAt": "2026-10-26T09:59:50Z", "loadActive": True},
            {"name": "cooldown", "startedAt": "2026-10-26T10:01:00Z", "loadActive": False},
            {"name": "cooldown", "startedAt": "2026-10-26T10:02:00Z", "loadActive": True},
        ]},
        "timeline": base_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["incomplete_cooldown"],
            "values": {"cooldown.complete": False},
        },
    },
    {
        "name": "an empty request record is malformed evidence",
        "valid": False,
        "timeline": base_timeline(),
        "requests": {"load": [{}, req()], "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["malformed_requests"],
            "values": {
                "phases.load.requests.total": 2,
                "phases.load.requests.succeeded": 1,
                "phases.load.requests.failed": 1,
                "phases.load.requests.failureReasons": {"malformed_record": 1},
            },
        },
    },
    {
        "name": "a nonfinite observation is malformed evidence",
        "valid": False,
        "timeline": base_timeline(),
        "requests": {"load": [req(ttft=float("nan")), req(tpot=float("inf"))], "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["malformed_requests"],
            "values": {
                "phases.load.requests.total": 2,
                "phases.load.requests.succeeded": 0,
                "phases.load.requests.failed": 2,
                "phases.load.requests.failureReasons": {"malformed_record": 2},
                "phases.load.latencies.ttft_ms.count": 0,
                "phases.load.latencies.tpot_ms.count": 0,
            },
        },
    },
    {
        "name": "an error record with an empty message stays failed",
        "valid": False,
        "timeline": base_timeline()[:1],
        "requests": {"load": [bad_req(error=""), req()], "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "values": {
                "phases.load.requests.total": 2,
                "phases.load.requests.succeeded": 1,
                "phases.load.requests.failed": 1,
                "phases.load.requests.failureReasons": {"": 1},
            },
        },
    },
    {
        "name": "a timestamp that does not parse does not integrate",
        "valid": False,
        "timeline": invalid_timestamp_timeline(),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {"reasons_include": ["malformed_timeline"]},
    },
    {
        "name": "samples without a uid have no identity",
        "valid": False,
        "timeline": without_uids(base_timeline()),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["uid_mismatch"],
            "values": {
                "uids.observedDeploymentUIDs": [],
                "uids.observedScalerUIDs": [],
            },
        },
    },
    {
        "name": "one sample without a uid invalidates the round",
        "valid": False,
        "timeline": without_uids(base_timeline(), index=2),
        "requests": {"load": LOAD_REQUESTS, "cooldown": COOLDOWN_REQUESTS},
        "expect": {
            "reasons_include": ["uid_mismatch"],
            "values": {"uids.observedDeploymentUIDs": ["md-uid-1"]},
        },
    },
]


def build_arm(root, case):
    """Materialize one case's inputs; every field is data written verbatim."""
    if not case.get("omit_run"):
        write_run(root, **case.get("run", {}))
    if "timeline" in case:
        write_timeline(root, case["timeline"])
    for phase, records in case.get("requests", {}).items():
        write_reqs(root, phase, records)
    for phase in case.get("empty_requests", []):
        (root / f"{phase}_reqs.jsonl").touch()


def report_value(report, dotted):
    """Read one dotted path from the report, indexing lists by position."""
    value = report
    for part in dotted.split("."):
        value = value[int(part)] if isinstance(value, list) else value[part]
    return value


def run_parser(arm):
    out = arm / "report.json"
    proc = subprocess.run(
        [sys.executable, str(SCRIPT), str(arm), "--out", str(out)],
        capture_output=True, text=True,
    )
    return proc, out


class AutoscalingReportFixtureTest(unittest.TestCase):
    """Every case drives the real CLI over a built arm directory.

    THE EXIT CODE IS FAIL-CLOSED: 0 only for a valid round, 1 for an invalid
    round whose report still carries the reasons, 2 for usage and IO errors.
    """

    def test_fixture_cases(self):
        for case in CASES:
            with self.subTest(case=case["name"]):
                with tempfile.TemporaryDirectory() as tmp:
                    arm = Path(tmp) / "arm"
                    arm.mkdir()
                    build_arm(arm, case)
                    proc, out = run_parser(arm)
                    self.assertEqual(proc.returncode, 0 if case["valid"] else 1,
                                     msg=proc.stderr)
                    report = json.loads(out.read_text())
                    self.assertEqual(report["schemaVersion"], "1")
                    self.assertEqual(report["validity"]["valid"], case["valid"])
                    expected = case["expect"]
                    if "reasons" in expected:
                        self.assertEqual(report["validity"]["reasons"], expected["reasons"])
                    for reason in expected.get("reasons_include", []):
                        self.assertIn(reason, report["validity"]["reasons"])
                    if "phase_names" in expected:
                        # THE WHOLE PHASE VECTOR IS COMPARED, so an extra or
                        # missing phase is rejected, not just the first two.
                        self.assertEqual(
                            [phase["phase"] for phase in report["timeline"]["phases"]],
                            expected["phase_names"])
                    for path, value in expected.get("values", {}).items():
                        self.assertEqual(report_value(report, path), value, path)
                    for word in expected.get("absent_strings", []):
                        self.assertNotIn(word, json.dumps(report))

    def test_usage_error_exits_two(self):
        proc = subprocess.run([sys.executable, str(SCRIPT)], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 2)

    def test_observed_baseline_and_cooldown_guards(self):
        cases = [
            {"name": "missing observed baseline", "event": "2026-10-26T09:59:40Z",
             "final": 2, "pending": None, "valid": True, "response": None},
            {"name": "ready above baseline", "event": "2026-10-26T09:59:50Z",
             "final": 4, "pending": None, "valid": False, "response": 20000},
            {"name": "pending cohort despite equal counts", "event": "2026-10-26T09:59:50Z",
             "final": 2, "pending": {"previousReplicas": {"server": 2}},
             "valid": False, "response": 20000},
            {"name": "real growth at event timestamp", "event": "2026-10-26T10:00:10Z",
             "final": 2, "pending": None, "valid": True, "response": 0},
            {"name": "truncated request record", "event": "2026-10-26T09:59:50Z",
             "final": 2, "pending": None, "valid": False, "response": 20000,
             "append": "{broken\n", "reason": "malformed_requests"},
            {"name": "empty request artifact", "event": "2026-10-26T09:59:50Z",
             "final": 2, "pending": None, "valid": False, "response": 20000,
             "empty_requests": True, "reason": "missing_requests"},
        ]
        for case in cases:
            with self.subTest(case=case["name"]), tempfile.TemporaryDirectory() as tmp:
                arm = Path(tmp) / "arm"
                arm.mkdir()
                build_complete(arm)
                run = json.loads((arm / "run.json").read_text())
                run["phaseEvents"][0]["startedAt"] = case["event"]
                write_json(arm / "run.json", run)
                rows = base_timeline()
                final = valid_md(roles=[("server", case["final"], case["final"])])
                rows[-1]["deployment"].update(spec=final["spec"], status=final["status"])
                if case["pending"] is not None:
                    rows[-1]["scaler"]["status"]["pendingCohort"] = case["pending"]
                write_timeline(arm, rows)
                if case.get("append"):
                    with (arm / "load_reqs.jsonl").open("a") as stream:
                        stream.write(case["append"])
                if case.get("empty_requests"):
                    (arm / "load_reqs.jsonl").write_text("")
                proc, out = run_parser(arm)
                report = json.loads(out.read_text())
                self.assertEqual(proc.returncode, 0 if case["valid"] else 1)
                response = report["timeline"]["events"]["load"]["roles"]["server"]
                self.assertEqual(response["recommendationResponseMs"]["value"], case["response"])
                if case["response"] is None:
                    for field in ("recommendationResponseMs", "commitResponseMs", "growthReadyResponseMs"):
                        self.assertIsNone(response[field]["value"])
                        self.assertIn("pre-event", response[field]["reason"])
                if not case["valid"]:
                    self.assertIn(case.get("reason", "incomplete_cooldown"), report["validity"]["reasons"])
                if case.get("append"):
                    counts = report["phases"]["load"]["requests"]
                    self.assertEqual(counts["total"], counts["succeeded"] + counts["failed"])


class QuantileDefinitionTest(unittest.TestCase):
    """The quantile rule is the driver's own nearest rank, so the report and
    the load summary print the same number for the same observations."""

    def test_matches_driver_nearest_rank(self):
        nearest_rank = autoscaling_report.nearest_rank
        self.assertEqual(nearest_rank([1.0, 2.0, 3.0, 4.0, 5.0], 95), 5.0)
        self.assertEqual(nearest_rank([1.0, 2.0, 3.0, 4.0, 5.0], 50), 3.0)
        self.assertEqual(nearest_rank([1.0, 2.0, 3.0, 4.0], 50), 2.0)
        self.assertEqual(nearest_rank([7.0], 95), 7.0)
        self.assertIsNone(nearest_rank([], 95))

    def test_missing_is_not_zero(self):
        block = autoscaling_report.quantile_block([])
        self.assertIsNone(block["p50"])
        self.assertEqual(block["count"], 0)
        self.assertEqual(block["reason"], "no observations")
        zero = autoscaling_report.quantile_block([0.0])
        self.assertEqual(zero["p50"], 0.0)
        self.assertIsNone(zero["reason"])

    def test_malformed_timeline_row_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm = Path(tmp)
            write_run(arm)
            (arm / "timeline.jsonl").write_text("{not json}\n")
            proc, out = run_parser(arm)
            self.assertEqual(proc.returncode, 1)
            report = json.loads(out.read_text())
            self.assertIn("malformed_timeline", report["validity"]["reasons"])
            self.assertFalse(report["validity"]["valid"])

    def test_missing_required_objects_are_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm = Path(tmp)
            write_run(arm)
            write_timeline(arm, [sample("2026-10-26T10:00:00Z", "load")])
            proc, out = run_parser(arm)
            self.assertEqual(proc.returncode, 1)
            report = json.loads(out.read_text())
            self.assertIn("missing_required_objects", report["validity"]["reasons"])

    def test_unsupported_schema_is_reported(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm = Path(tmp)
            write_run(arm, schemaVersion="2")
            write_timeline(arm, base_timeline()[:1])
            proc, out = run_parser(arm)
            self.assertEqual(proc.returncode, 1)
            report = json.loads(out.read_text())
            self.assertIn("unsupported_schema", report["validity"]["reasons"])

    def test_retirement_at_end_is_recorded_and_invalidates_cooldown(self):
        with tempfile.TemporaryDirectory() as tmp:
            arm = Path(tmp)
            write_run(arm)
            rows = base_timeline()
            rows[-1] = sample("2026-10-26T10:06:30Z", "cooldown",
                              md=valid_md(roles=[("server", 2, 2)],
                                          retirement={"roleName": "server", "replicaOrdinal": 3,
                                                      "state": "Draining"}),
                              scaler=valid_scaler())
            write_timeline(arm, rows)
            write_reqs(arm, "load", [req()])
            write_reqs(arm, "cooldown", [req()])
            proc, out = run_parser(arm)
            self.assertEqual(proc.returncode, 1)
            report = json.loads(out.read_text())
            self.assertTrue(report["timeline"]["retirement"]["presentAtEnd"])
            self.assertIn("incomplete_cooldown", report["validity"]["reasons"])


if __name__ == "__main__":
    unittest.main()
