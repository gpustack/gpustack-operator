"""Explicit qualified endpoint selection, per-agent isolation, memlock.

The route refuses to guess a network. A selection is valid only when the
control path and every payload rail are named explicitly and qualified:
wildcards and UCX_NET_DEVICES=all reach every plane a host carries —
compute, storage, management — and a wrong rail is not a performance
problem but a reachability and isolation one.

The backend never writes a process-wide UCX_* variable. The selection is
turned into a NEW settings dict handed to one agent; a live engine context
in the same process keeps its own settings untouched. The memory lock is
checked against the registered budgets before any registration, because a
registration that cannot be pinned is a failure at transfer time, not a
discovery.
"""

import re
from dataclasses import dataclass

from gpustack_model_transfer.backends.nixl.errors import (
    MemlockInsufficientError,
    UnqualifiedEndpointError,
)

# An explicit rail: an HCA name with an optional port, "mlx5_0:1". A bare
# "all", "any", a glob, or an empty name is a wildcard, not a rail.
_RAIL_PATTERN = re.compile(r"^[A-Za-z0-9_.-]+:\d+$|^[A-Za-z0-9_.-]+$")

# The one process-wide setting this route refuses outright.
_NET_DEVICES_VARIABLE = "UCX_NET_DEVICES"

# The variables that carry an agent's own UCX selection.
_AGENT_SETTING_KEYS = ("UCX_TLS", "UCX_NET_DEVICES", "UCX_SAFE_COPY")

UNLIMITED = -1


@dataclass(frozen=True)
class EndpointSelection:
    """One qualified selection: a control path and payload rails."""

    control: str
    payload_rails: tuple
    direction: str = "write"

    def validate(self) -> None:
        if not self.control:
            raise UnqualifiedEndpointError(
                "the control path is required: an authenticated, reachable "
                "IP endpoint for manifests, identity, and leases"
            )
        if not self.payload_rails:
            raise UnqualifiedEndpointError(
                "at least one payload rail is required: granted devices "
                "named explicitly, never discovered"
            )
        for rail in self.payload_rails:
            _require_rail(rail)
        if self.direction not in ("read", "write"):
            raise UnqualifiedEndpointError(
                f"unknown transfer direction {self.direction!r}"
            )

    def agent_settings(self) -> dict:
        """The per-agent UCX settings for this selection.

        A new dictionary for one agent: the process environment and every
        other live context keep their own settings.
        """
        self.validate()
        return {
            "UCX_TLS": "rc",
            _NET_DEVICES_VARIABLE: ",".join(self.payload_rails),
        }


def qualify_endpoints(
    control: str,
    payload_rails,
    env: dict,
    *,
    direction: str = "write",
    live_contexts=None,
) -> EndpointSelection:
    """Validate one selection against the environment it will run in.

    env is the process environment as a plain mapping (read-only use); an
    inherited UCX_NET_DEVICES wildcard is refused, because an agent created inside
    that process may inherit it. live_contexts, when given, counts other
    engine or transport contexts alive in the process; with any, a
    selection that would need process-wide changes is refused rather than
    disturb them.
    """
    raw = str(env.get(_NET_DEVICES_VARIABLE, "")).strip()
    if raw:
        for rail in raw.split(","):
            _require_rail(rail.strip())
    if live_contexts is not None and live_contexts() > 0:
        for key in _AGENT_SETTING_KEYS:
            if key in env and key != _NET_DEVICES_VARIABLE:
                # A live context owns its own settings; a selection that
                # would have to change them is refused, not applied.
                raise UnqualifiedEndpointError(
                    f"{key} is already set for a live engine context; use "
                    "per-agent selection instead"
                )
    selection = EndpointSelection(
        control=control,
        payload_rails=tuple(payload_rails or ()),
        direction=direction,
    )
    selection.validate()
    return selection


@dataclass(frozen=True)
class MemlockCheck:
    """The service memory lock against the registered budgets."""

    soft_limit: int
    hard_limit: int
    required_bytes: int
    sufficient: bool

    @property
    def unlimited(self) -> bool:
        return self.soft_limit == UNLIMITED


def check_memlock(
    required_bytes: int,
    getrlimit=None,
    resource_module=None,
) -> MemlockCheck:
    """Check RLIMIT_MEMLOCK against the bytes registration will pin.

    getrlimit is injectable for tests; the default reads the real limit.
    A lock that cannot cover the source and window budgets is a typed
    refusal before any registration, never a failure mid-transfer.
    """
    if getrlimit is None:
        resource = resource_module or _load_resource()
        getrlimit = resource.getrlimit
    soft, hard = getrlimit(_RLIMIT_MEMLOCK(resource_module))
    soft = _normalize(soft)
    hard = _normalize(hard)
    sufficient = soft == UNLIMITED or soft >= required_bytes
    return MemlockCheck(
        soft_limit=soft,
        hard_limit=hard,
        required_bytes=required_bytes,
        sufficient=sufficient,
    )


def require_memlock(
    required_bytes: int, soft_limit: int | None = None, **kwargs
) -> MemlockCheck:
    """The memlock check as a refusal: raise when insufficient.

    soft_limit, when given, stands in for the host limit: a caller that
    has already read it (or a test) gates on that number without touching
    the real resource module.
    """
    if soft_limit is not None:
        check = MemlockCheck(
            soft_limit=soft_limit,
            hard_limit=soft_limit,
            required_bytes=required_bytes,
            sufficient=soft_limit == UNLIMITED or soft_limit >= required_bytes,
        )
    else:
        check = check_memlock(required_bytes, **kwargs)
    if not check.sufficient:
        raise MemlockInsufficientError(
            f"RLIMIT_MEMLOCK soft limit {check.soft_limit} cannot cover the "
            f"registered budget {required_bytes} bytes; raise the service "
            "memory lock before registering"
        )
    return check


def _require_rail(rail: str) -> None:
    if not isinstance(rail, str) or not _RAIL_PATTERN.match(rail):
        raise UnqualifiedEndpointError(
            f"payload rail {rail!r} is not an explicit qualified endpoint; "
            "a wildcard or unnamed device is not a selection"
        )
    if rail.lower() in ("all", "any"):
        raise UnqualifiedEndpointError(
            f"payload rail {rail!r} is a wildcard, not a granted rail"
        )


def _load_resource():
    import resource

    return resource


def _RLIMIT_MEMLOCK(resource_module=None):
    resource = resource_module or _load_resource()
    return resource.RLIMIT_MEMLOCK


def _normalize(value) -> int:
    # resource.RLIM_INFINITY is -1 on Linux; treat any negative as such.
    if isinstance(value, int) and value < 0:
        return UNLIMITED
    return int(value)


__all__ = [
    "UNLIMITED",
    "EndpointSelection",
    "MemlockCheck",
    "check_memlock",
    "qualify_endpoints",
    "require_memlock",
]
