"""Report model for acceptance results: PASS, NO-GO, or UNRUN per capability."""

from dataclasses import dataclass

PASS = "PASS"
NO_GO = "NO-GO"
UNRUN = "UNRUN"


@dataclass
class CheckResult:
    """One capability's verdict with a short human-readable detail."""

    capability: str
    status: str  # PASS, NO-GO, UNRUN
    detail: str


__all__ = ["NO_GO", "PASS", "UNRUN", "CheckResult"]
