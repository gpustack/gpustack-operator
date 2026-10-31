"""The managed startup suite: a managed role from remote weights, honestly.

This suite runs on the approved cluster (T12 CUDA over nixl, T13 CANN
over hixl): a managed engine role starts from a metadata-only target,
consumes authorized remote weights, and matches native forward
behavior, with no hidden hub or full-checkpoint download behind the
scenes. Failures are honest under both policies, serving replicas
survive source loss, the run publishes its phase timings separately,
and configured connectors stay continuous. On vllm-ascend the CANN
gate is checked too: any missing source grant, binding, allocation, or
reclamation rule disables CANN remote delivery without blocking
qualified CUDA support.

The harness contract, one hook per observable, cross-checked against
the validated context: the metadata target's manifest digest must
    equal context['config']['artifact']['manifest_sha256'], and every
    reader budget must stay within context['config']['limits'] where the
    lifecycle suite reads them.

  metadata_target() -> {"manifest_digest", "metadata_only",
                        "local_checkpoint_bytes"}
  start_role(target, policy, qualified=True) -> role
      role.serving_status() -> {"phase", "reason"}
      role.download_events() -> [{"kind", "blocked"}]
      role.inventory_writes() -> [str]
      role.forward(inputs) / role.native_forward(inputs) -> outputs
      role.phase_timings() -> {"cold_fill_seconds", "warm_reuse_seconds",
                               "load_seconds", "serving_seconds"}
      role.fallback_record() -> {"occurred", "reason", "replacement_pod",
                                 "attempts", "route"}
      role.failed_rank_record() -> {"recreated", "reason"}
      role.replica_statuses() -> [{"replica", "phase", "delivery"}]
      role.connector_status() -> {"ec", "kv"}
  stale_identity(target) -> {"refused", "reason"}
  lose_source(role) -> role
  cann_gate() -> {"safe_binding", "source_grant", "device_allocation",
                  "network_allocation", "reclamation_rule", "cann_enabled",
                  "cuda_support_blocked"}

Without a harness every capability is UNRUN: a missing cluster fails
the run, never a skipped pass. A harness is injected as runtime= (a
zero-argument factory, fresh per capability) or discovered on the
cluster host through GPUSTACK_MODEL_TRANSFER_MANAGED_STARTUP_HARNESS as
"module:factory".
"""

from gpustack_model_transfer.acceptance.report import CheckResult

HARNESS_VARIABLE = "GPUSTACK_MODEL_TRANSFER_MANAGED_STARTUP_HARNESS"

CAPABILITIES = (
    "metadata-only-target",
    "no-hidden-checkpoint-download",
    "forward-equivalence",
    "require-refusal-honesty",
    "prefer-bounded-fallback",
    "stale-identity-refusal",
    "source-loss-serving-continuity",
    "phase-timing-publication",
    "connector-continuity",
)

# The CANN gate applies to the vllm-ascend engine alone; a CUDA run
# never claims it.
CANN_CAPABILITIES = ("source-npu-grant-and-recovery-gate",)


def capabilities_for(engine) -> tuple:
    """The capability set for one engine; CANN adds its own gate."""
    if engine == "vllm-ascend":
        return CAPABILITIES + CANN_CAPABILITIES
    return CAPABILITIES


def run(context, runtime=None, engine="vllm") -> list:
    """Run every managed-startup capability for one engine.

    runtime, when given, is a zero-argument factory producing a fresh
    harness per capability — the reference harness in tests. Without it
    the cluster host's harness is discovered through the environment;
    a host without one UNRUNs every capability.
    """
    capabilities = capabilities_for(engine)
    if runtime is None:
        harness, reason = _discover_harness(context)
        if harness is None:
            return [
                CheckResult(capability, "UNRUN", reason) for capability in capabilities
            ]

        def factory():
            return harness  # one live cluster harness, shared

    else:
        factory = runtime

    results = []
    for capability, check in _CHECKS:
        if capability not in capabilities:
            continue
        results.append(_run_check(capability, check, context, factory()))
    return results


def _run_check(capability, check, context, harness) -> CheckResult:
    """One capability's verdict: a failed check is NO-GO, never a skip."""
    try:
        detail = check(context, harness)
        if isinstance(detail, CheckResult):
            detail.capability = capability
            return detail
        return CheckResult(capability, "PASS", detail)
    except Exception as exc:
        return CheckResult(capability, "NO-GO", f"{type(exc).__name__}: {exc}")


