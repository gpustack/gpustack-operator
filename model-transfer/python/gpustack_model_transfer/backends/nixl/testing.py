"""The injected fake NIXL runtime: the seam's reference implementation.

The fake implements the NixlAPI surface with programmable outcomes, so the
backend logic — windows, states, quarantine, safety probes — is tested on
a CPU-only host against a fake whose faults are exact. It is also a
specification: a real runtime wrapper must satisfy these semantics.

Every fault mode exists so a probe can FAIL, not only pass: a comparison
that cannot fail proves nothing. The "mutate" modes reproduce the earlier
NIXL raw-key probe that changed source bytes; a probe must call that
NO-GO. The "deny-and-mutate" mode is the case a refusal must not hide:
the boundary refuses the attempt and still changes the source.
"""

import json
from dataclasses import dataclass, field

from gpustack_model_transfer.backends.nixl.errors import (
    RegistrationExpiredError,
    TransferRefusedError,
)
from gpustack_model_transfer.backends.nixl.runtime import TransferOutcome

SUBMIT_OK = "ok"
SUBMIT_REFUSE_PRE = "refuse-pre-submission"
SUBMIT_NONTERMINAL = "nonterminal"

# What a raw access attempt meets at the boundary.
RAW_BACKEND_ENFORCED = "backend"
RAW_WRAPPER_ENFORCED = "wrapper"
RAW_MUTATES = "mutate"
RAW_LANDS_READ = "exfiltrate"
RAW_DENIES_AND_MUTATES = "deny-and-mutate"
RAW_NONE = "none"


@dataclass(frozen=True)
class AttemptResult:
    """One raw access attempt's observable outcome.

    denied and enforcement say whether a boundary refused the attempt and
    who. landed says the attempt took effect: bytes written out of bounds,
    or out-of-bounds bytes exfiltrated.
    """

    denied: bool
    enforcement: str = ""
    detail: str = ""
    landed: bool = False


@dataclass(frozen=True)
class DisclosureRecord:
    """One record connection setup or rendezvous would emit.

    scope "wire" is what crosses to the peer; "internal" stays inside the
    trusted process. is_sensitive decides whether a field is a raw key or
    pointer that must not cross.
    """

    name: str
    scope: str
    content: dict
    is_sensitive = staticmethod(lambda key: key in ("rkey", "addr"))


@dataclass
class FakeAgent:
    name: str
    settings: dict
    submissions: list = field(default_factory=list)


@dataclass
class FakeRegistration:
    buffers: dict
    addresses: dict = field(default_factory=dict)
    revoked: bool = False


