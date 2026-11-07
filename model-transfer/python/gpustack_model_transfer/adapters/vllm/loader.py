"""CPU reference load path over the local reader, not native vLLM.

The separate native module registers the real engine loader. This fixture's
model, finalization counter, and KV profiling do not establish engine behavior.
"""

import logging
from dataclasses import dataclass, field

from gpustack_model_transfer.adapters.vllm.metadata import probe_checkpoint_metadata
from gpustack_model_transfer.adapters.vllm.ownership import LoadOptions, TensorOwnership
from gpustack_model_transfer.core.capabilities import OWNERSHIP_BORROWED
from gpustack_model_transfer.core.errors import (
    CapacityExhaustedError,
    InvalidDescriptorError,
    OperationStateError,
)
from gpustack_model_transfer.core.session import ReaderSession

_LOG = logging.getLogger(__name__)

_ROLE_PARAMETER = "parameter"
_ROLE_TIED_ALIAS = "tied-alias"
_ROLE_ENGINE_METADATA = "engine-metadata"


@dataclass(frozen=True)
class LoadPlanEntry:
    """One tensor's job in the load: which parameter, in which role."""

    tensor: object
    parameter: str
    role: str


@dataclass(frozen=True)
class LoadPlan:
    """The whole load, preflighted: entries plus the alias pairs.

    The plan is built and checked before any read and before the model's
    first parameter exists, so a budget refusal costs nothing.
    """

    entries: tuple
    total_parameter_bytes: int
    tied_aliases: tuple = ()


def build_load_plan(
    manifest, budgets, capabilities, tie_word_embeddings: bool = False
) -> LoadPlan:
    """Map the manifest onto the model and preflight every tensor.

    The owned-output budget is a per-tensor ceiling, separate from the
    window budget: a tensor larger than one window loads through several,
    and a tensor larger than the owned budget refuses the load outright,
    before any read and before any parameter is mutated.
    """
    names = {tensor.name for tensor in manifest.tensors}
    head, embed = "lm_head.weight", "model.embed_tokens.weight"
    tied_aliases = ()
    if tie_word_embeddings and embed in names:
        tied_aliases = ((head, embed),)

    entries = []
    total = 0
    for tensor in manifest.tensors:
        if tied_aliases and tensor.name == head:
            entries.append(
                LoadPlanEntry(
                    tensor=tensor, parameter=tensor.name, role=_ROLE_TIED_ALIAS
                )
            )
            continue
        byte_size = tensor.byte_size
        if byte_size > budgets.owned_output_bytes:
            raise CapacityExhaustedError(
                f"tensor {tensor.name!r} needs {byte_size} bytes above the "
                f"owned-output budget {budgets.owned_output_bytes}"
            )
        if byte_size > capabilities.max_request_bytes:
            raise CapacityExhaustedError(
                f"tensor {tensor.name!r} needs {byte_size} bytes above "
                f"backend max_request_bytes "
                f"{capabilities.max_request_bytes}"
            )
        if tensor.name.startswith("metadata."):
            entries.append(
                LoadPlanEntry(tensor=tensor, parameter="", role=_ROLE_ENGINE_METADATA)
            )
            continue
        entries.append(
            LoadPlanEntry(tensor=tensor, parameter=tensor.name, role=_ROLE_PARAMETER)
        )
        total += byte_size

    return LoadPlan(
        entries=tuple(entries), total_parameter_bytes=total, tied_aliases=tied_aliases
    )


@dataclass
class ResourceLedger:
    """What the load held, so KV profiling starts from a known state.

    load_only_bytes are staging bytes held for the transfer path alone:
    nonzero only while a tensor is being assembled, and zero again once the
    load released them. retained_parameter_bytes are the model's own
    resident weights, which stay through inference. kv_profiled records
    that the capacity answer was already taken for this load.
    """

    load_only_bytes: int = 0
    peak_staging_bytes: int = 0
    retained_parameter_bytes: int = 0
    kv_profiled: bool = False
    session_closed: bool = True

    def staging(self, byte_count: int) -> None:
        self.load_only_bytes = byte_count
        self.peak_staging_bytes = max(self.peak_staging_bytes, byte_count)

    def release_load_only(self) -> None:
        self.load_only_bytes = 0


@dataclass
class LoadReport:
    """What one load did, for the caller and for the acceptance suite."""

    model: object
    plan: LoadPlan
    ledger: ResourceLedger
    finalization_count: int
    loaded_parameters: tuple
    tied_aliases: tuple
    engine_metadata: dict = field(default_factory=dict)
    borrowed_fallbacks: tuple = ()
    session_released: bool = False
    manifest_consumed: bool = False
    engine_config: dict | None = None


def profile_kv_capacity(driver, model, ledger: ResourceLedger) -> int:
    """Run the engine's KV-capacity profiling on a released state.

    Refuses while any load-only byte is still held: capacity profiled over
    staging memory would understate the KV the engine can actually use.
    Refuses a second profile for the same load: the answer is taken once.
    """
    if ledger.load_only_bytes:
        raise OperationStateError(
            f"release load-only resources before KV profiling: "
            f"{ledger.load_only_bytes} bytes still held"
        )
    if not ledger.session_closed:
        raise OperationStateError("release reader session before KV profiling")
    if getattr(model, "finalization_count", None) != 1:
        raise OperationStateError(
            "model must be finalized exactly once before KV profiling"
        )
    if ledger.kv_profiled:
        raise OperationStateError("KV capacity was already profiled for this load")
    ledger.kv_profiled = True
    return driver.profile_kv_capacity(model)


