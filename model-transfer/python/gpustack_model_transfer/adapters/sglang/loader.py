"""The native SGLang load path over the local reader.

One load is: build and preflight the plan (so an over-budget refusal
reads nothing), probe metadata (payload-free), open the reader session,
assemble tensors through bounded windows with exact byte counts, store into
owned (or contracted borrowed) parameter storage, finalize the model
exactly once, and release every load-only resource before the engine's
KV profiling runs. Any failure after the model was built leaves it
marked mutated: a partially loaded process is not silently reusable.

The reader is the loader's for the duration of the load: every refusal
path, before and after the session opens, closes it.
"""

import logging
from dataclasses import dataclass, field

from gpustack_model_transfer.adapters.sglang.metadata import probe_checkpoint_metadata
from gpustack_model_transfer.adapters.sglang.ownership import (
    LoadOptions,
    TensorOwnership,
)
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
    first parameter exists, so a budget refusal costs nothing. With a
    shard assignment the entries carry the rank's sliced views, and the
    owned-output budget bounds one rank's storage, not the whole tensor.
    """

    entries: tuple
    total_parameter_bytes: int
    tied_aliases: tuple = ()


def build_load_plan(
    manifest,
    budgets,
    capabilities,
    tie_word_embeddings: bool = False,
    shard=None,
) -> LoadPlan:
    """Map the manifest onto the model and preflight every tensor.

    The owned-output budget is a per-tensor ceiling, separate from the
    window budget: a tensor larger than one window loads through several,
    and a tensor — or, sharded, one rank's share of a tensor — larger
    than the owned budget refuses the load outright, before any read and
    before any parameter is mutated.

    Tied embeddings never double-count: lm_head.weight is an alias of the
    embedding's storage, present or omitted in the checkpoint alike.
    """
    names = {tensor.name for tensor in manifest.tensors}
    head, embed = "lm_head.weight", "model.embed_tokens.weight"
    tied_aliases = ()
    if tie_word_embeddings and embed in names:
        # A checkpoint that omits the head is complete when tied: the
        # alias supplies it from the embedding.
        tied_aliases = ((head, embed),)

    views = shard.views if shard is not None else None
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
        view = tensor if views is None else views[tensor.name]
        byte_size = view.byte_size
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
            LoadPlanEntry(tensor=view, parameter=tensor.name, role=_ROLE_PARAMETER)
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
    partial_shard_reads: tuple = ()
    shard: object | None = None
    payload_bytes_read: int = 0


def profile_kv_capacity(driver, model, ledger: ResourceLedger) -> int:
    """Run the engine's KV-capacity profiling on a released state.

    Refuses while any load-only byte is still held: capacity profiled over
    staging memory would understate the KV the engine can actually use.
    Refuses a session still open and a model not finalized exactly once,
    and refuses a second profile for the same load.
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


class ModelTransferSGLangLoader:
    """The pinned loader an engine image registers for load_format "model-transfer"."""

    def __init__(self, reader, capabilities, budgets):
        self._reader = reader
        self._capabilities = capabilities
        self._budgets = budgets
        # A failed close remains accessible for an explicit fence and retry.
        self.retained_session = None
        self.last_model = None

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

    def load(
        self,
        driver,
        options: LoadOptions = None,
        shard=None,
        finalize: bool = True,
    ) -> LoadReport:
        """Load the verified artifact into the driver's model.

        Takes ownership of the reader for the duration: on success and on
        every cleanly classified failure — including the preflight and
        probe refusals that happen before a session exists — the reader is
        closed; a failure that retained UNKNOWN operations leaves the
        session for the caller's qualified fence, because closing over a
        retained operation is exactly what the contract refuses.

        shard, from a sharding plan, loads only this rank's share; the
        spans that do not cover a whole tensor are recorded as partial,
        because the reader contract verifies whole tensors only.
        finalize=False withholds the engine's finalization, so a load
        group can finalize every rank together or none at all.
        """
        if self.retained_session is not None:
            raise OperationStateError("a previous load session still needs cleanup")
        session = None
        model = None
        self.last_model = None
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

            # Preflight every tensor budget first, so an over-budget
            # refusal makes zero reads; then run the payload-free header
            # probe before the model's first parameter exists.
            plan = build_load_plan(
                self._reader.manifest,
                self._budgets,
                self._capabilities,
                tie_word_embeddings=options.tie_word_embeddings,
                shard=shard,
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
            partial = []
            payload_bytes_read = 0
            engine_metadata = {}
            model = driver.build_model(plan, options)
            tied_targets = {target for _, target in plan.tied_aliases}
            self.last_model = model
            for entry in plan.entries:
                if entry.role == _ROLE_TIED_ALIAS:
                    # The alias IS the embedding's storage: the head's own
                    # bytes are neither read nor staged nor counted, they
                    # would only be discarded. A borrowed contract still
                    # fences and releases the alias like any parameter.
                    if options.ownership is TensorOwnership.BORROWED:
                        options.borrowed.visibility_fence(entry.tensor.name)
                        options.borrowed.release(entry.tensor.name)
                        fallbacks.append(entry.tensor.name)
                    loaded.append(entry.tensor.name)
                    continue
                payload, complete = self._read_entry(session, entry, shard)
                if entry.role != _ROLE_ENGINE_METADATA:
                    # The traffic metric counts weight bytes; engine
                    # metadata is a handful of scalars, not traffic.
                    payload_bytes_read += len(payload)
                ledger.staging(len(payload))
                if entry.role == _ROLE_ENGINE_METADATA:
                    driver.store_engine_metadata(model, entry, payload)
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
                            model, entry, payload, ownership=ownership
                        )
                        options.borrowed.visibility_fence(entry.tensor.name)
                        released = options.borrowed.release(entry.tensor.name)
                        if tied:
                            outcome = "fell-back-to-owned"
                        elif outcome == "fell-back-to-owned" or not released:
                            driver.copy_to_owned(model, entry, payload)
                            outcome = "fell-back-to-owned"
                    else:
                        outcome = driver.store(
                            model, entry, payload, ownership=options.ownership
                        )
                    if outcome == "fell-back-to-owned":
                        fallbacks.append(entry.tensor.name)
                    loaded.append(entry.tensor.name)
                    ledger.retained_parameter_bytes += entry.tensor.byte_size
                if not complete:
                    partial.append(entry.tensor.name)
                ledger.release_load_only()

            if finalize:
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
            partial_shard_reads=tuple(sorted(set(partial))),
            shard=shard,
            payload_bytes_read=payload_bytes_read,
        )

    def _read_entry(self, session, entry, shard):
        """Read a whole tensor or the explicitly selected shard ranges.

        Whole-tensor reads must report complete coverage before mutation.
        Partial shard ranges remain explicitly recorded in the load report.
        """
        if shard is None or shard.slices[entry.tensor.name].covers_all:
            output = session.read_tensor(entry.tensor.name)
            if not output.complete:
                raise InvalidDescriptorError(
                    f"tensor {entry.tensor.name!r} was served without "
                    f"the complete byte range"
                )
            return output.payload, True
        chunks = []
        for offset, length in shard.slices[entry.tensor.name].segments:
            output = session.read_range(entry.tensor.name, offset, length)
            chunks.append(output.payload)
        return b"".join(chunks), False


__all__ = [
    "LoadPlan",
    "LoadPlanEntry",
    "LoadReport",
    "ModelTransferSGLangLoader",
    "ResourceLedger",
    "build_load_plan",
    "profile_kv_capacity",
]
