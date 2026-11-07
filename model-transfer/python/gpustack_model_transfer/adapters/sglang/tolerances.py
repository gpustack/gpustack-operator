"""Predeclared numerical tolerances, declared before any comparison runs.

A tolerance that appears after the comparison is a rationalization, not a
tolerance. Every comparison in this adapter names its ToleranceSpec first;
the defaults pin parameter values to exactness and forward outputs to the
small slack a different summation order may need.
"""

import math
import struct
from dataclasses import dataclass


@dataclass(frozen=True)
class ToleranceSpec:
    """Relative and absolute tolerance for an elementwise comparison."""

    rtol: float
    atol: float

    def validate(self) -> None:
        for name, value in (("rtol", self.rtol), ("atol", self.atol)):
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise ValueError(f"tolerance {name} must be a number")
            if math.isnan(float(value)) or math.isinf(float(value)):
                raise ValueError(f"tolerance {name} must be finite")
            if value < 0:
                raise ValueError(f"tolerance {name} must be non-negative")

    def within(self, loaded, expected) -> bool:
        """Return whether loaded matches expected within this tolerance.

        Elementwise; the sequences must have the same length. A NaN matches
        only the identical bit pattern, because a NaN equals nothing else
        and a comparison that waved NaNs through would wave corrupted
        payloads through too.
        """
        if len(loaded) != len(expected):
            return False
        rtol = float(self.rtol)
        atol = float(self.atol)
        for got, want in zip(loaded, expected, strict=False):
            if math.isnan(got) or math.isnan(want):
                if struct.pack("<d", got) != struct.pack("<d", want):
                    return False
                continue
            if math.isinf(got) or math.isinf(want):
                if got != want:
                    return False
                continue
            if math.isclose(got, want, rel_tol=rtol, abs_tol=atol):
                continue
            return False
        return True


# Parameter values are verified bytes handed to the engine untouched: the
# only honest tolerance is exactness.
DEFAULT_PARAMETER_TOLERANCE = ToleranceSpec(rtol=0.0, atol=0.0)

# Forward outputs run the same bytes through the same arithmetic, but a
# different engine build may reorder a reduction, so a small slack is
# declared up front.
DEFAULT_FORWARD_TOLERANCE = ToleranceSpec(rtol=1e-5, atol=1e-6)


__all__ = [
    "DEFAULT_FORWARD_TOLERANCE",
    "DEFAULT_PARAMETER_TOLERANCE",
    "ToleranceSpec",
]
