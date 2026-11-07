"""Make this directory's and the backend tests' helper modules importable."""

import sys
from pathlib import Path

_HERE = str(Path(__file__).resolve().parent)
_SIBLING = str(Path(__file__).resolve().parents[1] / "hixl")
for entry in (_HERE, _SIBLING):
    if entry not in sys.path:
        sys.path.insert(0, entry)
