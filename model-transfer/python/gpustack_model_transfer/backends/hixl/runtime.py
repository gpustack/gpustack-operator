"""The HIXL runtime seam: lazy, version-pinned, fail-closed.

Nothing in this module imports hixl, torch, or torch_npu at module scope,
so the wheel stays pure Python (py3-none-any) and installs anywhere: a
CPU-only host probes the stack — learning what is missing instead of
crashing — and an Ascend host reaches the vendor API through one seam,
the HixlAPI protocol below, implemented once over the reviewed package
(RealHixlAPI, every import inside a method) and once for tests (the
injected fake in testing.py). No C extension is built at wheel time; any
native bridge a later release needs stays a version-pinned separate
decision, never a compilation step smuggled into this package.
"""

import importlib.util
import threading
from dataclasses import dataclass
from importlib import metadata
from typing import Protocol

from gpustack_model_transfer.backends.hixl.errors import (
    RuntimeUnavailableError,
    TransferRefusedError,
    VersionMismatchError,
)
from gpustack_model_transfer.backends.hixl.memory import AlignedHostBuffer

# The pinned baseline. The gate identifies the wheel by its installed
# distribution version, exactly as the NIXL gate does: the reviewed wheel
# built from commit cd4c5e86 is hixl 0.0.1. The CANN 9.2.0 release the
# review ran against lives in the commit and the build provenance, not in
# a module attribute (the pinned binding exposes no __version__). A
# caller that has qualified another release passes it in; nothing
# accepts an unqualified one silently.
PINNED_VERSION = "0.0.1"
PINNED_COMMIT = "cd4c5e86f5ca5a654226b0a677eb07915540e03c"
SUPPORTED_VERSIONS = frozenset({PINNED_VERSION})

# The pieces the real-host path needs. torch must be imported before
# torch_npu (the npu backend registers onto the running torch), and hixl
# after both. All are probed, never imported eagerly.
_REQUIRED_MODULES = ("hixl",)
_DEVICE_MODULES = ("torch", "torch_npu")


@dataclass(frozen=True)
class RuntimeReadiness:
    """What the host has, and what is missing for the real transport."""

    hixl_available: bool
    version: str
    torch_available: bool
    torch_npu_available: bool
    npu_available: bool
    missing: tuple

    @property
    def ready(self) -> bool:
        return not self.missing


@dataclass(frozen=True)
class TransferOutcome:
    """One submitted transfer's observed state at a poll.

    state is the backend's own word: "succeeded" and "failed" are
    terminal, "pending" is not. enforcement says who refused a denied
    transfer — only "backend" counts as enforcement.
    """

    state: str
    detail: str = ""
    enforcement: str = ""


class RegistrationRetainedError(RuntimeError):
    """A registration the backend refused to release is now retained.

    retainment is the live quarantine owner: it holds the exact native
    handles and strong buffer references, so nothing is finalized until
    the source service disposes it through a qualified fence. The
    exception is how the wrapper tells the service what it must hold.
    """

    def __init__(self, retainment, message: str):
        super().__init__(message)
        self.retainment = retainment


class _Retainment:
    """Strong references to a leaked registration's handles and buffers.

    Holding the buffers is what keeps the pinned addresses real; holding
    the handles is what lets a qualified fence deregister them later. An
    exception message naming a leak would retain nothing — the objects
    would be finalized the moment the frames unwound.
    """

    def __init__(self, handles: dict, buffers: dict, detail: str):
        self.handles = dict(handles)
        self.buffers = dict(buffers)
        self.detail = detail
        self.owners = ()


_RETAINED: list = []
_RETAINED_LOCK = threading.Lock()


def retain_registration(handles: dict, buffers: dict, detail: str) -> _Retainment:
    """Move a leaked registration into the process-wide quarantine owner."""
    retainment = _Retainment(handles, buffers, detail)
    with _RETAINED_LOCK:
        _RETAINED.append(retainment)
    return retainment


def retained_registrations() -> tuple:
    """Every registration currently retained; diagnostics and fences read
    this, never shared status."""
    with _RETAINED_LOCK:
        return tuple(_RETAINED)


