"""The NIXL runtime seam: lazy, with explicit version qualification.

Nothing in this module imports nixl or torch at module scope. A CPU-only
host probes the runtime (learning what is missing instead of crashing on
the import), and the wheel works across vendor builds: nixl-cu12 and
nixl-cu13 differ in their CUDA binding, not in the surface this backend
drives, so the runtime is reached through one seam — the NixlAPI protocol
below — implemented once over the real package (RealNixlAPI, every import
inside a method) and once for tests (the injected fake in testing.py).

The mapping is taken from the pinned release's own sources, not from a
tutorial: nixl 1.5.0 (tag v1.5.0, commit 1683cf3b7f3d11674c03c5e861cea22339876c96)
exposes `nixl._api.nixl_agent(name, nixl_agent_config(...))` (_api.py:189)
with `register_memory`/`deregister_memory` (_api.py:407,433),
`initialize_xfer` (_api.py:612), `transfer` (_api.py:651),
`check_xfer_state` (_api.py:673), `release_xfer_handle` (_api.py:724),
`get_agent_metadata` (_api.py:869), `add_remote_agent` (_api.py:899),
`remove_remote_agent` (_api.py:912) and `get_xfer_descs` (_api.py:1012).
There is no registration-renewal call and no per-agent network-device
knob in that surface, so this wrapper invents neither: a refused renewal
does not exist to call, and a UCX selection the agent configuration
cannot scope per agent is refused (the server environment stays
authoritative).

The source-review pin describes the reviewed API surface. Injected runtimes
and explicit supported_versions use strict version checks. The default
installed-runtime path accepts a reported version; this is compatibility,
not evidence that every installed version has passed hardware qualification.
"""

import importlib.util
import threading
from dataclasses import dataclass
from typing import Protocol

from gpustack_model_transfer.backends.nixl.errors import (
    FenceRequiredError,
    RuntimeUnavailableError,
    TransferRefusedError,
    UnsupportedSettingError,
    VersionMismatchError,
)

# The pinned route: the NIXL release the source review and the transfer
# evidence were taken against. The commit is the v1.5.0 tag of
# ai-dynamo/nixl; the repository HEAD examined alongside it (9e7800fc) is
# newer and was NOT assumed to match. This is the default allowlist for
# injected runtimes; real-host version enforcement is an explicit opt-in.
DEFAULT_VERSION = "1.5.0"
PINNED_COMMIT = "1683cf3b7f3d11674c03c5e861cea22339876c96"
SUPPORTED_VERSIONS = frozenset({DEFAULT_VERSION})

# The pieces the real-host path needs beyond the transport itself. They are
# probed, never imported eagerly.
_REQUIRED_MODULES = ("nixl",)
_DEVICE_MODULES = ("torch",)


@dataclass(frozen=True)
class RuntimeReadiness:
    """What the host has, and what is missing for the real transport."""

    nixl_available: bool
    version: str
    device_module_available: bool
    missing: tuple

    @property
    def ready(self) -> bool:
        return not self.missing


@dataclass(frozen=True)
class TransferOutcome:
    """One submitted transfer's observed state at a poll.

    state is the backend's own word: "succeeded" or "failed" are terminal,
    "pending" is not. enforcement says who refused a denied transfer —
    "backend" is the only answer that counts as enforcement; a "wrapper"
    refusal proves nothing about the transport boundary.
    """

    state: str
    detail: str = ""
    enforcement: str = ""


class RegistrationRetainedError(RuntimeError):
    """A registration the backend refused to release is now retained.

    retainment is the live quarantine owner: it holds the exact native
    registration handles and strong buffer references, so nothing is
    finalized until the source service disposes of it through a qualified
    fence. The exception is how the wrapper tells the service what it
    must hold.
    """

    def __init__(self, retainment, message: str):
        super().__init__(message)
        self.retainment = retainment


class AmbiguousPostError(RuntimeError):
    """A post call failed somewhere past request creation.

    Whether the transfer actually posted is not observable from here, so
    the operation and its full ownership graph are retained and this
    error is raised. It deliberately subclasses RuntimeError, not
    ModelTransferError: a reader must classify it as an unclassified
    failure — the operation UNKNOWN and its window retained — and never
    as a proven pre-submission refusal.
    """

    def __init__(self, operation, message: str):
        super().__init__(message)
        self.operation = operation


class _Retainment:
    """Strong references to leaked native state, the complete Python
    ownership graph.

    Holding the buffers is what keeps the pinned addresses real; holding
    the handles is what lets a qualified fence dispose of them later;
    holding the agent and registration is what keeps the whole backend
    graph alive even after every caller reference is gone. An exception
    message naming a leak would retain nothing — the objects would be
    finalized the moment the frames unwound.
    """

    def __init__(
        self,
        handles: dict,
        buffers: dict,
        detail: str,
        agent=None,
        registration=None,
    ):
        self.handles = dict(handles)
        self.buffers = dict(buffers)
        self.detail = detail
        self.agent = agent
        self.registration = registration


_RETAINED: list = []
_RETAINED_LOCK = threading.Lock()


def retain_registration(
    handles: dict,
    buffers: dict,
    detail: str,
    *,
    agent=None,
    registration=None,
) -> _Retainment:
    """Move a leaked registration into the process-wide quarantine owner.

    The retainment snapshots the STILL-OWNED handles and buffers and
    keeps the owning agent and registration alive with them.
    """
    retainment = _Retainment(
        handles, buffers, detail, agent=agent, registration=registration
    )
    with _RETAINED_LOCK:
        _RETAINED.append(retainment)
    return retainment


