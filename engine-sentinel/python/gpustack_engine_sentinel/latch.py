"""Boot-local fail-closed readiness latch and pause state for engine processes."""

from __future__ import annotations

import os
import threading
import uuid
from dataclasses import dataclass

from gpustack_engine_sentinel.protocol import (
    ENV_REQUIRE_ACK,
    BootIDMismatchError,
    PauseResponse,
    ReadyResponse,
)


@dataclass(frozen=True)
class LatchSnapshot:
    boot_id: str
    acked: bool
    paused: bool
    observed_generation: int | None
    applied_config_digest: str
    last_operation_id: str


class BootLatch:
    """Tracks the current process boot_id, fail-closed readiness ack, and ASGI pause state."""

    def __init__(
        self,
        *,
        boot_id: str | None = None,
        require_ack: bool | None = None,
    ) -> None:
        self._lock = threading.Lock()
        self._boot_id = boot_id or str(uuid.uuid4())
        if require_ack is None:
            require_ack = os.environ.get(ENV_REQUIRE_ACK, "1").strip() != "0"
        self._require_ack = require_ack
        self._acked = not require_ack
        self._paused = False
        self._observed_generation: int | None = 0 if not require_ack else None
        self._applied_config_digest: str = ""
        self._last_operation_id: str = ""

    @property
    def boot_id(self) -> str:
        return self._boot_id

    @property
    def require_ack(self) -> bool:
        return self._require_ack

    @property
    def paused(self) -> bool:
        with self._lock:
            return self._paused

    def snapshot(self) -> LatchSnapshot:
        with self._lock:
            return LatchSnapshot(
                boot_id=self._boot_id,
                acked=self._acked,
                paused=self._paused,
                observed_generation=self._observed_generation,
                applied_config_digest=self._applied_config_digest,
                last_operation_id=self._last_operation_id,
            )

    def check_expected_boot_id(self, expected_boot_id: str) -> None:
        if expected_boot_id != self._boot_id:
            raise BootIDMismatchError(
                current_boot_id=self._boot_id,
                expected_boot_id=expected_boot_id,
            )

    def set_paused(
        self,
        paused: bool,
        *,
        expected_boot_id: str | None = None,
    ) -> PauseResponse:
        with self._lock:
            if expected_boot_id is not None and expected_boot_id != self._boot_id:
                raise BootIDMismatchError(
                    current_boot_id=self._boot_id,
                    expected_boot_id=expected_boot_id,
                )
            self._paused = paused
            return PauseResponse(paused=self._paused, boot_id=self._boot_id)

    def record_apply(
        self,
        *,
        operation_id: str,
        generation: int,
        applied_config_digest: str,
    ) -> LatchSnapshot:
        with self._lock:
            self._last_operation_id = operation_id
            self._applied_config_digest = applied_config_digest
            if self._acked:
                self._observed_generation = generation
            return LatchSnapshot(
                boot_id=self._boot_id,
                acked=self._acked,
                paused=self._paused,
                observed_generation=self._observed_generation,
                applied_config_digest=self._applied_config_digest,
                last_operation_id=self._last_operation_id,
            )

    def ack(
        self,
        *,
        boot_id: str,
        observed_generation: int,
        applied_config_digest: str,
    ) -> ReadyResponse:
        with self._lock:
            if boot_id != self._boot_id:
                raise BootIDMismatchError(
                    current_boot_id=self._boot_id,
                    expected_boot_id=boot_id,
                )
            self._acked = True
            self._observed_generation = observed_generation
            self._applied_config_digest = applied_config_digest
            return ReadyResponse(
                ready=True,
                boot_id=self._boot_id,
                observed_generation=self._observed_generation,
                applied_config_digest=self._applied_config_digest,
            )
