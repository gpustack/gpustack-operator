"""The source's native backend holders: register pinned host memory.

Each holder owns the vendor side of one generation: it starts the
backend in this process, registers complete safetensors-file blocks in
bounded resident batches, and revokes them before eviction or drain.
The source never publishes anything: register-and-hold never
calls the metadata APIs, because exporting the source's descriptors or
connection info is a disclosure decision, not a side effect of holding
memory.

An ambiguous registration or refused teardown retains the still-owned
handles, all blocks, allocation owner, and backend in process quarantine.
Nothing is freed on an unknown path.
"""

import base64
import logging
import threading
import uuid
from contextlib import suppress

from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.source.fill import fill_from_tree, resident_files

logger = logging.getLogger("gpustack_model_transfer.source")

_RETAINED: list = []
_RETAINED_LOCK = threading.Lock()

# The bounded stage vocabulary a failed HIXL source operation may stamp on
# its exception. The stage names only where an attempt failed; it is never
# evidence of submission, terminality, or DMA.
_NATIVE_STAGES = ("connect", "submit", "poll", "disconnect")


def _stamp_stage(exc, stage):
    """Tag the raised exception with the bounded stage label, best effort.

    Stamping never replaces or alters the exception: even a hostile
    attribute assignment that raises a BaseException is dropped here rather
    than allowed to mask the original failure, and a raced interrupt inside
    one attribute store is deferred instead of replacing it.
    """
    with suppress(BaseException):
        exc.gpustack_native_stage = stage


class SourceStateRetainedError(RuntimeError):
    """Native source state a refused step left behind is now retained.

    retainment is the live quarantine owner: it holds the still-owned
    handles, the strong blocks, the allocation owner, and the backend,
    so nothing is finalized until a qualified fence disposes of it.
    """

    def __init__(self, retainment, message: str):
        super().__init__(message)
        self.retainment = retainment


class SourceLocalOnlyRefusedError(SourceStateRetainedError):
    """The binding refused MemDesc.local_only, so the registration is retained.

    The class name is the diagnosis: it is fixed and safe, so no native text,
    no path and no address travel with it. A worker that reports the failure
    by its class name surfaces the diagnosis without carrying anything else.
    """

    def __init__(self, retainment):
        super().__init__(retainment, "the hixl binding refused MemDesc.local_only")


class _LocalOnlyRefused(Exception):
    """The binding refused MemDesc.local_only before the native call.

    This is an internal signal. It carries no text, and the caller turns it
    into a SourceLocalOnlyRefusedError that owns the retainment.
    """


class SourcePrePostError(RuntimeError):
    """Native request creation refused before any transfer was posted."""


class _SourceRetainment:
    """Strong references to leaked source state, whole ownership graph."""

    def __init__(self, handles, blocks, detail, owner=None, backend=None):
        self.handles = dict(handles)
        self.blocks = dict(blocks)
        self.detail = detail
        self.owner = owner
        self.backend = backend


def retain_source_state(handles, blocks, detail, *, owner=None, backend=None):
    retainment = _SourceRetainment(
        handles, blocks, detail, owner=owner, backend=backend
    )
    with _RETAINED_LOCK:
        _RETAINED.append(retainment)
    return retainment


def retained_source_states() -> tuple:
    with _RETAINED_LOCK:
        return tuple(_RETAINED)


def release_source_retained(retainment, *, external_fence=None) -> bool:
    """Drop one retainment record from the source quarantine owner.

    Dropping a record requires an external qualified fence, including
    when an ambiguous native call did not return a handle.
    """
    if not (isinstance(external_fence, str) and external_fence.strip()):
        raise ValueError(
            "the retained source state still owns native handles; "
            "dropping its quarantine record needs external fence "
            "evidence — a documented record that a qualified fence "
            "outside this wrapper disposed of the state"
        )
    with _RETAINED_LOCK:
        try:
            _RETAINED.remove(retainment)
        except ValueError:
            return False
        return True