def retain_operation(operation, detail: str) -> _Retainment:
    """Move an operation into the process-wide quarantine owner.

    The operation record carries the native handle, the owning agent,
    its registration, and strong buffer references, so one retainment
    holds the complete Python ownership graph; the retainment also names
    the agent and registration directly. Repeated observations reuse the
    same operation's retainment until it is removed.
    """
    retainment = _Retainment(
        {"operation": operation},
        dict(operation.buffers),
        detail,
        agent=operation.agent,
        registration=operation.registration,
    )
    with _RETAINED_LOCK:
        for existing in _RETAINED:
            if existing.handles.get("operation") is operation:
                return existing
        _RETAINED.append(retainment)
    return retainment


def retained_registrations() -> tuple:
    """Every retained native state; diagnostics and fences read this,
    never shared status."""
    with _RETAINED_LOCK:
        return tuple(_RETAINED)


def _retainment_holds_undisposed_state(retainment) -> bool:
    """True while the retained graph still owns undisposed native state.

    An operation retainment is judged by its operation alone: the agent
    and registration it carries are graph context, not additional state
    this retainment owns. A registration retainment is judged by the
    live registration's handles map; a retainment with no live owner is
    judged by its snapshot, so out-of-band disposal needs evidence.
    """
    operation = retainment.handles.get("operation")
    if operation is not None:
        return not operation.released
    registration = retainment.registration
    if registration is not None:
        return bool(registration.handles)
    return bool(retainment.handles)


def release_retained(retainment, *, external_fence=None) -> bool:
    """Drop one retainment record from the quarantine owner.

    While the retained graph still owns native state, dropping the
    record needs external fence evidence — a non-empty documented record
    that a qualified fence outside this wrapper disposed of the state
    (a host-verified abort or completion record). The evidence is
    recorded on the caller, never verified here: this API cannot prove
    an external fence, so it refuses to be the one that pretends. A
    retainment whose state is genuinely disposed (registration handles
    emptied by a succeeded revoke, operation released by a later
    succeeded release) drops freely.
    """
    if _retainment_holds_undisposed_state(retainment) and not (
        isinstance(external_fence, str) and external_fence.strip()
    ):
        raise ValueError(
            "the retained state still owns native handles or an "
            "unreleased operation; dropping its quarantine record needs "
            "external fence evidence — a documented record that a "
            "qualified fence outside this wrapper disposed of the state"
        )
    with _RETAINED_LOCK:
        try:
            _RETAINED.remove(retainment)
        except ValueError:
            return False
        return True


class NixlAPI(Protocol):
    """The transport surface this backend drives.

    The real implementation wraps the nixl package; tests inject a fake
    with the same surface. Everything here is a backend boundary: the
    reader, the safety probes, and the accounting all observe behavior
    through it and nothing else.

    Transfers are address-based between this process's registered memory
    and a connected remote agent's published descriptors. The
    source-initiated direction (submit_source_write) belongs to the
    source's own process; the target-initiated direction
    (submit_target_read) is the pull a consuming process can always
    honor.
    """

    version: str

    def create_agent(self, name: str, settings: dict) -> object:
        """Create one agent.

        settings carry what this agent alone should run with. Keys the
        pinned agent configuration cannot scope per agent (UCX_*
        environment-style selections) are refused: the wrapper never
        writes a process-wide variable to pretend per-agent isolation,
        and the server environment remains authoritative. The returned
        handle is opaque to this backend.
        """

    def register_memory(self, agent, buffers: dict) -> object:
        """Register named owned buffers; returns a registration handle.

        The descriptor is the buffer's data address and true byte extent
        — never a temporary copy's address, never len() of a tensor. A
        buffer that cannot back a registered window (read-only, strided,
        empty) is refused.
        """

    def renew_registration(self, agent, registration) -> None:
        """Renew a registration the backend reports expired.

        A capability, not a promise: only a backend that can detect and
        perform renewal implements this. The pinned nixl 1.5.0 surface
        has no renewal call and reports no expiry, so RealNixlAPI
        implements nothing here and never raises RegistrationExpiredError.
        """

    def revoke_registration(self, agent, registration) -> None:
        """Revoke a registration: no further access through it is legal.

        A refused deregistration retains the exact native handle and the
        strong buffer references in the quarantine owner and raises
        RegistrationRetainedError; a succeeded one removes its handle
        exactly once, so a retry cannot double-free.
        """

    def submit_write(
        self, agent, registration, window_name: str, target: dict, length: int
    ) -> str:
        """The legacy consumer-side WRITE: a local registration plus a
        file path and offset, with no remote-agent state.

        This shape cannot mean a source-initiated push — the pinned API
        moves addresses between registered memory and a connected remote
        agent, and a WRITE executed on the consumer would push the
        consumer's own window bytes outward. A real backend refuses
        here; the source role's WRITE is submit_source_write and belongs
        to the source's process.
        """

    def submit_source_write(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
        local_offset: int = 0,
    ) -> object:
        """Submit the source-initiated WRITE of length bytes from the
        named local window starting at local_offset to the peer's published descriptor; returns an
        opaque operation handle. remote carries the peer's published
        region {'addr': int, 'len': int, 'mem_type': 'DRAM'|'VRAM',
        'device_id': int} taken from the peer's metadata; the vendor
        library moves addresses, never file names."""

    def submit_target_read(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
    ) -> object:
        """Submit the target-initiated READ: pull length bytes from the
        peer's published descriptor into the named local window; the
        direction a consuming process can always honor."""

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        """Observe one submitted operation's state, without deciding it.

        operation is the submit's own return value, opaque and never a
        stringified native handle, and it belongs to the given agent.
        Only an explicit terminal status returned by post or poll
        permits releasing the native handle. A status exception,
        including a typed binding error, does not prove that native
        work has stopped. It is an observation failure: the
        operation is UNKNOWN, its complete ownership graph retained,
        and the exception re-raised. A terminal handle whose release the
        backend refuses is retained the same way.
        """

    def release_operation(self, agent, operation, *, external_fence=None) -> None:
        """Release one operation's native handle.

        A PROVEN-terminal operation's release is bookkeeping of a
        finished request. For a pending or UNKNOWN operation, releasing
        would ask the backend to cancel active work — a cancel is not a
        qualified fence — so normal cleanup is refused with
        FenceRequiredError unless external_fence carries a non-empty,
        documented record of a qualified fence outside this wrapper. A
        refused native release retains the complete ownership graph and
        re-raises."""

    def agent_metadata(self, agent) -> bytes:
        """The agent's own metadata bytes for a peer to import.

        Source facts (pinned release), and their limits: the bytes
        serialize this agent's connection info AND its registered
        descriptors (src/core/nixl_agent.cpp getLocalMD), so they MAY
        expose addressing and credential material; they establish no
        read-only mode — the pinned UCX registration path sets no
        protection field and requests remote read AND write
        (source-level evidence, not measured hardware behavior). What a
        backend actually grants a holder of these bytes is hardware
        behavior that stays UNRUN here; do not claim that possession
        always grants both directions on every backend, and do not
        claim the exchange enforces anything. Who may hold these bytes
        is the caller's decision, not this API's.
        """

    def connect_peer(self, agent, metadata: bytes) -> object:
        """Import a peer's metadata; returns the peer's opaque
        connection token for transfers toward it."""

    def disconnect_peer(self, agent, connection) -> None:
        """Drop a peer connection. This invalidates the peer's published
        descriptors and must precede revoking registrations the peer's
        transfers addressed."""

    def port_counters(self, agent, rail: str) -> dict:
        """The selected rail's port counters, for attribution only."""

    def device_memory_bytes(self, agent) -> int:
        """Device-wide runtime allocations, outside any framework
        allocator; the accounting compares this, not live tensor bytes."""