def _discover_harness(context):
    """Build the cluster host's harness, or (None, why there is none)."""
    import importlib
    import os

    spec = (os.environ.get(HARNESS_VARIABLE) or "").strip()
    if not spec:
        return None, (
            "no managed-startup harness on this host: this suite runs on the "
            "approved cluster through a configured harness or against a harness "
            f"injected as runtime=; set {HARNESS_VARIABLE} to "
            '"module:factory" on the cluster host'
        )
    module_name, _, attr = spec.partition(":")
    try:
        module = importlib.import_module(module_name)
        factory = getattr(module, attr or "build_harness")
        return factory(context), ""
    except Exception as exc:
        return None, f"the harness at {spec!r} could not be built: {exc}"


def _check_metadata_only_target(context, harness):
    target = harness.metadata_target()
    if not target.get("metadata_only"):
        raise AssertionError("the target is not metadata-only")
    if target.get("local_checkpoint_bytes", 0) != 0:
        raise AssertionError(
            f"the target carries {target['local_checkpoint_bytes']} local "
            "checkpoint bytes; a no-checkpoint startup starts from zero"
        )
    expected = context["config"]["artifact"]["manifest_sha256"]
    if target.get("manifest_digest") != expected:
        raise AssertionError(
            "the harness target names manifest digest "
            f"{target.get('manifest_digest')!r} while the validated "
            f"configuration pins {expected!r}; the role did not start "
            "from the artifact this run accepted"
        )
    role = harness.start_role(target, "require")
    if role.serving_status()["phase"] != "serving":
        raise AssertionError("the managed role never reached serving")
    return (
        "the role started from a metadata-only target with no local "
        f"checkpoint and reached serving on {target['manifest_digest'][:16]}..."
    )


