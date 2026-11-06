"""Typed errors with stable codes.

Every failure class a caller must distinguish has its own class and code, so
authentication failure, denied content, stale generation, capacity
exhaustion, and source unavailability never collapse into one message.
"""


class ModelTransferError(Exception):
    """Base class of every contract error."""

    code = "model-transfer"

    def __init__(self, message: str):
        super().__init__(message)
        self.message = message


class ContractVersionError(ModelTransferError):
    code = "contract-version"


class IdentityError(ModelTransferError):
    code = "identity"


class InvalidDescriptorError(ModelTransferError):
    code = "invalid-descriptor"


class RangeOverflowError(ModelTransferError):
    code = "range-overflow"


class AuthenticationError(ModelTransferError):
    code = "authentication"


class DeniedContentError(ModelTransferError):
    """A request the source is not authorized to serve.

    The local reader never raises this; the broker authorization path owns it.
    The contract defines it now so wire errors stay distinguishable.
    """

    code = "denied-content"


class StaleGenerationError(ModelTransferError):
    code = "stale-generation"


class CapacityExhaustedError(ModelTransferError):
    code = "capacity-exhausted"


class SourceUnavailableError(ModelTransferError):
    code = "source-unavailable"


class OperationStateError(ModelTransferError):
    code = "operation-state"


class AmbiguousReadError(ModelTransferError):
    """A submitted read whose backend state was not terminal at its deadline.

    The operation stays retained as UNKNOWN until a qualified fence resolves
    it; this error tells the caller that neither success nor failure is known.
    """

    code = "ambiguous-read"


class BudgetError(ModelTransferError):
    code = "budget"


def error_code(exc: BaseException) -> str:
    """Return the stable code of a contract error, or "internal"."""
    if isinstance(exc, ModelTransferError):
        return exc.code
    return "internal"