def probe_runtime() -> RuntimeReadiness:
    """Probe the runtime without importing it: availability by spec, the
    version by an import only when the package is there."""
    missing = []
    nixl_available = _importable("nixl")
    device_module_available = any(_importable(name) for name in _DEVICE_MODULES)
    version = ""
    if not nixl_available:
        missing.append("nixl")
    else:
        try:
            version = _nixl_version()
        except Exception as exc:  # a present-but-broken runtime is missing
            missing.append(f"nixl import: {exc}")
    if not device_module_available:
        missing.append("device module (torch)")
    return RuntimeReadiness(
        nixl_available=nixl_available,
        version=version,
        device_module_available=device_module_available,
        missing=tuple(missing),
    )


def load_runtime(api=None, supported_versions=None) -> NixlAPI:
    """Return the lazy API, refusing an absent or unversioned runtime.

    Injected APIs use the source-review allowlist by default. The installed
    runtime enforces a version allowlist only when supported_versions is
    supplied; accepting a version does not qualify its hardware behavior.
    """
    supported = SUPPORTED_VERSIONS
    if supported_versions is not None:
        supported = frozenset(supported_versions)
    if api is not None:
        _check_version(api.version, supported)
        return api
    readiness = probe_runtime()
    if not readiness.nixl_available:
        raise RuntimeUnavailableError(
            "the nixl runtime is unavailable: " + ", ".join(readiness.missing)
        )
    if supported_versions is not None or not readiness.version:
        _check_version(readiness.version, supported)
    return RealNixlAPI()


def _check_version(version: str, supported: frozenset) -> None:
    if version not in supported:
        raise VersionMismatchError(
            f"nixl runtime version {version!r} is not one this backend "
            f"was pinned against (pinned: {', '.join(sorted(supported))}, "
            f"commit {PINNED_COMMIT[:12]})"
        )