def release_retained(retainment) -> bool:
    """Drop one retainment once a qualified fence has disposed of it."""
    with _RETAINED_LOCK:
        try:
            _RETAINED.remove(retainment)
        except ValueError:
            return False
        return True


class HixlAPI(Protocol):
    """The transport surface this backend drives.

    The real implementation wraps the reviewed hixl package; tests inject
    a fake with the same surface. Everything here is a backend boundary.
    """

    version: str

    def create_agent(self, name: str, settings: dict) -> object:
        """Create one agent. settings carry the agent's own transport
        options; the implementation applies them to this agent alone."""

    def register_memory(self, agent, buffers: dict) -> object:
        """Register named owned buffers in the CONSUMING process, per the
        documented ordering; returns a registration handle."""

    def connect(self, agent, peer: dict) -> object:
        """Connect to a peer. Registration must precede this."""

    def disconnect(self, agent, connection) -> None:
        """Disconnect a connection. This must precede deregistration."""

    def deregister(self, agent, registration) -> None:
        """Deregister memory. Disconnection must precede this."""

    def submit_write(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        source: dict,
        length: int,
    ) -> str:
        """Submit the source-initiated direction for length bytes between
        the peer's published remote address and the named window; returns
        the operation id. source carries {'addr': int}: the vendor library
        moves addresses, never file names. A binding that executes a WRITE
        on the submitting engine — pushing that process's own window bytes
        outward — must refuse here rather than overwrite the peer, because
        the source-initiated shape belongs to the source's own process."""

    def submit_read(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        source: dict,
        length: int,
    ) -> str:
        """Submit the target-initiated direction: pull length bytes from
        the peer's published remote address into the named window; the
        direction a consuming process can always honor, present so the
        other direction's isolation can be probed, not presumed."""

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        """Observe one submitted operation's state, without deciding it."""

    def device_memory_bytes(self, agent) -> int:
        """Device-wide NPU runtime allocations outside any framework
        allocator; the accounting compares this, not live tensor bytes."""

    def describe_connection(self, connection) -> list:
        """The records connection setup and rendezvous would emit."""


def probe_runtime() -> RuntimeReadiness:
    """Probe the stack without importing it: availability by spec, the
    version by an import only when the package is there."""
    missing = []
    hixl_available = _importable("hixl")
    torch_available = _importable("torch")
    torch_npu_available = _importable("torch_npu")
    npu_available = False
    version = ""
    if not hixl_available:
        missing.append("hixl")
    else:
        try:
            version = _hixl_version()
        except Exception as exc:  # a present-but-unverifiable runtime is missing
            missing.append(f"hixl version: {exc}")
    if not torch_available:
        missing.append("torch")
    if not torch_npu_available:
        missing.append("torch_npu")
    if torch_available and torch_npu_available:
        try:
            import torch
            import torch_npu  # noqa: F401

            npu_available = bool(torch.npu.is_available())
        except Exception as exc:
            missing.append(f"npu stack: {exc}")
        else:
            if not npu_available:
                missing.append("npu")
    return RuntimeReadiness(
        hixl_available=hixl_available,
        version=version,
        torch_available=torch_available,
        torch_npu_available=torch_npu_available,
        npu_available=npu_available,
        missing=tuple(missing),
    )


def load_runtime(api=None, supported_versions=None) -> HixlAPI:
    """Return the HIXL API, failing closed when the stack is absent or its
    version is not the pinned one.

    api, when given, is an already-constructed implementation (the
    injected fake in tests); its version is validated the same way.
    """
    supported = SUPPORTED_VERSIONS
    if supported_versions is not None:
        supported = frozenset(supported_versions)
    if api is not None:
        _check_version(api.version, supported)
        return api
    readiness = probe_runtime()
    if not readiness.hixl_available:
        raise RuntimeUnavailableError(
            "the hixl stack is unavailable: " + ", ".join(readiness.missing)
        )
    _check_version(readiness.version, supported)
    return RealHixlAPI()


