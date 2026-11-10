"""Centralized environment variable management for gpustack_model_transfer.

Follows the singleton pattern from gpustack_runtime.envs: a single global
Envs instance (`ENVS`) and module-level `__getattr__` delegation provide
typed, normalized access to environment variables without scattering
`os.environ.get` across adapters, client, and source modules.
"""

from __future__ import annotations

import math
import os
from collections.abc import Callable, Mapping
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    GPUSTACK_MODEL_TRANSFER_TIMEOUT_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_WORKERS_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_BACKEND: str | None = None
    GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE: str | None = None
    GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND: str = "UCX"
    GPUSTACK_MODEL_TRANSFER_TIMEOUT: float = 1800.0
    GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR: str = "1800"
    GPUSTACK_MODEL_TRANSFER_WORKERS: str | None = None
    GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES: str | None = None
    GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES: int | None = None
    GPUSTACK_MODEL_TRANSFER_TP_MODE: str | None = None
    GPUSTACK_MODEL_TRANSFER_TP_MODE_RAW: str | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES: int | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES: int | None = None
    GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES: int | None = None
    GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES: int | None = None
    GPUSTACK_MODEL_TRANSFER_CONFIG: str | None = None
    GPUSTACK_MODEL_TRANSFER_TOKEN_FILE: str | None = None
    GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID: str | None = None
    GPUSTACK_MODEL_TRANSFER_DIGEST: str | None = None
    GPUSTACK_MODEL_TRANSFER_MANIFEST: str | None = None
    GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256: str | None = None
    GPUSTACK_MODEL_TRANSFER_POLICY: str = "require"
    GPUSTACK_MODEL_TRANSFER_PEER_P2P: bool = False
    GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS: str | None = None
    GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE: str | None = None
    KUBERNETES_POD_NAMESPACE: str | None = None
    KUBERNETES_POD_NAME: str | None = None
    KUBERNETES_POD_UID: str | None = None
    KUBERNETES_POD_IP: str | None = None
    KUBERNETES_NODE_NAME: str | None = None
    ASCEND_RT_VISIBLE_DEVICES: str = ""
    NIXL_ETCD_ENDPOINTS: str | None = None
    NIXL_TCPSTORE_ENDPOINT: str | None = None


def trim_str(value: str | None) -> str | None:
    """Trim leading and trailing whitespace from a string."""
    if isinstance(value, str):
        return value.strip()
    return None


def to_bool(value: str | None) -> bool:
    """Convert a string environment value to a boolean."""
    if isinstance(value, str) and value:
        return value.strip().lower() in ("1", "true", "yes", "on")
    return False


def to_int(value: str | None, default: int | None = None) -> int | None:
    """Convert a string environment value to an integer."""
    if value is not None and not isinstance(value, bool):
        try:
            return int(str(value).strip())
        except (ValueError, TypeError):
            pass
    return default


def to_float(value: str | None, default: float | None = None) -> float | None:
    """Convert a string environment value to a float."""
    if value is not None and not isinstance(value, bool):
        try:
            return float(str(value).strip())
        except (ValueError, TypeError):
            pass
    return default


def choice(
    value: str | None, options: tuple[str, ...] | list[str], default: str = ""
) -> str:
    """Return value if it is in options, otherwise default."""
    if value is None:
        return default
    if value in options:
        return value
    return default


def getenv(
    key: str,
    default: Any = None,
    env: Mapping[str, str] | None = None,
) -> Any:
    """Read an environment variable from the given mapping or os.environ."""
    source = os.environ if env is None else env
    value = source.get(key)
    if value is not None:
        return value
    return default


def setdefault(key: str, value: str, env: dict[str, str] | None = None) -> str:
    """Set an environment variable default on os.environ or a target dict."""
    target = os.environ if env is None else env
    return target.setdefault(key, value)


