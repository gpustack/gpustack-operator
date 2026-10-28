"""Offline tests for the H200 hardware-qualification suites."""

import copy
import importlib.util
import json
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent


def load_module(name):
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


qualification = load_module("autoscaling_qualification")
rounds = qualification.rounds


class Options:
    """The argparse surface one suite run consumes."""

    def __init__(self, **overrides):
        values = dict(
            case_timeout=0.2, phase_seconds=0.05, catalog_configmap=None,
            frozen_capacity="20000", tuned_capacity=None, sim_capacity="6000",
            cluster_queues="qual-queue", repeats=3, builtin_gpus="1", legs="both",
            ttft_target_ms="2000", itl_target_ms="33", input_tokens=32, output_tokens=4,
            request_timeout_seconds=5, seed=71, slot_gpus=1, max_replicas=2,
            model_path="/models", context="explicit-test-context",
        )
        values.update(overrides)
        self.__dict__.update(values)


class FakeCluster:
    """An in-memory stand-in for the kubectl surface one suite touches.

    Objects become ready a fixed number of polls after creation, so the happy
    path and the never-ready failure path are both reachable without a cluster.
    """

    def __init__(self, ready_after=1):
        self.objects = {}
        self.calls = []
        self.polls = 0
        self.ready_after = ready_after

    def kind_of(self, resource):
        if resource.startswith("modeldeploymentscalers"):
            return "ModelDeploymentScaler"
        if resource.startswith("modeldeployments"):
            return "ModelDeployment"
        if resource.startswith("modelprefetches"):
            return "ModelPrefetch"
        if resource.startswith("daemonsets"):
            return "DaemonSet"
        if resource.startswith("service"):
            return "Service"
        if resource.startswith("pod"):
            return "Pod"
        if resource.startswith("configmap"):
            return "ConfigMap"
        if resource.startswith("event"):
            return "Event"
        raise AssertionError(f"unexpected resource {resource}")

    def apply(self, manifest):
        kind, name = manifest["kind"], manifest["metadata"]["name"]
        self.calls.append(("apply", kind, name))
        self.objects[(kind, name)] = manifest
        if kind == "ModelDeployment":
            owned = {"app.kubernetes.io/instance": name, "app.kubernetes.io/component": "server"}
            self.objects[("Service", f"{name}-server")] = {
                "kind": "Service",
                "metadata": {"name": f"{name}-server", "labels": dict(owned)},
                "spec": {"ports": [{"port": 8123}]},
            }

    def with_status(self, manifest, kind):
        obj = copy.deepcopy(manifest)
        if kind == "DaemonSet":
            self.polls += 1
            ready = 1 if self.polls > self.ready_after else 0
            obj["status"] = {"desiredNumberScheduled": 1, "numberReady": ready}
        elif kind == "ModelPrefetch":
            self.polls += 1
            obj["status"] = {"conditions": [{"type": "Available", "status": "True" if self.polls > self.ready_after else "False"}]}
        elif kind == "Pod":
            self.polls += 1
            obj["status"] = {"conditions": [{"type": "Ready", "status": "True" if self.polls > self.ready_after else "False"}]}
        elif kind == "ModelDeployment":
            obj["status"] = {"roles": [{"name": "server", "desired": 1, "ready": 1}]}
        elif kind == "ModelDeploymentScaler":
            if "status" not in obj:
                obj["status"] = {
                    "conditions": [{"type": "Bound", "status": "True"}, {"type": "CapacityPriorsReady", "status": "True"}],
                    "roles": [{"name": "server", "rawRecommendedReplicas": 3, "constrainedReplicas": 2, "readyReplicas": 2,
                               "signalState": "Fresh", "promptTokensPerSecond": "500", "generationTokensPerSecond": "250",
                               "runningRequests": 12, "waitingRequests": 4, "ttftMilliseconds": "900",
                               "itlMilliseconds": "30", "reason": "scaling"}],
                    "resolvedCapacityPriors": [{"name": "server", "seedTokensPerSecond": "5000", "requestedQuantile": "0.95",
                                                "quantile": "0.95", "headroom": "750m",
                                                "fields": [{"field": "serverTargetTokensPerSecond", "source": "Catalog",
                                                            "effectiveTokensPerSecond": "3750"}],
                                                "latencyBounds": [{"metric": "TTFT", "quantile": "0.95", "conservative": True}]}],
                }
        return obj

    def get_json(self, resource, name=None, namespace=None):
        self.calls.append(("get", resource, name))
        kind = self.kind_of(resource)
        if name is None:
            return {"items": [self.with_status(obj, kind) for (candidate_kind, _), obj in self.objects.items() if candidate_kind == kind]}
        obj = self.objects.get((kind, name))
        if obj is None:
            raise qualification.ClusterError(f"Error from server (NotFound): {kind} {name}")
        return self.with_status(obj, kind)

    def get_json_or_none(self, resource, name, namespace=None):
        try:
            return self.get_json(resource, name, namespace)
        except qualification.ClusterError:
            return None

    def delete(self, resource, name, namespace=None, timeout=300):
        self.calls.append(("delete", resource, name))

    def logs(self, pod, namespace, tail=200):
        return f"log of {pod}"

    def run(self, args, namespace=None, data=None, timeout=120):
        self.calls.append(("run", args[0]))
        return ""

    def exec_driver(self, root, action, *args, data=None, timeout=120):
        self.calls.append(("exec", action, root))
        remote = Path(root)
        if action == "stage":
            remote.mkdir(parents=True, exist_ok=True)
            for filename, content in json.loads(data).items():
                (remote / filename).write_text(content)
            return ""
        if action == "read":
            return (remote / args[0]).read_text()
        if action == "remove":
            shutil.rmtree(remote, ignore_errors=True)
            return ""
        raise AssertionError(action)


