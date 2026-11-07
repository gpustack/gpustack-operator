"""TP sharding plans and the load-group lifecycle."""

import unittest

import sglangutil as fixtureutil

from gpustack_model_transfer.core.budgets import ReadBudgets
from gpustack_model_transfer.core.capabilities import local_backend_capabilities
from gpustack_model_transfer.core.errors import DeniedContentError
from gpustack_model_transfer.core.local_reader import LocalFileReader


def manifest():
    reader = LocalFileReader(fixtureutil.manifest_path())
    try:
        return reader.manifest
    finally:
        reader.close()


def make_plan(world_size):
    from gpustack_model_transfer.adapters.sglang.sharding import build_sharding_plan

    return build_sharding_plan(manifest(), world_size)


def rank_driver(root=None, **kwargs):
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

    return ReferenceRuntime(root or fixtureutil.manifest_dir(), **kwargs)


class TestShardingPlan(unittest.TestCase):
    def test_column_parallel_shards_split_dim_zero(self):
        plan = make_plan(2)
        self.assertEqual(plan.strategies["model.layers.0.attn.q_proj.weight"], "column")
        payload = fixtureutil.tensor_payload("model.layers.0.attn.q_proj.weight")
        rank0 = plan.assignment(0).slices["model.layers.0.attn.q_proj.weight"]
        rank1 = plan.assignment(1).slices["model.layers.0.attn.q_proj.weight"]
        self.assertFalse(rank0.covers_all)
        self.assertFalse(rank1.covers_all)
        # Ranks tile the tensor exactly once, in order.
        self.assertEqual(rank0.segments[0][0], 0)
        self.assertEqual(
            rank0.segments[0][0] + rank0.segments[0][1], rank1.segments[0][0]
        )
        self.assertEqual(rank1.segments[0][0] + rank1.segments[0][1], len(payload))
        self.assertEqual(
            sum(length for _, length in rank0.segments + rank1.segments), len(payload)
        )

    def test_one_dimensional_tensors_are_replicated(self):
        plan = make_plan(2)
        norm = plan.assignment(0).slices["model.norm.weight"]
        payload = fixtureutil.tensor_payload("model.norm.weight")
        self.assertTrue(norm.covers_all)
        self.assertEqual(norm.segments, ((0, len(payload)),))
        self.assertEqual(plan.strategies["model.norm.weight"], "replicate")

    def test_plan_is_deterministic_and_rank_consistent(self):
        first = make_plan(2)
        second = make_plan(2)
        self.assertEqual(
            [t.name for t in first.tensors], [t.name for t in second.tensors]
        )
        for rank in (0, 1):
            self.assertEqual(
                sorted(first.assignment(rank).slices),
                sorted(second.assignment(rank).slices),
            )

    def test_world_size_one_covers_everything(self):
        plan = make_plan(1)
        for name in fixtureutil.parameter_names():
            self.assertTrue(plan.assignment(0).slices[name].covers_all, name)

    def test_shard_shapes_are_element_aligned(self):
        plan = make_plan(2)
        q = plan.assignment(0).views["model.layers.0.attn.q_proj.weight"]
        whole = fixtureutil.tensor_entry("model.layers.0.attn.q_proj.weight")
        self.assertEqual(q.shape[0], (whole["shape"][0] + 1) // 2)
        self.assertEqual(q.shape[1:], tuple(whole["shape"][1:]))
        self.assertEqual(
            q.byte_size,
            fixtureutil.element_size("F32") * q.shape[0] * q.shape[1],
        )


class TestLoadGroup(unittest.TestCase):
    def reader_factory(self):
        def factory():
            return LocalFileReader(fixtureutil.manifest_path())

        return factory

    def test_single_rank_group_matches_a_plain_load(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        group = load_group(
            reader_factory=self.reader_factory(),
            driver_factory=lambda rank: rank_driver(),
            options=LoadOptions(),
            world_size=1,
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=32, max_inflight=2, owned_output_bytes=4096
            ),
        )
        self.assertEqual(group.world_size, 1)
        self.assertEqual(group.group_state, "loaded")
        self.assertFalse(group.tainted)
        self.assertEqual(len(group.rank_reports), 1)
        report = group.rank_reports[0]
        self.assertEqual(report.finalization_count, 1)
        self.assertEqual(group.traffic_amplification, 1.0)

    def test_two_rank_group_reassembles_the_checkpoint(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        group = load_group(
            reader_factory=self.reader_factory(),
            driver_factory=lambda rank: rank_driver(),
            options=LoadOptions(),
            world_size=2,
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=32, max_inflight=2, owned_output_bytes=4096
            ),
        )
        self.assertEqual(group.group_state, "loaded")
        self.assertEqual(len(group.rank_reports), 2)
        for report in group.rank_reports:
            self.assertEqual(report.finalization_count, 1)
        # Every replicated parameter is identical on both ranks; engine
        # metadata tensors are scalars, not parameters.
        for name in group.plan.replicated_names:
            if name.startswith("metadata."):
                continue
            for report in group.rank_reports:
                self.assertEqual(
                    rank_driver().model_parameter_bytes(report.model, name),
                    fixtureutil.tensor_payload(name),
                )
        # Column shards concatenate to the whole tensor, in rank order.
        for name in group.plan.sharded_names:
            if group.plan.strategies[name] != "column":
                continue
            whole = fixtureutil.tensor_payload(name)
            joined = b"".join(
                group.rank_payload(rank, name) for rank in range(group.world_size)
            )
            self.assertEqual(joined, whole)
        # Traffic amplification is measured, not asserted into being.
        self.assertGreater(group.bytes_read_across_ranks, 0)
        self.assertGreaterEqual(group.traffic_amplification, 1.0)

    def test_rank_failure_taints_the_group_and_bounds_it(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        def failing_driver_factory(rank):
            if rank == 1:
                return rank_driver(
                    fail_store_after=0,
                    store_error=DeniedContentError("rank 1 refused"),
                )
            return rank_driver()

        from gpustack_model_transfer.adapters.sglang.sharding import LoadGroupFailure

        with self.assertRaises(LoadGroupFailure):
            load_group(
                reader_factory=self.reader_factory(),
                driver_factory=failing_driver_factory,
                options=LoadOptions(),
                world_size=2,
                capabilities=local_backend_capabilities(),
                budgets=ReadBudgets(
                    window_bytes=32, max_inflight=2, owned_output_bytes=4096
                ),
            )

    def test_group_failure_report_taints_built_ranks(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        drivers = {}

        def failing_driver_factory(rank):
            driver = rank_driver()
            drivers[rank] = driver
            if rank == 1:
                # Fail late: rank 1 built its model, then its store refused.
                driver.fail_store_after = 10**9
                driver.store_error = DeniedContentError("rank 1 refused late")
                original_store = driver.store

                def store(model, entry, payload, ownership="owned"):
                    if entry.tensor.name.startswith("model.norm"):
                        raise driver.store_error
                    return original_store(model, entry, payload, ownership)

                driver.store = store
            return driver

        group = load_group(
            reader_factory=self.reader_factory(),
            driver_factory=failing_driver_factory,
            options=LoadOptions(),
            world_size=2,
            capabilities=local_backend_capabilities(),
            budgets=ReadBudgets(
                window_bytes=32, max_inflight=2, owned_output_bytes=4096
            ),
            raise_on_failure=False,
        )
        self.assertEqual(group.group_state, "failed")
        self.assertTrue(group.tainted)
        self.assertIn(1, group.tainted_ranks)
        for report in group.rank_reports:
            if report.model is not None:
                self.assertEqual(
                    report.finalization_count, 0, "a failed group finalizes nothing"
                )


if __name__ == "__main__":
    unittest.main()


# --- the crosscheck fixes, each pinned -------------------------------------


def build_row_parallel_tree(tmp, *, with_3d: bool = False):
    """Write a real safetensors tree carrying row-parallel weights.

    o_proj and down_proj are the names the route shards by row; the file is
    a genuine safetensors blob (length prefix + JSON header + payload), the
    manifest a genuine reader-contract manifest, so the probe cross-check
    and digest verification both run for real.
    """
    import json
    import struct
    from pathlib import Path

    root = Path(tmp)
    root.mkdir(parents=True, exist_ok=True)
    tensors = [
        ("model.layers.0.attn.o_proj.weight", "F32", [4, 6]),
        ("model.layers.0.mlp.down_proj.weight", "F32", [4, 6]),
    ]
    if with_3d:
        tensors.append(("model.layers.1.mlp.o_proj.weight", "F32", [2, 6, 2]))

    header = {}
    data = bytearray()
    payloads = {}
    for index, (name, dtype, shape) in enumerate(tensors):
        count = 1
        for dim in shape:
            count *= dim
        payload = bytes((i * 11 + index * 3) % 256 for i in range(count * 4))
        payloads[name] = payload
        begin = len(data)
        data += payload
        header[name] = {
            "dtype": dtype,
            "shape": shape,
            "data_offsets": [begin, len(data)],
        }
    header_bytes = json.dumps(header, separators=(",", ":"), sort_keys=True).encode()
    blob = struct.pack("<Q", len(header_bytes)) + header_bytes + bytes(data)
    (root / "checkpoint.safetensors").write_bytes(blob)

    entries = []
    base = 8 + len(header_bytes)
    for name, dtype, shape in tensors:
        begin, end = header[name]["data_offsets"]
        entries.append(
            {
                "name": name,
                "file": "checkpoint.safetensors",
                "dtype": dtype,
                "shape": shape,
                "data_begin": base + begin,
                "data_end": base + end,
            }
        )
    manifest = {
        "contract_version": 2,
        "artifact_uid": "row-fixture",
        "source_generation": 1,
        "files": [
            {
                "path": "checkpoint.safetensors",
                "size": len(blob),
            }
        ],
        "tensors": entries,
    }
    (root / "manifest.json").write_bytes(
        json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode() + b"\n"
    )
    return root, payloads


class TestRowParallelSharding(unittest.TestCase):
    def test_row_weights_shard_by_input_dimension(self):
        import tempfile

        from gpustack_model_transfer.adapters.sglang.sharding import build_sharding_plan
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        with tempfile.TemporaryDirectory() as tmp:
            root, _payloads = build_row_parallel_tree(tmp)
            reader = LocalFileReader(str(root / "manifest.json"))
            try:
                plan = build_sharding_plan(reader.manifest, 2)
            finally:
                reader.close()
        self.assertEqual(plan.strategies["model.layers.0.attn.o_proj.weight"], "row")
        rank0 = plan.assignment(0).slices["model.layers.0.attn.o_proj.weight"]
        rank1 = plan.assignment(1).slices["model.layers.0.attn.o_proj.weight"]
        # Every row contributes one segment per rank, four rows here.
        self.assertEqual(len(rank0.segments), 4)
        self.assertEqual(len(rank1.segments), 4)
        # The two ranks' segment lengths sum to the tensor for every row.
        for (off0, len0), (off1, len1) in zip(
            rank0.segments, rank1.segments, strict=True
        ):
            self.assertEqual(len0, len1)
            self.assertGreater(off1, off0)

    def test_row_shards_keep_trailing_dimensions(self):
        import tempfile

        from gpustack_model_transfer.adapters.sglang.sharding import build_sharding_plan
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        with tempfile.TemporaryDirectory() as tmp:
            root, _payloads = build_row_parallel_tree(tmp, with_3d=True)
            reader = LocalFileReader(str(root / "manifest.json"))
            try:
                plan = build_sharding_plan(reader.manifest, 2)
            finally:
                reader.close()
        view = plan.assignment(0).views["model.layers.1.mlp.o_proj.weight"]
        self.assertEqual(view.shape, (2, 3, 2), "trailing dimensions survive")
        self.assertEqual(
            view.byte_size, 2 * 3 * 2 * 4, "the shard's size matches its shape"
        )

    def test_two_rank_row_group_reassembles_and_replays_rows(self):
        import tempfile

        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        with tempfile.TemporaryDirectory() as tmp:
            root, payloads = build_row_parallel_tree(tmp)

            def reader_factory():
                return LocalFileReader(str(root / "manifest.json"))

            def driver_factory(rank):
                from gpustack_model_transfer.adapters.sglang.runtime import (
                    ReferenceRuntime,
                )

                return ReferenceRuntime(str(root))

            group = load_group(
                reader_factory=reader_factory,
                driver_factory=driver_factory,
                options=LoadOptions(),
                world_size=2,
                capabilities=local_backend_capabilities(),
                budgets=ReadBudgets(
                    window_bytes=32, max_inflight=2, owned_output_bytes=4096
                ),
                raise_on_failure=False,
            )
            self.assertEqual(group.group_state, "loaded")
            for name in (
                "model.layers.0.attn.o_proj.weight",
                "model.layers.0.mlp.down_proj.weight",
            ):
                whole = payloads[name]
                # Row reassembly: each row is rank 0's segment, then rank
                # 1's, four rows of three F32 elements each.
                rank0 = group.rank_payload(0, name)
                rank1 = group.rank_payload(1, name)
                row = len(whole) // 4
                for r in range(4):
                    chunk0 = rank0[r * (row // 2) : (r + 1) * (row // 2)]
                    chunk1 = rank1[r * (row // 2) : (r + 1) * (row // 2)]
                    self.assertEqual(
                        whole[r * row : (r + 1) * row],
                        chunk0 + chunk1,
                        f"{name} row {r} did not reassemble",
                    )
                for report in group.rank_reports:
                    self.assertIn(name, report.partial_shard_reads)


class TestGroupFinalizationAndTaint(unittest.TestCase):
    def reader_factory(self):
        from gpustack_model_transfer.core.local_reader import LocalFileReader

        def factory():
            return LocalFileReader(fixtureutil.manifest_path())

        return factory

    def budgets(self):
        return ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096)

    def test_finalization_runs_through_each_ranks_driver(self):
        import tempfile

        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        events = []

        with tempfile.TemporaryDirectory():
            from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime

            def driver_factory(rank):
                runtime = ReferenceRuntime(fixtureutil.manifest_dir())
                runtime.events = events
                return runtime

            group = load_group(
                reader_factory=self.reader_factory(),
                driver_factory=driver_factory,
                options=LoadOptions(),
                world_size=2,
                capabilities=local_backend_capabilities(),
                budgets=self.budgets(),
                raise_on_failure=False,
            )
        self.assertEqual(group.group_state, "loaded")
        self.assertEqual(
            events.count("finalize"),
            2,
            "the engine's finalization hook ran once per rank",
        )
        for report in group.rank_reports:
            self.assertEqual(report.finalization_count, 1)

    def test_preflight_refusal_taints_nothing(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group
        from gpustack_model_transfer.core.budgets import ReadBudgets as RB

        # Rank 0 refuses in the plan preflight: no model was built, so no
        # rank is tainted and nothing finalized.
        group = load_group(
            reader_factory=self.reader_factory(),
            driver_factory=lambda rank: rank_driver(),
            options=LoadOptions(),
            world_size=2,
            capabilities=local_backend_capabilities(),
            budgets=RB(window_bytes=32, max_inflight=2, owned_output_bytes=1),
            raise_on_failure=False,
        )
        self.assertEqual(group.group_state, "failed")
        self.assertEqual(
            group.tainted_ranks, (), "a pre-build refusal built nothing to taint"
        )
        self.assertEqual(group.rank_reports, ())

    def test_tied_group_reads_no_head_bytes(self):
        from gpustack_model_transfer.adapters.sglang import LoadOptions
        from gpustack_model_transfer.adapters.sglang.sharding import load_group

        group = load_group(
            reader_factory=self.reader_factory(),
            driver_factory=lambda rank: rank_driver(tie_word_embeddings=True),
            options=LoadOptions(tie_word_embeddings=True),
            world_size=1,
            capabilities=local_backend_capabilities(),
            budgets=self.budgets(),
            raise_on_failure=False,
        )
        self.assertEqual(group.group_state, "loaded")
        head = len(fixtureutil.tensor_payload("lm_head.weight"))
        self.assertEqual(
            group.bytes_read_across_ranks, group.unique_checkpoint_parameter_bytes
        )
        self.assertNotEqual(group.bytes_read_across_ranks, 0)
        self.assertNotEqual(head, 0, "the fixture's head carries bytes")
        self.assertEqual(group.traffic_amplification, 1.0)


def test_failed_rank_taint_does_not_require_driver_last_model():
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime
    from gpustack_model_transfer.adapters.sglang.sharding import load_group
    from gpustack_model_transfer.core.local_reader import LocalFileReader

    class Driver:
        def __init__(self):
            self.inner = ReferenceRuntime(fixtureutil.manifest_dir())

        def build_model(self, plan, options):
            return self.inner.build_model(plan, options)

        def store(self, *args, **kwargs):
            raise RuntimeError("store failed")

        def store_engine_metadata(self, *args, **kwargs):
            return self.inner.store_engine_metadata(*args, **kwargs)

    group = load_group(
        reader_factory=lambda: LocalFileReader(fixtureutil.manifest_path()),
        driver_factory=lambda rank: Driver(),
        world_size=2,
        budgets=ReadBudgets(window_bytes=32, max_inflight=2, owned_output_bytes=4096),
        raise_on_failure=False,
    )
    assert group.group_state == "failed"
    assert group.tainted_ranks == (0,)


def test_rank_factory_failure_reports_group_and_taints_loaded_ranks():
    from gpustack_model_transfer.adapters.sglang.runtime import ReferenceRuntime
    from gpustack_model_transfer.adapters.sglang.sharding import load_group
    from gpustack_model_transfer.core.local_reader import LocalFileReader

    for failure in ("driver", "reader"):
        reads = 0

        def reader_factory(failure=failure):
            nonlocal reads
            reads += 1
            if failure == "reader" and reads == 3:
                raise RuntimeError("reader factory failed")
            return LocalFileReader(fixtureutil.manifest_path())

        def driver_factory(rank, failure=failure):
            if failure == "driver" and rank == 1:
                raise RuntimeError("driver factory failed")
            return ReferenceRuntime(fixtureutil.manifest_dir())

        group = load_group(
            reader_factory=reader_factory,
            driver_factory=driver_factory,
            world_size=2,
            budgets=ReadBudgets(
                window_bytes=32, max_inflight=2, owned_output_bytes=4096
            ),
            raise_on_failure=False,
        )
        assert group.group_state == "failed"
        assert group.tainted_ranks == (0,)
        assert group.rank_reports[0].finalization_count == 0
        assert "rank 1" in group.failure
