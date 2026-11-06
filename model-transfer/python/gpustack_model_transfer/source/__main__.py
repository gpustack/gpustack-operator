"""The source holder's entry point: python3 -m gpustack_model_transfer.source.

Runs the real production supervisor: real vendor factories only — the
backend and allocation owners come from the environment's vendor scope,
never from a CPU fallback. The environment is the identity contract:
KUBERNETES_POD_UID is required (the holder refuses to fabricate a
worker identity), the backend selection names nixl or hixl, and the
broker address plus bootstrap credential file come from the holder's
own configuration. Nothing tenant-shaped reaches argv: the process
takes no arguments, so a status line or a ps snapshot can never leak a
path or a token.
"""

import ipaddress
import logging
import signal
import socket
import sys
import threading
import uuid
from contextlib import suppress

from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.envs import get_envs
from gpustack_model_transfer.source.backend import (
    real_source_allocator,
    real_source_backend,
)
from gpustack_model_transfer.source.control import BootstrapClient
from gpustack_model_transfer.source.supervisor import SourceSupervisor
from gpustack_model_transfer.source.transfer_service import PrivateServer
from gpustack_model_transfer.source.workorder import require_pod_identity

logger = logging.getLogger("gpustack_model_transfer.source")

_BROKER_ADDRESS_ENV = "GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS"
_BOOTSTRAP_FILE_ENV = "GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE"
_BACKEND_ENV = "GPUSTACK_MODEL_TRANSFER_BACKEND"
_NODE_NAME_ENV = "KUBERNETES_NODE_NAME"
_POD_IP_ENV = "KUBERNETES_POD_IP"


def build_factories(environ):
    """The real vendor factories for the environment's backend scope."""
    cfg = get_envs(environ)
    backend = cfg.GPUSTACK_MODEL_TRANSFER_BACKEND or ""
    if backend not in ("nixl", "hixl"):
        raise SourceUnavailableError(
            f"{_BACKEND_ENV} must name nixl or hixl; the holder refuses "
            "to guess a vendor scope"
        )
    if backend == "nixl" and (cfg.NIXL_ETCD_ENDPOINTS or cfg.NIXL_TCPSTORE_ENDPOINT):
        raise SourceUnavailableError(
            "external source metadata discovery is not allowed"
        )
    scope = cfg.GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE or ""
    if (backend, scope) not in (("nixl", "nvidia"), ("hixl", "ascend")):
        raise SourceUnavailableError("backend and vendor scope do not match")
    pod_ip = cfg.KUBERNETES_POD_IP or ""
    try:
        ipaddress.ip_address(pod_ip)
    except ValueError:
        raise SourceUnavailableError("a valid Pod IP is required") from None
    if not cfg.KUBERNETES_NODE_NAME:
        raise SourceUnavailableError("node identity is required")
    if ipaddress.ip_address(pod_ip).version != 4:
        raise SourceUnavailableError("source rendezvous requires an IPv4 Pod IP")
    endpoints = set()
    endpoint_lock = threading.Lock()

    def make_backend(assignment):
        address = ""
        if backend == "hixl":
            with endpoint_lock:
                for _ in range(64):
                    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
                        probe.bind((pod_ip, 0))
                        port = probe.getsockname()[1]
                    if port not in endpoints:
                        endpoints.add(port)
                        break
                else:
                    raise SourceUnavailableError("no unique source engine endpoint")
            address = f"{pod_ip}:{port}"
        result = real_source_backend(backend, address)
        if backend == "hixl":

            def release_endpoint():
                with endpoint_lock:
                    endpoints.discard(port)

            result.on_finalized = release_endpoint
        return result

    def make_allocator(assignment):
        return real_source_allocator(backend)

    return backend, make_backend, make_allocator


def main(argv=None, environ=None, stop_event=None) -> int:
    """Run the source holder until drained, stale, or stopped.

    argv is accepted and ignored: the holder's configuration lives in
    the environment, and no tenant value may pass through argv.
    """
    del argv
    environ = get_envs(environ)
    logging.basicConfig(level=logging.INFO)
    try:
        worker_id = require_pod_identity(environ)
        address = environ.GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS
        bootstrap_file = environ.GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE
        if not address or not bootstrap_file:
            raise SourceUnavailableError(
                f"{_BROKER_ADDRESS_ENV} and {_BOOTSTRAP_FILE_ENV} are "
                "both required: the holder refuses to start half-configured"
            )
        backend, backend_factory, allocator_factory = build_factories(environ)
    except SourceUnavailableError as exc:
        print(f"source holder refused to start: {exc}", file=sys.stderr)
        return 2

    if not address.startswith("127.0.0.1:"):
        print(
            "source holder refused to start: broker must use loopback", file=sys.stderr
        )
        return 2
    instance_id = uuid.uuid4().hex
    bootstrap = BootstrapClient(address, bootstrap_file)
    supervisor = SourceSupervisor(
        bootstrap,
        control_factory=lambda assignment: _control_for(
            environ, worker_id, assignment, instance_id
        ),
        backend_factory=backend_factory,
        allocator_factory=allocator_factory,
        worker_id=worker_id,
        backend=backend,
    )
    supervisor.worker_instance_id = instance_id
    try:
        private_server = PrivateServer(supervisor.operations, bootstrap_file)
    except OSError:
        print(
            "source holder refused to start: private service unavailable",
            file=sys.stderr,
        )
        return 2
    private_thread = threading.Thread(target=private_server.serve_forever, daemon=True)
    private_thread.start()
    stop = stop_event if stop_event is not None else threading.Event()

    def request_shutdown(signum, _frame):
        logger.info("signal %s: draining the source holder", signum)
        stop.set()

    for sig in (signal.SIGTERM, signal.SIGINT):
        # ValueError: signal handlers need the main thread; tests and
        # helper processes skip registration rather than fail.
        with suppress(ValueError):
            signal.signal(sig, request_shutdown)

    backoff = 0.2
    while not stop.is_set():
        try:
            supervisor.poll_once()
            backoff = 0.2
        except Exception as exc:
            logger.warning("assignment poll failed (%s)", type(exc).__name__)
            stop.wait(backoff)
            backoff = min(backoff * 2, 5.0)
            continue
        stop.wait(0.5)
    # Stop new native work before waiting for the broker's drain verdict.
    supervisor.operations.freeze()
    # No assignment disappearance or signal establishes native release. Keep
    # the process and control heartbeats alive until every owner is drained.
    while not supervisor.stop(timeout=0.2):
        threading.Event().wait(0.5)
    private_server.shutdown()
    private_server.server_close()
    private_thread.join(timeout=5)
    return 0


def _control_for(environ, worker_id: str, assignment, instance_id):
    from gpustack_model_transfer.source.control import ControlClient

    return ControlClient(
        environ.get(_BROKER_ADDRESS_ENV, ""),
        worker_id=worker_id,
        generation=assignment.generation,
        secret=assignment.worker_secret,
        worker_instance_id=instance_id,
    )


if __name__ == "__main__":
    sys.exit(main())