class SourceRegistration:
    """One generation's native registration: per-name handles, blocks."""

    def __init__(
        self, backend_name: str, handles: dict, blocks: dict, agent=None, native=None
    ):
        self.backend_name = backend_name
        self.handles = handles
        self.blocks = blocks
        self.agent = agent
        self.native = native

    def descriptors(self, worker_uid: str) -> list:
        """The filled report's descriptor list: one entry per file.

        The address is the pinned allocation's own address, the length
        the complete file's extent; no secret, key, or tenant path
        beyond the manifest-relative file name travels here.
        """
        return [
            {
                "name": name,
                "addr": block.data_ptr(),
                "length": block.byte_size,
                "memoryType": "HOST",
                "backend": self.backend_name,
                "workerUID": worker_uid,
            }
            for name, block in sorted(self.blocks.items())
        ]


def _validate_source_write(registration, name, offset, length, target):
    if not isinstance(name, str) or name not in registration.blocks:
        raise ValueError("source file is not registered")
    block = registration.blocks[name]
    address = block.data_ptr()
    if (
        isinstance(address, bool)
        or not isinstance(address, int)
        or address <= 0
        or address + block.byte_size > 1 << 64
    ):
        raise ValueError("source allocation address is invalid")
    for value in (offset, length, target.get("address"), target.get("length")):
        if isinstance(value, bool) or not isinstance(value, int):
            raise ValueError("source write range is not integral")
    if (
        offset < 0
        or length <= 0
        or offset > block.byte_size
        or length > block.byte_size - offset
        or target["address"] <= 0
        or target["length"] < length
        or target["address"] + target["length"] > 1 << 64
    ):
        raise ValueError("source write range exceeds an owned region")
    return block


class _PagingBackend:
    """Keep one resident batch alive until every posted operation is acknowledged."""

    def configure_paging(self, assignment, allocator, blocks, lock, should_abort):
        self._assignment = assignment
        self.allocator = allocator
        self._blocks = blocks
        self._blocks_lock = lock
        self._should_abort = should_abort

    def needs_window_swap(self, registration, name):
        assignment = getattr(self, "_assignment", None)
        return bool(assignment and assignment.paged and name not in registration.blocks)

    def has_active_peers(self):
        return bool(self._peers or self._in_use_counts)

    def _acquire_file(self, name):
        self._in_use_counts[name] = self._in_use_counts.get(name, 0) + 1

    def _release_file(self, name):
        count = self._in_use_counts[name]
        if count == 1:
            del self._in_use_counts[name]
        else:
            self._in_use_counts[name] = count - 1

    def _check_paging_state(self):
        if self._paging_failed or (
            getattr(self, "_should_abort", None) and self._should_abort()
        ):
            raise SourceUnavailableError("source paging state is unavailable")

    def _swap_resident_window(self, registration, start_name):
        self._check_paging_state()
        if self.has_active_peers():
            raise SourcePrePostError(
                "source window still has unacknowledged operations"
            )
        assignment = self._assignment
        selected = resident_files(
            assignment.manifest,
            assignment.resident_budget_bytes,
            start_name,
            backend=assignment.backend,
        )
        selected_names = {descriptor.path for descriptor in selected}
        added = {}
        try:
            self.revoke(registration)
            # All deregistrations must succeed before any host allocation is freed.
            for name, block in list(self._blocks.items()):
                if name in selected_names:
                    continue
                self._check_paging_state()
                self.allocator.release(block)
                with self._blocks_lock:
                    del self._blocks[name]
            for descriptor in selected:
                if descriptor.path in self._blocks:
                    continue
                self._check_paging_state()
                block = self.allocator.allocate(descriptor.path, descriptor.size)
                with self._blocks_lock:
                    self._blocks[descriptor.path] = block
                    added[descriptor.path] = block
            fill_from_tree(
                assignment.manifest,
                assignment.tree_path,
                added,
                should_abort=self._should_abort,
                resident_only=True,
            )
            self._check_paging_state()
            current = self.register(self._blocks)
            registration.handles = current.handles
            registration.native = current.native
        except BaseException:
            self._paging_failed = True
            retained = retain_source_state(
                registration.handles,
                self._blocks,
                "source page-in failed",
                owner=self.allocator,
                backend=self,
            )
            raise SourceStateRetainedError(retained, "source page-in failed") from None

    def _prepare_write(self, registration, name, offset, length, target):
        self._check_paging_state()
        if self.needs_window_swap(registration, name):
            self._swap_resident_window(registration, name)
        block = _validate_source_write(registration, name, offset, length, target)
        self._acquire_file(name)
        return block