def resolve_nixl_backend(
    env: Mapping[str, str] | None = None,
    sysfs_root: str = "/sys/class/infiniband",
) -> str:
    """Resolve the NIXL transport plugin the agents on this host create with.

    An explicit GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND names the plugin. The
    accepted spellings are UCX and LIBFABRIC, compared case-insensitively
    around surrounding whitespace; any other non-empty value is refused
    rather than mapped to a default, because a misspelled plugin would
    otherwise surface as an agent that initializes and transfers nothing.

    Unset or empty, the plugin follows the host. An AWS EFA device under
    sysfs_root is reachable only through LIBFABRIC, so a host whose
    InfiniBand class carries EFA devices and no conventional NIC selects
    it; every other host -- including one carrying an EFA device beside a
    conventional one, where UCX still reaches the conventional fabric --
    keeps UCX, the plugin the NIXL default installs. An unreadable or
    missing sysfs_root is a host with no answerable fabric question, and
    it keeps UCX too. In heterogeneous clusters that mix EFA-only nodes
    with conventional or dual-fabric nodes, pin
    GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND uniformly across source and
    target hosts so both peers initialize with the same plugin.
    """
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    raw = trim_str(getenv("GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", None, env))
    if raw:
        plugin = raw.upper()
        if plugin not in ("UCX", "LIBFABRIC"):
            raise SourceUnavailableError(
                f"unsupported GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND {raw!r}; "
                "supported: UCX, LIBFABRIC"
            )
        return plugin

    try:
        entries = os.listdir(sysfs_root)
    except OSError:
        return "UCX"
    has_efa = False
    has_other = False
    for dev_name in entries:
        if dev_name.startswith(("rdmap", "efa_")):
            has_efa = True
            continue
        vendor_path = os.path.join(sysfs_root, dev_name, "device", "vendor")
        try:
            with open(vendor_path, encoding="ascii") as handle:
                vendor = handle.read().strip().lower()
        except OSError:
            vendor = ""
        if vendor == "0x1d0f":
            has_efa = True
        else:
            has_other = True
    if has_efa and not has_other:
        return "LIBFABRIC"
    return "UCX"


def _transfer_timeout(raw):
    from gpustack_model_transfer.core.errors import SourceUnavailableError

    if raw is None or raw == "":
        return 1800.0
    try:
        value = float(raw)
    except (TypeError, ValueError, OverflowError):
        raise SourceUnavailableError("invalid transfer timeout") from None
    if isinstance(raw, bool) or not math.isfinite(value) or value <= 0:
        raise SourceUnavailableError("invalid transfer timeout")
    return value


