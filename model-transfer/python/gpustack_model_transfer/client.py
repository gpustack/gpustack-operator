"""Authenticated metadata-first engine reader over the remote source broker."""

import hashlib
import ipaddress
import json
import os
import re
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field, replace
from urllib.parse import urlencode

from gpustack_model_transfer.adapters.engine_reader import (
    DescriptorSequence,
)
from gpustack_model_transfer.client_metadata import load_view, verify_layout
from gpustack_model_transfer.client_native import TargetNative
from gpustack_model_transfer.client_transport import (
    BrokerTransport,
    WirePreAdmission,
    WireRefused,
    WireUnknown,
    path_id,
)
from gpustack_model_transfer.core.budgets import (
    ENV_TRANSFER_WORKERS,
    MEMORY_ALIGNMENT_BYTES,
    MIN_ADAPTIVE_WINDOW_BYTES,
    ReadBudgets,
    WindowSlots,
    align_down_2m,
    align_up_2m,
    effective_window_bytes,
    max_window_bytes,
    transfer_workers,
)
from gpustack_model_transfer.core.errors import BudgetError, SourceUnavailableError
from gpustack_model_transfer.core.manifest import (
    MAX_MANIFEST_BYTES,
    MAX_MANIFEST_FILES,
    MAX_MANIFEST_TENSORS,
    parse_manifest,
)
from gpustack_model_transfer.core.tensor_index import TensorIndex
from gpustack_model_transfer.envs import ENVS, get_envs

_PROCESS_INSTANCE = uuid.uuid4().hex
_PROCESS_PID = os.getpid()
_RETAINED = []
_RETAINED_LOCK = threading.Lock()
_SESSION_OWNERS = {}
_MAX_SESSIONS = 1024
_TIMEOUT = 1800.0


def transfer_timeout(env=None):
    """The transfer deadline in seconds: GPUSTACK_MODEL_TRANSFER_TIMEOUT, else 1800."""
    raw = get_envs(env).GPUSTACK_MODEL_TRANSFER_TIMEOUT_RAW
    if raw is None or raw == "":
        return _TIMEOUT
    try:
        value = int(raw)
    except (TypeError, ValueError):
        try:
            value = float(raw)
        except (TypeError, ValueError):
            raise SourceUnavailableError("invalid transfer timeout") from None
    if not (value > 0):
        raise SourceUnavailableError("invalid transfer timeout")
    return float(value)


def _process_instance():
    global _PROCESS_INSTANCE, _PROCESS_PID
    with _RETAINED_LOCK:
        if os.getpid() != _PROCESS_PID:
            _PROCESS_PID = os.getpid()
            _PROCESS_INSTANCE = uuid.uuid4().hex
        return _PROCESS_INSTANCE


def _retain(item):
    with _RETAINED_LOCK:
        if not any(value is item for value in _RETAINED):
            _RETAINED.append(item)


def _release_retained(item):
    with _RETAINED_LOCK:
        _RETAINED[:] = [value for value in _RETAINED if value is not item]


def _required(env, name):
    value = env.get(name)
    if not isinstance(value, str) or not value or len(value) > 4096:
        raise SourceUnavailableError("missing client configuration")
    return value


def _config(path, backend):
    try:
        with open(path, "rb") as file:
            raw = file.read((1 << 20) + 1)
        if len(raw) > 1 << 20:
            raise ValueError
        config = json.loads(raw)
        if (
            config.get("version") != 1
            or not isinstance(config.get("endpoints"), list)
            or not 0 < len(config["endpoints"]) <= 1024
        ):
            raise ValueError
        manufacturer = {"nixl": "nvidia", "hixl": "ascend"}[backend]
        endpoints = []
        for endpoint in config["endpoints"]:
            if (
                not isinstance(endpoint, dict)
                or endpoint.get("manufacturer") != manufacturer
            ):
                continue
            if any(
                not isinstance(endpoint.get(key), str) or not endpoint[key]
                for key in (
                    "address",
                    "serverName",
                    "caFile",
                    "podUID",
                    "podName",
                    "podNamespace",
                    "nodeName",
                )
            ):
                continue
            endpoints.append(endpoint)
        if not endpoints:
            raise ValueError
        return endpoints

    except (OSError, ValueError, KeyError, TypeError):
        raise SourceUnavailableError("endpoint configuration unavailable") from None


def _target_valid(descriptor, target):
    dtype_name = getattr(descriptor, "dtype", None)
    if not isinstance(dtype_name, str) or not dtype_name.strip():
        return False
    target_dtype = getattr(target, "dtype", None)
    if target_dtype is None:
        return False
    known_torch_dtype = {
        "F64": "float64",
        "F32": "float32",
        "F16": "float16",
        "BF16": "bfloat16",
        "F8_E4M3": "float8_e4m3fn",
        "F8_E5M2": "float8_e5m2",
        "F8_E8M0": "float8_e8m0fnu",
        "F8_E4M3FNUZ": "float8_e4m3fnuz",
        "F8_E5M2FNUZ": "float8_e5m2fnuz",
        "I64": "int64",
        "I32": "int32",
        "I16": "int16",
        "I8": "int8",
        "U64": "uint64",
        "U32": "uint32",
        "U16": "uint16",
        "U8": "uint8",
        "BOOL": "bool",
        "C64": "complex64",
    }.get(dtype_name)
    if (
        known_torch_dtype is not None
        and str(target_dtype) != "torch." + known_torch_dtype
    ):
        return False
    return (
        tuple(target.shape) == tuple(descriptor.shape)
        and target.device.type in ("cuda", "hip", "npu", "privateuseone", "cpu")
        and not getattr(target, "is_meta", False)
        and target.is_contiguous()
        and target.numel() * target.element_size() == descriptor.byte_size
    )


