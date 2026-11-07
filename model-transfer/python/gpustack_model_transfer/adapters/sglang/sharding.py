"""Tensor-parallel sharding plans and the load-group lifecycle.

A sharding plan maps every manifest tensor onto world_size ranks with a
deterministic, rank-consistent strategy: column-parallel weights split
their output dimension into contiguous per-rank byte spans, row-parallel
weights split their input dimension into per-row spans, and everything
else — norms, biases, embeddings on this route — is replicated whole.

load_group drives one rank per loader through the plan and owns the group
lifecycle: every rank loads with finalization withheld, the group
finalizes each rank exactly once only when every rank loaded, and a rank
that fails after partial initialization taints the ranks that already
built a model, none of which finalize. The group is bounded: ranks load
in rank order, a failure stops the group at once, and nothing retries.

Traffic amplification is measured, never assumed: the report carries the
payload bytes every rank read and the checkpoint's unique parameter
bytes, and their ratio says what the group actually cost.
"""

from dataclasses import dataclass

from gpustack_model_transfer.adapters.sglang.loader import ModelTransferSGLangLoader
from gpustack_model_transfer.core.descriptors import DTYPE_SIZES

STRATEGY_COLUMN = "column"
STRATEGY_ROW = "row"
STRATEGY_REPLICATE = "replicate"

# The projection-name conventions this route shards by, the same split
# points tensor-parallel engines use for their projector weights.
_COLUMN_MARKERS = ("q_proj", "k_proj", "v_proj", "gate_proj", "up_proj")
_ROW_MARKERS = ("o_proj", "down_proj")


def classify(name: str, shape) -> str:
    """Return a tensor's sharding strategy for this route.

    One-dimensional tensors replicate: norms and biases have no dimension
    to split. Projector weights follow the engine's own convention; the
    rest replicates.
    """
    if len(shape) < 2:
        return STRATEGY_REPLICATE
    for marker in _COLUMN_MARKERS:
        if marker in name:
            return STRATEGY_COLUMN
    for marker in _ROW_MARKERS:
        if marker in name:
            return STRATEGY_ROW
    return STRATEGY_REPLICATE


def _split(length: int, world_size: int):
    """Split one dimension across ranks: earlier ranks take the remainder."""
    base, remainder = divmod(length, world_size)
    spans = []
    begin = 0
    for rank in range(world_size):
        count = base + (1 if rank < remainder else 0)
        spans.append((begin, count))
        begin += count
    return spans


@dataclass(frozen=True)
class ShardView:
    """One rank's storage view of one tensor: the sliced shape and size."""

    name: str
    dtype: str
    shape: tuple
    byte_size: int


@dataclass(frozen=True)
class ShardSlice:
    """One rank's share of one tensor's bytes.

    segments are (offset, length) byte spans inside the tensor, in order.
    covers_all is True only for a whole-tensor share, because only a whole
    tensor can be digest-verified by the reader contract; a shard's
    integrity rides the manifest and header cross-checks plus the source
    digests a later wire addition carries.
    """

    name: str
    segments: tuple
    covers_all: bool


@dataclass(frozen=True)
class ShardAssignment:
    """One rank's complete assignment: views for storage, slices for reads."""

    rank: int
    world_size: int
    views: dict
    slices: dict


class ShardingPlan:
    """The deterministic, rank-consistent plan for one manifest."""

    def __init__(self, tensors, strategies, assignments):
        self._tensors = tuple(tensors)
        self.strategies = dict(strategies)
        self._assignments = assignments

    @property
    def tensors(self):
        return self._tensors

    def assignment(self, rank: int) -> ShardAssignment:
        return self._assignments[rank]

    @property
    def sharded_names(self) -> tuple:
        return tuple(
            tensor.name
            for tensor in self._tensors
            if self.strategies[tensor.name] != STRATEGY_REPLICATE
        )

    @property
    def replicated_names(self) -> tuple:
        return tuple(
            tensor.name
            for tensor in self._tensors
            if self.strategies[tensor.name] == STRATEGY_REPLICATE
        )