class NixlSourceBackend(_PagingBackend):
    """The NVIDIA source holder over the pinned NIXL wrapper.

    The agent is created with NO per-agent settings: the server
    environment stays authoritative for UCX selection, and this holder
    never writes the process environment. register-and-hold never calls
    get_agent_metadata: the source publishes nothing by holding memory.
    """

    backend_name = "nixl"

    def __init__(self, api=None):
        self._api = api
        self._agent = None
        self._peers = []
        self._in_use_counts = {}
        self._paging_failed = False

    def start(self, engine_address: str = "") -> object:
        if self._api is None:
            from gpustack_model_transfer.backends.nixl.runtime import load_runtime

            self._api = load_runtime()
        self._agent = self._api.create_agent(
            "model-transfer-source-" + uuid.uuid4().hex, {"enable_listen_thread": False}
        )
        return self._agent

    def register(self, blocks: dict) -> SourceRegistration:
        if self._agent is None:
            raise RuntimeError("the source backend is not started")
        try:
            registration = self._api.register_memory(self._agent, dict(blocks))
        except BaseException:
            retained = retain_source_state(
                {}, blocks, "registration outcome unknown", backend=self
            )
            raise SourceStateRetainedError(
                retained, "registration outcome unknown"
            ) from None
        registration.agent = self._agent
        return SourceRegistration(
            self.backend_name,
            registration.handles,
            blocks,
            agent=self._agent,
            native=registration,
        )

    def revoke(self, registration) -> None:
        if self._agent is None:
            raise RuntimeError("the source backend is not started")
        if self.has_active_peers():
            raise SourceUnavailableError(
                "source operations still hold the registration"
            )
        self._api.revoke_registration(self._agent, registration.native)

    def disconnect(self, registration) -> None:
        # The source never imported a peer, so there is nothing to
        # disconnect; deregistration is the supported teardown order.
        return None

    def post_write(self, registration, name, offset, length, target):
        """Connect to target metadata and post a bounded source WRITE."""
        try:
            metadata = base64.b64decode(target["metadata"], validate=True)
            remote = {
                "addr": target["address"],
                "len": target["length"],
                "mem_type": "DRAM",
                "device_id": 0,
            }
        except (KeyError, TypeError, ValueError):
            raise SourcePrePostError("invalid target metadata") from None
        self._prepare_write(registration, name, offset, length, target)
        connection = self._api.connect_peer(self._agent, metadata)
        self._peers.append(connection)
        # Keep the connection even if the post raises after submission.
        from gpustack_model_transfer.backends.nixl.errors import TransferRefusedError

        try:
            operation = self._api.submit_source_write(
                self._agent,
                registration.native,
                connection,
                name,
                remote,
                length,
                local_offset=offset,
            )
        except TransferRefusedError:
            self._api.disconnect_peer(self._agent, connection)
            self._peers.remove(connection)
            self._release_file(name)
            raise SourcePrePostError("native request refused before post") from None
        return {"connection": connection, "operation": operation, "file": name}

    def poll_write(self, operation):
        result = self._api.poll(self._agent, operation["operation"])
        if result.state == "succeeded" and not operation["operation"].released:
            raise SourceUnavailableError("native operation release is unknown")
        return result.state

    def close_write(self, operation):
        self._api.disconnect_peer(self._agent, operation["connection"])
        self._peers.remove(operation["connection"])
        self._release_file(operation["file"])


