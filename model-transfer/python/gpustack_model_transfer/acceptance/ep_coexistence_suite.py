"""The EP coexistence suite: nixl_ep beside loader agents, honestly.

This suite runs only on its separately approved EP environment (T17),
on an already qualified native EP tuple: the nixl_ep library coexists
with the loader's own agents and contexts, NIC and memory-region
budgets hold, initialization and teardown order cleanly, serving
matches native EP, and online joiners migrate experts natively instead
of cold-loading duplicates. No CANN EP claim follows from HIXL: the
suite itself is a nixl qualification, and a harness claiming CANN EP
qualification fails the run.

The harness contract, one hook per observable:

  ep_tuple() -> {"engine", "ep_backend", "world_size"}
  coexistence_run() -> {"loader_agents", "ep_agents", "isolated_contexts"}
  budget_report() -> {"nic_budget", "nic_used", "mr_budget", "mr_used"}
  teardown_order() -> [event] with "ep-context-init" before
      "loader-agents-init", and "loader-agents-teardown" before
      "ep-context-teardown"
  serving_parity() -> {"with_model_transfer", "native"}
  joiner_join() -> {"joined_online", "cold_loads", "migration"}
  cann_ep_claimed() -> bool

Without a harness every capability is UNRUN: a missing EP environment
fails the run, never a skipped pass. A harness is injected as runtime=
(a zero-argument factory, fresh per capability) or discovered on the
cluster host through GPUSTACK_MODEL_TRANSFER_EP_HARNESS as "module:factory".
"""

from gpustack_model_transfer.acceptance.report import CheckResult

HARNESS_VARIABLE = "GPUSTACK_MODEL_TRANSFER_EP_HARNESS"

CAPABILITIES = (
    "nixl-ep-agent-coexistence",
    "nic-and-mr-budgets",
    "init-teardown-ordering",
    "serving-equivalence",
    "online-joiner-no-cold-reload",
    "no-cann-ep-claim",
)


def run(context, runtime=None) -> list:
    """Run every EP coexistence capability; see the module docstring.

    runtime, when given, is a zero-argument factory producing a fresh
    harness per capability — the reference harness in tests. Without it
    the approved EP harness is discovered through the environment; a
    host without one UNRUNs every capability.
    """
    if runtime is None:
        harness, reason = _discover_harness(context)
        if harness is None:
            return [
                CheckResult(capability, "UNRUN", reason) for capability in CAPABILITIES
            ]

        def factory():
            return harness  # one live EP environment harness, shared

    else:
        factory = runtime

    results = []
    for capability, check in _CHECKS:
        results.append(_run_check(capability, check, context, factory()))
    return results


def _run_check(capability, check, context, harness) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip."""
    try:
        detail = check(context, harness)
        return CheckResult(capability, "PASS", detail)
    except Exception as exc:
        return CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")


def _discover_harness(context):
    """Build the approved EP harness, or (None, why there is none)."""
    import importlib
    import os

    spec = (os.environ.get(HARNESS_VARIABLE) or "").strip()
    if not spec:
        return None, (
            "no EP coexistence harness on this host: this suite runs only on "
            "its separately approved EP environment through a configured harness or "
            f"against a harness injected as runtime=; set {HARNESS_VARIABLE} "
            'to "module:factory" on that host'
        )
    module_name, _, attr = spec.partition(":")
    try:
        module = importlib.import_module(module_name)
        factory = getattr(module, attr or "build_harness")
        return factory(context), ""
    except Exception as exc:
        return None, f"the harness at {spec!r} could not be built: {exc}"


def _check_agent_coexistence(context, harness):
    if context["config"]["backend"] != "nixl":
        raise AssertionError(
            "the EP coexistence suite is a nixl qualification; no CANN EP "
            "claim follows from HIXL"
        )
    tup = harness.ep_tuple()
    if tup.get("ep_backend") != "nixl_ep":
        raise AssertionError(f"the tuple is not a nixl_ep tuple: {tup}")
    if not tup.get("world_size", 0) >= 2:
        raise AssertionError(f"an EP tuple needs at least two ranks: {tup}")
    run = harness.coexistence_run()
    if run.get("loader_agents", 0) < 1:
        raise AssertionError("the loader created no agents beside nixl_ep")
    if run.get("ep_agents", 0) < 1:
        raise AssertionError("nixl_ep created no agents beside the loader")
    if not run.get("isolated_contexts"):
        raise AssertionError("loader and EP agents shared a context")
    return (
        f"{run['loader_agents']} loader agents and {run['ep_agents']} EP "
        f"agents coexist on {tup.get('engine')} world size "
        f"{tup['world_size']} with isolated contexts"
    )


def _check_budgets(context, harness):
    report = harness.budget_report()
    for kind in ("nic", "mr"):
        budget = report.get(f"{kind}_budget", 0)
        used = report.get(f"{kind}_used", 0)
        if used > budget:
            raise AssertionError(
                f"the {kind.upper()} budget was exceeded: {used} > {budget}"
            )
    return (
        f"NIC {report['nic_used']}/{report['nic_budget']} and MR "
        f"{report['mr_used']}/{report['mr_budget']} within budget"
    )


def _check_teardown_ordering(context, harness):
    events = harness.teardown_order()
    order = {event: index for index, event in enumerate(events)}
    for required in (
        "ep-context-init",
        "loader-agents-init",
        "loader-agents-teardown",
        "ep-context-teardown",
    ):
        if required not in order:
            raise AssertionError(f"teardown events miss {required!r}: {events}")
    if order["loader-agents-init"] < order["ep-context-init"]:
        raise AssertionError("loader agents initialized before the EP context")
    if order["ep-context-teardown"] < order["loader-agents-teardown"]:
        raise AssertionError("the EP context tore down before the loader's agents")
    return "loader agents initialize inside the EP context and tear down first"


def _check_serving_equivalence(context, harness):
    parity = harness.serving_parity()
    if parity.get("with_model_transfer") != parity.get("native"):
        raise AssertionError("serving with nixl_ep coexistence differs from native")
    return "serving with nixl_ep coexistence matches native EP serving"


def _check_online_joiner(context, harness):
    joiner = harness.joiner_join()
    if not joiner.get("joined_online"):
        raise AssertionError("the joiner did not join online")
    if joiner.get("cold_loads", 0) != 0:
        raise AssertionError(
            f"the online joiner cold-loaded {joiner['cold_loads']} time(s)"
        )
    if joiner.get("migration") != "native":
        raise AssertionError(
            f"experts migrated through {joiner.get('migration')!r}, not natively"
        )
    return "an online joiner migrated experts natively with no duplicate cold load"


def _check_no_cann_claim(context, harness):
    if harness.cann_ep_claimed():
        raise AssertionError(
            "the harness claimed CANN EP qualification; no CANN EP claim "
            "follows from HIXL"
        )
    return "no CANN EP claim was made; EP coexistence stays a nixl qualification"


_CHECKS = (
    ("nixl-ep-agent-coexistence", _check_agent_coexistence),
    ("nic-and-mr-budgets", _check_budgets),
    ("init-teardown-ordering", _check_teardown_ordering),
    ("serving-equivalence", _check_serving_equivalence),
    ("online-joiner-no-cold-reload", _check_online_joiner),
    ("no-cann-ep-claim", _check_no_cann_claim),
)


__all__ = [
    "CAPABILITIES",
    "HARNESS_VARIABLE",
    "run",
]