def build_sharding_plan(manifest, world_size: int) -> ShardingPlan:
    """Plan one manifest's tensors across world_size ranks."""
    if (
        not isinstance(world_size, int)
        or isinstance(world_size, bool)
        or world_size < 1
    ):
        raise ValueError(f"world_size must be a positive integer, got {world_size!r}")
    strategies = {}
    views = {rank: {} for rank in range(world_size)}
    slices = {rank: {} for rank in range(world_size)}
    for tensor in manifest.tensors:
        strategy = classify(tensor.name, tensor.shape)
        strategies[tensor.name] = strategy
        element = DTYPE_SIZES[tensor.dtype]
        if strategy == STRATEGY_REPLICATE or world_size == 1:
            for rank in range(world_size):
                views[rank][tensor.name] = ShardView(
                    name=tensor.name,
                    dtype=tensor.dtype,
                    shape=tuple(tensor.shape),
                    byte_size=tensor.byte_size,
                )
                slices[rank][tensor.name] = ShardSlice(
                    name=tensor.name,
                    segments=((0, tensor.byte_size),) if tensor.byte_size else (),
                    covers_all=True,
                )
            continue
        if strategy == STRATEGY_COLUMN:
            row_stride = 1
            for dim in tensor.shape[1:]:
                row_stride *= dim
            row_bytes = row_stride * element
            for rank, (begin, count) in enumerate(_split(tensor.shape[0], world_size)):
                views[rank][tensor.name] = ShardView(
                    name=tensor.name,
                    dtype=tensor.dtype,
                    shape=(count, *tensor.shape[1:]),
                    byte_size=count * row_bytes,
                )
                slices[rank][tensor.name] = ShardSlice(
                    name=tensor.name,
                    segments=((begin * row_bytes, count * row_bytes),) if count else (),
                    covers_all=False,
                )
            continue
        # Row-parallel: split the input dimension; every leading-dim row
        # contributes one segment per rank, strided over any trailing
        # dimensions the tensor carries, which the shard keeps.
        columns = tensor.shape[1]
        trailing = 1
        for dim in tensor.shape[2:]:
            trailing *= dim
        for rank, (begin, count) in enumerate(_split(columns, world_size)):
            shard_elements = count * trailing
            row_elements = columns * trailing
            views[rank][tensor.name] = ShardView(
                name=tensor.name,
                dtype=tensor.dtype,
                shape=(tensor.shape[0], count, *tensor.shape[2:]),
                byte_size=tensor.shape[0] * shard_elements * element,
            )
            segments = tuple(
                (
                    (row * row_elements + begin * trailing) * element,
                    shard_elements * element,
                )
                for row in range(tensor.shape[0])
                if count
            )
            slices[rank][tensor.name] = ShardSlice(
                name=tensor.name, segments=segments, covers_all=False
            )
    assignments = {
        rank: ShardAssignment(
            rank=rank,
            world_size=world_size,
            views=views[rank],
            slices=slices[rank],
        )
        for rank in range(world_size)
    }
    return ShardingPlan(manifest.tensors, strategies, assignments)


@dataclass
class LoadGroupReport:
    """What one load group did, and what it cost."""

    world_size: int
    plan: ShardingPlan
    rank_reports: tuple
    bytes_read_across_ranks: int = 0
    unique_checkpoint_parameter_bytes: int = 0
    group_state: str = "loaded"
    tainted_ranks: tuple = ()
    failure: str = ""

    @property
    def tainted(self) -> bool:
        return bool(self.tainted_ranks)

    @property
    def traffic_amplification(self) -> float:
        if self.unique_checkpoint_parameter_bytes == 0:
            return 0.0
        return self.bytes_read_across_ranks / self.unique_checkpoint_parameter_bytes

    def rank_payload(self, rank: int, name: str) -> bytes:
        report = self.rank_reports[rank]
        if report.model is None or name not in report.model.parameters:
            return b""
        return bytes(report.model.parameters[name].data)


