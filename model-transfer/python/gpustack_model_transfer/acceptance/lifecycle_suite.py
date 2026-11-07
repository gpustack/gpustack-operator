"""The lifecycle suite: source churn, contention, and bounded recovery.

This suite runs on an approved source plus at least two target hosts
(T15): concurrent authorized readers share the source within their
declared budgets, admission is fair across namespaces, generation
eviction isolates surviving readers, a daemon restart reconstructs live
state without treating empty process lists as DMA-drain evidence,
repeated reconnects stay within budget, a submitted nonterminal failure
stays quarantined until a qualified fence releases it, and no recycled
address ever exposes another generation.

The harness contract, one hook per observable, cross-checked against
the validated context: every reader's negotiated max_inflight and
window_bytes must stay within context['config']['limits'].

  open_readers(count) -> [reader]
      reader.reads() -> [payload]
      reader.budget() -> {"max_inflight", "observed_inflight",
                          "window_bytes", "observed_window_bytes"}
  admission_trace(namespaces) -> [{"namespace", "admitted",
                                   "wait_cycles"}]
  evict_generation(generation) -> {"surviving_reads_byte_exact",
                                   "stale_exposure"}
  restart_daemon() -> {"live_reservations_rebuilt", "drain_evidence",
                       "released_only_qualified"}
  reconnect_cycles(count) -> {"cycles", "within_budget",
                              "bytes_transferred", "budget_bytes"}
  submit_nonterminal_fault() -> {"quarantined", "retained_bytes",
                                 "fence", "released_after_fence"}
  recycle_address_probe() -> {"distinct_generations",
                              "cross_generation_exposure"}

Without a harness every capability is UNRUN: a missing multi-host
environment fails the run, never a skipped pass. A harness is injected
as runtime= (a zero-argument factory, fresh per capability) or
discovered on the cluster host through
GPUSTACK_MODEL_TRANSFER_LIFECYCLE_HARNESS as "module:factory".
"""

from gpustack_model_transfer.acceptance.report import CheckResult

HARNESS_VARIABLE = "GPUSTACK_MODEL_TRANSFER_LIFECYCLE_HARNESS"

CAPABILITIES = (
    "concurrent-authorized-readers",
    "namespace-admission-fairness",
    "generation-eviction-isolation",
    "daemon-restart-reconstruction",
    "repeated-reconnect-budgets",
    "submitted-failure-quarantine-fence",
    "no-recycled-address-exposure",
)


def run(context, runtime=None) -> list:
    """Run every lifecycle capability; see the module docstring.

    runtime, when given, is a zero-argument factory producing a fresh
    harness per capability — the reference harness in tests. Without it
    the multi-host harness is discovered through the environment; a
    host without one UNRUNs every capability.
    """
    if runtime is None:
        harness, reason = _discover_harness(context)
        if harness is None:
            return [
                CheckResult(capability, "UNRUN", reason) for capability in CAPABILITIES
            ]

        def factory():
            return harness  # one live multi-host harness, shared

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
    """Build the multi-host harness, or (None, why there is none)."""
    import importlib
    import os

    spec = (os.environ.get(HARNESS_VARIABLE) or "").strip()
    if not spec:
        return None, (
            "no lifecycle harness on this host: this suite runs on an "
            "approved source plus target hosts through a configured harness or "
            f"against a harness injected as runtime=; set {HARNESS_VARIABLE} "
            'to "module:factory" on the cluster host'
        )
    module_name, _, attr = spec.partition(":")
    try:
        module = importlib.import_module(module_name)
        factory = getattr(module, attr or "build_harness")
        return factory(context), ""
    except Exception as exc:
        return None, f"the harness at {spec!r} could not be built: {exc}"


def _check_concurrent_readers(context, harness):
    limits = context["config"]["limits"]
    readers = harness.open_readers(3)
    if len(readers) != 3:
        raise AssertionError(f"only {len(readers)} of 3 authorized readers opened")
    payloads = []
    for reader in readers:
        reads = reader.reads()
        if not reads:
            raise AssertionError("an authorized reader completed no reads")
        budget = reader.budget()
        if budget["max_inflight"] > limits["max_inflight"]:
            raise AssertionError(
                f"a reader negotiated an in-flight budget above the run's "
                f"limits: {budget['max_inflight']} > {limits['max_inflight']}"
            )
        if budget["window_bytes"] > limits["window_bytes"]:
            raise AssertionError(
                f"a reader negotiated a window above the run's limits: "
                f"{budget['window_bytes']} > {limits['window_bytes']}"
            )
        if budget["observed_inflight"] > budget["max_inflight"]:
            raise AssertionError(
                f"a reader exceeded its in-flight budget: "
                f"{budget['observed_inflight']} > {budget['max_inflight']}"
            )
        if budget["observed_window_bytes"] > budget["window_bytes"]:
            raise AssertionError(
                f"a reader exceeded its window budget: "
                f"{budget['observed_window_bytes']} > {budget['window_bytes']}"
            )
        payloads.append(b"".join(reads))
    if len(set(payloads)) != 1:
        raise AssertionError("concurrent authorized readers observed different bytes")
    return (
        f"{len(readers)} concurrent authorized readers read byte-identical "
        "content within their declared budgets"
    )


