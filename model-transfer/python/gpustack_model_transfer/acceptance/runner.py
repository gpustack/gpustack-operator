"""Suite registry and the PASS/NO-GO/UNRUN report."""

import argparse
import json
import sys
from datetime import datetime

from gpustack_model_transfer.acceptance.config import RefusedError, load_and_validate
from gpustack_model_transfer.acceptance.report import CheckResult

# Backends and suites this runner knows by name. A known but missing
# combination is refused as "not implemented", never silently skipped.
KNOWN_BACKENDS = ("local", "nixl", "hixl")
KNOWN_SUITES = {
    "contract": None,  # no engine applies
    "backend": None,
    "model-load": ("vllm", "vllm-ascend", "sglang", "modelexpress"),
    "managed-startup": ("vllm", "vllm-ascend"),
    "lifecycle": None,
    "safety": None,
    "ep-coexistence": None,
}
IMPLEMENTED = {
    ("local", "contract"),
    ("local", "model-load"),
    ("nixl", "backend"),
    ("nixl", "safety"),
    ("hixl", "backend"),
    ("hixl", "safety"),
    ("nixl", "managed-startup"),
    ("hixl", "managed-startup"),
    ("nixl", "lifecycle"),
    ("nixl", "ep-coexistence"),
}
# model-load is implemented per engine; managed-startup runs on the
# cluster harnesses (nixl/vllm and hixl/vllm-ascend), lifecycle on the
# multi-host harness, and ep-coexistence on nixl alone: no CANN EP
# claim follows from HIXL, so hixl stays refused.
IMPLEMENTED_MODEL_LOAD_ENGINES = frozenset(
    {"vllm", "sglang", "vllm-ascend", "modelexpress"}
)

EXIT_PASS = 0
EXIT_NO_GO = 1
EXIT_REFUSED = 2
EXIT_NOT_IMPLEMENTED = 3

# Sentinel for an unparsable --now value.
_INVALID_NOW = object()