def load_group(
    reader_factory,
    driver_factory,
    options=None,
    world_size: int = 1,
    capabilities=None,
    budgets=None,
    raise_on_failure: bool = True,
) -> LoadGroupReport:
    """Load one checkpoint across world_size ranks as one bounded group.

    reader_factory returns a fresh reader per rank; driver_factory returns
    the driver for one rank, by rank number. Finalization is withheld
    until every rank has loaded, then runs exactly once per rank: a rank
    that fails after partial initialization taints the ranks that already
    built a model and the group reports failure, with nothing finalized
    and no retry.
    """
    from gpustack_model_transfer.adapters.sglang.ownership import LoadOptions
    from gpustack_model_transfer.core.budgets import ReadBudgets
    from gpustack_model_transfer.core.capabilities import local_backend_capabilities

    options = options or LoadOptions()
    capabilities = capabilities or local_backend_capabilities()
    budgets = budgets or ReadBudgets(
        window_bytes=1 << 20, max_inflight=1, owned_output_bytes=64 << 20
    )

    probe = reader_factory()
    try:
        plan = build_sharding_plan(probe.manifest, world_size)
    finally:
        close = getattr(probe, "close", None)
        if callable(close):
            close()

    unique = 0
    head = "lm_head.weight"
    for tensor in probe.manifest.tensors:
        if tensor.name.startswith("metadata."):
            continue
        if options.tie_word_embeddings and tensor.name == head:
            # The tied alias is the embedding's storage: counted once.
            continue
        unique += tensor.byte_size

    reports = []
    drivers = []
    failure = ""
    failed_rank = None
    for rank in range(world_size):
        driver = driver_factory(rank)
        drivers.append(driver)
        loader = ModelTransferSGLangLoader(
            reader=reader_factory(), capabilities=capabilities, budgets=budgets
        )
        try:
            report = loader.load(
                driver,
                options,
                shard=plan.assignment(rank),
                finalize=False,
            )
        except BaseException as exc:
            failure = f"rank {rank}: {type(exc).__name__}: {exc}"
            failed_rank = rank
            break
        reports.append(report)

    if failed_rank is None:
        # Finalization runs through each rank's own driver — the engine's
        # post-load hook, not a counter bump — once, after every rank
        # loaded: all ranks finalize or none does.
        for driver, report in zip(drivers, reports, strict=True):
            driver.finalize(report.model)
            report.finalization_count = report.model.finalization_count
        return LoadGroupReport(
            world_size=world_size,
            plan=plan,
            rank_reports=tuple(reports),
            bytes_read_across_ranks=sum(
                report.payload_bytes_read for report in reports
            ),
            unique_checkpoint_parameter_bytes=unique,
            group_state="loaded",
        )

    # A bounded group failure: every rank that built a model is tainted,
    # nothing finalizes, and the failure is reported rather than retried.
    # A rank that refused before its model was built — a preflight or
    # probe refusal — tainted nothing, and is not recorded as if it had.
    tainted = []
    for rank in range(failed_rank + 1):
        if rank < len(reports):
            if reports[rank].model is not None:
                reports[rank].model.mutated = True
                tainted.append(rank)
        elif loader.last_model is not None:
            tainted.append(rank)
    report = LoadGroupReport(
        world_size=world_size,
        plan=plan,
        rank_reports=tuple(reports),
        bytes_read_across_ranks=sum(report.payload_bytes_read for report in reports),
        unique_checkpoint_parameter_bytes=unique,
        group_state="failed",
        tainted_ranks=tuple(tainted),
        failure=failure,
    )
    if raise_on_failure:
        raise LoadGroupFailure(report)
    return report


class LoadGroupFailure(Exception):
    """A load group failed; the report says which ranks and why."""

    def __init__(self, report: LoadGroupReport):
        super().__init__(report.failure)
        self.report = report


__all__ = [
    "STRATEGY_COLUMN",
    "STRATEGY_REPLICATE",
    "STRATEGY_ROW",
    "LoadGroupFailure",
    "LoadGroupReport",
    "ShardAssignment",
    "ShardSlice",
    "ShardView",
    "ShardingPlan",
    "build_sharding_plan",
    "classify",
    "load_group",
]