def _check_no_hidden_download(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    downloads = role.download_events()
    hidden = [
        event
        for event in downloads
        if event.get("kind") in ("hub-download", "full-checkpoint")
        and not event.get("blocked")
    ]
    if hidden:
        raise AssertionError(
            "a hidden download completed behind the remote load: "
            + ", ".join(sorted(event["kind"] for event in hidden))
        )
    writes = role.inventory_writes()
    if writes:
        raise AssertionError(f"the remote load wrote inventory destinations: {writes}")
    return (
        f"{len(downloads)} download attempts all blocked, zero inventory "
        "destination writes"
    )


def _check_forward_equivalence(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    inputs = ["a", "b"]
    remote = role.forward(inputs)
    native = role.native_forward(inputs)
    if remote != native:
        raise AssertionError("remote-loaded forward differs from native forward")
    return "remote-loaded forward matches native forward"


def _check_require_refusal_honesty(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require", qualified=False)
    status = role.serving_status()
    if status["phase"] != "failed":
        raise AssertionError(f"an unqualified Require startup served anyway: {status}")
    if not status.get("reason"):
        raise AssertionError("the Require refusal carried no visible reason")
    fallback = role.fallback_record()
    if fallback.get("occurred"):
        raise AssertionError("Require remote fell back instead of failing visibly")
    failed_rank = role.failed_rank_record()
    if not failed_rank.get("recreated"):
        raise AssertionError(
            "a failed rank left its worker/load group standing; the "
            "group is recreated, never reused after partial mutation"
        )
    return (
        f"Require remote failed visibly and distinctly: {status['reason']}; "
        "the failed rank's load group was recreated"
    )


def _check_prefer_bounded_fallback(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "prefer", qualified=False)
    fallback = role.fallback_record()
    if not fallback.get("occurred"):
        raise AssertionError(
            "Prefer remote recorded no fallback on an unqualified path"
        )
    if not fallback.get("reason"):
        raise AssertionError("the fallback carried no visible reason")
    if not fallback.get("replacement_pod"):
        raise AssertionError("the fallback did not replace the affected Pod")
    attempts = fallback.get("attempts", 0)
    if not 1 <= attempts <= 3:
        raise AssertionError(f"the fallback was not bounded: {attempts} attempts")
    status = role.serving_status()
    if status["phase"] != "serving":
        raise AssertionError(f"the replaced replica never served: {status}")
    replicas = role.replica_statuses()
    if not replicas:
        raise AssertionError("no per-replica status was reported")
    if len({entry.get("replica") for entry in replicas}) != len(replicas):
        raise AssertionError(f"the status record is not per-replica: {replicas}")
    if not any(entry.get("phase") == "serving" for entry in replicas):
        raise AssertionError(f"no replica reported serving: {replicas}")
    deliveries = {entry.get("delivery") for entry in replicas}
    if len(deliveries) < 2:
        raise AssertionError(
            f"mixed replica deliveries were flattened into one status: {replicas}"
        )
    return (
        f"Prefer replaced the Pod in {attempts} bounded attempt(s) through "
        f"{fallback.get('route', 'the declared route')} and served, with "
        f"{len(replicas)} replicas reporting distinct deliveries "
        + "/".join(sorted(d for d in deliveries if d))
    )


def _check_stale_identity_refusal(context, harness):
    target = harness.metadata_target()
    outcome = harness.stale_identity(target)
    if not outcome.get("refused"):
        raise AssertionError("a stale source identity was accepted")
    if not outcome.get("reason"):
        raise AssertionError("the stale-identity refusal named no identity")
    return f"stale identity refused: {outcome['reason']}"


def _check_source_loss_continuity(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    if role.serving_status()["phase"] != "serving":
        raise AssertionError("the role was not serving before source loss")
    harness.lose_source(role)
    status = role.serving_status()
    if status["phase"] != "serving":
        raise AssertionError(
            f"serving replicas changed phase during source loss: {status}"
        )
    return "serving replicas stayed serving through source loss"


def _check_phase_timing_publication(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    timings = role.phase_timings()
    phases = (
        "cold_fill_seconds",
        "warm_reuse_seconds",
        "load_seconds",
        "serving_seconds",
    )
    missing = [name for name in phases if name not in timings]
    if missing:
        raise AssertionError(f"phases not published separately: {missing}")
    for name in phases:
        if not isinstance(timings[name], (int, float)) or timings[name] < 0:
            raise AssertionError(f"{name} is not a non-negative duration")
    if timings["cold_fill_seconds"] <= 0:
        raise AssertionError("a cold source fill of zero seconds was published")
    return (
        "cold fill, warm reuse, load, and serving time published separately: "
        + ", ".join(f"{name}={timings[name]}" for name in phases)
    )


def _check_connector_continuity(context, harness):
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    status = role.connector_status()
    for connector in ("ec", "kv"):
        if status.get(connector) != "continuous":
            raise AssertionError(
                f"the configured {connector.upper()} connector did not stay "
                f"continuous: {status.get(connector)!r}"
            )
    return "configured EC and KV connectors stayed continuous through the load"


def _check_cann_gate(context, harness):
    if not callable(getattr(harness, "cann_gate", None)):
        return CheckResult(
            "source-npu-grant-and-recovery-gate",
            "UNRUN",
            "the harness exposes no CANN gate record",
        )
    target = harness.metadata_target()
    role = harness.start_role(target, "require")
    if role.serving_status()["phase"] != "serving":
        raise AssertionError("the role was not serving when the CANN gate was read")
    gate = harness.cann_gate()
    prerequisites = (
        "safe_binding",
        "source_grant",
        "device_allocation",
        "network_allocation",
        "reclamation_rule",
    )
    missing = [name for name in prerequisites if not gate.get(name)]
    if missing and gate.get("cann_enabled"):
        raise AssertionError(
            "CANN remote delivery stayed enabled without: " + ", ".join(missing)
        )
    if gate.get("cuda_support_blocked"):
        raise AssertionError("the CANN gate blocked qualified CUDA support")
    state = "enabled" if gate.get("cann_enabled") else "disabled"
    detail = f"CANN remote delivery {state}"
    if missing:
        detail += " (missing: " + ", ".join(missing) + ")"
    detail += "; qualified CUDA support unblocked"
    return detail


_CHECKS = (
    ("metadata-only-target", _check_metadata_only_target),
    ("no-hidden-checkpoint-download", _check_no_hidden_download),
    ("forward-equivalence", _check_forward_equivalence),
    ("require-refusal-honesty", _check_require_refusal_honesty),
    ("prefer-bounded-fallback", _check_prefer_bounded_fallback),
    ("stale-identity-refusal", _check_stale_identity_refusal),
    ("source-loss-serving-continuity", _check_source_loss_continuity),
    ("phase-timing-publication", _check_phase_timing_publication),
    ("connector-continuity", _check_connector_continuity),
    ("source-npu-grant-and-recovery-gate", _check_cann_gate),
)


__all__ = [
    "CANN_CAPABILITIES",
    "CAPABILITIES",
    "HARNESS_VARIABLE",
    "capabilities_for",
    "run",
]