class HixlSourceBackend(_PagingBackend):
    """The Ascend source holder over the hixl engine.

    The engine initializes with the local rendezvous address and the
    holder's options; the ACL runtime reference is held for the
    backend's whole life, so a generation's teardown never finalizes a
    context another generation or a quarantine retainment still holds.
    register-and-hold never touches metadata APIs.
    """

    backend_name = "hixl"

    def __init__(self, engine_factory=None, allocator=None, engine_address=""):
        self.engine_address = engine_address
        self.on_finalized = None
        self._engine_factory = engine_factory
        self._engine = None
        self._peers = []
        self._in_use_counts = {}
        self._paging_failed = False
        self._uncertain_handles = []
        self._uncertain_operations = []
        self._cached_peers = {}
        self._peer_owners = {}
        self._uncertain_connections = set()
        self.allocator = allocator

    def start(self, engine_address: str = "") -> object:
        engine_address = engine_address or self.engine_address
        if self._engine_factory is None:
            from gpustack_model_transfer.backends.hixl.runtime import RealHixlAPI

            self._hixl = RealHixlAPI()._require()
            self._engine_factory = self._hixl.Hixl
        else:
            self._hixl = None
        engine = self._engine_factory()
        self._engine = engine
        options = {"BufferPool": "0:0"}
        status = int(engine.initialize(engine_address, options))
        if status != 0:
            raise SourceUnavailableError(
                f"hixl engine initialize failed with status {status}"
            )
        self._engine = engine
        return engine

    def _register_one(self, engine, block):
        registration_bytes = getattr(block, "registration_bytes", block.byte_size)
        if self._hixl is not None:
            desc = self._hixl.MemDesc(block.data_ptr(), registration_bytes)
            self._mark_local_only(desc)
            mem_type = self._hixl.MemType.MEM_HOST
            success = self._hixl.SUCCESS
        else:
            desc = self._mem_desc(block.data_ptr(), registration_bytes)
            mem_type = self._mem_type_host()
            success = 0
        status, handle = engine.register_mem(desc, mem_type)
        if int(status) != success or int(handle) <= 0:
            self._uncertain_handles.append((block, handle))
            raise SourceUnavailableError(
                f"hixl register_mem refused {block.name!r} with status {status}"
            )
        return handle

    def _mark_local_only(self, desc):
        """Require local-only registration before calling HIXL.

        HIXL excludes this region from its server export list. This flag
        does not establish hardware isolation or safe reclamation.

        A binding that refuses the write, that keeps the old value, or
        whose getter raises cannot establish that the flag was retained.
        All three take the same sanitized refusal. The refusal is raised
        outside the handler, so the binding's own exception never joins
        this error's chain and its text cannot travel with it.
        """
        kept = False
        try:
            desc.local_only = True
            kept = getattr(desc, "local_only", None) is True
        except Exception:
            kept = False
        if not kept:
            raise _LocalOnlyRefused

    def _mem_desc(self, address: int, byte_size: int):
        return (address, byte_size)

    def _mem_type_host(self):
        return "MEM_HOST"

    def register(self, blocks: dict) -> SourceRegistration:
        if self._engine is None:
            raise RuntimeError("the source backend is not started")

        handles = {}
        try:
            for name, block in blocks.items():
                handle = self._register_one(self._engine, block)
                handles[name] = handle
        except _LocalOnlyRefused:
            # The binding refused the flag before this block crossed the
            # native boundary, so this block's outcome is known. The earlier
            # registration really happened and its handle must not be
            # dropped, so every block, handle, owner and backend stays.
            retained = retain_source_state(
                handles,
                blocks,
                SourceLocalOnlyRefusedError.__name__,
                owner=self.allocator,
                backend=self,
            )
            raise SourceLocalOnlyRefusedError(retained) from None
        except BaseException:
            # Even an exception from the current register call may have crossed
            # the native boundary. Keep every allocation and the engine alive.
            retained = retain_source_state(
                handles,
                blocks,
                "registration outcome unknown",
                owner=self.allocator,
                backend=self,
            )
            raise SourceStateRetainedError(
                retained, "registration outcome unknown"
            ) from None
        registration = SourceRegistration(
            self.backend_name, handles, blocks, agent=self._engine
        )
        return registration

    def revoke(self, registration) -> None:
        if self._engine is None:
            raise RuntimeError("the source backend is not started")
        if self.has_active_peers():
            raise SourceUnavailableError(
                "source operations still hold the registration"
            )
        self.disconnect(registration)
        self._revoke_handles(registration.handles, registration.blocks)

    def _revoke_handles(self, handles: dict, blocks: dict) -> None:
        for name in list(handles.keys()):
            handle = handles[name]
            try:
                status = int(self._engine.deregister_mem(handle))
            except Exception as exc:
                retainment = retain_source_state(
                    {name: handle},
                    {name: blocks.get(name)},
                    f"hixl deregister_mem refused {name!r}: {exc}",
                    owner=self.allocator,
                    backend=self,
                )
                raise SourceStateRetainedError(
                    retainment,
                    f"hixl deregister_mem refused {name!r}; the handle, "
                    "its block, the allocation owner, and the backend are "
                    "retained for a qualified fence",
                ) from None
            if status != 0:
                retainment = retain_source_state(
                    {name: handle},
                    {name: blocks.get(name)},
                    f"hixl deregister_mem refused {name!r} with status {status}",
                    owner=self.allocator,
                    backend=self,
                )
                raise SourceStateRetainedError(
                    retainment,
                    f"hixl deregister_mem refused {name!r} with status "
                    f"{status}; the handle, its block, the allocation "
                    "owner, and the backend are retained for a qualified "
                    "fence",
                )
            handles.pop(name)

    def _log_failed_stage(self, stage: str) -> None:
        """Log the fixed label of the stage whose attempt failed.

        Covers a non-success status and a raised exception during the
        stage's native call alike. The label is the whole diagnostic: no
        status value, endpoint, address, handle, descriptor, identifier,
        exception text, or traceback ever joins the record, including
        when an injected exception carries a secret. The label names the
        attempted stage only; it is not proof that a transfer was posted
        or that the operation took any particular path. Interrupts are
        not labeled; they propagate untouched.
        """
        logger.warning("hixl source operation failed at stage %s", stage)

    def post_write(self, registration, name, offset, length, target):
        """Submit WRITE using this source engine and an imported target."""
        return self._post_write(registration, name, offset, length, target)

    def post_session_write(
        self, registration, name, offset, length, target, session, owner
    ):
        """Reuse a peer only for the same authenticated session and target window."""
        key = (session, owner)
        identity = (target["endpoint"], target["address"], target["length"])
        cached = self._cached_peers.get(key)
        if cached is not None and cached["identity"] != identity:
            raise SourcePrePostError("target window identity changed")
        previous = self._peer_owners.get(target["endpoint"])
        if previous is not None and previous != key:
            raise SourcePrePostError("target endpoint has another owner")
        if cached is None and len(self._cached_peers) >= 8192:
            raise SourcePrePostError("source connection limit reached")
        return self._post_write(registration, name, offset, length, target, key)

    def _post_write(self, registration, name, offset, length, target, owner=None):
        if self._uncertain_connections:
            raise SourceUnavailableError("HIXL connection release remains unknown")
        hixl = self._hixl
        if hixl is None:
            raise SourcePrePostError("HIXL source API is unavailable")
        block = self._prepare_write(registration, name, offset, length, target)
        endpoint = target["endpoint"]
        cached = self._cached_peers.get(owner)
        if cached is None:
            self._connect_peer(endpoint)
            if owner is not None:
                cached = {
                    "identity": (endpoint, target["address"], target["length"]),
                    "active": 0,
                }
                self._cached_peers[owner] = cached
                self._peer_owners[endpoint] = owner
        if cached is not None:
            cached["active"] += 1
        self._peers.append(endpoint)
        try:
            status, handle = self._engine.transfer_async(
                endpoint,
                hixl.TransferOp.WRITE,
                [
                    hixl.TransferOpDesc(
                        block.data_ptr() + offset, target["address"], length
                    )
                ],
            )
        except Exception as exc:
            _stamp_stage(exc, "submit")
            self._log_failed_stage("submit")
            raise
        if int(status) != int(hixl.SUCCESS):
            self._uncertain_operations.append(handle)
            failure = SourceUnavailableError("HIXL post outcome unknown")
            _stamp_stage(failure, "submit")
            self._log_failed_stage("submit")
            raise failure
        return {
            "connection": endpoint,
            "operation": handle,
            "file": name,
            "connection_owner": owner,
        }

    def _connect_peer(self, endpoint):
        try:
            status = int(self._engine.connect(endpoint))
        except Exception as exc:
            _stamp_stage(exc, "connect")
            self._log_failed_stage("connect")
            raise
        if status != int(self._hixl.SUCCESS):
            failure = SourceUnavailableError("HIXL connection failed")
            _stamp_stage(failure, "connect")
            self._log_failed_stage("connect")
            raise failure

    def poll_write(self, operation):
        try:
            status, result = self._engine.get_transfer_status(operation["operation"])
        except Exception as exc:
            _stamp_stage(exc, "poll")
            self._log_failed_stage("poll")
            raise
        if int(status) != int(self._hixl.SUCCESS):
            failure = SourceUnavailableError("HIXL poll outcome unknown")
            _stamp_stage(failure, "poll")
            self._log_failed_stage("poll")
            raise failure
        if result == self._hixl.TransferStatus.COMPLETED:
            return "succeeded"
        if result == self._hixl.TransferStatus.FAILED:
            return "failed"
        return "pending"

    def close_write(self, operation):
        owner = operation.get("connection_owner")
        if owner is None:
            self._disconnect_peer(operation["connection"])
        else:
            self._cached_peers[owner]["active"] -= 1
        self._peers.remove(operation["connection"])
        self._release_file(operation["file"])

    def _disconnect_peer(self, endpoint):
        try:
            status = int(self._engine.disconnect(endpoint))
        except BaseException as exc:
            self._uncertain_connections.add(endpoint)
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            _stamp_stage(exc, "disconnect")
            self._log_failed_stage("disconnect")
            raise
        if status != int(self._hixl.SUCCESS):
            self._uncertain_connections.add(endpoint)
            failure = SourceUnavailableError("HIXL disconnect outcome unknown")
            _stamp_stage(failure, "disconnect")
            self._log_failed_stage("disconnect")
            raise failure

    def has_active_peers(self):
        return bool(self._uncertain_connections) or super().has_active_peers()

    def _close_cached_peer(self, owner):
        cached = self._cached_peers[owner]
        endpoint = cached["identity"][0]
        if cached["active"] or endpoint in self._uncertain_connections:
            raise SourceUnavailableError("source connection still has native owners")
        self._disconnect_peer(endpoint)
        del self._peer_owners[endpoint]
        del self._cached_peers[owner]

    def close_session_connections(self, session):
        """Disconnect a session only after all its operations were acknowledged."""
        for owner in list(self._cached_peers):
            if owner[0] == session:
                self._close_cached_peer(owner)

    def disconnect(self, registration) -> None:
        if self.has_active_peers():
            raise SourceUnavailableError("source connections still have native owners")
        for owner in list(self._cached_peers):
            self._close_cached_peer(owner)

    def finalize(self) -> None:
        if self._engine is None:
            return
        self.disconnect(None)
        status = self._engine.finalize()
        if status is not None and int(status) != 0:
            raise SourceUnavailableError(
                f"hixl engine finalize failed with status {status}"
            )
        self._engine = None
        if self.on_finalized is not None:
            self.on_finalized()
            self.on_finalized = None