class EngineReader:
    """Probe metadata before opening a distinct immutable transfer session."""

    def __init__(
        self,
        env,
        *,
        transport_factory=BrokerTransport,
        native_factory=TargetNative,
        target_validator=_target_valid,
        budgets=None,
    ):
        try:
            self.workers = transfer_workers(env)
            default_window = max_window_bytes(env)
        except BudgetError:
            raise SourceUnavailableError("invalid transfer workers") from None
        self.budgets = budgets or ReadBudgets(default_window, self.workers, 4 << 30)
        self.budgets.validate()
        if budgets is not None and not (env.get(ENV_TRANSFER_WORKERS) or "").strip():
            self.workers = self.budgets.max_inflight
        self.backend = _required(env, "GPUSTACK_MODEL_TRANSFER_BACKEND")
        if self.backend not in ("nixl", "hixl"):
            raise SourceUnavailableError("unsupported transfer backend")
        if (
            self.backend == "hixl"
            and getattr(native_factory, "registration_alignment_bytes", 0)
            == MEMORY_ALIGNMENT_BYTES
        ):
            self.budgets = replace(
                self.budgets,
                window_bytes=max(
                    2 * MEMORY_ALIGNMENT_BYTES, align_down_2m(self.budgets.window_bytes)
                ),
            )
        # The configured env view travels with the reader: the session reads
        # its target memory mode from the same view the budgets came from.
        self.env = env
        self.artifact = _required(env, "GPUSTACK_MODEL_TRANSFER_ARTIFACT")
        self.artifact_uid = _required(env, "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID")
        self.digest = _required(env, "GPUSTACK_MODEL_TRANSFER_MANIFEST_DIGEST")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", self.digest):
            raise SourceUnavailableError("invalid artifact digest")
        self.model_path = _required(env, "GPUSTACK_MODEL_TRANSFER_MODEL_PATH")
        self.token_file = _required(env, "GPUSTACK_MODEL_TRANSFER_TOKEN")
        self.pod_uid = _required(env, "KUBERNETES_POD_UID")
        self.pod_name = _required(env, "KUBERNETES_POD_NAME")
        self.pod_namespace = _required(env, "KUBERNETES_POD_NAMESPACE")
        self.pod_ip = _required(env, "KUBERNETES_POD_IP")
        try:
            target_ip = ipaddress.ip_address(self.pod_ip)
            if self.backend == "hixl" and target_ip.version != 4:
                raise ValueError
        except ValueError:
            raise SourceUnavailableError("invalid target Pod IP") from None
        if not os.path.isabs(self.model_path) or not os.path.isabs(self.token_file):
            raise SourceUnavailableError("client paths must be absolute")
        self.endpoints = _config(
            _required(env, "GPUSTACK_MODEL_TRANSFER_CONFIG"), self.backend
        )
        self.timeout = transfer_timeout(env)
        self.endpoint = None
        self.transport = None
        self._transport_factory = transport_factory
        self.native_factory = native_factory
        self.target_validator = target_validator
        self.target_instance_id = _process_instance()
        self.manifest = None
        self.identity = None
        self.descriptors = None
        self.probe_request = None
        self.pending_opens = {}
        self.pending_may_have_posted = set()
        self.pending_closes = {}
        self._metadata_closes = {}
        self._session_requests = {}
        self.poisoned = False
        self._reader_lock = threading.Lock()

    def _model_path(self, model_config):
        model = getattr(model_config, "model", self.model_path)
        if not isinstance(model, str) or os.path.realpath(model) != os.path.realpath(
            self.model_path
        ):
            raise SourceUnavailableError("model path differs from metadata view")

    def _open(self, tensor, generation, *, deadline):
        request_id = uuid.uuid4().hex
        request = {
            "requestID": request_id,
            "targetInstanceID": self.target_instance_id,
            "podNamespace": self.pod_namespace,
            "podName": self.pod_name,
            "podUID": self.pod_uid,
            "artifactName": self.artifact,
            "artifactUID": self.artifact_uid,
            "manifestDigest": self.digest,
            "sourceGeneration": generation,
            "tensor": tensor,
            "offset": 0,
            "length": 1,
        }
        if self.pending_opens:
            previous = next(iter(self.pending_opens.values()))
            comparable = {k: v for k, v in request.items() if k != "requestID"}
            if comparable != {k: v for k, v in previous.items() if k != "requestID"}:
                raise SourceUnavailableError("another session opening remains unknown")
            request = previous
            request_id = request["requestID"]
        else:
            if self.poisoned:
                raise SourceUnavailableError("client retains an unknown session")
            with _RETAINED_LOCK:
                if len(_SESSION_OWNERS) >= _MAX_SESSIONS:
                    raise SourceUnavailableError("client session journal is full")
                _SESSION_OWNERS[request_id] = self
            self.pending_opens[request_id] = request
        self.probe_request = request
        # Session-open replay must use the same request ID even if its response was lost.
        for attempt in range(3):
            try:
                response = self.transport.request(
                    "POST", "/v1/sessions", request, deadline=deadline
                )
                session_id = response.get("sessionID")
                if isinstance(session_id, str) and session_id:
                    old_request = self._session_requests.get(session_id)
                    if old_request is not None and old_request != request_id:
                        raise WireUnknown("session identity was reused")
                    self._session_requests[session_id] = request_id
                    self.pending_opens.pop(request_id, None)
                    self.pending_may_have_posted.discard(request_id)
                    if not self.pending_closes:
                        self.poisoned = False
                        _release_retained(self)
                return response
            except WirePreAdmission as exc:  # noqa: PERF203
                if request_id in self.pending_may_have_posted and not exc.authoritative:
                    self.poisoned = True
                    _retain(self)
                    raise WireUnknown(
                        "earlier session attempt may have posted"
                    ) from None
                self.pending_opens.pop(request_id, None)
                self.pending_may_have_posted.discard(request_id)
                with _RETAINED_LOCK:
                    _SESSION_OWNERS.pop(request_id, None)
                self.poisoned = False
                raise
            except WireUnknown:
                self.pending_may_have_posted.add(request_id)
                remaining = deadline - time.monotonic()
                if attempt == 2 or remaining <= 0:
                    self.poisoned = True
                    _retain(self)
                    raise
                time.sleep(min(remaining, 0.05 * (2**attempt), 0.2))
            except BaseException:
                self.pending_may_have_posted.add(request_id)
                self.poisoned = True
                _retain(self)
                raise
        raise SourceUnavailableError("session opening failed")

    def _validate(self, response, layouts, hashes, listing_verified, *, deadline=None):
        if not isinstance(response, dict):
            raise SourceUnavailableError("invalid session response")
        if any(
            key in response for key in ("descriptors", "sourceMetadata", "sourceKey")
        ):
            raise SourceUnavailableError(
                "source registration disclosure is not accepted"
            )
        if response.get("requestID") != self.probe_request["requestID"]:
            raise SourceUnavailableError("session request identity mismatch")
        for key in (
            "sessionID",
            "generation",
            "sourcePodUID",
            "workerInstanceID",
            "manifestJSON",
            "manifestSHA256",
        ):
            if not isinstance(response.get(key), str) or not response[key]:
                raise SourceUnavailableError("incomplete session response")
        if any(
            len(response[key]) > 512
            for key in ("sessionID", "generation", "sourcePodUID", "workerInstanceID")
        ):
            raise SourceUnavailableError("session identity exceeds bound")
        if (
            response.get("digest") != self.digest
            or response.get("artifactUID") != self.artifact_uid
            or response["sourcePodUID"] != self.endpoint["podUID"]
        ):
            raise SourceUnavailableError("session source identity mismatch")
        generation = response.get("sourceGeneration")
        if (
            isinstance(generation, bool)
            or not isinstance(generation, int)
            or generation < 1
        ):
            raise SourceUnavailableError("invalid artifact generation")
        source_window = response.get("sourceWindowBytes")
        if source_window is not None and (
            isinstance(source_window, bool)
            or not isinstance(source_window, int)
            or not 1 <= source_window <= 1 << 40
        ):
            raise SourceUnavailableError("invalid source window figure")
        raw = response["manifestJSON"].encode("utf-8")
        if (
            len(raw) > MAX_MANIFEST_BYTES
            or hashlib.sha256(raw).hexdigest() != response["manifestSHA256"]
        ):
            raise SourceUnavailableError("portable manifest digest mismatch")
        manifest = parse_manifest(
            raw,
            response["manifestSHA256"],
            page_reader=lambda page: self._manifest_page(
                response, page, deadline=deadline
            ),
        )
        if (
            len(manifest.files) > MAX_MANIFEST_FILES
            or len(manifest.tensors) > MAX_MANIFEST_TENSORS
        ):
            raise SourceUnavailableError("portable manifest count exceeds bound")
        if (
            manifest.artifact_uid != self.artifact_uid
            or manifest.source_generation != generation
        ):
            raise SourceUnavailableError("portable manifest identity mismatch")
        verify_layout(manifest, layouts, hashes, listing_verified)
        operations = 0
        for tensor in manifest.tensors:
            if tensor.byte_size > self.budgets.owned_output_bytes:
                raise SourceUnavailableError("tensor exceeds the owned-output budget")
            operations += (
                tensor.byte_size + self.budgets.window_bytes - 1
            ) // self.budgets.window_bytes
        if operations > (1 << 20):
            raise SourceUnavailableError("load exceeds the operation journal budget")
        identity = (
            response["generation"],
            response["sourcePodUID"],
            response["workerInstanceID"],
            generation,
            response["manifestSHA256"],
            self.digest,
        )
        return manifest, identity

    def _manifest_page(self, response, page, *, deadline=None):
        payload = {
            "podNamespace": self.pod_namespace,
            "podName": self.pod_name,
            "podUID": self.pod_uid,
            "sessionID": response["sessionID"],
            "targetInstanceID": self.target_instance_id,
            "sourcePodUID": response["sourcePodUID"],
            "workerInstanceID": response["workerInstanceID"],
            "generation": response["generation"],
            "manifestSHA256": response["manifestSHA256"],
            "pageSHA256": page["sha256"],
        }
        answer = self.transport.request(
            "POST", "/v1/manifest-pages", payload, deadline=deadline
        )
        if (
            answer.get("manifestSHA256") != response["manifestSHA256"]
            or answer.get("pageSHA256") != page["sha256"]
            or not isinstance(answer.get("pageJSON"), str)
        ):
            raise SourceUnavailableError("manifest page identity mismatch")
        return answer["pageJSON"].encode("utf-8")

    def _close_body(self, request_id, *, target_released=False):
        return {
            "podNamespace": self.pod_namespace,
            "podName": self.pod_name,
            "podUID": self.pod_uid,
            "targetInstanceID": self.target_instance_id,
            "artifactUID": self.artifact_uid,
            "manifestDigest": self.digest,
            "requestID": request_id,
            "targetReleased": target_released,
        }

    def _close_phase(self, session_id, request_id, *, finalize):
        path = "/v1/sessions/" + path_id(session_id)
        if not finalize:
            path += "/closing"
        self.pending_closes[session_id] = request_id
        deadline = time.monotonic() + min(self.timeout, 60.0)
        attempt = 0
        while True:
            try:
                answer = self.transport.request(
                    "DELETE" if finalize else "POST",
                    path,
                    self._close_body(request_id, target_released=finalize),
                    deadline=deadline,
                )
                expected = ("closed", "released") if finalize else ("closing",)
                if answer.get("state") not in expected:
                    raise WireUnknown("session close phase not confirmed")
                if finalize:
                    self.pending_closes.pop(session_id, None)
                    opened = self._session_requests.pop(session_id, None)
                    with _RETAINED_LOCK:
                        _SESSION_OWNERS.pop(opened, None)
                    if not self.pending_opens and not self.pending_closes:
                        self.poisoned = False
                        _release_retained(self)
                return
            except WireUnknown:  # noqa: PERF203
                remaining = deadline - time.monotonic()
                if (finalize and attempt == 2) or remaining <= 0:
                    self.poisoned = True
                    raise
                # Several ranks can close together. Their source connections
                # drain on one owning thread before target windows may be freed.
                time.sleep(min(remaining, 0.05 * (2 ** min(attempt, 4))))
                attempt += 1

    def _close_session(self, session_id, request_id):
        record = self._metadata_closes.setdefault(
            session_id, {"requestID": request_id, "started": False}
        )
        if not record["started"]:
            self._close_phase(session_id, record["requestID"], finalize=False)
            record["started"] = True
        self._close_phase(session_id, record["requestID"], finalize=True)
        del self._metadata_closes[session_id]

    def _recover_metadata_closes(self):
        for session_id, record in list(self._metadata_closes.items()):
            self._close_session(session_id, record["requestID"])

    def probe(self, model_config):
        with self._reader_lock:
            return self._probe(model_config)

    def _probe(self, model_config):
        self._model_path(model_config)
        try:
            self._recover_metadata_closes()
            layouts, hashes, listing_verified = load_view(self.model_path, self.digest)
            first = next(
                name for name, layout in layouts.items() if layout[4] > layout[3]
            )
            if self.identity is not None or self.pending_opens:
                candidates = [self.endpoint]
            else:
                candidates = self.endpoints
            deadline = time.monotonic() + self.timeout
            response = None
            while time.monotonic() < deadline:
                retry_after = None
                for endpoint in candidates:
                    if time.monotonic() >= deadline:
                        break
                    if self.endpoint is not endpoint or self.transport is None:
                        try:
                            self.endpoint = endpoint
                            self.transport = self._transport_factory(
                                endpoint, self.token_file
                            )
                        except (OSError, ValueError, SourceUnavailableError):
                            continue
                    try:
                        response = self._open(first, 0, deadline=deadline)
                        break
                    except WirePreAdmission as exc:
                        if exc.retry_after is not None:
                            retry_after = (
                                exc.retry_after
                                if retry_after is None
                                else min(retry_after, exc.retry_after)
                            )
                        continue
                if response is not None:
                    break
                # Every candidate refused with a named wait: sleep out the
                # shortest one and sweep again while the deadline lasts. A
                # sweep that drew no wait at all is not a busy node, so it
                # is not repeated.
                if retry_after is None:
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                time.sleep(min(retry_after, remaining))
            if response is None:
                raise SourceUnavailableError("no source admitted the artifact")
            session_id = response.get("sessionID")
            try:
                manifest, identity = self._validate(
                    response, layouts, hashes, listing_verified, deadline=deadline
                )
            finally:
                if isinstance(session_id, str) and session_id:
                    try:
                        self._close_session(session_id, uuid.uuid4().hex)
                    except BaseException:
                        _retain(self)
                        raise
                else:
                    _retain(self)
            if self.identity is not None and self.identity != identity:
                raise SourceUnavailableError("source incarnation changed")
            self.manifest, self.identity = manifest, identity
            ordered_tensors = manifest.tensors
            if not isinstance(ordered_tensors, TensorIndex):
                ordered_tensors = sorted(
                    ordered_tensors,
                    key=lambda item: (item.file, item.data_begin, item.name),
                )
            self.descriptors = DescriptorSequence(ordered_tensors)
            return self.descriptors
        except BaseException as exc:
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise SourceUnavailableError("metadata probe failed") from None

    def start(self, model_config):
        with self._reader_lock:
            return self._start(model_config)

    def _start(self, model_config):
        self._model_path(model_config)
        if self._metadata_closes:
            try:
                self._recover_metadata_closes()
            except Exception:
                raise SourceUnavailableError(
                    "metadata session release remains unknown"
                ) from None
        if self.manifest is None:
            raise SourceUnavailableError("metadata probe is required")
        try:
            layouts, hashes, listing_verified = load_view(self.model_path, self.digest)
            first = next(t.name for t in self.manifest.tensors if t.byte_size)
            pinned_endpoint = self.endpoint
            candidates = [self.endpoint] + [
                endpoint for endpoint in self.endpoints if endpoint is not self.endpoint
            ]
            deadline = time.monotonic() + self.timeout
            response = None
            while time.monotonic() < deadline:
                retry_after = None
                for endpoint in candidates:
                    if time.monotonic() >= deadline:
                        break
                    if self.endpoint is not endpoint or self.transport is None:
                        try:
                            self.endpoint = endpoint
                            self.transport = self._transport_factory(
                                endpoint, self.token_file
                            )
                        except (OSError, ValueError, SourceUnavailableError):
                            continue
                    try:
                        response = self._open(
                            first, self.identity[3], deadline=deadline
                        )
                        break
                    except WirePreAdmission as exc:
                        if exc.retry_after is not None:
                            retry_after = (
                                exc.retry_after
                                if retry_after is None
                                else min(retry_after, exc.retry_after)
                            )
                        continue
                if response is not None:
                    break
                if retry_after is None:
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                time.sleep(min(retry_after, remaining))
            if response is None:
                raise SourceUnavailableError("no source admitted the artifact")
            session_id = response.get("sessionID")
            try:
                manifest, identity = self._validate(
                    response, layouts, hashes, listing_verified, deadline=deadline
                )
                # Verified content hashes bind the tensor pages; independent
                # disk indexes need not be the same Python object.
                if (
                    manifest.contract_version != self.manifest.contract_version
                    or manifest.identity() != self.manifest.identity()
                    or manifest.files != self.manifest.files
                ):
                    raise SourceUnavailableError("source incarnation changed")
                if identity != self.identity:
                    if self.endpoint is pinned_endpoint:
                        raise SourceUnavailableError("source incarnation changed")
                    # The pinned peer was full and another source served the
                    # same digest: the manifest pins the content, so the new
                    # incarnation is adopted for this session.
                    self.identity = identity
            except BaseException:
                if isinstance(session_id, str) and session_id:
                    try:
                        self._close_session(session_id, uuid.uuid4().hex)
                    except BaseException:
                        _retain(self)
                else:
                    _retain(self)
                raise
            session = EngineSession(
                self,
                session_id,
                manifest,
                self.identity,
                source_window_bytes=response.get("sourceWindowBytes"),
            )
            with _RETAINED_LOCK:
                _SESSION_OWNERS[self._session_requests[session_id]] = session
            return session
        except BaseException as exc:
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise SourceUnavailableError("native session start failed") from None


