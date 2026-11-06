"""The injected fake HIXL runtime: the seam's reference implementation.

Programmable outcomes with exact faults, and — the lesson from the NIXL
reviews — real payloads: a successful WRITE or READ lands the source
file's actual slice into the registered window, so a reader test can
digest what it served against the manifest. Every fault mode exists so a
probe can FAIL, not only pass.
"""

from dataclasses import dataclass, field

from gpustack_model_transfer.backends.hixl.errors import TransferRefusedError
from gpustack_model_transfer.backends.hixl.runtime import TransferOutcome

SUBMIT_OK = "ok"
SUBMIT_REFUSE_PRE = "refuse-pre-submission"
SUBMIT_TIMEOUT_PRE = "timeout-pre-submission"
SUBMIT_NONTERMINAL = "nonterminal"

RAW_BACKEND_ENFORCED = "backend"
RAW_WRAPPER_ENFORCED = "wrapper"
RAW_MUTATES = "mutate"
RAW_LANDS_READ = "exfiltrate"
RAW_DENIES_AND_MUTATES = "deny-and-mutate"
RAW_NONE = "none"


@dataclass(frozen=True)
class AttemptResult:
    """One raw access attempt's observable outcome."""

    denied: bool
    enforcement: str = ""
    detail: str = ""
    landed: bool = False


@dataclass(frozen=True)
class DisclosureRecord:
    """One record connection setup or rendezvous would emit."""

    name: str
    scope: str
    content: dict
    is_sensitive = staticmethod(lambda key: key in ("rkey", "addr", "mr_key"))


@dataclass
class FakeAgent:
    name: str
    settings: dict
    submissions: list = field(default_factory=list)
    # What a binding that answers transport options would echo; the
    # reviewed binding answers nothing, which is the default here.
    applied_transport_options: tuple = ()


@dataclass
class FakeRegistration:
    buffers: dict
    revoked: bool = False


@dataclass
class FakeConnection:
    peer: dict