def real_source_backend(backend_name: str, engine_address: str = ""):
    """Build the REAL vendor source backend for one generation.

    This factory and the CPU fake are the only two constructors; the
    fake is an explicit Python/test-helper injection, never an
    environment or CLI fallback.
    """
    if backend_name == "nixl":
        return NixlSourceBackend()
    if backend_name == "hixl":
        return HixlSourceBackend(engine_address=engine_address)
    raise ValueError(f"unknown source backend {backend_name!r}; supported: nixl, hixl")


def real_source_allocator(backend_name: str):
    """Build the REAL pinned-host allocation owner for one vendor scope."""
    if backend_name == "nixl":
        from gpustack_model_transfer.source.memory import TorchPinnedOwner

        return TorchPinnedOwner(cache=False)
    if backend_name == "hixl":
        from gpustack_model_transfer.source.memory import AclHostOwner

        return AclHostOwner()
    raise ValueError(f"unknown source backend {backend_name!r}; supported: nixl, hixl")


__all__ = [
    "HixlSourceBackend",
    "NixlSourceBackend",
    "SourceLocalOnlyRefusedError",
    "SourceRegistration",
    "SourceStateRetainedError",
    "real_source_allocator",
    "real_source_backend",
    "release_source_retained",
    "retained_source_states",
]
