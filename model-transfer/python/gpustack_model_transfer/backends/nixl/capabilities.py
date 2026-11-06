"""NIXL capabilities: the negotiation record, plus what is NIXL's own.

The core BackendCapabilities carries the requested/applied/effective
distinction the reader contract demands. This module wraps it with the
facts that are the NIXL route's own: the transfer direction (the measured
source-initiated WRITE is a CANDIDATE, never an immutability answer) and
the rail pairs that were individually qualified.

Multi-rail support is advertised pair by pair or not at all: a qualified
(source rail, target rail) pair is one that was probed end to end. Port
counters attribute traffic to a selected port; they never prove another
pair reachable, and nothing here infers reachability or performance from
counter deltas, ordinal names, LIDs, or PCIe proximity.
"""

from dataclasses import dataclass, field

from gpustack_model_transfer.backends.nixl.errors import UnqualifiedEndpointError
from gpustack_model_transfer.core.capabilities import (
    OWNERSHIP_OWNED,
    BackendCapabilities,
    TransportSelection,
)

DIRECTION_WRITE = "write"
DIRECTION_READ = "read"
_KNOWN_DIRECTIONS = (DIRECTION_WRITE, DIRECTION_READ)


@dataclass(frozen=True)
class QualifiedRailPair:
    """One rail pair qualified by an explicit end-to-end probe.

    counters, when present, are attribution evidence for the probe's own
    traffic. They are never reachability evidence for any other pair.
    """

    source_rail: str
    target_rail: str
    probe_passed: bool
    counters: tuple = ()

    def validate(self) -> None:
        if not self.source_rail or not self.target_rail:
            raise UnqualifiedEndpointError("a qualified pair names both rails")


@dataclass(frozen=True)
class NixlCapabilities:
    """The NIXL route's capability record.

    base is the core record the reader session validates; direction and
    qualified_rail_pairs are the route's own facts. The initial direction
    is the measured source-initiated WRITE — a candidate the safety gate
    must still qualify, never an immutability guarantee; protected pull is
    the alternative, only after its own proof.
    """

    base: BackendCapabilities
    direction: str = DIRECTION_WRITE
    qualified_rail_pairs: tuple = ()
    window_bytes: int = 0
    max_inflight: int = 1
    memlock_soft_limit: int = 0

    def validate(self) -> None:
        self.base.validate()
        if self.direction not in _KNOWN_DIRECTIONS:
            raise UnqualifiedEndpointError(
                f"unknown transfer direction {self.direction!r}"
            )
        if not self.qualified_rail_pairs:
            raise UnqualifiedEndpointError(
                "no rail pair is qualified: a record with no qualified "
                "payload path advertises nothing"
            )
        for pair in self.qualified_rail_pairs:
            pair.validate()
            if not pair.probe_passed:
                raise UnqualifiedEndpointError(
                    f"rail pair {pair.source_rail}->{pair.target_rail} was "
                    "not qualified by a passing probe; an unqualified pair "
                    "is not advertised"
                )


def nixl_backend_capabilities(
    requested: tuple,
    applied: str,
    effective: str,
    effective_observed: bool,
    rejected: tuple = (),
    *,
    direction: str = DIRECTION_WRITE,
    qualified_rail_pairs: tuple = (),
    window_bytes: int = 1 << 20,
    max_inflight: int = 2,
    memlock_soft_limit: int = 0,
    control: str = "tcp://source:9100",
) -> NixlCapabilities:
    """Build the route's record from a negotiation's raw answers.

    Every rejected option is reported, never silently dropped; an
    effective backend nobody observed stays "unknown"; the transport
    selection carries the control path and the qualified rails separately.
    """
    rejected_options = tuple(rejected)
    base = BackendCapabilities(
        requested=tuple(requested),
        applied=applied,
        effective=effective,
        effective_observed=effective_observed,
        rejected=rejected_options,
        formats=("safetensors",),
        memory_types=("host", "device"),
        max_request_bytes=window_bytes,
        alignment_bytes=1,
        tensor_ownership=OWNERSHIP_OWNED,
        device_visibility=("host", "device"),
        transport=TransportSelection(
            control=control,
            payload=tuple(
                f"{pair.source_rail}->{pair.target_rail}"
                for pair in qualified_rail_pairs
            ),
        ),
    )
    record = NixlCapabilities(
        base=base,
        direction=direction,
        qualified_rail_pairs=tuple(qualified_rail_pairs),
        window_bytes=window_bytes,
        max_inflight=max_inflight,
        memlock_soft_limit=memlock_soft_limit,
    )
    record.validate()
    return record


@dataclass
class RailQualifier:
    """Qualify rail pairs one explicit probe at a time.

    A pair enters the advertised set only through probe_pair returning a
    passing outcome. Counters the probe collected are recorded beside the
    pair as attribution evidence for that probe alone.
    """

    _qualified: list = field(default_factory=list)

    def probe_pair(
        self, source_rail: str, target_rail: str, probe
    ) -> QualifiedRailPair:
        """Run one end-to-end probe through the caller's callable.

        probe is called with (source_rail, target_rail) and returns
        (passed: bool, counters: tuple). Nothing here infers a pass: an
        exception from the probe is a failed probe, not a skipped one.
        """
        try:
            passed, counters = probe(source_rail, target_rail)
        except Exception as exc:
            passed, counters = False, (f"probe error: {exc}",)
        pair = QualifiedRailPair(
            source_rail=source_rail,
            target_rail=target_rail,
            probe_passed=bool(passed),
            counters=tuple(counters or ()),
        )
        pair.validate()
        self._qualified.append(pair)
        return pair

    def qualified_pairs(self) -> tuple:
        return tuple(pair for pair in self._qualified if pair.probe_passed)


__all__ = [
    "DIRECTION_READ",
    "DIRECTION_WRITE",
    "NixlCapabilities",
    "QualifiedRailPair",
    "RailQualifier",
    "nixl_backend_capabilities",
]