def _check_version(version: str, supported: frozenset) -> None:
    if version not in supported:
        raise VersionMismatchError(
            f"hixl runtime version {version!r} is not the pinned baseline "
            f"(pinned: {', '.join(sorted(supported))}, commit "
            f"{PINNED_COMMIT[:12]})"
        )


def _importable(name: str) -> bool:
    try:
        return importlib.util.find_spec(name) is not None
    except (ImportError, ValueError):
        return False


def _hixl_version() -> str:
    """The installed wheel's distribution version.

    The pinned binding exposes no module __version__, so the identity is
    the installed distribution's metadata. Absent or broken metadata is a
    refusal, never a silent fallback: an unverifiable wheel must read as
    missing, not as matching the pin.
    """
    try:
        return str(metadata.version("hixl"))
    except metadata.PackageNotFoundError as exc:
        raise RuntimeError(
            "the hixl distribution's version metadata is missing; "
            "refusing an unverifiable wheel"
        ) from exc
    except Exception as exc:
        raise RuntimeError(
            f"the hixl distribution's version metadata is unreadable: {exc}"
        ) from exc


def _address_of(buffer) -> int:
    """Return the buffer's data address, never a temporary copy's.

    A device tensor answers through its own data_ptr (torch and torch_npu
    tensors both expose it). A writable host buffer answers through a
    ctypes view of its own storage: building a temporary bytes copy and
    taking its address would register memory the transfer never touches.
    A read-only buffer cannot back a registered window and is refused.
    """
    data_ptr = getattr(buffer, "data_ptr", None)
    if callable(data_ptr):
        return int(data_ptr())
    if isinstance(buffer, (bytearray, memoryview)) and not (
        isinstance(buffer, memoryview) and buffer.readonly
    ):
        from ctypes import addressof, c_char

        return addressof(c_char.from_buffer(buffer))
    # bytes lands here and is refused: a read-only source snapshot has no
    # writable storage to register, and a temporary copy's address would
    # be freed the moment this call returns — a dangling registration.
    raise TypeError("registered buffer must be writable or expose data_ptr()")


_KNOWN_DEVICE_TYPES = ("cpu", "cuda", "npu", "mlu", "rocm", "privateuseone")


def _buffer_extent(buffer) -> tuple:
    """Return (byte extent, "HOST" | "DEVICE") for a registrable buffer.

    A tensor answers through its device contract: device.type names the
    memory class — "cpu" is host, a known accelerator type is device, and
    an unknown device type is refused rather than guessed. The extent is
    the tensor's true byte size (nbytes when the framework exposes it,
    numel()*element_size() otherwise) — len() is an element count for
    anything but byte views. Storage must report C-contiguity and the
    window must be non-empty: an address-based registration of a strided
    view would pin bytes the transfer never touches. A writable host
    buffer is host memory sized by its true byte count — a memoryview's
    nbytes, not its item count.
    """
    if isinstance(buffer, AlignedHostBuffer):
        return buffer.size_bytes, "HOST"
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
        return extent, ("HOST" if device_type == "cpu" else "DEVICE")
    if isinstance(buffer, memoryview):
        if buffer.readonly:
            raise TypeError("a host memoryview window must be writable")
        if buffer.format not in ("B", "b"):
            raise TypeError(
                "a host memoryview window must be a byte view; another "
                "format's len() is an item count, not a byte extent"
            )
        return buffer.nbytes, "HOST"
    if isinstance(buffer, bytearray):
        return len(buffer), "HOST"
    raise TypeError("registered buffer must be writable or expose data_ptr()")


class _RealAgent:
    """The native client plus the wrapper's own metadata.

    The pinned binding's Hixl object is a plain pybind class without
    dynamic attributes, so the agent's name and settings live on this
    Python wrapper next to the native client, never on the vendor object.
    """

    def __init__(self, client, name: str, settings: dict):
        self.client = client
        self.name = name
        self.settings = settings