_VARIABLES: dict[str, Callable[[Mapping[str, str]], Any]] = {
    "GPUSTACK_MODEL_TRANSFER_BACKEND": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_BACKEND", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_VENDOR_SCOPE", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND": lambda env: resolve_nixl_backend(env),
    "GPUSTACK_MODEL_TRANSFER_TIMEOUT_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_TIMEOUT", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_TIMEOUT_STR": lambda env: str(
        math.ceil(
            _transfer_timeout(getenv("GPUSTACK_MODEL_TRANSFER_TIMEOUT", None, env))
        )
    ),
    "GPUSTACK_MODEL_TRANSFER_TIMEOUT": lambda env: _transfer_timeout(
        getenv("GPUSTACK_MODEL_TRANSFER_TIMEOUT", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_WORKERS_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_WORKERS", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_WORKERS": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_WORKERS", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_MAX_WINDOW_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_MIN_SOURCE_WINDOW_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_TP_MODE_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_TP_MODE", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_TP_MODE": lambda env: (
        trim_str(getenv("GPUSTACK_MODEL_TRANSFER_TP_MODE", None, env)) or ""
    ).lower(),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_SOURCE_MAX_MEMORY_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_SOURCE_MIN_MEMORY_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES_RAW": lambda env: getenv(
        "GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES", None, env
    ),
    "GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_SOURCE_RESIDENT_BUDGET_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES": lambda env: to_int(
        getenv("GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES", None, env), None
    ),
    "GPUSTACK_MODEL_TRANSFER_CONFIG": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_CONFIG", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_TOKEN_FILE": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_TOKEN_FILE", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_ARTIFACT_UID", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_DIGEST": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_DIGEST", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_MANIFEST": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_MANIFEST", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256": lambda env: trim_str(
        getenv("GPUSTACK_MODEL_TRANSFER_MANIFEST_SHA256", None, env)
    ),
    "GPUSTACK_MODEL_TRANSFER_POLICY": lambda env: (
        trim_str(getenv("GPUSTACK_MODEL_TRANSFER_POLICY", None, env)) or "require"
    ).lower(),
    "GPUSTACK_MODEL_TRANSFER_PEER_P2P": lambda env: to_bool(
        getenv("GPUSTACK_MODEL_TRANSFER_PEER_P2P", "0", env)
    ),
    "GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS": lambda env: (
        trim_str(getenv("GPUSTACK_MODEL_TRANSFER_BROKER_ADDRESS", "", env)) or ""
    ),
    "GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE": lambda env: (
        trim_str(getenv("GPUSTACK_MODEL_TRANSFER_BOOTSTRAP_FILE", "", env)) or ""
    ),
    "KUBERNETES_POD_NAMESPACE": lambda env: trim_str(
        getenv("KUBERNETES_POD_NAMESPACE", None, env)
    ),
    "KUBERNETES_POD_NAME": lambda env: trim_str(
        getenv("KUBERNETES_POD_NAME", None, env)
    ),
    "KUBERNETES_POD_UID": lambda env: trim_str(getenv("KUBERNETES_POD_UID", None, env)),
    "KUBERNETES_POD_IP": lambda env: trim_str(getenv("KUBERNETES_POD_IP", None, env)),
    "KUBERNETES_NODE_NAME": lambda env: trim_str(
        getenv("KUBERNETES_NODE_NAME", None, env)
    ),
    "ASCEND_RT_VISIBLE_DEVICES": lambda env: (
        trim_str(getenv("ASCEND_RT_VISIBLE_DEVICES", "", env)) or ""
    ),
    "NIXL_ETCD_ENDPOINTS": lambda env: trim_str(
        getenv("NIXL_ETCD_ENDPOINTS", None, env)
    ),
    "NIXL_TCPSTORE_ENDPOINT": lambda env: trim_str(
        getenv("NIXL_TCPSTORE_ENDPOINT", None, env)
    ),
}


class Envs:
    """Singleton-style environment accessor backed by os.environ or a mapping."""

    _instance: Envs | None = None

    def __new__(cls, env: Mapping[str, str] | None = None) -> Envs:
        if env is None or env is os.environ:
            if cls._instance is None:
                cls._instance = super().__new__(cls)
                cls._instance._env = os.environ
            return cls._instance
        instance = super().__new__(cls)
        instance._env = env
        return instance

    def get(self, key: str, default: Any = None) -> Any:
        return self._env.get(key, default)

    def setdefault(self, key: str, value: str) -> str:
        if hasattr(self._env, "setdefault"):
            return self._env.setdefault(key, value)
        raise TypeError("underlying environment mapping is read-only")

    def __contains__(self, key: object) -> bool:
        return key in self._env

    def __getitem__(self, key: str) -> str:
        return self._env[key]

    def __getattr__(self, name: str) -> Any:
        if name in _VARIABLES:
            return _VARIABLES[name](self._env)
        msg = f"{type(self).__name__!r} object has no attribute {name!r}"
        raise AttributeError(msg)

    def __dir__(self) -> list[str]:
        return sorted(set(super().__dir__()) | set(_VARIABLES.keys()))


ENVS = Envs()


def get_envs(env: Mapping[str, str] | Envs | None = None) -> Envs:
    """Return the singleton ENVS for process env, or a bound Envs view for a custom mapping."""
    if isinstance(env, Envs):
        return env
    return Envs(env)


def __getattr__(name: str) -> Any:
    if name in _VARIABLES:
        return getattr(ENVS, name)
    msg = f"module {__name__!r} has no attribute {name!r}"
    raise AttributeError(msg)


def __dir__() -> list[str]:
    return list(_VARIABLES.keys())


__all__ = [
    "ENVS",
    "Envs",
    "choice",
    "get_envs",
    "getenv",
    "resolve_nixl_backend",
    "setdefault",
    "to_bool",
    "to_float",
    "to_int",
    "trim_str",
]