def _emit(payload: dict) -> None:
    json.dump(payload, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    sys.stdout.flush()


def _refuse(reason: str, code: int) -> int:
    _emit(
        {
            "schema_version": 1,
            "runner": "gpustack_model_transfer.acceptance",
            "refused": True,
            "reason": reason,
        }
    )
    print(f"refused: {reason}", file=sys.stderr)
    return code


def _parse_args(argv):
    parser = argparse.ArgumentParser(
        prog="gpustack_model_transfer.acceptance",
        description="Fail-closed model-transfer acceptance runner.",
    )
    parser.add_argument(
        "--config", required=True, help="path to the acceptance configuration JSON"
    )
    parser.add_argument("--backend", required=True, help="backend under acceptance")
    parser.add_argument(
        "--engine",
        default=None,
        help="engine under acceptance, when the suite needs one",
    )
    parser.add_argument("--suite", required=True, help="acceptance suite to run")
    parser.add_argument(
        "--now",
        default=None,
        help="ISO-8601 instant with timezone to judge "
        "readiness freshness against, for "
        "reproducible runs",
    )
    return parser.parse_args(argv)


def _parse_now(value):
    """Parse the --now instant; None means the current UTC time."""
    if value is None:
        return None
    try:
        instant = datetime.fromisoformat(value)
    except ValueError:
        return _INVALID_NOW
    if instant.tzinfo is None:
        return _INVALID_NOW
    return instant


def main(argv=None, now=None) -> int:
    """Run one acceptance invocation; now overrides the freshness clock."""
    try:
        args = _parse_args(argv)
    except SystemExit as exc:
        # argparse already explained itself on stderr.
        return int(exc.code or EXIT_REFUSED)

    if args.now is not None and now is not None:
        return _refuse(
            "pass either --now or a programmatic now, not both", EXIT_REFUSED
        )
    clock = now
    if clock is None and args.now is not None:
        clock = _parse_now(args.now)
        if clock is _INVALID_NOW:
            return _refuse(
                f"--now is not an ISO-8601 instant with a timezone: {args.now!r}",
                EXIT_REFUSED,
            )

    if args.backend not in KNOWN_BACKENDS:
        return _refuse(f"unknown backend: {args.backend!r}", EXIT_REFUSED)
    if args.suite not in KNOWN_SUITES:
        return _refuse(f"unknown suite: {args.suite!r}", EXIT_REFUSED)
    allowed_engines = KNOWN_SUITES[args.suite]
    if allowed_engines is None and args.engine is not None:
        return _refuse(
            f"engine {args.engine!r} is not applicable to suite {args.suite!r}",
            EXIT_REFUSED,
        )
    if allowed_engines is not None and args.engine not in allowed_engines:
        return _refuse(
            f"engine {args.engine!r} is not qualified for suite {args.suite!r}",
            EXIT_REFUSED,
        )
    # model-load is implemented per engine, whatever backend carries it;
    # every other suite is implemented per (backend, suite) pair.
    if args.suite == "model-load":
        if args.engine not in IMPLEMENTED_MODEL_LOAD_ENGINES:
            return _refuse(
                f"engine {args.engine!r} is not implemented for suite "
                f"model-load yet (implemented: "
                f"{', '.join(sorted(IMPLEMENTED_MODEL_LOAD_ENGINES))})",
                EXIT_NOT_IMPLEMENTED,
            )
    elif (args.backend, args.suite) not in IMPLEMENTED:
        return _refuse(
            f"backend {args.backend!r} suite {args.suite!r} is not implemented yet",
            EXIT_NOT_IMPLEMENTED,
        )

    try:
        context = load_and_validate(
            args.config, args.backend, args.suite, args.engine, now=clock
        )
    except RefusedError as exc:
        return _refuse(exc.reason, EXIT_REFUSED)

    # Imported here to keep the refusal path free of suite code.
    if args.suite == "model-load":
        if args.engine == "modelexpress":
            from gpustack_model_transfer.acceptance import modelexpress_suite

            results = modelexpress_suite.run(context, backend=args.backend)
        else:
            from gpustack_model_transfer.acceptance import model_load_suite

            results = model_load_suite.run(
                context, engine=args.engine, backend=args.backend
            )
    elif args.suite == "managed-startup":
        from gpustack_model_transfer.acceptance import managed_startup_suite

        results = managed_startup_suite.run(context, engine=args.engine)
    elif args.suite == "lifecycle":
        from gpustack_model_transfer.acceptance import lifecycle_suite

        results = lifecycle_suite.run(context)
    elif args.suite == "ep-coexistence":
        from gpustack_model_transfer.acceptance import ep_coexistence_suite

        results = ep_coexistence_suite.run(context)
    elif args.suite == "backend":
        if args.backend == "hixl":
            from gpustack_model_transfer.acceptance import hixl_backend_suite

            results = hixl_backend_suite.run(context)
        else:
            from gpustack_model_transfer.acceptance import nixl_backend_suite

            results = nixl_backend_suite.run(context)
    elif args.suite == "safety":
        if args.backend == "hixl":
            from gpustack_model_transfer.acceptance import hixl_safety_suite

            results = hixl_safety_suite.run(context)
        else:
            from gpustack_model_transfer.acceptance import nixl_safety_suite

            results = nixl_safety_suite.run(context)
    else:
        from gpustack_model_transfer.acceptance import contract_suite

        results = contract_suite.run(context)
    passed = sum(1 for r in results if r.status == "PASS")
    no_go = sum(1 for r in results if r.status == "NO-GO")
    unrun = sum(1 for r in results if r.status == "UNRUN")
    overall = "PASS" if no_go == 0 and unrun == 0 else "NO-GO"
    _emit(
        {
            "schema_version": 1,
            "runner": "gpustack_model_transfer.acceptance",
            "backend": args.backend,
            "engine": args.engine,
            "suite": args.suite,
            "contract_version": context["manifest"].contract_version,
            "results": [
                {"capability": r.capability, "status": r.status, "detail": r.detail}
                for r in results
            ],
            "summary": {"pass": passed, "no_go": no_go, "unrun": unrun},
            "overall": overall,
        }
    )
    print(
        f"overall: {overall} (pass={passed} no_go={no_go} unrun={unrun})",
        file=sys.stderr,
    )
    return EXIT_PASS if overall == "PASS" else EXIT_NO_GO


__all__ = ["CheckResult", "main"]
