"""Session ordering and capabilities: the CANN route's own contract.

The reviewed HIXL documentation is explicit about the safe order —
register memory before connecting, and disconnect before deregistering —
and reports a FabricMem relaxation whose safety this route has not
proven. The session here enforces the conservative order by default and
accepts the relaxed one only through an explicit, evidence-carrying
opt-in.

The capability record keeps three transport answers apart: what was
requested, what the binding applied, and what was independently observed
as the effective backend. The reviewed Python API rejected explicit
transport options — the successful PoC case ran without them — so a
record whose options were all rejected and whose effective backend nobody
observed stays functional_only: it moved bytes, and it does not claim the
RDMA path.
"""

from dataclasses import dataclass, field

from gpustack_model_transfer.backends.hixl.errors import (
    SessionOrderError,
    SourceNpuContextRequiredError,
    TransportOptionRejectedError,
)

# The documented session phases, in their enforced order.
PHASE_CREATED = "created"
PHASE_REGISTERED = "registered"
PHASE_CONNECTED = "connected"
PHASE_DISCONNECTED = "disconnected"
PHASE_DEREGISTERED = "deregistered"

_ORDER = (
    (PHASE_CREATED, PHASE_REGISTERED),
    (PHASE_REGISTERED, PHASE_CONNECTED),
    (PHASE_CONNECTED, PHASE_DISCONNECTED),
    (PHASE_DISCONNECTED, PHASE_DEREGISTERED),
)


@dataclass(frozen=True)
class FabricMemRelaxation:
    """The explicit, evidence-carrying opt-in to the relaxed fabric mode.

    evidence names what proved the relaxation safe on this tuple (a
    qualified fence record, a backend admission); without it the session
    keeps the documented conservative order.
    """

    evidence: str

    def validate(self) -> None:
        if not isinstance(self.evidence, str) or not self.evidence.strip():
            raise SessionOrderError(
                "the relaxed fabric mode needs the evidence that proved it "
                "safe on this tuple; an unevidenced flag is exactly the "
                "ordering the documentation warns about"
            )

    def __post_init__(self) -> None:
        self.validate()


class HixlSession:
    """One agent session held to its documented order.

    register before connect, disconnect before deregister: the calls the
    reviewed documentation requires for a safe teardown. The relaxed
    order is a constructor opt-in with its evidence, never a default.
    """

    def __init__(self, agent, relaxation: FabricMemRelaxation | None = None):
        self.agent = agent
        self._relaxed = relaxation
        if relaxation is not None:
            relaxation.validate()
        self.phase = PHASE_CREATED
        self.registrations: list = []
        self.connections: list = []

    def register(self, registration) -> None:
        """Accept a registration; only registrations may precede connect,
        and a fully torn-down session may register again (renewal)."""
        self._require(
            PHASE_CREATED,
            PHASE_REGISTERED,
            PHASE_DEREGISTERED,
            action="register",
        )
        self.registrations.append(registration)
        self.phase = PHASE_REGISTERED

    def connect(self, connection) -> None:
        """Connect a peer; every memory must already be registered, and a
        connected session may connect further peers."""
        self._require(PHASE_REGISTERED, PHASE_CONNECTED, action="connect")
        if not self.registrations:
            raise SessionOrderError(
                "connect needs at least one registration: the documented "
                "order is register, connect, transfer, disconnect, deregister"
            )
        self.connections.append(connection)
        self.phase = PHASE_CONNECTED

    def disconnect(self, connection) -> None:
        """Disconnect one connection; every connection must close before a
        conservative deregistration. A relaxed session whose last
        registration left while connections remain stays connected, so
        those connections can still close after it."""
        self._require(PHASE_CONNECTED, PHASE_DISCONNECTED, action="disconnect")
        if connection not in self.connections:
            raise SessionOrderError("the connection is not this session's")
        self.connections.remove(connection)
        if not self.connections:
            self.phase = (
                PHASE_DEREGISTERED if not self.registrations else PHASE_DISCONNECTED
            )

    def deregister(self, registration) -> None:
        """Deregister memory; every connection must already be closed,
        unless the relaxed fabric mode carries its proof. The phase
        reaches 'deregistered' only when nothing remains on either side:
        a relaxed deregistration with connections still open leaves the
        session connected so those connections can close after it."""
        if self.connections and self._relaxed is None:
            raise SessionOrderError(
                "disconnect before deregister: the documented order, "
                "relaxed only with proven evidence for this backend"
            )
        if registration not in self.registrations:
            raise SessionOrderError("the registration is not this session's")
        self.registrations.remove(registration)
        if not self.registrations and not self.connections:
            self.phase = PHASE_DEREGISTERED

    def _require(self, *allowed: str, action: str) -> None:
        if self.phase not in allowed:
            raise SessionOrderError(
                f"{action} is out of order from phase {self.phase!r}: the "
                "documented order is register, connect, transfer, "
                "disconnect, deregister"
            )