def _importable(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _nixl_version() -> str:
    """The installed distribution's version.

    The nixl meta package does not reliably expose __version__, so the
    gate reads the distribution's metadata. Absent or broken metadata is
    a refusal, never a silent fallback: an unverifiable install must read
    as missing, not as matching the pin.
    """
    try:
        from importlib import metadata

        return str(metadata.version("nixl"))
    except metadata.PackageNotFoundError as exc:
        raise RuntimeError(
            "the nixl distribution's version metadata is missing; "
            "refusing an unverifiable install"
        ) from exc
    except Exception as exc:
        raise RuntimeError(
            f"the nixl distribution's version metadata is unreadable: {exc}"
        ) from exc


# The nixl_agent_config knobs the pinned release can scope to one agent,
# with the value contract each must satisfy. Anything else — UCX_*
# environment-style selections above all — is refused: the pinned
# configuration has no such knob, and the wrapper will not write a
# process-wide variable to pretend otherwise.
_AGENT_CONFIG_KEYS = {
    "enable_prog_thread": bool,
    "enable_listen_thread": bool,
    "listen_port": int,
    "capture_telemetry": bool,
    "num_threads": int,
    "backends": list,
    "sync_mode": str,
}


def _address_of(buffer) -> int:
    """Return the buffer's data address, never a temporary copy's.

    A device or host tensor answers through its own data_ptr. A writable
    host buffer answers through a ctypes view of its own storage: addressof
    of the buffer itself would raise, and a temporary bytes copy's address
    would be freed the moment this call returns — a dangling registration.
    A read-only buffer cannot be registered for transport at all and is
    refused here.
    """
    data_ptr = getattr(buffer, "data_ptr", None)
    if callable(data_ptr):
        return int(data_ptr())
    if isinstance(buffer, (bytearray, memoryview)) and not (
        isinstance(buffer, memoryview) and buffer.readonly
    ):
        from ctypes import addressof, c_char

        return addressof(c_char.from_buffer(buffer))
    # bytes lands here and is refused: a read-only snapshot has no
    # writable storage to register, and copying it to take an address
    # would register memory the transfer never touches.
    raise TypeError("registered window buffer must be writable or expose data_ptr()")


_KNOWN_DEVICE_TYPES = ("cpu", "cuda", "npu", "mlu", "rocm", "privateuseone")

# The pinned release's own segment names: DRAM is host memory, VRAM is
# accelerator memory (nixlBind.DRAM_SEG / VRAM_SEG behind these keys).
_HOST_MEMORY = "DRAM"
_DEVICE_MEMORY = "VRAM"
_MEMORY_CLASSES = (_HOST_MEMORY, _DEVICE_MEMORY)


def _buffer_extent(buffer) -> tuple:
    """Return (byte extent, "DRAM" | "VRAM", device id) for a registrable
    buffer.

    A tensor answers through its device contract: device.type names the
    memory class — "cpu" is DRAM, a known accelerator type is VRAM, and
    an unknown device type is refused rather than guessed. The extent is
    the tensor's true byte size (nbytes when the framework exposes it,
    numel()*element_size() otherwise) — len() is an element count for
    anything but byte views. Storage must report C-contiguity and the
    window must be non-empty: an address-based registration of a strided
    view would pin bytes the transfer never touches. A writable host
    buffer is DRAM sized by its true byte count — a memoryview's nbytes,
    not its item count. The device id is 0 for DRAM and the tensor's own
    device index for VRAM, as the pinned descriptor contract expects.
    """
    data_ptr = getattr(buffer, "data_ptr", None)
    if callable(data_ptr):
        device_type = str(getattr(getattr(buffer, "device", None), "type", ""))
        if device_type not in _KNOWN_DEVICE_TYPES:
            raise TypeError(
                f"unsupported tensor device {device_type!r}: the memory "
                "class must be a known device type, never a guess"
            )
        extent = getattr(buffer, "nbytes", None)
        if not isinstance(extent, int) or callable(extent):
            numel = getattr(buffer, "numel", None)
            element_size = getattr(buffer, "element_size", None)
            if not (callable(numel) and callable(element_size)):
                raise TypeError(
                    "a tensor window must expose nbytes or numel() and "
                    "element_size() so the registration pins the true "
                    "byte extent"
                )
            extent = int(numel()) * int(element_size())
        if extent <= 0:
            raise ValueError("an empty tensor cannot back a transfer window")
        is_contiguous = getattr(buffer, "is_contiguous", None)
        if not callable(is_contiguous):
            raise TypeError(
                "a tensor window must report its contiguity: a strided "
                "view's address would not pin its logical bytes"
            )
        if not is_contiguous():
            raise TypeError(
                "a registered tensor window must be C-contiguous: a "
                "strided view's address would not pin its logical bytes"
            )
        if device_type == "cpu":
            return extent, _HOST_MEMORY, 0
        device_id = int(buffer.get_device())
        if device_id < 0:
            raise ValueError(
                "a VRAM tensor must carry its own device index; the "
                "descriptor's device id is never guessed"
            )
        return extent, _DEVICE_MEMORY, device_id
    if isinstance(buffer, memoryview):
        if buffer.readonly:
            raise TypeError("a host memoryview window must be writable")
        if buffer.format not in ("B", "b"):
            raise TypeError(
                "a host memoryview window must be a byte view; another "
                "format's len() is an item count, not a byte extent"
            )
        return buffer.nbytes, _HOST_MEMORY, 0
    if isinstance(buffer, bytearray):
        return len(buffer), _HOST_MEMORY, 0
    raise TypeError("registered buffer must be writable or expose data_ptr()")


class _RealRegistration:
    """The per-window native registration handles, the descriptor records
    the transfer primitives build descriptors from, the strong buffer
    references that keep the pinned addresses real, and the owning
    agent."""

    def __init__(self, handles: dict, windows: dict, buffers: dict, agent):
        self.handles = handles
        self.windows = windows
        self.buffers = buffers
        self.agent = agent


def _rollback_registrations(agent, handles: dict, buffers: dict) -> None:
    """Undo completed registrations, newest first.

    Each succeeded deregistration is removed from handles and buffers
    BEFORE the next one is attempted, so a later refusal retains
    exactly what is still natively owned — never a stale, already
    released handle — and a retry of a fence disposal cannot
    double-deregister. A refused rollback cannot be undone: the exact
    remaining handles, buffers, and the owning agent move into the
    quarantine owner and the typed error carries them to the source
    service — naming a leak would retain nothing.
    """
    for name in reversed(list(handles.keys())):
        handle = handles[name]
        try:
            agent.deregister_memory(handle)
        except Exception as exc:
            retainment = retain_registration(
                handles,
                buffers,
                f"nixl registration rollback refused window {name!r}: {exc}",
                agent=agent,
            )
            raise RegistrationRetainedError(
                retainment,
                f"nixl registration rollback refused window {name!r}; "
                f"{len(handles)} registration(s) and their buffers are "
                "retained for a qualified fence",
            ) from None
        handles.pop(name)
        buffers.pop(name, None)


def _deregister_one(agent, registration, name: str) -> None:
    """Deregister one window, removing its handle exactly on success.

    A refused deregistration retains that handle with the registration,
    its buffer, and the owning agent, and raises; nothing is removed
    from the registration, so a retry cannot double-free.
    """
    handle = registration.handles[name]
    try:
        agent.deregister_memory(handle)
    except Exception as exc:
        retainment = retain_registration(
            {name: handle},
            {name: registration.buffers.get(name)},
            f"nixl deregister_memory refused window {name!r}: {exc}",
            agent=agent,
            registration=registration,
        )
        raise RegistrationRetainedError(
            retainment,
            f"nixl deregister_memory refused window {name!r}; the handle "
            "and its buffer are retained for a qualified fence",
        ) from None
    registration.handles.pop(name)
    registration.windows.pop(name, None)


class _RealOperation:
    """One submitted transfer and its complete ownership graph: the
    native handle, the owning agent, its registration, strong buffer
    references, the state the post call reported, whether a terminal
    verdict has been observed, and the release bookkeeping that keeps
    the release exact-once."""

    def __init__(self, handle, posted: str, agent, registration):
        self.handle = handle
        self.posted = posted
        self.agent = agent
        self.registration = registration
        self.buffers = dict(registration.buffers)
        self.terminal = False
        self.released = False


class RealNixlAPI:
    """The NixlAPI surface over the real nixl package.

    Every nixl import happens inside a method, so probing or merely
    importing this module on a host without the runtime costs nothing and
    refuses cleanly. The wrapper maps the NixlAPI protocol onto exactly
    the pinned surface — nixl 1.5.0, commit 1683cf3b — and refuses, with
    the pin named, when the installed binding does not carry it.

    Return semantics, verified in the pinned sources: the pybind layer
    converts every negative nixl_status_t into a raised typed exception
    (nixl_bindings.cpp throw_nixl_exception) and NIXL_IN_PROG=1 /
    NIXL_SUCCESS=0 are the only non-raising answers (nixl_types.h), so
    success at this boundary is the absence of an exception — the wrapper
    never tests a return value for truthiness. `transfer` and
    `check_xfer_state` return "DONE" or "PROC" (_api.py:651,673); a
    negative status raises a typed binding error. Such errors also
    cover missing peer metadata and incomplete backend observations,
    so their type cannot prove that native work has stopped. Every
    status exception retains the operation's complete ownership graph.
    Transfer handles are opaque objects; the wrapper never stringifies one.
    """

    # The pinned binding's own names, checked before first use: a
    # binding that moved or dropped one is not the reviewed baseline.
    _PINNED_MODULE_NAMES = ("nixl_agent", "nixl_agent_config", "nixl_thread_sync_t")
    _PINNED_AGENT_METHODS = (
        "register_memory",
        "deregister_memory",
        "initialize_xfer",
        "transfer",
        "check_xfer_state",
        "release_xfer_handle",
        "get_agent_metadata",
        "add_remote_agent",
        "remove_remote_agent",
        "get_xfer_descs",
    )

    # The nixl_agent_config knobs the pinned release can scope to one
    # agent, with the value contract each must satisfy. Anything else —
    # UCX_* environment-style selections above all — is refused: the
    # pinned configuration has no such knob, and the wrapper will not
    # write a process-wide variable to pretend otherwise.
    _SYNC_MODES = ("NONE", "STRICT", "RW", "DEFAULT")

    def __init__(self):
        self._nixl_api = None

    @property
    def version(self) -> str:
        return _nixl_version()

    def _require(self):
        if self._nixl_api is None:
            import nixl._api as nixl_api

            self._check_surface(nixl_api)
            self._nixl_api = nixl_api
        return self._nixl_api

    def _check_surface(self, nixl_api) -> None:
        bind = getattr(nixl_api, "nixlBind", None)
        missing = [
            name
            for name in self._PINNED_MODULE_NAMES
            if not hasattr(nixl_api, name) and not hasattr(bind, name)
        ]
        agent = getattr(nixl_api, "nixl_agent", None)
        if agent is not None:
            missing += [
                name for name in self._PINNED_AGENT_METHODS if not hasattr(agent, name)
            ]
        if missing:
            raise VersionMismatchError(
                "the installed nixl binding lacks the pinned surface "
                f"({', '.join(missing)}); pinned: {DEFAULT_VERSION} "
                f"commit {PINNED_COMMIT[:12]}"
            )

    def create_agent(self, name: str, settings: dict) -> object:
        nixl_api = self._require()
        config_kwargs = self._agent_config_kwargs(dict(settings or {}), nixl_api)
        config = nixl_api.nixl_agent_config(**config_kwargs)
        return nixl_api.nixl_agent(name, config)

    def _agent_config_kwargs(self, settings: dict, nixl_api) -> dict:
        """Map the caller's per-agent settings onto nixl_agent_config.

        The pinned configuration scopes exactly these knobs per agent;
        a UCX_TLS/UCX_NET_DEVICES-style key has no per-agent home in it,
        and the honest answer is a named refusal — the server environment
        stays authoritative for UCX selection, and this wrapper never
        mutates the process environment to fake per-agent scoping.
        """
        kwargs = {}
        for key, value in settings.items():
            expected = _AGENT_CONFIG_KEYS.get(key)
            if expected is None:
                raise UnsupportedSettingError(
                    f"the pinned nixl {DEFAULT_VERSION} agent configuration "
                    f"cannot scope {key!r} per agent; this backend never "
                    "writes a process-wide environment variable to pretend "
                    "per-agent isolation, and the server environment remains "
                    "authoritative"
                )
            if expected is bool:
                if not isinstance(value, bool):
                    raise UnsupportedSettingError(
                        f"agent setting {key!r} must be a bool, got {value!r}"
                    )
                kwargs[key] = value
                continue
            if expected is str:
                mode = str(value).upper()
                if mode not in self._SYNC_MODES:
                    raise UnsupportedSettingError(
                        f"agent setting {key!r} must be one of "
                        f"{self._SYNC_MODES}, got {value!r}"
                    )
                sync_enum = (
                    getattr(nixl_api, "nixl_thread_sync_t", None)
                    or nixl_api.nixlBind.nixl_thread_sync_t
                )
                kwargs[key] = getattr(sync_enum, f"NIXL_THREAD_SYNC_{mode}")
                continue
            if expected is list:
                if not isinstance(value, list) or not all(
                    isinstance(item, str) for item in value
                ):
                    raise UnsupportedSettingError(
                        f"agent setting {key!r} must be a list of backend "
                        f"names, got {value!r}"
                    )
                kwargs[key] = list(value)
                continue
            # int keys: bool is an int subclass and is not accepted.
            if isinstance(value, bool) or not isinstance(value, int):
                raise UnsupportedSettingError(
                    f"agent setting {key!r} must be an int, got {value!r}"
                )
            kwargs[key] = value
        return kwargs

    def register_memory(self, agent, buffers: dict) -> object:
        self._require()
        # Address-based descriptors, one native register_memory call per
        # named window; the pinned call returns that window's own
        # nixlRegDList, which deregister_memory consumes later. Success
        # is the absence of the binding's typed exception — the return
        # value is a registration handle, never a status code. The
        # buffer contract decides DRAM versus VRAM and the true byte
        # extent; the name mapping travels in the wrapper because the
        # native handles alone are not addressable by window name.
        handles = {}
        windows = {}
        owned = {}
        try:
            for name, buffer in buffers.items():
                addr = _address_of(buffer)
                extent, memory_class, device_id = _buffer_extent(buffer)
                windows[name] = {
                    "addr": addr,
                    "len": extent,
                    "mem_type": memory_class,
                    "dev_id": device_id,
                }
                owned[name] = buffer
                handles[name] = agent.register_memory(
                    [(addr, extent, device_id, "")], memory_class
                )
        except BaseException:
            # A later failure must not leak the earlier handles: roll the
            # completed registrations back. The rollback owns its own
            # bookkeeping copies, so the caller's dicts are never
            # mutated; a refused rollback cannot be undone, so the exact
            # still-owned handles, buffers, and the agent move into the
            # quarantine owner and the typed error carries them to the
            # source service — naming a leak retains nothing.
            _rollback_registrations(agent, handles, owned)
            raise
        return _RealRegistration(
            handles=handles, windows=windows, buffers=dict(buffers), agent=agent
        )

    def revoke_registration(self, agent, registration) -> None:
        """Deregister one window per held native handle.

        A handle leaves the registration exactly once, when its
        deregistration succeeds; a refused handle stays on the
        registration and its buffer moves into the quarantine owner, so
        a retry can never double-free and a refusal can never silently
        drop the native registration or finalize its buffer.
        """
        self._require()
        if registration.agent is not agent:
            raise ValueError(
                "the registration belongs to a different agent than the "
                "one the revoke addresses"
            )
        for name in list(registration.handles.keys()):
            _deregister_one(agent, registration, name)

    def submit_write(
        self, agent, registration, window_name: str, target: dict, length: int
    ) -> str:
        self._require()
        raise TransferRefusedError(
            "the pinned nixl "
            f"{DEFAULT_VERSION} binding moves addresses between this "
            "process's registered memory and a connected remote agent's "
            "published descriptors: a call carrying a local registration "
            "and a file path with offset, and no remote-agent state, "
            "cannot mean a source-initiated WRITE, and a WRITE executed "
            "on this process would push its own window bytes outward. "
            "The source role's WRITE is submit_source_write in the "
            "source's own process; a consuming process pulls with "
            "submit_target_read."
        )

    def submit_source_write(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
        local_offset: int = 0,
    ) -> object:
        return self._submit(
            "WRITE",
            agent,
            registration,
            connection,
            window_name,
            remote,
            length,
            local_offset=local_offset,
        )

    def submit_target_read(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
    ) -> object:
        return self._submit(
            "READ", agent, registration, connection, window_name, remote, length
        )

    def _submit(
        self,
        direction,
        agent,
        registration,
        connection,
        window_name,
        remote,
        length,
        local_offset=0,
    ) -> _RealOperation:
        """Build both descriptor lists and post one transfer request.

        The local descriptor is the named window's own registration
        record, bounded by the window's true byte extent; the remote
        descriptor is the peer's published region, and the peer's
        connection token comes from connect_peer. The pinned path
        creates the request with `initialize_xfer` and posts it with
        `transfer` (_api.py:612,651), and the two are failure-separated
        here: only a refusal of the CREATE is a proven pre-submission
        failure, because nothing could have posted. A POST exception is
        ambiguous — whether anything was submitted is not observable
        from this call — so the operation's complete ownership graph is
        retained and AmbiguousPostError is raised, which a reader must
        classify as an unclassified failure (operation UNKNOWN, window
        retained), never as a pre-submission refusal that frees the
        window.
        """
        self._require()
        if not isinstance(connection, (str, bytes)) or not connection:
            raise ValueError(
                "connection must be the token connect_peer returned for "
                "the peer the transfer addresses"
            )
        window = registration.windows.get(window_name)
        if window is None:
            raise ValueError(f"no registered window named {window_name!r}")
        if not isinstance(length, int) or isinstance(length, bool) or length <= 0:
            raise ValueError(f"transfer length must be a positive int, got {length!r}")
        remote_region = self._remote_region(remote)
        if (
            isinstance(local_offset, bool)
            or not isinstance(local_offset, int)
            or local_offset < 0
        ):
            raise ValueError("local offset must be a non-negative integer")
        if local_offset > window["len"] or length > window["len"] - local_offset:
            raise ValueError(
                f"the transfer of {length} bytes exceeds window "
                f"{window_name!r} ({window['len']} bytes)"
            )
        if length > remote_region["len"]:
            raise ValueError(
                f"the transfer of {length} bytes exceeds the peer's "
                f"published region ({remote_region['len']} bytes)"
            )
        local_descs = agent.get_xfer_descs(
            [(window["addr"] + local_offset, length, window["dev_id"])],
            window["mem_type"],
        )
        remote_descs = agent.get_xfer_descs(
            [
                (
                    remote_region["addr"],
                    length,
                    remote_region["dev_id"],
                )
            ],
            remote_region["mem_type"],
        )
        try:
            handle = agent.initialize_xfer(
                direction, local_descs, remote_descs, connection, notif_msg=b""
            )
        except Exception as exc:
            # The request was never created, so nothing could have
            # posted: a proven pre-submission refusal, with no handle
            # and nothing to retain.
            raise TransferRefusedError(
                f"nixl refused to create the {direction} request "
                f"(nothing was posted): {type(exc).__name__}: {exc}"
            ) from exc
        operation = _RealOperation(handle, "", agent, registration)
        try:
            operation.posted = agent.transfer(handle)
        except Exception as exc:
            # Past request creation the post call failed somewhere, and
            # a transfer exception may follow actual submission: the
            # outcome is not observable here, so no release is attempted
            # (a cancel must never be disguised as pre-submission
            # cleanup) and the whole graph is retained for a fence.
            retain_operation(operation, f"ambiguous post of the {direction}: {exc}")
            raise AmbiguousPostError(
                operation,
                f"the nixl post of the {direction} failed past request "
                f"creation; whether anything was submitted is unknown, "
                f"the operation and its registration, buffers, and agent "
                f"are retained: {type(exc).__name__}: {exc}",
            ) from exc
        if operation.posted in ("DONE", "ERR"):
            # The post call returned its own verdict.
            operation.terminal = True
        return operation

    @staticmethod
    def _remote_region(remote: dict) -> dict:
        """Validate the peer's published descriptor.

        The fields come from the peer's own metadata — its registered
        descriptors carry the address, length, and memory class — and
        none of them is guessed here.
        """
        if not isinstance(remote, dict):
            raise ValueError(
                "the remote descriptor is the peer's published region: "
                "{'addr': int, 'len': int, 'mem_type': 'DRAM'|'VRAM', "
                "'device_id': int}"
            )
        try:
            addr = int(remote["addr"])
            region_len = int(remote["len"])
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError(
                "the remote descriptor carries the peer's published "
                "'addr' and 'len': " + repr(remote)
            ) from exc
        mem_type = remote.get("mem_type")
        if mem_type not in _MEMORY_CLASSES:
            raise ValueError(
                f"the remote descriptor's memory class must be one of "
                f"{_MEMORY_CLASSES}, from the peer's published "
                f"descriptor, got {mem_type!r}"
            )
        device_id = remote.get("device_id", 0)
        if (
            isinstance(device_id, bool)
            or not isinstance(device_id, int)
            or device_id < 0
        ):
            raise ValueError(
                f"the remote descriptor's device_id must be a "
                f"non-negative int, got {device_id!r}"
            )
        if addr <= 0 or region_len <= 0:
            raise ValueError(
                f"the peer's published region must carry a positive "
                f"address and length, got {(addr, region_len)}"
            )
        return {
            "addr": addr,
            "len": region_len,
            "mem_type": mem_type,
            "dev_id": device_id,
        }

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        """Observe one submitted transfer without deciding it.

        "DONE" and "ERR" are explicit terminal replies. "PROC" is
        observed through check_xfer_state; other replies retain the
        handle. Status exceptions do not prove completion: NIXL can
        report NOT_FOUND while the native request remains IN_PROG.
        Retain the complete ownership graph and re-raise every status
        exception so the caller keeps the window UNKNOWN.

        A refused terminal-handle release also retains the ownership
        graph. A successful poll still establishes final window bytes,
        but the retained handle needs a later successful release.
        """
        self._require()
        operation = operation_id
        if not isinstance(operation, _RealOperation):
            raise ValueError(
                "poll observes an operation this runtime's submit "
                "returned, not a caller-chosen id"
            )
        if operation.released:
            raise ValueError(
                "this operation's handle was already released; there is "
                "no state left to observe"
            )
        if operation.agent is not agent or operation.registration.agent is not agent:
            raise ValueError(
                "the operation belongs to a different agent than the "
                "one the poll addresses"
            )
        if operation.posted == "DONE":
            # The post call reported completion within itself; the only
            # observation left is releasing the handle exactly once.
            operation.terminal = True
            retained = self._release_operation(agent, operation)
            return TransferOutcome(
                state="succeeded",
                detail="DONE at post"
                + ("; handle release refused and retained" if retained else ""),
            )
        if operation.posted == "ERR":
            # The post call returned its own negative verdict: terminal
            # failed, the handle released exactly once, a refused
            # release retained.
            operation.terminal = True
            retained = self._release_operation(agent, operation)
            if not retained:
                return TransferOutcome(state="failed", detail="ERR at post")
            raise RuntimeError(
                "nixl reported ERR at post; releasing the handle failed "
                "and the operation is retained"
            )
        if operation.posted != "PROC":
            # An unclassifiable word from the post call holds: nothing
            # about the request is decided, and the handle is untouched.
            return TransferOutcome(
                state="pending", detail=f"unclassified post state {operation.posted!r}"
            )
        try:
            state = agent.check_xfer_state(operation.handle)
        except BaseException as exc:
            # A status error can leave native work active. Releasing
            # here would request cancellation without a drain proof.
            retain_operation(operation, f"unclassified observation failure: {exc}")
            raise
        if state == "DONE":
            operation.terminal = True
            retained = self._release_operation(agent, operation)
            return TransferOutcome(
                state="succeeded",
                detail="DONE"
                + ("; handle release refused and retained" if retained else ""),
            )
        if state == "ERR":
            operation.terminal = True
            retained = self._release_operation(agent, operation)
            if not retained:
                return TransferOutcome(state="failed", detail="ERR")
            raise RuntimeError(
                "nixl reported ERR; releasing the handle failed and the "
                "operation is retained"
            )
        # "PROC" or any other nonterminal word: nothing is decided, the
        # operation and its window stay held.
        return TransferOutcome(state="pending", detail=str(state))

    def release_operation(self, agent, operation, *, external_fence=None) -> None:
        """Release one operation's native handle.

        A PROVEN-terminal operation's release is bookkeeping of a
        finished request (the backend already reported completion or
        failure). For a pending or UNKNOWN operation, releasing would
        ask the backend to cancel active work, and a cancel is NOT a
        qualified fence — normal cleanup is refused with
        FenceRequiredError unless external_fence carries a non-empty,
        documented record of a qualified fence outside this wrapper
        (this API offers no callback that could prove itself). A
        refused native release retains the complete ownership graph —
        operation, agent, registration, buffers — and re-raises.
        """
        self._require()
        if not isinstance(operation, _RealOperation):
            raise ValueError(
                "release_operation takes an operation this runtime's submit returned"
            )
        if operation.released:
            raise ValueError("this operation's handle was already released")
        if operation.agent is not agent or operation.registration.agent is not agent:
            raise ValueError(
                "the operation belongs to a different agent than the "
                "one the release addresses"
            )
        if not operation.terminal and not (
            isinstance(external_fence, str) and external_fence.strip()
        ):
            raise FenceRequiredError(
                "the operation is not proven terminal (posted "
                f"{operation.posted!r}); releasing it would request a "
                "cancel, and a cancel is not a qualified fence — "
                "cleanup needs external fence evidence, a documented "
                "record of a qualified fence outside this wrapper"
            )
        retained = self._release_operation(agent, operation)
        if retained:
            raise RuntimeError(
                "nixl refused to release the operation's handle; the "
                "operation, its agent, registration, and buffers are "
                "retained for a qualified fence"
            )

    def _release_operation(self, agent, operation) -> bool:
        """Release the handle exactly once; True when the release was
        refused and the operation was retained with its whole graph."""
        if operation.released:
            return False
        try:
            agent.release_xfer_handle(operation.handle)
        except Exception as exc:
            operation.released = False
            retain_operation(
                operation,
                f"nixl release_xfer_handle refused: {exc}",
            )
            return True
        operation.released = True
        return False

    def agent_metadata(self, agent) -> bytes:
        self._require()
        return agent.get_agent_metadata()

    def connect_peer(self, agent, metadata: bytes):
        self._require()
        if not isinstance(metadata, bytes):
            raise ValueError(
                "peer metadata is the bytes agent_metadata produced, "
                "carried out of band"
            )
        # The pinned loadRemoteMD binding answers with the remote
        # agent's name as bytes; the token goes back into the transfer
        # calls untouched and is never decoded or stringified here.
        return agent.add_remote_agent(metadata)

    def disconnect_peer(self, agent, connection) -> None:
        self._require()
        agent.remove_remote_agent(connection)

    def port_counters(self, agent, rail: str) -> dict:
        # Counter reading is host tooling (perfquery et al.), not the nixl
        # API; the host suite injects the reader. The seam stays so the
        # attribution record has one shape everywhere.
        raise NotImplementedError("port counters are read by the host suite")

    def device_memory_bytes(self, agent) -> int:
        # Measured outside the framework allocator on the host (torch's
        # device-wide memory stats); injected there for the same reason.
        raise NotImplementedError("device memory is measured by the host suite")


__all__ = [
    "DEFAULT_VERSION",
    "PINNED_COMMIT",
    "SUPPORTED_VERSIONS",
    "AmbiguousPostError",
    "NixlAPI",
    "RealNixlAPI",
    "RegistrationRetainedError",
    "RuntimeReadiness",
    "TransferOutcome",
    "load_runtime",
    "probe_runtime",
    "release_retained",
    "retain_operation",
    "retain_registration",
    "retained_registrations",
]