class FakeHixlAPI:
    """A programmable HixlAPI for tests, honest about its faults."""

    def __init__(
        self,
        *,
        version: str = "0.0.1",
        submit_mode: str = SUBMIT_OK,
        raw_mode: str = RAW_NONE,
        source_window_mode: str | None = None,
        disclosure_mode: str = "clean",
        device_memory_bytes_value: int = 0,
        agent_applied_transport_options: tuple = (),
        connect_error: Exception | None = None,
        register_error: Exception | None = None,
    ):
        self.version = version
        self.submit_mode = submit_mode
        self.raw_mode = raw_mode
        # The source-window write has its own knob so a test can refuse an
        # out-of-registration address and still let a peer change the
        # registered window. None follows raw_mode.
        self.source_window_mode = source_window_mode
        self.disclosure_mode = disclosure_mode
        self._device_memory = device_memory_bytes_value
        self._agent_applied_options = tuple(agent_applied_transport_options)
        self._connect_error = connect_error
        self._register_error = register_error
        # The remote memory the source side registered, keyed by the base
        # address it published: a successful transfer lands the region's
        # real bytes into the registered window, exactly as an
        # address-based transfer would.
        self.remote_regions: dict[int, bytes] = {}
        self.agents: list[FakeAgent] = []
        self.registrations: list[FakeRegistration] = []
        self.connections: list[FakeConnection] = []
        self.disconnects = 0
        self.deregistrations = 0
        self.operations: dict[str, str] = {}
        self._submissions: dict[str, dict] = {}
        self._next_operation = 0

    def publish_remote_region(self, addr: int, data: bytes) -> None:
        """The source side registers its checkpoint bytes at an address."""
        self.remote_regions[int(addr)] = bytes(data)

    # -- the agent, memory, and connection surface ----------------------

    def create_agent(self, name: str, settings: dict) -> FakeAgent:
        agent = FakeAgent(
            name=name,
            settings=dict(settings),
            applied_transport_options=self._agent_applied_options,
        )
        self.agents.append(agent)
        return agent

    def register_memory(self, agent, buffers: dict) -> FakeRegistration:
        if self._register_error is not None:
            raise self._register_error
        registration = FakeRegistration(buffers=dict(buffers))
        self.registrations.append(registration)
        return registration

    def connect(self, agent, peer: dict) -> FakeConnection:
        if self._connect_error is not None:
            raise self._connect_error
        connection = FakeConnection(peer=dict(peer))
        self.connections.append(connection)
        return connection

    def disconnect(self, agent, connection) -> None:
        self.disconnects += 1
        if connection in self.connections:
            self.connections.remove(connection)

    def deregister(self, agent, registration) -> None:
        registration.revoked = True
        self.deregistrations += 1

    # -- the transfer surface --------------------------------------------

    def _submit(
        self, direction, agent, registration, connection, window_name, source, length
    ) -> str:
        if self.submit_mode in (SUBMIT_REFUSE_PRE, SUBMIT_TIMEOUT_PRE):
            raise TransferRefusedError(
                f"the fake backend {self.submit_mode.replace('-', ' ')} "
                f"on the {direction} submission"
            )
        self._next_operation += 1
        operation_id = f"op-{self._next_operation}"
        self.operations[operation_id] = self.submit_mode
        submission = {
            "direction": direction,
            "window": window_name,
            "source": dict(source),
            "length": length,
            "registration": registration,
        }
        self._submissions[operation_id] = submission
        agent.submissions.append(dict(submission))
        return operation_id

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

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        state = self.operations.get(operation_id, "pending")
        if state == SUBMIT_NONTERMINAL:
            return TransferOutcome(state="pending", detail="still in flight")
        if state == SUBMIT_OK:
            self._land_payload(operation_id)
            return TransferOutcome(state="succeeded", detail="ok")
        return TransferOutcome(state="failed", detail=state)

    def fence(self, operation_id: str, state: str) -> None:
        """A qualified fence resolves one submitted operation out of band."""
        if operation_id not in self.operations:
            raise KeyError(operation_id)
        self.operations[operation_id] = state

    def _land_payload(self, operation_id: str) -> None:
        """Land the transfer's real bytes into its registered window.

        The slice comes from the remote region containing the submitted
        address; without any published region a deterministic non-zero
        pattern lands. The landing writes exactly length bytes and
        nothing beyond, so a later, smaller window read never sees a
        stale tail.
        """
        submission = self._submissions[operation_id]
        window = submission["registration"].buffers[submission["window"]]
        length = submission["length"]
        payload = self._region_slice(submission["source"], length)
        if payload is None:
            marker = int(operation_id.rsplit("-", 1)[1])
            payload = bytes((marker * 5 + index) % 251 for index in range(length))
        window[:length] = payload

    def _region_slice(self, source: dict, length: int) -> bytes | None:
        """The real bytes at the submitted remote address, if published."""
        addr = int(source["addr"])
        for base, data in self.remote_regions.items():
            if base <= addr < base + len(data):
                offset = addr - base
                return data[offset : offset + length]
        return None

    def device_memory_bytes(self, agent) -> int:
        return self._device_memory

    def describe_connection(self, connection) -> list:
        if self.disclosure_mode == "disclose":
            return [
                DisclosureRecord(
                    name="rendezvous",
                    scope="wire",
                    content={"handle": "ok", "mr_key": "RAW-KEY-7"},
                )
            ]
        return [
            DisclosureRecord(name="rendezvous", scope="wire", content={"handle": "ok"}),
            DisclosureRecord(
                name="local-keys", scope="internal", content={"mr_key": "RAW-KEY-7"}
            ),
        ]

    # -- the raw escape hatches the safety probes drive -------------------

    def _attempt(self) -> AttemptResult:
        if self.raw_mode == RAW_BACKEND_ENFORCED:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the raw access",
            )
        if self.raw_mode == RAW_WRAPPER_ENFORCED:
            return AttemptResult(
                denied=True,
                enforcement=RAW_WRAPPER_ENFORCED,
                detail="only the fake wrapper refused the raw access",
            )
        return AttemptResult(denied=False, detail="the attempt ran")

    def _alias_target(self, registration) -> FakeRegistration | None:
        """The live registration a stale handle's address would alias.

        A revoked registration's memory may have been re-registered (the
        repair): an unsafe backend that ignores revocation lets the
        stale handle reach the new generation's memory. The newest live
        registration is that target; none exists means nothing to alias.
        """
        if not getattr(registration, "revoked", False):
            return registration
        live = [reg for reg in self.registrations if not reg.revoked]
        return live[-1] if live else None

    def _write_attempt(self, target, index: int, landed_detail: str) -> AttemptResult:
        """The outcome of a raw WRITE, by the fake's mode.

        RAW_DENIES_AND_MUTATES is the case a refusal must not hide: the
        boundary refuses the attempt and writes the source anyway.
        """
        if self.raw_mode == RAW_DENIES_AND_MUTATES:
            if target is not None:
                buffer = next(iter(target.buffers.values()))
                buffer[index] ^= 0xFF
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the raw access",
                landed=True,
            )
        result = self._attempt()
        if result.denied or self.raw_mode == RAW_NONE:
            return result
        if self.raw_mode == RAW_MUTATES and target is not None:
            buffer = next(iter(target.buffers.values()))
            buffer[index] ^= 0xFF
            return AttemptResult(denied=False, detail=landed_detail, landed=True)
        return result

    def raw_write_out_of_bounds(self, agent, registration) -> AttemptResult:
        target = self._alias_target(registration)
        if getattr(registration, "revoked", False) and target is None:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the revoked handle",
            )
        return self._write_attempt(target, 0, "the raw write landed")

    def raw_read_out_of_bounds(self, agent, registration) -> AttemptResult:
        target = self._alias_target(registration)
        if getattr(registration, "revoked", False) and target is None:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the revoked handle",
            )
        if self.raw_mode == RAW_DENIES_AND_MUTATES:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the raw access",
                landed=True,
            )
        result = self._attempt()
        if result.denied or self.raw_mode == RAW_NONE:
            return result
        if self.raw_mode in (RAW_MUTATES, RAW_LANDS_READ) and target is not None:
            return AttemptResult(
                denied=False,
                detail="out-of-bounds source bytes were exfiltrated",
                landed=True,
            )
        return result

    def raw_atomic_on_source(self, agent, registration) -> AttemptResult:
        target = self._alias_target(registration)
        if getattr(registration, "revoked", False) and target is None:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the revoked handle",
            )
        return self._write_attempt(target, -1, "the atomic landed")

    def raw_write_in_source_window(self, agent, registration) -> AttemptResult:
        """A peer's WRITE into the registered source window.

        The out-of-bounds hooks ask whether the boundary refuses an
        address outside the registration. This one asks whether a peer
        can change the source weights inside the window the source
        published. source_window_mode controls it independently of
        raw_mode.
        """
        mode = (
            self.raw_mode
            if self.source_window_mode is None
            else self.source_window_mode
        )
        if mode == RAW_BACKEND_ENFORCED:
            return AttemptResult(
                denied=True,
                enforcement=RAW_BACKEND_ENFORCED,
                detail="the fake backend refused the write into the source window",
            )
        if mode == RAW_WRAPPER_ENFORCED:
            return AttemptResult(
                denied=True,
                enforcement=RAW_WRAPPER_ENFORCED,
                detail="only the fake wrapper refused the write into the source window",
            )
        if mode in (RAW_MUTATES, RAW_DENIES_AND_MUTATES):
            buffer = next(iter(registration.buffers.values()))
            buffer[0] ^= 0xFF
            if mode == RAW_DENIES_AND_MUTATES:
                return AttemptResult(
                    denied=True,
                    enforcement=RAW_BACKEND_ENFORCED,
                    detail="the fake backend refused the write into the source window",
                    landed=True,
                )
            return AttemptResult(
                denied=False,
                detail="the write into the source window landed",
                landed=True,
            )
        return AttemptResult(denied=False, detail="the attempt ran")


__all__ = [
    "RAW_BACKEND_ENFORCED",
    "RAW_DENIES_AND_MUTATES",
    "RAW_LANDS_READ",
    "RAW_MUTATES",
    "RAW_NONE",
    "RAW_WRAPPER_ENFORCED",
    "SUBMIT_NONTERMINAL",
    "SUBMIT_OK",
    "SUBMIT_REFUSE_PRE",
    "SUBMIT_TIMEOUT_PRE",
    "AttemptResult",
    "DisclosureRecord",
    "FakeAgent",
    "FakeConnection",
    "FakeHixlAPI",
    "FakeRegistration",
]