class FakeNixlAPI:
    """A programmable NixlAPI for tests, honest about its faults."""

    def __init__(
        self,
        *,
        version: str = "1.5.0",
        submit_mode: str = SUBMIT_OK,
        raw_mode: str = RAW_NONE,
        source_window_mode: str | None = None,
        disclosure_mode: str = "clean",
        expire_registration_after: int | None = None,
        device_memory_bytes_value: int = 0,
        source_files: dict | None = None,
    ):
        self.version = version
        self.submit_mode = submit_mode
        self.raw_mode = raw_mode
        # The source-window write has its own knob so a test can refuse an
        # out-of-registration address and still let a peer change the
        # registered window. None follows raw_mode.
        self.source_window_mode = source_window_mode
        self.disclosure_mode = disclosure_mode
        self.expire_registration_after = expire_registration_after
        self._submissions_since_renewal = 0
        self._expired_once = False
        self._device_memory = device_memory_bytes_value
        # The source payloads a successful WRITE lands in its window, by
        # file path. Without a map, a deterministic non-zero pattern lands
        # so a window's contents are always observable.
        self.source_files = dict(source_files or {})
        self.agents: list[FakeAgent] = []
        self.registrations: list[FakeRegistration] = []
        self.registrations_by_agent: dict[str, FakeRegistration] = {}
        self.renewals = 0
        self.revocations = 0
        self.operations: dict[str, str] = {}
        self._next_operation = 0
        self._submissions: dict[str, dict] = {}
        self.released_operations: set = set()
        self.terminal_operations: set = set()
        # Peer plumbing for the source/target primitives: addresses the
        # fake hands out in agent_metadata, and what connect_peer
        # imported under each connection token.
        self._next_address = 0x100000
        self._address_index: dict[int, bytearray] = {}
        self._peers: dict[str, dict[int, bytearray]] = {}
        self._disconnected: set = set()

    # -- the agent and memory surface ----------------------------------

    def create_agent(self, name: str, settings: dict) -> FakeAgent:
        agent = FakeAgent(name=name, settings=dict(settings))
        self.agents.append(agent)
        return agent

    def register_memory(self, agent, buffers: dict) -> FakeRegistration:
        addresses = {}
        for name, buffer in buffers.items():
            self._next_address += 0x1000
            addresses[name] = self._next_address
            self._address_index[self._next_address] = buffer
        registration = FakeRegistration(buffers=dict(buffers), addresses=addresses)
        self.registrations.append(registration)
        self.registrations_by_agent[agent.name] = registration
        return registration

    def renew_registration(self, agent, registration) -> None:
        self.renewals += 1
        self._submissions_since_renewal = 0
        self._expired_once = False

    def revoke_registration(self, agent, registration) -> None:
        registration.revoked = True
        self.revocations += 1

    # -- the transfer surface -------------------------------------------

    def _gate_submission(self, registration) -> None:
        """The refusal and expiry gates every submit direction shares."""
        if self.submit_mode == SUBMIT_REFUSE_PRE:
            raise TransferRefusedError(
                "the fake backend refused the transfer before submission"
            )
        if (
            self.expire_registration_after is not None
            and self._submissions_since_renewal >= self.expire_registration_after
            and not self._expired_once
        ):
            self._expired_once = True
            raise RegistrationExpiredError("the fake registration expired; renew it")
        self._submissions_since_renewal += 1

    def submit_write(
        self, agent, registration, window_name: str, target: dict, length: int
    ) -> str:
        self._gate_submission(registration)
        self._next_operation += 1
        operation_id = f"op-{self._next_operation}"
        self.operations[operation_id] = self.submit_mode
        submission = {
            "direction": "consumer-write",
            "window": window_name,
            "target": dict(target),
            "length": length,
            "registration": registration,
        }
        self._submissions[operation_id] = submission
        agent.submissions.append(dict(submission))
        return operation_id

    def submit_source_write(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
        local_offset: int = 0,
    ) -> str:
        peer_buffer = self._resolve_peer_region(connection, remote, length)
        if window_name not in registration.buffers:
            raise ValueError(f"no registered window named {window_name!r}")
        if (
            isinstance(local_offset, bool)
            or not isinstance(local_offset, int)
            or local_offset < 0
        ):
            raise ValueError("invalid local source offset")
        if (
            local_offset > len(registration.buffers[window_name])
            or length > len(registration.buffers[window_name]) - local_offset
        ):
            raise ValueError(
                f"the transfer of {length} bytes exceeds window {window_name!r}"
            )
        self._gate_submission(registration)
        self._next_operation += 1
        operation_id = f"op-{self._next_operation}"
        self.operations[operation_id] = self.submit_mode
        submission = {
            "direction": "source-write",
            "window": window_name,
            "length": length,
            "registration": registration,
            "source": registration.buffers[window_name],
            "source_offset": local_offset,
            "destination": peer_buffer,
        }
        self._submissions[operation_id] = submission
        agent.submissions.append(
            {key: value for key, value in submission.items() if key != "registration"}
        )
        return operation_id

    def submit_target_read(
        self,
        agent,
        registration,
        connection,
        window_name: str,
        remote: dict,
        length: int,
    ) -> str:
        peer_buffer = self._resolve_peer_region(connection, remote, length)
        if window_name not in registration.buffers:
            raise ValueError(f"no registered window named {window_name!r}")
        if length > len(registration.buffers[window_name]):
            raise ValueError(
                f"the transfer of {length} bytes exceeds window {window_name!r}"
            )
        self._gate_submission(registration)
        self._next_operation += 1
        operation_id = f"op-{self._next_operation}"
        self.operations[operation_id] = self.submit_mode
        submission = {
            "direction": "target-read",
            "window": window_name,
            "length": length,
            "registration": registration,
            "source": peer_buffer,
            "destination": registration.buffers[window_name],
        }
        self._submissions[operation_id] = submission
        agent.submissions.append(
            {key: value for key, value in submission.items() if key != "registration"}
        )
        return operation_id

    def agent_metadata(self, agent) -> bytes:
        """The fake's published metadata: the agent's name and its latest
        registration's window regions, shaped like the pinned release's
        descriptors (address, length, window label, memory class)."""
        registration = self.registrations_by_agent.get(agent.name)
        regions = [
            {
                "addr": registration.addresses[name],
                "len": len(registration.buffers[name]),
                "window": name,
                "mem_type": "DRAM",
            }
            for name in registration.buffers
        ]
        return json.dumps({"agent": agent.name, "regions": regions}).encode()

    def connect_peer(self, agent, metadata: bytes) -> str:
        """Import a peer's metadata bytes; the peer's own name is the
        connection token, exactly as the pinned add_remote_agent answers."""
        published = json.loads(metadata.decode())
        name = published["agent"]
        regions = {}
        for region in published["regions"]:
            buffer = self._address_index.get(region["addr"])
            regions[int(region["addr"])] = (buffer, int(region["len"]))
        self._peers[name] = regions
        self._disconnected.discard(name)
        return name

    def disconnect_peer(self, agent, connection) -> None:
        if connection not in self._peers:
            raise ValueError(f"no connected peer under {connection!r}")
        self._disconnected.add(connection)
        self._peers.pop(connection)

    def _resolve_peer_region(self, connection, remote: dict, length: int):
        """Resolve the peer's published descriptor to its buffer."""
        if connection in self._disconnected or connection not in self._peers:
            raise ValueError(f"no connected peer under {connection!r}")
        if not isinstance(remote, dict) or "addr" not in remote:
            raise ValueError(
                "the remote descriptor carries the peer's published "
                "'addr' and 'len', taken from its metadata"
            )
        entry = self._peers[connection].get(int(remote["addr"]))
        if entry is None:
            raise ValueError(
                "the remote address was not published by the connected peer"
            )
        buffer, region_len = entry
        if remote.get("mem_type") not in ("DRAM", "VRAM"):
            raise ValueError(
                "the remote descriptor's memory class comes from the "
                "peer's published descriptor, never a guess"
            )
        if int(remote.get("len", 0)) > region_len:
            raise ValueError("the descriptor exceeds the peer's published region")
        if not isinstance(length, int) or isinstance(length, bool) or length <= 0:
            raise ValueError(f"transfer length must be a positive int, got {length!r}")
        if length > region_len:
            raise ValueError(
                f"the transfer of {length} bytes exceeds the peer's "
                f"published region ({region_len} bytes)"
            )
        return buffer

    def release_operation(
        self, agent, operation_id: str, *, external_fence=None
    ) -> None:
        """The fence path, mirroring the real contract: a terminal
        operation's release is bookkeeping; a pending operation's
        release is a cancel, not a qualified fence, and needs external
        evidence. A double release is refused."""
        if operation_id not in self.operations:
            raise ValueError(f"unknown operation: {operation_id!r}")
        if operation_id in self.released_operations:
            raise ValueError("this operation's handle was already released")
        if operation_id not in self.terminal_operations and not (
            isinstance(external_fence, str) and external_fence.strip()
        ):
            from gpustack_model_transfer.backends.nixl.errors import FenceRequiredError

            raise FenceRequiredError(
                "the operation is not proven terminal; releasing it "
                "would cancel active work, which is not a qualified "
                "fence — external fence evidence is required"
            )
        self.released_operations.add(operation_id)
        if self.operations[operation_id] == SUBMIT_NONTERMINAL:
            self.operations[operation_id] = "failed"

    def poll(self, agent, operation_id: str) -> TransferOutcome:
        if operation_id in self.released_operations:
            raise ValueError(
                "this operation's handle was already released; there is "
                "no state left to observe"
            )
        state = self.operations.get(operation_id, "pending")
        if state == SUBMIT_NONTERMINAL:
            return TransferOutcome(state="pending", detail="still in flight")
        self.terminal_operations.add(operation_id)
        if state == SUBMIT_OK:
            self._land_payload(operation_id)
            return TransferOutcome(state="succeeded", detail="ok")
        return TransferOutcome(state="failed", detail=state)

    def _land_payload(self, operation_id: str) -> None:
        """Land a successful transfer's bytes where the direction says.

        A consumer WRITE lands the manifest slice in its registered
        window — from the bound source file map when one matches the
        submitted target, else a deterministic non-zero pattern distinct
        from any stale window tail. A source WRITE moves the source
        window's own bytes into the peer's buffer; a target READ moves
        the peer's bytes into the window. The landing writes exactly
        length bytes and nothing beyond.
        """
        submission = self._submissions[operation_id]
        direction = submission.get("direction", "consumer-write")
        length = submission["length"]
        if direction in ("source-write", "target-read"):
            offset = submission.get("source_offset", 0)
            payload = submission["source"][offset : offset + length]
            submission["destination"][:length] = payload
            return
        window = submission["registration"].buffers[submission["window"]]
        target = submission["target"]
        source = self.source_files.get(target.get("file"))
        if source is not None:
            offset = target.get("offset", 0)
            payload = source[offset : offset + length]
        else:
            marker = int(operation_id.rsplit("-", 1)[1])
            payload = bytes((marker * 7 + index) % 251 for index in range(length))
        window[:length] = payload

    def fence(self, operation_id: str, state: str) -> None:
        """A qualified fence resolves one submitted operation out of band."""
        if operation_id not in self.operations:
            raise KeyError(operation_id)
        self.operations[operation_id] = state

    def port_counters(self, agent, rail: str) -> dict:
        submissions = sum(len(a.submissions) for a in self.agents)
        return {"rail": rail, "port_xmit_data": submissions * 4096}

    def device_memory_bytes(self, agent) -> int:
        return self._device_memory

    # -- the raw escape hatches the safety probes drive -----------------

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

    def _write_attempt(
        self, registration, index: int, landed_detail: str
    ) -> AttemptResult:
        """The outcome of a raw WRITE, by the fake's mode.

        RAW_DENIES_AND_MUTATES is the case a refusal must not hide: the
        boundary refuses the attempt and writes the source anyway.
        """
        if self.raw_mode == RAW_DENIES_AND_MUTATES:
            buffer = next(iter(registration.buffers.values()))
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
        if self.raw_mode == RAW_MUTATES:
            buffer = next(iter(registration.buffers.values()))
            buffer[index] ^= 0xFF
            return AttemptResult(denied=False, detail=landed_detail, landed=True)
        return result

    def raw_write_out_of_bounds(self, agent, registration) -> AttemptResult:
        return self._write_attempt(registration, 0, "the raw write landed")

    def raw_read_out_of_bounds(self, agent, registration) -> AttemptResult:
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
        if self.raw_mode in (RAW_MUTATES, RAW_LANDS_READ):
            return AttemptResult(
                denied=False,
                detail="out-of-bounds source bytes were exfiltrated",
                landed=True,
            )
        return result

    def raw_atomic_on_source(self, agent, registration) -> AttemptResult:
        return self._write_attempt(registration, -1, "the atomic landed")

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

    def describe_connection(self, registration) -> list:
        """The records connection setup and rendezvous would emit."""
        if self.disclosure_mode == "disclose":
            return [
                DisclosureRecord(
                    name="rendezvous",
                    scope="wire",
                    content={"handle": "ok", "rkey": "RAW-KEY-1"},
                )
            ]
        return [
            DisclosureRecord(name="rendezvous", scope="wire", content={"handle": "ok"}),
            DisclosureRecord(
                name="local-keys",
                scope="internal",
                content={"rkey": "RAW-KEY-1"},
            ),
        ]


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
    "AttemptResult",
    "DisclosureRecord",
    "FakeAgent",
    "FakeNixlAPI",
    "FakeRegistration",
]