def fake_run_load_phase(cluster, root, endpoint, workload, phase, concurrency, duration, out_dir, seed, sampler=None):
    """A driver stand-in: one compliant record per unit of concurrency, one second of wall."""
    records = []
    for index in range(max(1, concurrency)):
        records.append({
            "ttft_ms": 100.0 + concurrency, "itl_ms": [10.0, 12.0], "tpot_ms": 11.0,
            "e2e_ms": 200.0 + concurrency, "prompt_tokens": 32, "output_tokens": 4,
            "started_at": time.time(),
        })
    (Path(out_dir) / f"{phase}_reqs.jsonl").write_text("".join(json.dumps(record) + "\n" for record in records))
    (Path(out_dir) / f"{phase}.json").write_text(json.dumps({"wall_seconds": 1.0}))


class PatchedDriver:
    def __init__(self, function):
        self.function = function
        self.original = None

    def __enter__(self):
        self.original = qualification.run_load_phase
        qualification.run_load_phase = self.function
        return self

    def __exit__(self, *_):
        qualification.run_load_phase = self.original


class BuilderTests(unittest.TestCase):
    def test_prefetch_daemonset_pulls_without_requesting_accelerators(self):
        manifest = qualification.prefetch_daemonset("image-prefetch-vllm", "gpustack/runner:cuda13.0-vllm0.30.0")
        self.assertEqual(manifest["kind"], "DaemonSet")
        container = manifest["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], "gpustack/runner:cuda13.0-vllm0.30.0")
        self.assertNotIn("resources", container)
        self.assertEqual(manifest["spec"]["template"]["spec"]["nodeSelector"], {"feature.gpustack.ai/acceleratable": "true"})
        self.assertEqual(manifest["metadata"]["namespace"], qualification.namespace())

    def test_model_artifact_carries_known_layouts(self):
        qwen = qualification.model_artifact("qwen3-8b")
        source = qwen["spec"]["source"]["huggingFace"]
        self.assertEqual(source["repository"], "Qwen/Qwen3-8B")
        self.assertIn("revision", source)
        self.assertTrue(any(pattern.endswith(".safetensors") for pattern in qwen["spec"]["allowPatterns"]))
        gpt = qualification.model_artifact("gpt-oss-120b")
        self.assertEqual(gpt["spec"]["source"]["huggingFace"]["repository"], "openai/gpt-oss-120b")
        self.assertNotIn("allowPatterns", gpt["spec"])

    def test_model_prefetch_places_on_the_instance_type(self):
        prefetch = qualification.model_prefetch("qwen3-8b", "nvidia-h200")
        self.assertEqual(prefetch["spec"]["placement"], {"instanceTypes": ["nvidia-h200"]})
        self.assertEqual(prefetch["spec"]["bindingRef"], {"name": "bench-models"})
        self.assertEqual(prefetch["spec"]["minReady"], 0)

    def test_server_deployment_widens_tensor_parallel_with_the_gpu_count(self):
        for gpus, accelerator in ((1, "1"), (4, "4")):
            deployment = qualification.server_deployment("qual-sweep", "qwen3-8b", "/models", gpus, "gpustack/runner:cuda13.0-vllm0.30.0")
            role = deployment["spec"]["roles"][0]
            self.assertEqual(role["resources"]["accelerator"], accelerator)
            arguments = role["extraArgs"]
            self.assertEqual(arguments[arguments.index("--tensor-parallel-size") + 1], accelerator)
            self.assertIn("--enable-scale-out", arguments)
            self.assertEqual(deployment["spec"]["model"]["artifactRef"], {"name": "qwen3-8b"})

    def test_scaler_shapes_cover_the_three_algorithms(self):
        proportional = qualification.proportional_scaler("s", "t")
        algorithm = proportional["spec"]["algorithm"]
        self.assertEqual(algorithm["proportional"]["metric"], "RunningRequests")
        self.assertEqual(algorithm["proportional"]["roleTargets"], [{"name": "server", "target": "12"}])
        throughput = qualification.token_throughput_spec("6000", "2000", "33")
        self.assertEqual(throughput["tokenThroughput"]["serverTargetTokensPerSecond"], "6000")
        manual = qualification.token_throughput_spec(None, "2000", "33")
        self.assertNotIn("serverTargetTokensPerSecond", manual["tokenThroughput"])
        predictive = qualification.predictive_spec(None, "2000", "33")
        self.assertNotIn("serverTokenCapacityPerReplica", predictive["predictive"])
        self.assertNotIn("serverTargetTokensPerSecond", predictive["predictive"]["tokenThroughput"])
        builtin = qualification.base_scaler("s", "t", qualification.token_throughput_spec(None, "2000", "33"), {})
        self.assertEqual(builtin["spec"]["algorithm"]["capacityPrior"], {})

    def test_catalog_configmap_uses_the_priors_yaml_key(self):
        header = "apiVersion: worker.gpustack.ai/v1alpha1\nkind: ModelDeploymentScalerCapacityPriorCatalog\n"
        configmap = qualification.catalog_configmap("qual-sweep-priors", header)
        self.assertEqual(configmap["data"]["priors.yaml"], header)


class MetricsTests(unittest.TestCase):
    def records(self):
        def good(ttft, tpot):
            return {"ttft_ms": ttft, "itl_ms": [tpot, tpot], "tpot_ms": tpot, "e2e_ms": 1000.0,
                    "prompt_tokens": 32, "output_tokens": 4, "started_at": 1.0}
        return [
            good(100.0, 10.0), good(150.0, 20.0),
            {"error": "http 500", "e2e_ms": 50.0},
            {"error": "timeout", "e2e_ms": 5000.0},
        ]

    def test_true_slo_keeps_failed_and_timed_out_in_the_denominator(self):
        metrics = qualification.true_slo_metrics(self.records(), 2000.0, 33.0)
        self.assertEqual(metrics["requests"], 4)
        self.assertEqual(metrics["completed"], 2)
        self.assertEqual(metrics["failed"], 1)
        self.assertEqual(metrics["timedOut"], 1)
        self.assertEqual(metrics["compliant"], 2)
        self.assertEqual(metrics["trueSloCompliance"], 0.5)
        self.assertIsNotNone(metrics["goodputPerSecond"])
        self.assertIsNotNone(metrics["outputTokensPerSecond"])
        self.assertIsNotNone(metrics["totalTokensPerSecond"])

    def test_true_slo_quantiles_carry_the_p90(self):
        metrics = qualification.true_slo_metrics(self.records(), 2000.0, 33.0)
        for field in ("ttftMilliseconds", "itlMilliseconds", "tpotMilliseconds"):
            self.assertEqual(sorted(metrics[field]), ["p50", "p90", "p95", "p99"], field)
        self.assertEqual(metrics["ttftMilliseconds"]["p90"], 150.0)
        self.assertIsNotNone(metrics["tokensPerSecondPerUser"])

    def test_sweep_row_aggregates_rates_and_errors(self):
        phase_records = [
            {"ttft_ms": 120.0, "itl_ms": [10.0], "tpot_ms": 10.0, "e2e_ms": 500.0,
             "prompt_tokens": 32, "output_tokens": 4, "started_at": 0.0},
            {"error": "timeout", "e2e_ms": 1000.0},
        ]
        row = qualification.sweep_row(phase_records, 1.0)
        self.assertEqual(row["requests"], 2)
        self.assertEqual(row["errors"], 1)
        self.assertEqual(row["totalTokensPerSecond"], 36.0)
        self.assertEqual(row["inputTokensPerSecond"], 32.0)
        self.assertEqual(row["outputTokensPerSecond"], 4.0)
        self.assertIn("p90", row["ttftMilliseconds"])

    def test_sweep_catalog_document_is_a_valid_flat_profile(self):
        rows = {concurrency: qualification.sweep_row([
            {"ttft_ms": 120.0, "itl_ms": [10.0], "tpot_ms": 10.0, "e2e_ms": 500.0,
             "prompt_tokens": 32, "output_tokens": 4, "started_at": 0.0},
        ], 1.0) for concurrency in (2, 4)}
        document = qualification.sweep_catalog_document(rows)
        self.assertIs(document, rounds.validate_catalog_document(document))
        profile = document["profiles"][0]
        self.assertEqual(profile["role"], "Server")
        self.assertEqual(profile["model"], "Qwen/Qwen3-8B")
        self.assertEqual(profile["topology"], {"gpusPerReplica": 1, "tensorParallel": 1})
        self.assertEqual([point["concurrency"] for point in profile["points"]], [2, 4])

    def test_point_feasibility_treats_absent_quantiles_as_not_failing(self):
        self.assertTrue(qualification.point_within_targets({"ttftMilliseconds": {}, "itlMilliseconds": {}}, 100.0, 10.0))
        self.assertTrue(qualification.point_within_targets({"ttftMilliseconds": {"p90": 90.0}, "itlMilliseconds": {"p90": 5.0}}, 100.0, 10.0))
        self.assertFalse(qualification.point_within_targets({"ttftMilliseconds": {"p90": 200.0}, "itlMilliseconds": {}}, 100.0, 10.0))

    def test_catalog_capacity_reads_the_highest_feasible_point_scaled_to_the_slot(self):
        document = {"profiles": [{
            "role": "Server", "model": "Qwen/Qwen3-8B", "quantization": "bf16", "engine": "vllm",
            "hardware": "nvidia-h200", "topology": {"gpusPerReplica": 1, "tensorParallel": 1},
            "points": [
                {"concurrency": 2, "totalTokensPerSecond": 100.0, "inputTokensPerSecond": 80.0,
                 "outputTokensPerSecond": 20.0, "ttftMilliseconds": {"p90": 100.0}, "itlMilliseconds": {"p90": 5.0}},
                {"concurrency": 8, "totalTokensPerSecond": 400.0, "inputTokensPerSecond": 320.0,
                 "outputTokensPerSecond": 80.0, "ttftMilliseconds": {"p90": 900.0}, "itlMilliseconds": {"p90": 5.0}},
                {"concurrency": 16, "totalTokensPerSecond": 500.0, "inputTokensPerSecond": 400.0,
                 "outputTokensPerSecond": 100.0, "ttftMilliseconds": {"p90": 9000.0}, "itlMilliseconds": {"p90": 5.0}},
            ],
        }]}
        tuned = qualification.catalog_capacity_for(document, "Qwen/Qwen3-8B", "vllm", "nvidia-h200", "bf16",
                                                   1, 1000.0, 10.0, 4)
        self.assertEqual(tuned, 1600.0)
        with self.assertRaises(qualification.ClusterError):
            qualification.catalog_capacity_for(document, "openai/gpt-oss-120b", "vllm", "nvidia-h200", "fp4",
                                               1, 1000.0, 10.0, 4)

    def test_sample_row_extracts_the_controller_and_resolution_fields(self):
        cluster = FakeCluster()
        name = "qual-sim"
        cluster.apply(qualification.server_deployment(name, "qwen3-8b", "/models", 1, "image"))
        cluster.apply(qualification.base_scaler(name, name, qualification.token_throughput_spec("6000", "2000", "33"), {}))
        row = qualification.sample_row(cluster, name, "ramp", "TokenThroughput")
        role = row["roles"]["server"]
        self.assertEqual(role["rawRecommendedReplicas"], 3)
        self.assertEqual(role["desiredReplicas"], 2)
        self.assertEqual(role["readyReplicas"], 2)
        self.assertEqual(role["signalState"], "Fresh")
        self.assertEqual(role["processedTokensPerSecond"], 750.0)
        self.assertEqual(role["runningRequests"], 12)
        self.assertEqual(role["waitingRequests"], 4)
        self.assertEqual(role["ttftMilliseconds"], 900.0)
        self.assertEqual(role["reason"], "scaling")
        prior = row["resolvedCapacityPriors"]["server"]
        self.assertEqual(prior["effectiveTokensPerSecond"], {"serverTargetTokensPerSecond": "3750"})
        self.assertEqual(prior["requestedQuantile"], "0.95")
        self.assertEqual(prior["boundaryReason"], "scaling")
        self.assertTrue(prior["latencyBounds"][0]["conservative"])

    def test_sample_row_records_missing_signals_as_null(self):
        cluster = FakeCluster()
        name = "qual-sim"
        cluster.apply(qualification.server_deployment(name, "qwen3-8b", "/models", 1, "image"))
        scaler = qualification.base_scaler(name, name, qualification.token_throughput_spec("6000", "2000", "33"), {})
        scaler["status"] = {"roles": [{"name": "server", "signalState": "Missing"}]}
        cluster.objects[("ModelDeploymentScaler", name)] = scaler
        row = qualification.sample_row(cluster, name, "ramp", "TokenThroughput")
        role = row["roles"]["server"]
        self.assertEqual(role["signalState"], "Missing")
        self.assertIsNone(role["rawRecommendedReplicas"])
        self.assertIsNone(role["processedTokensPerSecond"])


class RunnerTests(unittest.TestCase):
    def test_bounded_case_times_out_dumps_and_the_suite_continues(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            timed_out = qualification.run_case("slow", [sys.executable, "-B", "-c", "import time; time.sleep(5)"], root / "slow", 1.0)
            self.assertEqual(timed_out["outcome"], "timed-out")
            self.assertIn("exceeded its", timed_out["error"])
            self.assertTrue((root / "slow" / "failure_summary.json").exists())
            summary = json.loads((root / "slow" / "failure_summary.json").read_text())
            self.assertEqual(summary["case"], "slow")
            quick = qualification.run_case("quick", [sys.executable, "-B", "-c", "pass"], root / "quick", 10.0)
            self.assertEqual(quick["outcome"], "succeeded")

    def test_failure_summary_without_a_cluster_carries_the_error_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            qualification.dump_failure(None, "case", root, "boom")
            summary = json.loads((root / "failure_summary.json").read_text())
            self.assertEqual(summary["error"], "boom")
            self.assertIsNone(summary["pods"])

    def test_failing_case_dumps_diagnostics_and_records_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            failed = qualification.run_case("boom", [sys.executable, "-B", "-c", "raise SystemExit(3)"], root / "boom", 10.0)
            self.assertEqual(failed["outcome"], "failed")
            self.assertTrue((root / "boom" / "failure_summary.json").exists())


class PrepareTests(unittest.TestCase):
    def test_prepare_pulls_images_warms_models_and_gates_the_driver(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = FakeCluster(ready_after=1)
            records = qualification.suite_prepare(cluster, Path(directory), Options(case_timeout=5.0))
            record = records[0]
            self.assertEqual(record["outcome"], "succeeded", record.get("errors"))
            for engine, image in qualification.ENGINE_IMAGES.items():
                self.assertEqual(record["images"][engine], {"image": image, "ready": True})
                self.assertIn(("apply", "DaemonSet", f"image-prefetch-{engine}"), cluster.calls)
            self.assertEqual(sorted(record["models"]), sorted(qualification.MODELS))
            for name, entry in record["models"].items():
                self.assertTrue(entry["ready"], name)
            self.assertIn(("apply", "ModelStore", qualification.MODEL_STORE), cluster.calls)
            self.assertTrue(record["driver"]["ready"])

    def test_prepare_reports_a_model_that_never_lands(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = FakeCluster(ready_after=1000)
            records = qualification.suite_prepare(cluster, Path(directory), Options(case_timeout=0.1))
            record = records[0]
            self.assertEqual(record["outcome"], "failed")
            self.assertFalse(all(entry.get("ready") for entry in record["models"].values()))
            self.assertTrue((Path(directory) / "prepare" / "failure_summary.json").exists())


class SweepTests(unittest.TestCase):
    def test_sweep_emits_a_valid_catalog_configmap_and_summary(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = FakeCluster(ready_after=0)
            with PatchedDriver(fake_run_load_phase):
                records = qualification.suite_sweep(cluster, Path(directory), Options())
            record = records[0]
            self.assertEqual(record["outcome"], "succeeded", record.get("error"))
            sweep_dir = Path(directory) / "sweep"
            document = rounds.validate_catalog_document(rounds.load_catalog_document((sweep_dir / "priors.yaml").read_text()))
            profile = document["profiles"][0]
            self.assertEqual([point["concurrency"] for point in profile["points"]], list(qualification.SWEEP_CONCURRENCIES))
            configmap = json.loads((sweep_dir / "priors-configmap.json").read_text())
            self.assertEqual(configmap["metadata"]["name"], "qual-sweep-priors")
            self.assertIn("priors.yaml", configmap["data"])
            rounds.validate_catalog_document(rounds.load_catalog_document(configmap["data"]["priors.yaml"]))
            summary = (sweep_dir / "summary.md").read_text()
            self.assertIn("| 2 |", summary)
            self.assertIn("Tuned manual capacity", summary)
            self.assertEqual(record["rows"]["2"]["totalTokensPerSecond"], 36 * 2)
            self.assertGreater(record["tunedCapacityPerReplica"], 0)
            self.assertIn(("delete", qualification.MD_RESOURCE, "qual-sweep"), cluster.calls)


class SimSensitivityTests(unittest.TestCase):
    def test_sim_sensitivity_records_timelines_and_true_slo_metrics(self):
        original_phases = qualification.SIM_PHASES
        original_interval = qualification.SAMPLER_INTERVAL_SECONDS
        qualification.SIM_PHASES = (("baseline", 0.05, 2), ("ramp", 0.05, 24), ("sustain", 0.05, 24), ("cooldown", 0.05, 0))
        qualification.SAMPLER_INTERVAL_SECONDS = 0.01
        try:
            with tempfile.TemporaryDirectory() as directory:
                cluster = FakeCluster(ready_after=0)
                with PatchedDriver(fake_run_load_phase):
                    records = qualification.suite_sim_sensitivity(cluster, Path(directory), Options())
                for record in records:
                    self.assertEqual(record["outcome"], "succeeded", record.get("error"))
                directory = Path(directory) / "sim-sensitivity"
                algorithms = {record["algorithm"] for record in records}
                self.assertEqual(algorithms, {"Proportional", "TokenThroughput", "Predictive"})
                predictive = json.loads((directory / "sensitivity.json").read_text())
                self.assertEqual(predictive["reference"], "docs/modules/model-deployment/scaling-simulation.md")
                for algorithm, phases in predictive["arms"].items():
                    self.assertEqual(sorted(phases), ["baseline", "cooldown", "ramp", "sustain"], algorithm)
                    for phase in ("baseline", "ramp", "sustain"):
                        self.assertEqual(phases[phase]["trueSloCompliance"], 1.0, (algorithm, phase))
                    self.assertEqual(phases["cooldown"]["requests"], 0)
                summary = (directory / "summary.md").read_text()
                self.assertIn("| Predictive | sustain |", summary)
                timelines = list(directory.glob("*/timeline.jsonl"))
                self.assertEqual(len(timelines), 3)
                for timeline in timelines:
                    rows = [json.loads(line) for line in timeline.read_text().splitlines()]
                    self.assertTrue(rows)
                    for row in rows:
                        self.assertIn("rawRecommendedReplicas", json.dumps(row))
                    prepared = rows[0]
                    self.assertEqual(prepared["phase"], "prepare")
                    self.assertIn("resolvedCapacityPriors", json.dumps(rows[-1]))
                for algorithm in ("proportional", "tokenthroughput", "predictive"):
                    self.assertIn(("delete", qualification.SCALER_RESOURCE, f"qual-sim-{algorithm}"), cluster.calls)
                    self.assertIn(("delete", qualification.MD_RESOURCE, f"qual-sim-{algorithm}"), cluster.calls)
        finally:
            qualification.SIM_PHASES = original_phases
            qualification.SAMPLER_INTERVAL_SECONDS = original_interval


class BuiltinCatalogTests(unittest.TestCase):
    def test_builtin_catalog_resolves_from_the_embedded_prior(self):
        with tempfile.TemporaryDirectory() as directory:
            cluster = FakeCluster(ready_after=0)
            options = Options(builtin_gpus="1,2")
            with PatchedDriver(fake_run_load_phase):
                records = qualification.suite_builtin_catalog(cluster, Path(directory), options)
            self.assertEqual([record["gpusPerReplica"] for record in records], [1, 2])
            for record in records:
                self.assertEqual(record["outcome"], "succeeded", record.get("error"))
                self.assertEqual(record["model"], "openai/gpt-oss-120b")
                self.assertEqual(record["quantization"], "fp4")
                self.assertTrue(record["resolvedCapacityPriors"])
                self.assertEqual(record["metrics"]["trueSloCompliance"], 1.0)
            self.assertIn(("delete", qualification.MD_RESOURCE, "qual-builtin-1gpu"), cluster.calls)
            self.assertIn(("delete", qualification.MD_RESOURCE, "qual-builtin-2gpu"), cluster.calls)


class CapacityArmsTests(unittest.TestCase):
    def catalog_document(self):
        points = []
        for concurrency, total, ttft in ((2, 100, 100.0), (4, 120, 900.0)):
            points.append({
                "concurrency": concurrency, "totalTokensPerSecond": float(total),
                "inputTokensPerSecond": float(total) * 0.8, "outputTokensPerSecond": float(total) * 0.2,
                "ttftMilliseconds": {"p90": ttft}, "itlMilliseconds": {"p90": 5.0},
            })
        return {
            "apiVersion": rounds.CATALOG_API_VERSION,
            "kind": rounds.CATALOG_KIND,
            "profiles": [{
                "role": "Server", "model": "Qwen/Qwen3-8B", "quantization": "bf16", "engine": "vllm",
                "hardware": "nvidia-h200", "topology": {"gpusPerReplica": 1, "tensorParallel": 1},
                "points": points,
            }],
        }

    def test_generated_inputs_pass_the_real_harness_validator(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            options = Options()
            catalog_text = json.dumps(self.catalog_document())
            for algorithm in ("TokenThroughput", "Predictive"):
                inputs = qualification.write_capacity_inputs(root / algorithm.lower(), "qual-tt", algorithm, options,
                                                             400.0, catalog_text, "priors.yaml")
                config, deployment, scalers, cm, plan = rounds.validate(inputs)
                self.assertEqual(plan["algorithm"], algorithm)
                self.assertEqual(plan["catalogMode"], "configmap")
                self.assertEqual(len(plan["order"]), 9)
                catalog_scaler = scalers["catalog"]["spec"]["algorithm"]
                if algorithm == "TokenThroughput":
                    self.assertNotIn("serverTargetTokensPerSecond", catalog_scaler["tokenThroughput"])
                else:
                    self.assertNotIn("serverTokenCapacityPerReplica", catalog_scaler["predictive"])
                    self.assertNotIn("serverTargetTokensPerSecond", catalog_scaler["predictive"]["tokenThroughput"])
                self.assertEqual(catalog_scaler["capacityPrior"]["catalogRef"]["key"], "priors.yaml")
                frozen = scalers["frozen-manual"]["spec"]["algorithm"]
                tuned = scalers["tuned-manual"]["spec"]["algorithm"]
                if algorithm == "TokenThroughput":
                    self.assertEqual(frozen["tokenThroughput"]["serverTargetTokensPerSecond"], "20000")
                    self.assertEqual(tuned["tokenThroughput"]["serverTargetTokensPerSecond"], "400.0")
                else:
                    self.assertEqual(frozen["predictive"]["serverTokenCapacityPerReplica"], "20000")
                    self.assertEqual(tuned["predictive"]["serverTokenCapacityPerReplica"], "400.0")

    def test_predictive_inputs_carry_both_capacity_fields_on_manual_arms(self):
        with tempfile.TemporaryDirectory() as directory:
            options = Options()
            catalog_text = json.dumps(self.catalog_document())
            inputs = qualification.write_capacity_inputs(Path(directory) / "predictive", "qual-pred", "Predictive", options,
                                                         400.0, catalog_text, "priors.yaml")
            _, _, scalers, _, _ = rounds.validate(inputs)
            tuned = scalers["tuned-manual"]["spec"]["algorithm"]["predictive"]
            self.assertEqual(tuned["serverTokenCapacityPerReplica"], "400.0")
            self.assertEqual(tuned["tokenThroughput"]["serverTargetTokensPerSecond"], "400.0")
            catalog = scalers["catalog"]["spec"]["algorithm"]["predictive"]
            self.assertNotIn("serverTokenCapacityPerReplica", catalog)
            self.assertNotIn("serverTargetTokensPerSecond", catalog["tokenThroughput"])

    def test_capacity_arms_requires_the_sweep_catalog(self):
        with tempfile.TemporaryDirectory() as directory:
            records = qualification.suite_capacity_arms(FakeCluster(), Path(directory), Options())
            self.assertEqual(records[0]["outcome"], "failed")
            self.assertIn("--catalog-configmap", records[0]["error"])

    def test_pd_epd_commands_delegate_to_the_scenario_case(self):
        pd_only = qualification.pd_epd_commands("pd", Path("/out"))
        self.assertEqual(len(pd_only), 4)
        both = qualification.pd_epd_commands("both", Path("/out"))
        self.assertEqual(len(both), 5)
        for name, command in both:
            self.assertEqual(command[0], "/bin/bash")
            self.assertTrue(command[1].endswith("case-5-autoscaling.sh"))
            self.assertIn("--arm", command)
            self.assertIn("--serial", command)
        self.assertTrue(any("vllm-epd-predictive" in command for _, command in both))


if __name__ == "__main__":
    unittest.main()
