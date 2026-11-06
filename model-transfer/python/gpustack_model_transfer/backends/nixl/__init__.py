"""The NIXL transport backend: version-pinned, lazily loaded, fail-closed.

Importing this package imports no vendor runtime: nixl and torch are
reached through the runtime seam, inside methods, so the wheel installs
and runs anywhere and works across vendor builds (nixl-cu12, nixl-cu13).
The pinned route is explicit and validated; a runtime with another version
is a typed refusal, never a surprise.

The route refuses to guess a network: control and payload rails are named
explicitly and qualified, multi-rail support is advertised pair by pair
from passing probes, and port counters attribute traffic without ever
being read as a transfer-path proof. Every submitted operation owns its
terminal state, and an outcome that cannot be observed is UNKNOWN and
retained — with its window and registration — until a qualified fence
resolves it.
"""

from gpustack_model_transfer.backends.nixl.capabilities import (
    DIRECTION_READ,
    DIRECTION_WRITE,
    NixlCapabilities,
    QualifiedRailPair,
    RailQualifier,
    nixl_backend_capabilities,
)
from gpustack_model_transfer.backends.nixl.endpoints import (
    EndpointSelection,  # noqa: F401  (re-exported)
    MemlockCheck,
    check_memlock,
    qualify_endpoints,
    require_memlock,
)
from gpustack_model_transfer.backends.nixl.errors import (
    FenceRequiredError,
    MemlockInsufficientError,
    RegistrationExpiredError,
    RuntimeUnavailableError,
    TransferRefusedError,
    UnqualifiedEndpointError,
    UnsupportedSettingError,
    VersionMismatchError,
)
from gpustack_model_transfer.backends.nixl.memory import (
    CounterAttribution,
    MemoryLedger,
)
from gpustack_model_transfer.backends.nixl.reader import NixlReader, TransferWindow
from gpustack_model_transfer.backends.nixl.runtime import (
    DEFAULT_VERSION,
    PINNED_COMMIT,
    SUPPORTED_VERSIONS,
    AmbiguousPostError,
    NixlAPI,
    RealNixlAPI,
    RegistrationRetainedError,
    RuntimeReadiness,
    TransferOutcome,
    load_runtime,
    probe_runtime,
    release_retained,
    retained_registrations,
)
from gpustack_model_transfer.backends.nixl.safety import (
    NO_GO,
    PASS,
    UNRUN,
    SafetyReport,
    SafetyVerdict,
    audit_credential_disclosure,
    check_post_submission_quarantine,
    check_pre_submission_fault,
    probe_raw_access,
    probe_stale_access_after_revoke,
)

__all__ = [
    "DEFAULT_VERSION",
    "DIRECTION_READ",
    "DIRECTION_WRITE",
    "NO_GO",
    "PASS",
    "PINNED_COMMIT",
    "SUPPORTED_VERSIONS",
    "UNRUN",
    "AmbiguousPostError",
    "CounterAttribution",
    "FenceRequiredError",
    "MemlockCheck",
    "MemlockInsufficientError",
    "MemoryLedger",
    "NixlAPI",
    "NixlCapabilities",
    "NixlReader",
    "QualifiedRailPair",
    "RailQualifier",
    "RealNixlAPI",
    "RegistrationExpiredError",
    "RegistrationRetainedError",
    "RuntimeReadiness",
    "RuntimeUnavailableError",
    "SafetyReport",
    "SafetyVerdict",
    "TransferOutcome",
    "TransferRefusedError",
    "TransferWindow",
    "UnqualifiedEndpointError",
    "UnsupportedSettingError",
    "VersionMismatchError",
    "audit_credential_disclosure",
    "check_memlock",
    "check_post_submission_quarantine",
    "check_pre_submission_fault",
    "load_runtime",
    "nixl_backend_capabilities",
    "probe_raw_access",
    "probe_runtime",
    "probe_stale_access_after_revoke",
    "qualify_endpoints",
    "release_retained",
    "require_memlock",
    "retained_registrations",
]