class _SafeTransferFailure(Exception):
    """A refusal or terminal failure whose owners have been acknowledged."""


@dataclass(frozen=True)
class _TransferSpan:
    tensor: object
    offset: int
    length: int
    following: tuple | None

    @property
    def begin(self):
        return self.tensor.data_begin + self.offset

    @property
    def end(self):
        return self.begin + self.length

    def contains(self, tensor, position):
        return (
            tensor.file == self.tensor.file
            and self.begin <= tensor.data_begin + position < self.end
        )


@dataclass
class _PrefetchWindow:
    """Foreground-owned state; the worker receives only an operation payload."""

    native: object
    span: _TransferSpan | None = None
    future: object = None
    operation: dict | None = None
    slot: int | None = None
    error: BaseException | None = None
    targets: dict = field(default_factory=dict)


class EngineSession:
    """Bounded staging windows, terminal receipts, targets and consumers."""

    def __init__(
        self, reader, session_id, manifest, identity, source_window_bytes=None
    ):
        self.reader, self.session_id, self.manifest, self.identity = (
            reader,
            session_id,
            manifest,
            identity,
        )
        self.native = None
        self._native_owners = []
        self._windows = []
        self._prefetch_executor = None
        self.targets = []
        self.inflight = None
        self.operation_count = 0
        self.unknown = False
        self.closed = False
        self.close_request = uuid.uuid4().hex
        self.workers = reader.workers
        self.slots = WindowSlots(reader.budgets.max_inflight)
        self._closing_started = False
        self._target_released = False
        self._close_uncertain = False
        self._lock = threading.Lock()
        self._closing = threading.Event()
        from gpustack_model_transfer.core.tensor_index import tensor_tables

        self._tensors_by_name, by_file, self._file_index = tensor_tables(manifest)
        max_span = 0
        for items in by_file.values():
            run_begin = None
            run_end = None
            for item in items:
                if item.byte_size <= 0:
                    continue
                if run_begin is None or item.data_begin != run_end:
                    run_begin = item.data_begin
                    run_end = item.data_end
                else:
                    run_end = item.data_end
                if run_end - run_begin > max_span:
                    max_span = run_end - run_begin
        self._tensors_by_file = by_file
        self._max_tensor_bytes = max((t.byte_size for t in manifest.tensors), default=0)
        self._max_contiguous_span = max_span
        self._source_window_bytes = source_window_bytes
        self._cached_window = None
        self.window_bytes = self._resolve_window_bytes()
        self.adaptive_span_bytes = self.window_bytes
        self.last_operation_seconds = 0.0
        self.last_throughput_bps = 0.0
        self.ema_throughput_bps = 0.0

    def _create_native(self, size=None):
        native = self.reader.native_factory(
            self.reader.backend,
            self.reader.pod_ip,
            self.window_bytes if size is None else size,
        )
        # Retain partial initialization too: a failed register may still own
        # native memory.
        if not hasattr(self, "_native_owners"):
            self._native_owners = []
        self._native_owners.append(native)
        if not 0 < native.size <= (self.window_bytes if size is None else size):
            raise SourceUnavailableError("native window exceeds session budget")
        native.start()
        return native

    def _initialize_native(self):
        self.window_bytes = self._resolve_window_bytes()
        aligned_hixl = (
            getattr(self.reader, "backend", None) == "hixl"
            and getattr(self.reader.native_factory, "registration_alignment_bytes", 0)
            == MEMORY_ALIGNMENT_BYTES
        )
        pipelined = (
            self.slots.count >= 2
            and self.window_bytes >= (4 * MEMORY_ALIGNMENT_BYTES if aligned_hixl else 2)
            and getattr(self.reader.native_factory, "supports_window_offset", False)
        )
        size = self.window_bytes // 2 if pipelined else self.window_bytes
        if aligned_hixl:
            size = align_down_2m(size)
        self.native = self._create_native(size)
        if pipelined:
            self._windows.append(_PrefetchWindow(self.native))
            secondary = self._create_native(size)
            self._windows.append(_PrefetchWindow(secondary))
            if (
                self.native.registration_id,
                self.native.window_id,
            ) == (secondary.registration_id, secondary.window_id):
                raise SourceUnavailableError(
                    "native windows must have distinct identities"
                )
        self.adaptive_span_bytes = self.native.size

    def _resolve_window_bytes(self):
        max_needed = self._max_tensor_bytes
        if getattr(self.reader.native_factory, "supports_window_offset", False):
            max_needed = max(max_needed, self._max_contiguous_span)
        if max_needed <= 0:
            return 0
        window = self.reader.budgets.window_bytes
        source = self._source_window_bytes
        if (
            isinstance(source, int)
            and not isinstance(source, bool)
            and 0 < source < window
        ):
            # The source named the buffer it can back right now: the
            # session's window is the smaller of the two budgets, floored
            # at the adaptive minimum, so a client configured larger than
            # the source buffer still transfers in bounds, chunked.
            window = max(MIN_ADAPTIVE_WINDOW_BYTES, align_down_2m(source))
        if (
            getattr(self.reader, "backend", None) == "hixl"
            and getattr(self.reader.native_factory, "registration_alignment_bytes", 0)
            == MEMORY_ALIGNMENT_BYTES
        ):
            minimum = 2 * MEMORY_ALIGNMENT_BYTES
            ceiling = max(minimum, align_down_2m(window))
            needed = align_up_2m(max_needed) + 2 * MEMORY_ALIGNMENT_BYTES
            return min(ceiling, max(minimum, needed))
        if window >= MEMORY_ALIGNMENT_BYTES and max_needed >= MEMORY_ALIGNMENT_BYTES:
            return min(align_down_2m(window), align_up_2m(max_needed))
        return min(window, max_needed)

    def _update_adaptive_window(self, transferred_bytes, elapsed_seconds):
        elapsed = max(float(elapsed_seconds), 1e-6)
        throughput = float(transferred_bytes) / elapsed
        self.last_operation_seconds = elapsed
        self.last_throughput_bps = throughput
        if self.ema_throughput_bps <= 0.0:
            self.ema_throughput_bps = throughput
        else:
            self.ema_throughput_bps = 0.7 * self.ema_throughput_bps + 0.3 * throughput
        if self.native is None or self.native.size <= 0:
            return
        min_chunk = min(
            self.native.size,
            max(self._max_tensor_bytes, MIN_ADAPTIVE_WINDOW_BYTES),
        )
        degraded = elapsed > self.reader.timeout * 0.25 or (
            elapsed > 1.0
            and self.ema_throughput_bps > 0.0
            and throughput < self.ema_throughput_bps * 0.4
        )
        if degraded:
            next_span = max(min_chunk, self.adaptive_span_bytes // 2)
            if next_span >= MEMORY_ALIGNMENT_BYTES:
                next_span = max(min_chunk, align_down_2m(next_span))
            self.adaptive_span_bytes = next_span
        elif (
            self.adaptive_span_bytes < self.native.size
            and elapsed < self.reader.timeout * 0.1
        ):
            step = max(
                self.native.size // 4,
                self.adaptive_span_bytes // 2,
                1,
            )
            if step >= MEMORY_ALIGNMENT_BYTES:
                step = align_down_2m(step)
            self.adaptive_span_bytes = min(
                self.native.size, self.adaptive_span_bytes + step
            )

    def _hold_unknown(self):
        self.unknown = True
        _retain(self)

    def _status(self, request_id, *, deadline=None):
        query = urlencode(
            {
                "sessionID": self.session_id,
                "targetInstanceID": self.reader.target_instance_id,
                "podUID": self.reader.pod_uid,
                "podName": self.reader.pod_name,
                "podNamespace": self.reader.pod_namespace,
            }
        )
        return self.reader.transport.request(
            "GET",
            "/v1/transfers/" + path_id(request_id) + "?" + query,
            deadline=deadline,
        )

    def _wait(self, request_id, accepted, initial=None, *, deadline=None):
        if deadline is None:
            deadline = time.monotonic() + self.reader.timeout
        states = (
            "prepared",
            "dispatching",
            "submitted",
            "terminal",
            "consumerReleased",
            "acknowledged",
        )
        progress = -1
        delay = 0.005
        answer = initial
        while time.monotonic() < deadline:
            if answer is not None:
                if (
                    answer.get("requestID") != request_id
                    or answer.get("sessionID") != self.session_id
                ):
                    raise WireUnknown("operation identity mismatch")
                state = answer.get("state")
                if state in accepted:
                    return answer
                if state not in states:
                    raise WireUnknown("operation outcome unknown")
                position = states.index(state)
                if position > progress:
                    progress = position
                    delay = 0.005
            time.sleep(min(delay, max(0, deadline - time.monotonic())))
            if time.monotonic() >= deadline:
                break
            delay = min(delay * 2, 0.1)
            try:
                answer = self._status(request_id, deadline=deadline)
            except WireUnknown:
                answer = None
        raise WireUnknown("operation deadline exceeded")

    def _ack(self, operation):
        request_id = operation["requestID"]
        payload = {
            "sessionID": self.session_id,
            "podNamespace": self.reader.pod_namespace,
            "podName": self.reader.pod_name,
            "podUID": self.reader.pod_uid,
            "targetInstanceID": self.reader.target_instance_id,
            "registrationID": operation["registrationID"],
            "windowID": operation["windowID"],
        }
        deadline = time.monotonic() + self.reader.timeout
        while time.monotonic() < deadline:
            try:
                answer = self.reader.transport.request(
                    "POST",
                    "/v1/transfers/" + path_id(request_id) + "/ack",
                    payload,
                    deadline=deadline,
                )
            except (WireUnknown, WireRefused):
                answer = None
            if answer is None:
                try:
                    answer = self._status(request_id, deadline=deadline)
                except WireUnknown:
                    time.sleep(min(0.005, max(0, deadline - time.monotonic())))
                    continue
            if (
                answer.get("requestID") != request_id
                or answer.get("sessionID") != self.session_id
            ):
                raise WireUnknown("operation acknowledgement identity mismatch")
            if answer.get("state") == "acknowledged":
                return
            if answer.get("state") not in ("terminal", "consumerReleased"):
                raise WireUnknown("operation acknowledgement outcome unknown")
            time.sleep(min(0.005, max(0, deadline - time.monotonic())))
        raise WireUnknown("operation acknowledgement deadline exceeded")

    def fill(self, descriptor, target):
        with self._lock:
            try:
                if self._closing.is_set():
                    raise SourceUnavailableError("session is closing")
                self._finish_prefetches(wait=False)
                self._fill(descriptor, target)
            except BaseException:
                self._join_prefetches()
                raise

    def fill_many(self, items):
        """Fill on the caller's device thread, overlapping CPU-only prefetches."""
        try:
            for descriptor, target in items:
                self.fill(descriptor, target)
        except BaseException:
            # The iterable itself can fail between fills, with a prefetch live.
            with self._lock:
                self._join_prefetches()
            raise

    def _fill(self, descriptor, target):
        if (
            self.closed
            or self.unknown
            or (self.inflight is not None and not self._windows)
        ):
            raise SourceUnavailableError("session cannot accept another transfer")
        # These checks are definite pre-post refusals: no native owner or target
        # window is created before they finish.
        try:
            device_type = target.device.type
            if device_type != "cpu" and (
                (self.reader.backend == "nixl" and device_type not in ("cuda", "hip"))
                or (
                    getattr(self.reader, "backend", None) == "hixl"
                    and device_type not in ("npu", "privateuseone")
                )
            ):
                raise SourceUnavailableError("target device and backend differ")
            tensor = self._tensors_by_name.get(descriptor.name)
            if tensor is None:
                tensor = self.manifest.tensor(descriptor.name)
            if not self.reader.target_validator(tensor, target):
                raise SourceUnavailableError("target tensor is incompatible")
            if (
                tuple(descriptor.shape) != tuple(tensor.shape)
                or descriptor.dtype != tensor.dtype
            ):
                raise SourceUnavailableError("descriptor differs from pinned manifest")
        except Exception:
            raise SourceUnavailableError(
                "target tensor or descriptor invalid"
            ) from None
        if tensor.byte_size and self.native is None:
            try:
                self._initialize_native()
            except BaseException as exc:
                if self._native_owners:
                    self._hold_unknown()
                if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                    raise
                raise SourceUnavailableError(
                    f"native tensor transfer failed ({type(exc).__name__})"
                ) from None
        try:
            self.targets[:] = [target]
            if self._windows:
                self._fill_pipelined(tensor, target)
                return
            coalesce_enabled = bool(
                self.native is not None
                and getattr(self.native, "supports_window_offset", False)
            )
            if coalesce_enabled and self._cached_window is not None:
                cached_file, span_begin, span_end = self._cached_window
                if (
                    tensor.file == cached_file
                    and tensor.data_begin >= span_begin
                    and tensor.data_end <= span_end
                ):
                    if tensor.byte_size:
                        self.native.copy_to(
                            target,
                            0,
                            tensor.byte_size,
                            tensor.data_begin - span_begin,
                        )
                    return
                self._cached_window = None
            position = 0
            while position < tensor.byte_size:
                if self.operation_count >= (1 << 20):
                    raise _SafeTransferFailure("operation journal is full")
                if getattr(self.native, "requires_synchronization", False):
                    self.native.synchronize(target)
                length = min(self.native.size, tensor.byte_size - position)
                span_length = length
                effective_limit = max(
                    length, min(self.native.size, self.adaptive_span_bytes)
                )
                if (
                    coalesce_enabled
                    and position == 0
                    and tensor.byte_size <= effective_limit
                ):
                    file_tensors = self._tensors_by_file.get(tensor.file, ())
                    idx = self._file_index.get((tensor.file, tensor.name), -1)
                    if idx >= 0:
                        span_end = tensor.data_end
                        for next_t in file_tensors[idx + 1 :]:
                            if next_t.data_begin != span_end:
                                break
                            if next_t.data_end - tensor.data_begin > effective_limit:
                                break
                            span_end = next_t.data_end
                        span_length = span_end - tensor.data_begin
                request_id = uuid.uuid4().hex
                slot = self.slots.acquire()
                self.slots.bind(slot, request_id)
                operation = {
                    "requestID": request_id,
                    "sessionID": self.session_id,
                    "podNamespace": self.reader.pod_namespace,
                    "podName": self.reader.pod_name,
                    "podUID": self.reader.pod_uid,
                    "targetInstanceID": self.reader.target_instance_id,
                    "sourcePodUID": self.identity[1],
                    "workerInstanceID": self.identity[2],
                    "generation": self.identity[0],
                    "registrationID": self.native.registration_id,
                    "windowID": self.native.window_id,
                    "tensor": tensor.name,
                    "tensorOffset": position,
                    "length": span_length,
                    "target": self.native.descriptor(),
                }
                self.inflight = operation
                self.operation_count += 1
                op_start = time.monotonic()
                deadline = op_start + self.reader.timeout
                try:
                    answer = self.reader.transport.request(
                        "POST", "/v1/transfers", operation, deadline=deadline
                    )
                except (WireUnknown, WireRefused):
                    answer = None
                terminal = self._wait(
                    request_id, {"terminal"}, answer, deadline=deadline
                )
                if terminal.get("verdict") in ("not-posted", "failed"):
                    self._ack(operation)
                    self.slots.mark_terminal(request_id)
                    self.slots.free(slot, request_id)
                    self.inflight = None
                    raise _SafeTransferFailure("native operation refused or failed")
                if terminal.get("verdict") != "succeeded":
                    raise WireUnknown("native operation outcome unknown")
                self._update_adaptive_window(span_length, time.monotonic() - op_start)
                # Hardware RDMA CRC guarantees transfer integrity; copy the
                # registered window directly without software hashing.
                if coalesce_enabled:
                    self.native.copy_to(target, position, length, 0)
                else:
                    self.native.copy_to(target, position, length)
                self._ack(operation)
                self.slots.mark_terminal(request_id)
                self.slots.free(slot, request_id)
                self.inflight = None
                if (
                    coalesce_enabled
                    and position == 0
                    and span_length > tensor.byte_size
                ):
                    self._cached_window = (
                        tensor.file,
                        tensor.data_begin,
                        tensor.data_begin + span_length,
                    )
                position += length
        except _SafeTransferFailure:
            raise SourceUnavailableError("native tensor verification failed") from None
        except BaseException as exc:
            self._hold_unknown()
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            raise SourceUnavailableError(
                f"native tensor transfer failed ({type(exc).__name__})"
            ) from None

    def _plan_span(self, tensor, position, size):
        length = min(size, tensor.byte_size - position)
        limit = max(length, min(size, self.adaptive_span_bytes))
        items = self._tensors_by_file[tensor.file]
        index = self._file_index[(tensor.file, tensor.name)]
        if position == 0 and tensor.byte_size <= limit:
            end = tensor.data_end
            for following_index in range(index + 1, len(items)):
                item = items[following_index]
                if item.data_begin != end or item.data_end - tensor.data_begin > limit:
                    break
                end = item.data_end
                index = following_index
            length = end - tensor.data_begin
        end = tensor.data_begin + position + length
        following = None
        if end < tensor.data_end:
            following = (tensor, position + length)
        else:
            for following_index in range(index + 1, len(items)):
                item = items[following_index]
                if item.data_begin != end:
                    break
                if item.byte_size:
                    following = (item, 0)
                    break
        return _TransferSpan(tensor, position, length, following)

    def _transfer_span(self, operation):
        """CPU-only worker: no native calls or mutable session bookkeeping."""
        started = time.monotonic()
        deadline = started + self.reader.timeout
        try:
            answer = self.reader.transport.request(
                "POST", "/v1/transfers", operation, deadline=deadline
            )
        except WirePreAdmission:
            raise _SafeTransferFailure("native operation was not admitted") from None
        except (WireUnknown, WireRefused):
            # A plain conflict may follow durable admission. Only an explicit
            # pre-admission refusal or acknowledged terminal verdict releases it.
            answer = None
        terminal = self._wait(
            operation["requestID"], {"terminal"}, answer, deadline=deadline
        )
        verdict = terminal.get("verdict")
        if verdict not in ("not-posted", "failed", "succeeded"):
            raise WireUnknown("native operation outcome unknown")
        self._ack(operation)
        if verdict != "succeeded":
            raise _SafeTransferFailure("native operation refused or failed")
        return time.monotonic() - started

    def _finish_prefetches(self, *, wait):
        for window in self._windows:
            self._finish_prefetch(window, wait=wait)

    def _finish_prefetch(self, window, *, wait):
        future = window.future
        if future is None or (not wait and not future.done()):
            return
        try:
            elapsed = future.result()
        except BaseException as exc:
            if not future.done():
                # An interrupted wait is not proof that its worker stopped.
                raise
            window.error = exc
            if not isinstance(exc, _SafeTransferFailure):
                self._hold_unknown()
        else:
            self._update_adaptive_window(window.span.length, elapsed)
        window.future = None
        if window.error is None or isinstance(window.error, _SafeTransferFailure):
            request_id = window.operation["requestID"]
            self.slots.mark_terminal(request_id)
            self.slots.free(window.slot, request_id)
            window.operation = None
            window.slot = None
        self.inflight = next(
            (item.operation for item in self._windows if item.operation is not None),
            None,
        )

    def _join_prefetches(self):
        executor = self._prefetch_executor
        interrupted = None
        if executor is not None:
            # Shutdown joins even if the foreground was interrupted during a
            # future wait. Harvest every outcome only after all writes stopped.
            while True:
                try:
                    executor.shutdown(wait=True)
                except (KeyboardInterrupt, SystemExit) as exc:
                    interrupted = exc
                    self._hold_unknown()
                    continue
                break
            self._prefetch_executor = None
        self._finish_prefetches(wait=True)
        if interrupted is not None:
            raise interrupted

    def _synchronize_window(self, window):
        for target in window.targets.values():
            window.native.synchronize(target)
        window.targets.clear()

    def _start_prefetch(self, window, span):
        self._finish_prefetch(window, wait=True)
        if self.unknown:
            raise WireUnknown("session retains unknown native state")
        if self._closing.is_set():
            raise _SafeTransferFailure("session is closing")
        if self.operation_count >= (1 << 20):
            raise _SafeTransferFailure("operation journal is full")
        self._synchronize_window(window)
        native = window.native
        operation = {
            "requestID": uuid.uuid4().hex,
            "sessionID": self.session_id,
            "podNamespace": self.reader.pod_namespace,
            "podName": self.reader.pod_name,
            "podUID": self.reader.pod_uid,
            "targetInstanceID": self.reader.target_instance_id,
            "sourcePodUID": self.identity[1],
            "workerInstanceID": self.identity[2],
            "generation": self.identity[0],
            "registrationID": native.registration_id,
            "windowID": native.window_id,
            "tensor": span.tensor.name,
            "tensorOffset": span.offset,
            "length": span.length,
            "target": native.descriptor(),
        }
        if self._prefetch_executor is None:
            self._prefetch_executor = ThreadPoolExecutor(
                max_workers=1, thread_name_prefix="model-transfer-prefetch"
            )
        slot = self.slots.acquire()
        self.slots.bind(slot, operation["requestID"])
        window.span, window.operation, window.slot = span, operation, slot
        window.error = None
        self.inflight = operation
        self.operation_count += 1
        # Submit can fail after enqueueing. Keep the operation bound even when
        # it returns no Future; the error path joins the executor and retains it.
        window.future = self._prefetch_executor.submit(self._transfer_span, operation)

    def _window_for(self, tensor, position):
        self._finish_prefetches(wait=False)
        if self.unknown:
            raise WireUnknown("session retains unknown native state")
        window = next(
            (
                item
                for item in self._windows
                if item.span is not None and item.span.contains(tensor, position)
            ),
            None,
        )
        if window is None:
            window = self._windows[0]
            span = self._plan_span(tensor, position, window.native.size)
            self._start_prefetch(window, span)
        self._finish_prefetch(window, wait=True)
        if window.error is not None:
            raise window.error
        return window

    def _prefetch_following(self, current):
        if (
            current.span.following is None
            or self._closing.is_set()
            or self.operation_count >= (1 << 20)
        ):
            return
        tensor, position = current.span.following
        other = next(item for item in self._windows if item is not current)
        if other.span is not None and other.span.contains(tensor, position):
            return
        span = self._plan_span(tensor, position, other.native.size)
        self._start_prefetch(other, span)

    def _fill_pipelined(self, tensor, target):
        position = 0
        while position < tensor.byte_size:
            if self._closing.is_set():
                raise _SafeTransferFailure("session is closing")
            window = self._window_for(tensor, position)
            self._prefetch_following(window)
            begin = tensor.data_begin + position
            length = min(tensor.data_end, window.span.end) - begin
            device = target.device
            window.targets[(device.type, getattr(device, "index", None))] = target
            window.native.copy_to(target, position, length, begin - window.span.begin)
            position += length
        self._finish_prefetches(wait=False)
        if self.unknown:
            raise WireUnknown("session retains unknown native state")

    def close(self):
        self._closing.set()
        with self._lock:
            self._close()

    def _close(self):
        if self.closed:
            return
        self._join_prefetches()
        if self.unknown or self.inflight is not None:
            self._hold_unknown()
            raise SourceUnavailableError("session retains unknown native state")
        try:
            if not self._closing_started:
                self.reader._close_phase(
                    self.session_id, self.close_request, finalize=False
                )
                self._closing_started = True
        except BaseException:
            self._close_uncertain = True
            _retain(self)
            raise SourceUnavailableError("session closing outcome unknown") from None
        if not self._target_released:
            try:
                if self._windows:
                    for window in self._windows:
                        self._synchronize_window(window)
                    for native in self._native_owners:
                        native.close()
                elif self.native is not None:
                    for target in self.targets:
                        self.native.synchronize(target)
                    self.native.close()
                self._target_released = True
            except BaseException:
                self._hold_unknown()
                raise SourceUnavailableError("target release outcome unknown") from None
        try:
            self.reader._close_phase(self.session_id, self.close_request, finalize=True)
        except BaseException:
            self._close_uncertain = True
            _retain(self)
            raise SourceUnavailableError("session release outcome unknown") from None
        self.closed = True
        self._close_uncertain = False
        self.targets.clear()
        _release_retained(self)

    def abort(self):
        self._closing.set()
        with self._lock:
            self._join_prefetches()
            if self.unknown or self.inflight is not None:
                self._hold_unknown()
                return
            self._close()


def create_engine_reader(load_config, tp_size: int = 1):
    """Build the production reader; no CPU or local-file fallback.

    tp_size is the TP group's world size: under the "all" TP load mode the
    configured window ceiling is one budget the ranks share, so each rank's
    window is its even share floored at the 1 GiB minimum. Every other mode
    reads the full ceiling per rank, exactly as before.
    """
    options = getattr(load_config, "model_loader_extra_config", None) or {}
    if not isinstance(options, dict) or set(options) - {
        "window_bytes",
        "max_window_bytes",
        "max_inflight",
        "owned_output_bytes",
    }:
        raise SourceUnavailableError("invalid model-transfer reader budgets")
    if (
        "window_bytes" in options
        and "max_window_bytes" in options
        and options["window_bytes"] != options["max_window_bytes"]
    ):
        raise SourceUnavailableError("conflicting window_bytes and max_window_bytes")
    try:
        default_window = effective_window_bytes(ENVS, tp_size=tp_size)
    except BudgetError:
        raise SourceUnavailableError("invalid model-transfer reader budgets") from None
    configured_window = options.get(
        "max_window_bytes", options.get("window_bytes", default_window)
    )
    budgets = ReadBudgets(
        window_bytes=configured_window,
        max_inflight=options.get("max_inflight", transfer_workers()),
        owned_output_bytes=options.get("owned_output_bytes", 4 << 30),
    )
    budgets.validate()
    return EngineReader(ENVS, budgets=budgets)