@dataclass(frozen=True)
class TransportNegotiation:
    """What was asked of the transport, what was applied, what was seen.

    requested_transport_options is every option the route asked for;
    applied_transport_options is what the binding accepted;
    effective_backend is the independently observed backend, "unknown"
    until someone observed it. An option the binding rejected is carried
    in rejected with its reason — reported, never silently dropped.
    """

    requested_transport_options: tuple
    applied_transport_options: tuple
    effective_backend: str
    effective_observed: bool
    rejected: tuple = ()

    def validate(self) -> None:
        for name in self.applied_transport_options:
            if name not in self.requested_transport_options:
                raise TransportOptionRejectedError(
                    f"applied option {name!r} was never requested"
                )
        for item in self.rejected:
            if item.option not in self.requested_transport_options:
                raise TransportOptionRejectedError(
                    f"rejected option {item.option!r} was never requested"
                )
            if item.option in self.applied_transport_options:
                raise TransportOptionRejectedError(
                    f"option {item.option!r} is both applied and rejected"
                )
        if self.effective_observed and (
            not self.effective_backend or self.effective_backend == "unknown"
        ):
            raise TransportOptionRejectedError(
                "an observed effective backend must be named"
            )
        if not self.effective_observed and self.effective_backend != "unknown":
            raise TransportOptionRejectedError(
                "an unobserved effective backend must stay unknown"
            )

    @property
    def functional_only(self) -> bool:
        """True when the required path could not be established: bytes may
        have moved, and the record does not claim the RDMA path."""
        if self.effective_observed and self.rejected:
            return True
        return not self.effective_observed


@dataclass(frozen=True)
class HixlCapabilities:
    """The CANN route's capability record."""

    negotiation: TransportNegotiation
    # The measured PoC initialized an NPU context on the source host: an
    # accelerator-free source stays unqualified, and the record says so.
    requires_source_npu_context: bool = True
    window_bytes: int = 1 << 20
    max_inflight: int = 1
    fabric_mem_relaxed: bool = False

    def __post_init__(self) -> None:
        self.validate()

    def validate(self) -> None:
        self.negotiation.validate()
        if not self.requires_source_npu_context:
            # The measured source initialized an NPU context; no
            # accelerator-free alternative is qualified, so a record
            # claiming one is refusing the route's own contract.
            raise SourceNpuContextRequiredError(
                "the CANN route has no qualified accelerator-free source: "
                "a capability record cannot drop the source NPU context "
                "requirement"
            )
        if self.window_bytes > 1 << 30:
            raise ValueError("the window ceiling is 1 GiB")
        if not 1 <= self.max_inflight <= 2:
            raise ValueError("at most two operations may be in flight")


def negotiate_transport(
    requested: tuple,
    binding_answers: dict,
) -> TransportNegotiation:
    """Build a negotiation record from the binding's raw answers.

    binding_answers maps each requested option to either True (applied)
    or a rejection reason string. Every requested option must be answered;
    an unanswered option is a rejection with an explicit reason, because
    silence about a transport option is how a missing RDMA path gets
    called one.
    """
    applied = []
    rejected = []
    for option in requested:
        answer = binding_answers.get(option, "the binding did not answer")
        if answer is True:
            applied.append(option)
        else:
            rejected.append(RejectedTransportOption(option=option, reason=str(answer)))
    return TransportNegotiation(
        requested_transport_options=tuple(requested),
        applied_transport_options=tuple(applied),
        effective_backend="unknown",
        effective_observed=False,
        rejected=tuple(rejected),
    )


@dataclass(frozen=True)
class RejectedTransportOption:
    """One requested transport option the binding refused, with why."""

    option: str
    reason: str

    def __post_init__(self) -> None:
        if not self.option or not self.reason:
            raise TransportOptionRejectedError(
                "a rejected option carries its name and its reason"
            )


@dataclass
class RecoveryCapability:
    """The stale-replay recovery standing, disabled until proven.

    The replay scenario that timed out before payload submission proved
    nothing about reclamation; a safe reclaim rule is a separate, explicit
    proof. Recording one is not enough on its own: the rule needs a
    qualified fence that retains both endpoints until it resolves the
    retained operation, and until such a harness substantiates it,
    recovery stays disabled and managed CANN promotion stays blocked.
    """

    enabled: bool = False
    proof: str = ""
    blockers: list = field(default_factory=list)

    def record_blocker(self, blocker: str) -> None:
        if blocker not in self.blockers:
            self.blockers.append(blocker)

    def enable(self, proof: str) -> None:
        if not proof or not proof.strip():
            raise ValueError("enabling recovery needs the reclaim proof")
        if self.blockers:
            # A reclaim rule cannot stand on an unproven or failed
            # boundary; enabling over blockers is refused outright.
            raise ValueError(
                "recovery cannot be enabled over blockers: " + "; ".join(self.blockers)
            )
        self.proof = proof
        self.enabled = True

    @property
    def status(self) -> str:
        if self.enabled:
            return f"enabled: {self.proof}"
        if self.blockers:
            return "disabled: " + "; ".join(self.blockers)
        return "disabled: no safe reclaim rule has been proven"


__all__ = [
    "PHASE_CONNECTED",
    "PHASE_CREATED",
    "PHASE_DEREGISTERED",
    "PHASE_DISCONNECTED",
    "PHASE_REGISTERED",
    "FabricMemRelaxation",
    "HixlCapabilities",
    "HixlSession",
    "RecoveryCapability",
    "RejectedTransportOption",
    "TransportNegotiation",
    "negotiate_transport",
]