class _RealRegistration:
    """The per-window vendor handles, the strong buffer references that
    keep the pinned addresses real, and the name-to-window mapping."""

    def __init__(self, handles: dict, windows: dict, buffers: dict):
        self.handles = handles
        self.windows = windows
        self.buffers = buffers


class RealHixlAPI:
    """The HixlAPI surface over the reviewed hixl package, address-based.

    Every vendor import happens inside a method, so probing or importing
    this module on a host without the stack costs nothing and refuses
    cleanly. The pinned binding (hixl 0.0.1, commit cd4c5e86) exposes one
    client constructed as Hixl() and driven through
    initialize(local_engine, options), register_mem(MemDesc, MemType) ->
    (status, handle) per buffer, status-returning connect/disconnect,
    transfer_async(remote_engine, op, [TransferOpDesc]) -> (status,
    request), and get_transfer_status(request) -> (status, state): this
    wrapper maps the HixlAPI protocol onto exactly that surface and
    refuses, with the pin named, when the installed binding does not
    carry it. Descriptors the reader submits are remote ADDRESSES the
    peer published — never file names or window labels, which the vendor
    library does not understand.
    """

    # The pinned binding's own names, checked before first use: a
    # binding that moved or dropped one is not the reviewed baseline.
    _PINNED_METHODS = (
        "initialize",
        "finalize",
        "register_mem",
        "deregister_mem",
        "connect",
        "disconnect",
        "transfer_async",
        "get_transfer_status",
    )
    _PINNED_NAMES = ("MemDesc", "MemType", "TransferOp", "TransferStatus", "SUCCESS")

    def __init__(self):
        self._hixl = None

    @property
    def version(self) -> str:
        return _hixl_version()

    def _require(self):
        if self._hixl is None:
            import hixl

            self._check_surface(hixl)
            self._hixl = hixl
        return self._hixl

    def _check_surface(self, hixl) -> None:
        client = getattr(hixl, "Hixl", None)
        missing = [name for name in self._PINNED_METHODS if not hasattr(client, name)]
        missing += [name for name in self._PINNED_NAMES if not hasattr(hixl, name)]
        if client is None:
            missing = ["Hixl"]
        if missing:
            raise VersionMismatchError(
                "the installed hixl binding lacks the pinned surface "
                f"({', '.join(missing)}); pinned: {PINNED_VERSION} "
                f"commit {PINNED_COMMIT[:12]}"
            )

    def create_agent(self, name: str, settings: dict) -> object:
        hixl = self._require()
        settings = dict(settings or {})
        # The pinned binding requires an explicit local engine identity at
        # initialize; a caller that cannot name its own engine has not
        # been wired for this deployment, and failing closed here is the
        # honest answer.
        local_engine = settings.get("local_engine")
        if not isinstance(local_engine, str) or not local_engine:
            raise ValueError(
                "agent settings must carry a non-empty 'local_engine': "
                "the pinned binding requires initialize(local_engine, options)"
            )
        options = settings.get("initialize_options") or {}
        if not isinstance(options, dict) or not all(
            isinstance(key, str) and isinstance(value, str)
            for key, value in options.items()
        ):
            raise ValueError(
                "agent settings 'initialize_options' must be a str->str map"
            )
        client = hixl.Hixl()
        status = client.initialize(local_engine, options)
        if status != hixl.SUCCESS:
            raise RuntimeError(
                f"hixl initialize refused agent {name!r} (status {status})"
            )
        return _RealAgent(client=client, name=name, settings=settings)

    def register_memory(self, agent, buffers: dict) -> object:
        hixl = self._require()
        # Address-based descriptors, one register_mem call per named
        # window; the pinned call returns that buffer's own handle. The
        # buffer contract decides host versus device memory and the true
        # byte extent; the name mapping travels in the wrapper because
        # the vendor handles alone are not addressable by window name.
        handles = {}
        windows = {}
        try:
            for name, buffer in buffers.items():
                addr = _address_of(buffer)
                extent, memory_class = _buffer_extent(buffer)
                registered_extent = (
                    buffer.registration_bytes
                    if isinstance(buffer, AlignedHostBuffer)
                    else extent
                )
                mem_type = (
                    hixl.MemType.MEM_DEVICE
                    if memory_class == "DEVICE"
                    else hixl.MemType.MEM_HOST
                )
                windows[name] = {
                    "addr": addr,
                    "len": extent,
                    "type": memory_class,
                }
                status, handle = agent.client.register_mem(
                    hixl.MemDesc(addr, registered_extent), mem_type
                )
                if status != hixl.SUCCESS:
                    raise RuntimeError(
                        f"hixl register_mem refused buffer {name!r} (status {status})"
                    )
                handles[name] = handle
        except BaseException:
            # A later failure must not leak the earlier handles: roll the
            # completed registrations back. A refused rollback cannot be
            # undone, so the exact handles and strong buffer references
            # move into the quarantine owner and the typed error carries
            # it to the source service — naming a leak retains nothing.
            for name, handle in reversed(list(handles.items())):
                try:
                    rollback = agent.client.deregister_mem(handle)
                except BaseException:
                    retainment = retain_registration(
                        handles, buffers, "hixl registration rollback raised"
                    )
                    raise RegistrationRetainedError(
                        retainment, "hixl rollback failed; registrations retained"
                    ) from None
                if rollback != hixl.SUCCESS:
                    retainment = retain_registration(
                        handles,
                        buffers,
                        f"hixl rollback refused buffer {name!r} (status {rollback})",
                    )
                    raise RegistrationRetainedError(
                        retainment,
                        f"hixl rollback of buffer {name!r} refused "
                        f"(status {rollback}); {len(handles)} registration(s) "
                        "and their buffers are retained for a qualified fence",
                    ) from None
                handles.pop(name)
            raise
        return _RealRegistration(
            handles=handles, windows=windows, buffers=dict(buffers)
        )

    def connect(self, agent, peer: dict) -> object:
        hixl = self._require()
        if "endpoint" not in peer:
            raise ValueError("the peer descriptor carries no endpoint to connect")
        status = agent.client.connect(peer["endpoint"])
        if status != hixl.SUCCESS:
            raise RuntimeError(
                f"hixl connect refused {peer['endpoint']!r} (status {status})"
            )
        # The connection's identity on this binding is the remote engine
        # string itself: every later call addresses the peer by it.
        return peer["endpoint"]

    def disconnect(self, agent, connection) -> None:
        hixl = self._require()
        status = agent.client.disconnect(connection)
        if status != hixl.SUCCESS:
            raise RuntimeError(
                f"hixl disconnect refused {connection!r} (status {status})"
            )

    def deregister(self, agent, registration) -> None:
        """Deregister one window per held handle.

        A handle leaves the registration exactly once, when its
        deregistration succeeds; a refused handle stays on the
        registration and its buffers move into the quarantine owner, so a
        retry can never double-free and a refusal can never silently drop
        the vendor registration or finalize its buffers.
        """
        hixl = self._require()
        for name in list(registration.handles.keys()):
            handle = registration.handles[name]
            status = agent.client.deregister_mem(handle)
            if status == hixl.SUCCESS:
                registration.handles.pop(name)
                registration.windows.pop(name, None)
                continue
            retainment = retain_registration(
                registration.handles,
                {key: registration.buffers[key] for key in registration.handles},
                f"hixl deregister_mem refused window {name!r} (status {status})",
            )
            raise RegistrationRetainedError(
                retainment,
                f"hixl deregister_mem refused window {name!r} "
                f"(status {status}); all remaining handles and buffers are "
                "retained for a qualified fence",
            )

    def submit_write(
        self, agent, registration, connection, window_name, source, length
    ) -> str:
        return self._submit(
            "write", agent, registration, connection, window_name, source, length
        )

    def submit_read(
        self, agent, registration, connection, window_name, source, length
    ) -> str:
        return self._submit(
            "read", agent, registration, connection, window_name, source, length
        )

    def _submit(
        self, direction, agent, registration, connection, window_name, source, length
    ) -> str:
        """Submit one address-based transfer over the pinned surface.

        The pinned binding executes every op on the submitting process's
        own engine: TransferOp.WRITE pushes this process's memory to the
        peer address and TransferOp.READ pulls the peer address into this
        process's memory. A consuming process whose registration windows
        are its own may therefore only pull: submit_read maps onto
        TransferOp.READ with TransferOpDesc(local window, remote address,
        length), and submit_write is refused — a WRITE submitted here
        would push this process's window bytes over the peer's registered
        region. The measured source-initiated WRITE belongs to the
        source's own process and registration, which is the T22 service
        wiring, not this call.
        """
        hixl = self._require()
        remote = self._remote_descriptor(source, length)
        local = registration.windows[window_name]
        if length > local["len"]:
            raise ValueError(
                f"the transfer of {length} bytes exceeds window "
                f"{window_name!r} ({local['len']} bytes)"
            )
        if direction == "write":
            raise TransferRefusedError(
                "the pinned hixl binding executes a WRITE on the submitting "
                "engine: a consuming process must not push its window bytes "
                "onto the peer's registered region; submit reads instead"
            )
        status, operation = agent.client.transfer_async(
            connection,
            hixl.TransferOp.READ,
            [hixl.TransferOpDesc(local["addr"], remote["addr"], length)],
        )
        if status != hixl.SUCCESS:
            raise TransferRefusedError(
                f"hixl transfer_async refused the read (status {status})"
            )
        return str(operation)

    @staticmethod
    def _remote_descriptor(source: dict, length: int) -> dict:
        try:
            addr = int(source["addr"])
        except (KeyError, TypeError, ValueError) as exc:
            raise ValueError(
                "a remote source descriptor carries the peer's published "
                "address: {'addr': int, 'length': int}"
            ) from exc
        return {"addr": addr, "len": int(length), "type": "HOST"}

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        """Observe one submitted transfer without deciding it.

        get_transfer_status is the pinned completion observation, compared
        by exact enum identity: only COMPLETED and FAILED are released
        requests per the pinned interface documentation. TIMEOUT is NOT
        released — the documentation requires disconnect and cleanup
        before reuse — so an unresolved answer stays non-terminal and the
        caller keeps the operation and its buffers retained until a
        qualified fence applies. A refused observation raises so the
        caller retains UNKNOWN instead of inventing a terminal state.
        """
        hixl = self._require()
        try:
            request = int(operation_id)
        except (TypeError, ValueError) as exc:
            raise ValueError(
                f"operation id {operation_id!r} is not a pinned request handle"
            ) from exc
        status, transfer_status = agent.client.get_transfer_status(request)
        if status != hixl.SUCCESS:
            raise RuntimeError(
                f"hixl get_transfer_status refused handle {operation_id} "
                f"(status {status})"
            )
        if transfer_status == hixl.TransferStatus.COMPLETED:
            return TransferOutcome(state="succeeded", detail="COMPLETED")
        if transfer_status == hixl.TransferStatus.FAILED:
            return TransferOutcome(state="failed", detail="FAILED")
        if transfer_status == hixl.TransferStatus.TIMEOUT:
            return TransferOutcome(
                state="pending",
                detail=(
                    "TIMEOUT: the request is not released; retained until "
                    "disconnect and cleanup"
                ),
            )
        return TransferOutcome(
            state="pending", detail=f"unresolved transfer state {transfer_status!r}"
        )

    def device_memory_bytes(self, agent) -> int:
        # Measured on the host through torch_npu's device-wide stats,
        # outside any framework allocator; injected by the host suite.
        raise NotImplementedError("device memory is measured by the host suite")

    def describe_connection(self, connection) -> list:
        # The records connection setup emits are host-specific; the host
        # suite injects their reader. The seam keeps one audit shape.
        raise NotImplementedError("connection records are read by the host suite")


__all__ = [
    "PINNED_COMMIT",
    "PINNED_VERSION",
    "SUPPORTED_VERSIONS",
    "HixlAPI",
    "RealHixlAPI",
    "RuntimeReadiness",
    "TransferOutcome",
    "load_runtime",
    "probe_runtime",
]