def _check_admission_fairness(context, harness):
    namespaces = ["tenant-a", "tenant-b", "tenant-c"]
    trace = harness.admission_trace(namespaces)
    by_namespace = {entry["namespace"]: entry for entry in trace}
    missing = [ns for ns in namespaces if ns not in by_namespace]
    if missing:
        raise AssertionError(f"namespaces never admitted: {missing}")
    unadmitted = [entry["namespace"] for entry in trace if not entry.get("admitted")]
    if unadmitted:
        raise AssertionError(f"namespaces starved of admission: {unadmitted}")
    waits = [entry["wait_cycles"] for entry in trace]
    if max(waits) > len(namespaces):
        raise AssertionError(
            "admission starved a namespace: waits "
            f"{ {e['namespace']: e['wait_cycles'] for e in trace} }"
        )
    spread = max(waits) - min(waits)
    if spread > 1:
        raise AssertionError(f"admission was unfair across namespaces: {waits}")
    return f"all {len(namespaces)} namespaces admitted, wait spread {spread} cycle(s)"


def _check_generation_eviction(context, harness):
    outcome = harness.evict_generation(1)
    if not outcome.get("surviving_reads_byte_exact"):
        raise AssertionError("surviving reads changed bytes after eviction")
    if outcome.get("stale_exposure"):
        raise AssertionError("eviction exposed a stale generation's bytes")
    return "evicting a generation isolated its readers and exposed nothing stale"


def _check_daemon_restart(context, harness):
    outcome = harness.restart_daemon()
    if not outcome.get("live_reservations_rebuilt"):
        raise AssertionError("the daemon restart did not rebuild live reservations")
    evidence = outcome.get("drain_evidence")
    if evidence != "qualified-fence":
        raise AssertionError(
            "reconstruction released reservations without a qualified fence: "
            f"drain evidence was {evidence!r}; an empty process list is not "
            "DMA-drain evidence"
        )
    if not outcome.get("released_only_qualified"):
        raise AssertionError("the restart released reservations it could not drain")
    return (
        "the daemon restart rebuilt live reservations and released only "
        "fence-qualified reservations"
    )


def _check_reconnect_budgets(context, harness):
    outcome = harness.reconnect_cycles(5)
    if outcome.get("cycles") != 5:
        raise AssertionError(
            f"only {outcome.get('cycles')} of 5 reconnect cycles completed"
        )
    if not outcome.get("within_budget"):
        raise AssertionError(
            f"reconnect traffic exceeded the budget: "
            f"{outcome.get('bytes_transferred')} > {outcome.get('budget_bytes')}"
        )
    return (
        f"{outcome['cycles']} load/reconnect cycles stayed within "
        f"{outcome['budget_bytes']} declared bytes"
    )


def _check_quarantine_fence(context, harness):
    outcome = harness.submit_nonterminal_fault()
    if not outcome.get("quarantined"):
        raise AssertionError("a submitted nonterminal failure was not quarantined")
    if outcome.get("retained_bytes", 0) <= 0:
        raise AssertionError("the quarantine retained no bytes; nothing was held")
    if outcome.get("fence") != "qualified":
        raise AssertionError(
            f"the quarantine fence was not qualified: {outcome.get('fence')!r}"
        )
    if not outcome.get("released_after_fence"):
        raise AssertionError("the qualified fence did not release the retention")
    return (
        f"a submitted nonterminal failure held {outcome['retained_bytes']} bytes "
        "until a qualified fence released them"
    )


def _check_no_recycled_address(context, harness):
    outcome = harness.recycle_address_probe()
    if not outcome.get("distinct_generations"):
        raise AssertionError("recycled addresses were not pinned to new generations")
    if outcome.get("cross_generation_exposure"):
        raise AssertionError("a recycled address exposed another generation")
    return "recycled addresses never exposed another generation's bytes"


_CHECKS = (
    ("concurrent-authorized-readers", _check_concurrent_readers),
    ("namespace-admission-fairness", _check_admission_fairness),
    ("generation-eviction-isolation", _check_generation_eviction),
    ("daemon-restart-reconstruction", _check_daemon_restart),
    ("repeated-reconnect-budgets", _check_reconnect_budgets),
    ("submitted-failure-quarantine-fence", _check_quarantine_fence),
    ("no-recycled-address-exposure", _check_no_recycled_address),
)


__all__ = [
    "CAPABILITIES",
    "HARNESS_VARIABLE",
    "run",
]
