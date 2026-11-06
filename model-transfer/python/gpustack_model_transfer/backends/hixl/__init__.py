"""The HIXL transport backend for CANN: pinned, ordered, fail-closed.

Importing this package imports no vendor stack: hixl, torch, and
torch_npu are reached through the runtime seam, inside methods, so the
wheel is pure Python and installs anywhere. The pinned baseline is the
reviewed HIXL release and commit; another version is a typed refusal.

The route enforces the documented session order (register before
connect, disconnect before deregister) with the relaxed fabric mode an
explicit, evidence-carrying opt-in; keeps requested, applied, and
effective transport answers apart so a rejected option is reported and a
functional-only run never claims the RDMA path; registers target memory
in its consuming process; and requires a source NPU context until an
accelerator-free source is qualified. Every operation owns its terminal
state, and an unobservable outcome is UNKNOWN and retained until a
qualified fence resolves it.
"""

from gpustack_model_transfer.backends.hixl.errors import (
    RuntimeUnavailableError,
    SessionOrderError,
    SourceNpuContextRequiredError,
    TransferRefusedError,
    TransportOptionRejectedError,
    VersionMismatchError,
)
from gpustack_model_transfer.backends.hixl.reader import (
    DEFAULT_POLL_DEADLINE_SECONDS,
    DIRECTION_READ,
    DIRECTION_WRITE,
    HixlReader,
    InFlightOperation,
    TransferWindow,
)
from gpustack_model_transfer.backends.hixl.runtime import (
    PINNED_COMMIT,
    PINNED_VERSION,
    SUPPORTED_VERSIONS,
    HixlAPI,
    RealHixlAPI,
    RuntimeReadiness,
    TransferOutcome,
    load_runtime,
    probe_runtime,
)
from gpustack_model_transfer.backends.hixl.safety import (
    NO_GO,
    PASS,
    UNRUN,
    SafetyReport,
    SafetyVerdict,
    audit_credential_disclosure,
    check_pre_submission_timeout,
    check_recovery_standing,
    check_submitted_nonterminal_quarantine,
    probe_raw_access,
    probe_stale_replay_after_repair,
)
from gpustack_model_transfer.backends.hixl.session import (
    FabricMemRelaxation,
    HixlCapabilities,
    HixlSession,
    RecoveryCapability,
    RejectedTransportOption,
    TransportNegotiation,
    negotiate_transport,
)

__all__ = [
    "DEFAULT_POLL_DEADLINE_SECONDS",
    "DIRECTION_READ",
    "DIRECTION_WRITE",
    "NO_GO",
    "PASS",
    "PINNED_COMMIT",
    "PINNED_VERSION",
    "SUPPORTED_VERSIONS",
    "UNRUN",
    "FabricMemRelaxation",
    "HixlAPI",
    "HixlCapabilities",
    "HixlReader",
    "HixlSession",
    "InFlightOperation",
    "RealHixlAPI",
    "RecoveryCapability",
    "RejectedTransportOption",
    "RuntimeReadiness",
    "RuntimeUnavailableError",
    "SafetyReport",
    "SafetyVerdict",
    "SessionOrderError",
    "SourceNpuContextRequiredError",
    "TransferOutcome",
    "TransferRefusedError",
    "TransferWindow",
    "TransportNegotiation",
    "TransportOptionRejectedError",
    "VersionMismatchError",
    "audit_credential_disclosure",
    "check_pre_submission_timeout",
    "check_recovery_standing",
    "check_submitted_nonterminal_quarantine",
    "load_runtime",
    "negotiate_transport",
    "probe_raw_access",
    "probe_runtime",
    "probe_stale_replay_after_repair",
]