class ModelTransferModelLoader:
    """The pinned loader an engine image registers for load_format "model-transfer"."""

    def __init__(self, reader, capabilities, budgets):
        self._reader = reader
        self._capabilities = capabilities
        self._budgets = budgets
        # A failed close remains accessible for an explicit fence and retry.
        self.retained_session = None

    def close(self) -> None:
        """Retry cleanup after the caller resolves any retained operations.

        This never supplies a fence or releases UNKNOWN ownership. A failed
        close keeps the session reachable for another cleanup attempt. Use a
        new reader and loader for another model load after cleanup succeeds.
        """
        if self.retained_session is not None:
            self.retained_session.close()
            self.retained_session = None
        else:
            self._close_reader()

    def _close_reader(self) -> None:
        close = getattr(self._reader, "close", None)
        if callable(close):
            close()

    def load(self, driver, options: LoadOptions = None) -> LoadReport:
        """Load the verified artifact into the driver's model.

        Takes ownership of the reader for the duration: on success and on
        cleanly classified failure the reader is closed; a failure that
        retained UNKNOWN operations leaves the session for the caller's
        qualified fence, because closing over a retained operation is
        exactly what the contract refuses.
        """
        if self.retained_session is not None:
            raise OperationStateError("a previous load session still needs cleanup")
        session = None
        model = None
        ledger = ResourceLedger(session_closed=False)
        try:
            options = options or LoadOptions()
            options.validate()
            if options.ownership is TensorOwnership.BORROWED:
                if self._capabilities.tensor_ownership != OWNERSHIP_BORROWED:
                    raise InvalidDescriptorError(
                        "borrowed storage was requested but the backend "
                        "capabilities do not declare borrowed support"
                    )
                if options.borrowed is None:
                    raise InvalidDescriptorError(
                        "borrowed storage needs an explicit async visibility "
                        "fence and release contract"
                    )

            # Preflight every tensor budget before reading even header bytes,
            # then run the metadata/header probe before building the model.
            plan = build_load_plan(
                self._reader.manifest,
                self._budgets,
                self._capabilities,
                tie_word_embeddings=options.tie_word_embeddings,
            )
            probe_checkpoint_metadata(
                self._reader,
                tie_word_embeddings=options.tie_word_embeddings,
                max_chunk_bytes=self._budgets.window_bytes,
            )

            session = ReaderSession(
                identity=self._reader.manifest.identity(),
                capabilities=self._capabilities,
                budgets=self._budgets,
                reader=self._reader,
            )
            loaded = []
            fallbacks = []
            engine_metadata = {}
            model = driver.build_model(plan, options)
            tied_targets = {target for _, target in plan.tied_aliases}
            for entry in plan.entries:
                if entry.role == _ROLE_TIED_ALIAS:
                    loaded.append(entry.tensor.name)
                    continue
                output = session.read_tensor(entry.tensor.name)
                if not output.complete:
                    raise InvalidDescriptorError(
                        f"tensor {entry.tensor.name!r} was served without "
                        f"the complete byte range"
                    )
                ledger.staging(len(output.payload))
                if entry.role == _ROLE_ENGINE_METADATA:
                    driver.store_engine_metadata(model, entry, output.payload)
                    engine_metadata[entry.tensor.name] = model.engine_metadata.get(
                        entry.tensor.name
                    )
                else:
                    if options.ownership is TensorOwnership.BORROWED:
                        # Tied aliases share one storage but may release at different
                        # times. Keep that storage owned for every consumer.
                        tied = entry.tensor.name in tied_targets
                        ownership = TensorOwnership.OWNED if tied else options.ownership
                        outcome = driver.store(
                            model, entry, output.payload, ownership=ownership
                        )
                        options.borrowed.visibility_fence(entry.tensor.name)
                        released = options.borrowed.release(entry.tensor.name)
                        if tied:
                            outcome = "fell-back-to-owned"
                        elif outcome == "fell-back-to-owned" or not released:
                            driver.copy_to_owned(model, entry, output.payload)
                            outcome = "fell-back-to-owned"
                    else:
                        outcome = driver.store(
                            model, entry, output.payload, ownership=options.ownership
                        )
                    if outcome == "fell-back-to-owned":
                        fallbacks.append(entry.tensor.name)
                    loaded.append(entry.tensor.name)
                    ledger.retained_parameter_bytes += entry.tensor.byte_size
                ledger.release_load_only()

            driver.finalize(model)
            session.close()
            session_released = True
            ledger.session_closed = True
        except BaseException:
            if model is not None:
                model.mutated = True
            if session is not None:
                try:
                    session.close()
                    ledger.session_closed = True
                except Exception as close_error:
                    self.retained_session = session
                    _LOG.warning(
                        "load session cleanup remains pending (%s)",
                        type(close_error).__name__,
                    )
            else:
                self._close_reader()
            raise

        return LoadReport(
            model=model,
            plan=plan,
            ledger=ledger,
            finalization_count=model.finalization_count,
            loaded_parameters=tuple(loaded),
            tied_aliases=tuple(alias for alias, _ in plan.tied_aliases),
            engine_metadata=engine_metadata,
            borrowed_fallbacks=tuple(fallbacks),
            session_released=session_released,
            manifest_consumed=len(loaded) + len(engine_metadata) == len(plan.entries),
            engine_config=options.engine_config,
        )


__all__ = [
    "LoadPlan",
    "LoadPlanEntry",
    "LoadReport",
    "ModelTransferModelLoader",
    "ResourceLedger",
    "build_load_plan",
    "profile_kv_capacity",
]
