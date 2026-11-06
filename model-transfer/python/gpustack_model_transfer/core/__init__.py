"""Transport-independent reader contract core.

The modules here define the versioned contract shared with the Go wire types
in pkg/modeltransfer/protocol: identity, descriptors, budgets, operations,
capabilities, and the local reference reader.
"""

from gpustack_model_transfer.core import errors

__all__ = ["errors"]
