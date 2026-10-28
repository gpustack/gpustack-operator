#!/usr/bin/env python3
"""Hardware qualification for the ModelDeployment scaling chain on one 8x NVIDIA H200 SXM node.

The suites prepare the node (engine images, model weights, the bench driver), measure a
concurrency sweep into a valid priors.yaml catalog ConfigMap, run the frozen-manual versus
tuned-manual versus catalog capacity arms serially across TokenThroughput and Predictive, drive
Proportional, TokenThroughput and Predictive through a four-phase load wave with a two-second
sampler, exercise the embedded catalog with zero configuration, and delegate the P/D and E/P/D
legs to the scenario case. Every case is bounded: a case that fails or overruns its timeout dumps
a structured failure summary and the suite moves on to the next case.
"""

import argparse
import copy
import json
import os
import statistics
import subprocess
import sys
import time
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
CASES = SCRIPTS.parent / "cases"
sys.path.insert(0, str(SCRIPTS))

import autoscaling_rounds as rounds

HARDWARE = os.environ.get("BENCH_QUAL_HARDWARE", "nvidia-h200")
GPU_COUNT = 8
ENGINE_IMAGES = {
    "vllm": os.environ.get("BENCH_QUAL_IMAGE_VLLM", "gpustack/runner:cuda13.0-vllm0.30.0"),
    "sglang": os.environ.get("BENCH_QUAL_IMAGE_SGLANG", "gpustack/runner:cuda12.9-sglang0.5.18"),
}
MODELS = {
    "qwen3-8b": {
        "repository": "Qwen/Qwen3-8B",
        "revision": "b968826d9c46dd6066d109eabc6255188de91218",
        "quantization": "bf16",
        "allowPatterns": [
            "model-00001-of-00005.safetensors", "model-00002-of-00005.safetensors",
            "model-00003-of-00005.safetensors", "model-00004-of-00005.safetensors",
            "model-00005-of-00005.safetensors", "config.json", "generation_config.json",
            "merges.txt", "model.safetensors.index.json", "tokenizer.json",
            "tokenizer_config.json", "vocab.json",
        ],
    },
    "qwen2-5-vl-7b-instruct": {
        "repository": "Qwen/Qwen2.5-VL-7B-Instruct",
        "revision": "cc594898137f460bfe9f0759e9844b3ce807cfb5",
        "allowPatterns": [
            "model-00001-of-00005.safetensors", "model-00002-of-00005.safetensors",
            "model-00003-of-00005.safetensors", "model-00004-of-00005.safetensors",
            "model-00005-of-00005.safetensors", "chat_template.json", "config.json",
            "generation_config.json", "merges.txt", "model.safetensors.index.json",
            "preprocessor_config.json", "tokenizer.json", "tokenizer_config.json", "vocab.json",
        ],
    },
    "gpt-oss-120b": {
        "repository": "openai/gpt-oss-120b",
        "quantization": "fp4",
        "ignorePatterns": ["original/", "metal/"],
    },
}
SWEEP_MODEL = "Qwen/Qwen3-8B"
SWEEP_ENGINE = "vllm"
SWEEP_QUANTIZATION = "bf16"
SWEEP_CONCURRENCIES = (2, 4, 8, 16, 24, 32)
MODEL_STORE = "bench-models"
DRIVER = {
    "namespace": os.environ.get("BENCH_DRIVER_NS", "default"),
    "pod": os.environ.get("BENCH_DRIVER", "bench-driver"),
    "container": "driver",
}
MD_RESOURCE = "modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER_RESOURCE = "modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
SUITES = ("sweep", "capacity-arms", "sim-sensitivity", "builtin-catalog", "pd-epd")
SAMPLER_INTERVAL_SECONDS = 2.0
SIM_PHASES = (
    ("baseline", 60, 2),
    ("ramp", 90, 24),
    ("sustain", 90, 24),
    ("cooldown", 120, 0),
)
PD_ARMS = ("vllm-pd-proportional", "vllm-pd-predictive", "vllm-pd-token-throughput", "sglang-pd-token-throughput")
EPD_ARMS = ("vllm-epd-predictive",)


def namespace():
    return os.environ.get("BENCH_NS", "gpustack-bench")


def namespace_manifest(name=None):
    return {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {"name": name or namespace()},
    }


def instance_type():
    return os.environ.get("BENCH_QUAL_INSTANCE_TYPE", f"gpustack--{HARDWARE}-linux-amd64")


# --------------------------------------------------------------- manifest builders


def labels(name, component):
    return {
        "app.kubernetes.io/name": "gpustack-qualification",
        "app.kubernetes.io/instance": name,
        "app.kubernetes.io/component": component,
    }


def prefetch_daemonset(name, image):
    """A DaemonSet that parks one idle container of the image on every accelerator node.

    The Pod asks for no accelerator, so the pull is the only thing it costs; once every
    node reports the Pod Ready, the image is in the node's local store.
    """
    pod_labels = labels(name, "image-prefetch")
    return {
        "apiVersion": "apps/v1",
        "kind": "DaemonSet",
        "metadata": {"name": name, "namespace": namespace(), "labels": pod_labels},
        "spec": {
            "selector": {"matchLabels": pod_labels},
            "template": {
                "metadata": {"labels": pod_labels},
                "spec": {
                    "nodeSelector": {"feature.gpustack.ai/acceleratable": "true"},
                    "containers": [{
                        "name": "prefetch",
                        "image": image,
                        "imagePullPolicy": "IfNotPresent",
                        "command": ["sleep", "infinity"],
                    }],
                    "terminationGracePeriodSeconds": 0,
                },
            },
        },
    }


def model_artifact(name):
    model = MODELS[name]
    spec = {"source": {"huggingFace": {"repository": model["repository"]}}}
    if model.get("revision"):
        spec["source"]["huggingFace"]["revision"] = model["revision"]
    if model.get("allowPatterns"):
        spec["allowPatterns"] = list(model["allowPatterns"])
    if model.get("ignorePatterns"):
        spec["ignorePatterns"] = list(model["ignorePatterns"])
    return {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelArtifact",
        "metadata": {"name": name, "namespace": namespace()},
        "spec": spec,
    }


def model_prefetch(name, placement_type):
    model = MODELS[name]
    manifest = {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelPrefetch",
        "metadata": {"name": name, "namespace": namespace()},
        "spec": {
            "artifactRef": {"name": name},
            "bindingRef": {"name": MODEL_STORE},
            "placement": {"instanceTypes": [placement_type]},
            "minReady": 0,
        },
    }
    if model.get("quantization"):
        manifest["metadata"]["annotations"] = {"gpustack.ai/qualification-quantization": model["quantization"]}
    return manifest


def model_store():
    return {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelStore",
        "metadata": {"name": MODEL_STORE},
        "spec": {"nodeSelector": {"matchLabels": {"feature.gpustack.ai/acceleratable": "true"}}},
    }


def model_store_binding():
    return {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelStoreBinding",
        "metadata": {"name": MODEL_STORE, "namespace": namespace()},
        "spec": {
            "storeRefs": [{"name": MODEL_STORE}],
            "quota": {"bytes": "1600Gi"},
            "allowPinned": True,
        },
    }


def bench_driver_pod(image):
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": DRIVER["pod"], "namespace": DRIVER["namespace"], "labels": {"app.kubernetes.io/name": "bench-driver"}},
        "spec": {
            "affinity": {
                "nodeAffinity": {
                    "preferredDuringSchedulingIgnoredDuringExecution": [{
                        "weight": 100,
                        "preference": {
                            "matchExpressions": [{
                                "key": "feature.gpustack.ai/acceleratable",
                                "operator": "NotIn",
                                "values": ["true"],
                            }],
                        },
                    }],
                },
            },
            "containers": [{
                "name": DRIVER["container"],
                "image": image,
                "command": ["sh", "-c", "pip install --no-cache-dir aiohttp && exec sleep infinity"],
            }],
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 0,
        },
    }


def server_deployment(name, artifact, model_path, gpus_per_replica, image):
    """One Server role of Qwen3-8B (or the builtin model) at a given replica width."""
    role = {
        "name": "server",
        "kind": "Server",
        "replicas": 1,
        "size": 1,
        "instanceType": instance_type(),
        "image": image,
        "imagePullPolicy": "IfNotPresent",
        "resources": {"accelerator": str(gpus_per_replica)},
        "extraArgs": [
            "--tensor-parallel-size", str(gpus_per_replica),
            "--max-model-len", "16384",
            "--max-num-seqs", "64",
            "--gpu-memory-utilization", "0.90",
            # The scaler counts tokens through POST /v1/chat/completions/render, which
            # vLLM serves only under this flag; without it the failure is silent.
            "--enable-scale-out",
        ],
        "env": [{"name": "VLLM_HTTP_TIMEOUT_KEEP_ALIVE", "value": "3600"}],
    }
    metadata = {"name": name, "namespace": namespace()}
    quantization = MODELS.get(artifact, {}).get("quantization")
    if quantization:
        metadata["annotations"] = {"worker.gpustack.ai/precision": quantization}
    return {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelDeployment",
        "metadata": metadata,
        "spec": {
            "model": {"name": model_path, "artifactRef": {"name": artifact}},
            "engine": {"name": "vLLM", "image": image},
            "roles": [role],
        },
    }


def base_scaler(name, target_name, algorithm_spec, capacity_prior, max_replicas=4):
    if capacity_prior is not None:
        algorithm_spec = {**algorithm_spec, "capacityPrior": capacity_prior}
    return {
        "apiVersion": "worker.gpustack.ai/v1",
        "kind": "ModelDeploymentScaler",
        "metadata": {"name": name, "namespace": namespace()},
        "spec": {
            "targetRef": {"name": target_name},
            "mode": "Active",
            "roles": [{"name": "server", "minReplicas": 1, "maxReplicas": max_replicas}],
            "sampling": {"intervalSeconds": 5, "pendingTimeoutSeconds": 60},
            "behavior": {"scaleDown": {"stabilizationWindowSeconds": 60}},
            "algorithm": algorithm_spec,
        },
    }


def proportional_scaler(name, target_name, per_replica_running=12):
    return base_scaler(name, target_name, {
        "type": "Proportional",
        "proportional": {
            "metric": "RunningRequests",
            "roleTargets": [{"name": "server", "target": str(per_replica_running)}],
            "tolerance": "0.1",
        },
    }, None)


def token_throughput_spec(capacity, ttft_target_ms, itl_target_ms):
    spec = {
        "ttftTargetMilliseconds": str(ttft_target_ms),
        "itlTargetMilliseconds": str(itl_target_ms),
        "stableWindowSeconds": 30,
        "panicWindowSeconds": 10,
    }
    if capacity is not None:
        spec["serverTargetTokensPerSecond"] = str(capacity)
    return {"type": "TokenThroughput", "tokenThroughput": spec}


def predictive_spec(capacity, ttft_target_ms, itl_target_ms):
    spec = {
        "horizonSeconds": 300,
        "minPoints": 6,
        "tokenThroughput": token_throughput_spec(capacity, ttft_target_ms, itl_target_ms)["tokenThroughput"],
    }
    if capacity is not None:
        spec["serverTokenCapacityPerReplica"] = str(capacity)
    return {"type": "Predictive", "predictive": spec}


def catalog_configmap(name, document_text):
    return {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": name, "namespace": namespace()},
        "data": {"priors.yaml": document_text},
    }


# --------------------------------------------------------------- metrics


def percentile(values, pct):
    """Nearest-rank percentile over a sorted copy of values."""
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = max(1, min(len(ordered), int(round(pct / 100.0 * len(ordered) + 0.5))))
    return ordered[rank - 1]


def percentiles(values):
    if not values:
        return {}
    return {
        "p50": round(percentile(values, 50), 3),
        "p90": round(percentile(values, 90), 3),
        "p95": round(percentile(values, 95), 3),
        "p99": round(percentile(values, 99), 3),
    }


def tokens_per_second_per_user(record):
    """The per-request decode rate one user saw: output tokens over end-to-end seconds."""
    output, e2e = record.get("output_tokens"), record.get("e2e_ms")
    if not output or not e2e:
        return None
    return output / (e2e / 1000.0)


def true_slo_metrics(records, ttft_target_ms, itl_target_ms):
    """Request-level outcome of one load phase.

    True SLO compliance counts a request only when it completed inside both latency
    targets, and the denominator carries every request including the failed and the
    timed-out ones, so a phase that drops traffic cannot look compliant. Goodput is
    the compliant requests per wall-clock second of the same window.
    """
    good = [r for r in records if not r.get("error")]
    failed = [r for r in records if r.get("error") and r.get("error") != "timeout"]
    timed_out = [r for r in records if r.get("error") == "timeout"]
    compliant = [
        r for r in good
        if r.get("ttft_ms") is not None and r.get("tpot_ms") is not None
        and r["ttft_ms"] <= ttft_target_ms and r["tpot_ms"] <= itl_target_ms
    ]
    total = len(records)
    starts = [r["started_at"] for r in records if r.get("started_at") is not None]
    ends = [r["started_at"] + (r.get("e2e_ms") or 0) / 1000.0 for r in records if r.get("started_at") is not None]
    wall = (max(ends) - min(starts)) if starts and max(ends) > min(starts) else None
    output_tokens = sum(r.get("output_tokens") or 0 for r in good)
    prompt_tokens = sum(r.get("prompt_tokens") or 0 for r in good)
    rates = [tokens_per_second_per_user(r) for r in good]
    rates = [rate for rate in rates if rate is not None]
    return {
        "requests": total,
        "completed": len(good),
        "failed": len(failed),
        "timedOut": len(timed_out),
        "compliant": len(compliant),
        "trueSloCompliance": round(len(compliant) / total, 4) if total else None,
        "goodputPerSecond": round(len(compliant) / wall, 3) if wall else None,
        "wallSeconds": round(wall, 3) if wall else None,
        "ttftMilliseconds": percentiles([r["ttft_ms"] for r in good if r.get("ttft_ms") is not None]),
        "itlMilliseconds": percentiles([g for r in good for g in r.get("itl_ms", [])]),
        "tpotMilliseconds": percentiles([r["tpot_ms"] for r in good if r.get("tpot_ms") is not None]),
        "tokensPerSecondPerUser": round(statistics.fmean(rates), 3) if rates else None,
        "outputTokensPerSecond": round(output_tokens / wall, 3) if wall else None,
        "totalTokensPerSecond": round((output_tokens + prompt_tokens) / wall, 3) if wall else None,
    }


def quantity_value(value):
    """Read the plain-number Quantity strings the scaler status carries."""
    if value is None or isinstance(value, (int, float)):
        return value
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def processed_tokens_per_second(role):
    prompt = quantity_value(role.get("promptTokensPerSecond"))
    generation = quantity_value(role.get("generationTokensPerSecond"))
    if prompt is not None and generation is not None:
        return round(prompt + generation, 3)
    return prompt if prompt is not None else generation


def sample_row(cluster, name, phase, algorithm):
    """One controller-plus-deployment snapshot for the two-second sampler."""
    row = {"ts": rounds.stamp(), "phase": phase, "algorithm": algorithm, "roles": {}, "deployment": None, "resolvedCapacityPriors": {}}
    scaler = cluster.get_json(SCALER_RESOURCE, name, namespace())
    deployment = cluster.get_json(MD_RESOURCE, name, namespace())
    for role in scaler.get("status", {}).get("roles", []):
        row["roles"][role.get("name")] = {
            "rawRecommendedReplicas": role.get("rawRecommendedReplicas"),
            "desiredReplicas": role.get("constrainedReplicas"),
            "readyReplicas": role.get("readyReplicas"),
            "signalState": role.get("signalState"),
            "processedTokensPerSecond": processed_tokens_per_second(role),
            "runningRequests": role.get("runningRequests"),
            "waitingRequests": role.get("waitingRequests"),
            "ttftMilliseconds": quantity_value(role.get("ttftMilliseconds")),
            "itlMilliseconds": quantity_value(role.get("itlMilliseconds")),
            "reason": role.get("reason"),
        }
    row["deployment"] = {
        "roles": {
            role.get("name"): {"desired": role.get("desired"), "ready": role.get("ready")}
            for role in deployment.get("status", {}).get("roles", [])
        },
        "retirement": deployment.get("status", {}).get("retirement"),
    }
    for prior in scaler.get("status", {}).get("resolvedCapacityPriors", []):
        row["resolvedCapacityPriors"][prior.get("name")] = {
            "seedTokensPerSecond": prior.get("seedTokensPerSecond"),
            "headroom": prior.get("headroom"),
            "metric": prior.get("metric"),
            "quantile": prior.get("quantile"),
            "requestedQuantile": prior.get("requestedQuantile"),
            "effectiveTokensPerSecond": {
                field.get("field"): field.get("effectiveTokensPerSecond")
                for field in prior.get("fields", [])
            },
            "latencyBounds": prior.get("latencyBounds", []),
            "boundaryReason": None,
        }
    for role_name, prior in row["resolvedCapacityPriors"].items():
        prior["boundaryReason"] = row["roles"].get(role_name, {}).get("reason")
    return row


# --------------------------------------------------------------- cluster access


class ClusterError(Exception):
    """A cluster interaction failed, with the kubectl diagnostic retained."""


class Cluster:
    """Thin kubectl wrapper; every call goes through one override point."""

    def __init__(self, context=None):
        self.context = context

    def command(self, args, namespace=None):
        command = ["kubectl"]
        if self.context:
            command += ["--context", self.context]
        if namespace:
            command += ["-n", namespace]
        return command + list(args)

    def run(self, args, namespace=None, data=None, timeout=120):
        stdin_kw = {"input": data} if data is not None else {"stdin": subprocess.DEVNULL}
        last_error = None
        for attempt in range(6):
            try:
                result = subprocess.run(
                    self.command(args, namespace), text=True,
                    capture_output=True, timeout=timeout, **stdin_kw,
                )
            except (OSError, subprocess.TimeoutExpired) as error:
                raise ClusterError(f"kubectl {args[0]} failed: {error}") from error
            if result.returncode == 0:
                return result.stdout
            stderr = result.stderr.strip()
            last_error = ClusterError(f"kubectl {args[0]} failed ({result.returncode}): {stderr}")
            if "unexpected EOF" not in stderr and "error reading from error stream" not in stderr:
                raise last_error
            time.sleep(0.5 * (attempt + 1))
        raise last_error

    def apply(self, manifest):
        return self.run(["apply", "-f", "-", "-o", "name"], data=json.dumps(manifest))

    def get_json(self, resource, name=None, namespace=None):
        args = ["get", resource] + ([name] if name else []) + ["-o", "json"]
        text = self.run(args, namespace)
        return json.loads(text)

    def get_json_or_none(self, resource, name, namespace=None):
        try:
            return self.get_json(resource, name, namespace)
        except ClusterError as error:
            if "NotFound" in str(error) or "not found" in str(error):
                return None
            raise

    def delete(self, resource, name, namespace=None, timeout=300):
        self.run(["delete", resource, name, "--ignore-not-found", "--wait=true", f"--timeout={int(timeout)}s"], namespace)

    def logs(self, pod, namespace, tail=200):
        return self.run(["logs", pod, f"--tail={tail}"], namespace)

    def exec_driver(self, root, action, *args, data=None, timeout=120):
        exec_args = ["exec"] + (["-i"] if data is not None else []) + [
            DRIVER["pod"], "-c", DRIVER["container"], "--",
            "python3", "-c", rounds.REMOTE, action, root, *args,
        ]
        return self.run(
            exec_args,
            DRIVER["namespace"], data=data, timeout=timeout,
        )


def condition(conditions, kind):
    for condition in conditions or []:
        if condition.get("type") == kind:
            return condition
    return None


def poll(deadline, interval, read, label, out=None, sink=None):
    """Poll read() until it returns truthy or the deadline passes."""
    while True:
        value = read()
        if value:
            return value
        if sink is not None:
            sink(out)
        if time.monotonic() >= deadline:
            raise ClusterError(f"timeout waiting for {label}")
        time.sleep(interval)


def wait_interval(timeout):
    """Poll as fast as the bound allows, so a short bound cannot waste its budget sleeping."""
    return min(5.0, max(0.05, timeout / 10.0))


def wait_daemonset_ready(cluster, name, timeout):
    deadline = time.monotonic() + timeout
    return poll(deadline, wait_interval(timeout), lambda: _daemonset_ready(cluster, name), f"DaemonSet {name} ready")


def _daemonset_ready(cluster, name):
    daemonset = cluster.get_json("daemonsets", name, namespace())
    status = daemonset.get("status", {})
    desired = status.get("desiredNumberScheduled") or 0
    ready = status.get("numberReady") or 0
    return desired > 0 and ready >= desired


def wait_prefetch_ready(cluster, name, timeout):
    deadline = time.monotonic() + timeout
    return poll(deadline, wait_interval(timeout), lambda: _prefetch_ready(cluster, name), f"ModelPrefetch {name} ready")


def _prefetch_ready(cluster, name):
    prefetch = cluster.get_json("modelprefetches.worker.gpustack.ai", name, namespace())
    available = condition(prefetch.get("status", {}).get("conditions"), "Available")
    return available is not None and available.get("status") == "True"


def ensure_driver(cluster, timeout):
    """The load only runs in-cluster, so the driver Pod is a precondition, not a nicety."""
    pod = cluster.get_json_or_none("pod", DRIVER["pod"], DRIVER["namespace"])
    if not pod or not pod.get("metadata", {}).get("uid"):
        cluster.apply(bench_driver_pod("docker.io/library/python:3.11-slim"))
    deadline = time.monotonic() + timeout
    poll(deadline, wait_interval(timeout), lambda: _pod_ready(cluster, DRIVER["pod"], DRIVER["namespace"]), f"driver Pod {DRIVER['pod']} ready")
    cluster.run(["exec", DRIVER["pod"], "--", "python3", "-c", "import aiohttp"], DRIVER["namespace"])
    return {"namespace": DRIVER["namespace"], "pod": DRIVER["pod"], "ready": True}


def _pod_ready(cluster, pod_name, pod_namespace):
    pod = cluster.get_json("pod", pod_name, pod_namespace)
    ready = condition(pod.get("status", {}).get("conditions"), "Ready")
    return ready is not None and ready.get("status") == "True"


def wait_deployment_ready(cluster, name, timeout, expect_capacity_priors=False):
    """Wait for the exact baseline: role ready and, for a catalog arm, priors resolved."""
    deadline = time.monotonic() + timeout

    def ready():
        deployment = cluster.get_json(MD_RESOURCE, name, namespace())
        roles = deployment.get("status", {}).get("roles", [])
        base = roles and all(role.get("desired") and role.get("ready") == role.get("desired") for role in roles)
        if not base:
            return None
        if not expect_capacity_priors:
            return True
        scaler = cluster.get_json(SCALER_RESOURCE, name, namespace())
        conditions = scaler.get("status", {}).get("conditions", [])
        priors_ready = condition(conditions, "CapacityPriorsReady")
        resolved = scaler.get("status", {}).get("resolvedCapacityPriors") or []
        catalog_fields = any(
            field.get("source") == "Catalog"
            for prior in resolved
            for field in prior.get("fields", [])
        )
        return priors_ready is not None and priors_ready.get("status") == "True" and catalog_fields

    return poll(deadline, wait_interval(timeout), ready, f"deployment {name} ready")


def serving_endpoint(cluster, name):
    """Resolve the Service that fronts the server role and prove the target owns it."""
    service_name = f"{name}-server"
    service = cluster.get_json("service", service_name, namespace())
    labels_map = service.get("metadata", {}).get("labels", {})
    if labels_map.get("app.kubernetes.io/instance") != name:
        raise ClusterError(f"serving Service {service_name} does not belong to {name}")
    ports = service.get("spec", {}).get("ports", [])
    if len(ports) != 1 or not ports[0].get("port"):
        raise ClusterError(f"serving Service {service_name} must publish exactly one port")
    return service_name, ports[0]["port"]


# --------------------------------------------------------------- bounded case runner


def dump_failure(cluster, case, out_dir, message):
    """One structured failure summary: pod logs, events and scaler status, best effort."""
    summary = {"case": case, "capturedAt": rounds.stamp(), "error": message, "pods": None, "podLogs": {}, "events": None, "scalers": None}
    readers = (
        ("pods", lambda: cluster.get_json("pods", namespace=namespace())),
        ("events", lambda: cluster.get_json("events", namespace=namespace())),
        ("scalers", lambda: cluster.get_json(SCALER_RESOURCE, namespace=namespace())),
    )
    if cluster is not None:
        for key, reader in readers:
            try:
                summary[key] = reader()
            except (ClusterError, ValueError) as error:
                summary[key] = f"unavailable: {error}"
        pods = summary["pods"] if isinstance(summary["pods"], dict) else {}
        for pod in pods.get("items", []):
            pod_name = pod.get("metadata", {}).get("name")
            phase = pod.get("status", {}).get("phase")
            if not pod_name or phase == "Pending":
                continue
            try:
                summary["podLogs"][pod_name] = cluster.logs(pod_name, namespace())
            except (ClusterError, ValueError) as error:
                summary["podLogs"][pod_name] = f"unavailable: {error}"
    rounds.write_json(Path(out_dir) / "failure_summary.json", summary)


def run_case(name, command, out_dir, timeout, cluster=None, cwd=None):
    """Run one bounded case; a failure or an overrun is dumped and returned, never hung on."""
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    record = {"case": name, "command": [str(part) for part in command], "startedAt": rounds.stamp(), "timeoutSeconds": timeout}
    started = time.monotonic()
    try:
        result = subprocess.run(
            [str(part) for part in command], cwd=cwd, text=True,
            capture_output=True, timeout=timeout, stdin=subprocess.DEVNULL,
        )
        record.update(
            finishedAt=rounds.stamp(), exitCode=result.returncode,
            durationSeconds=round(time.monotonic() - started, 3),
            stdoutTail=result.stdout[-4000:], stderrTail=result.stderr[-4000:],
            outcome="succeeded" if result.returncode == 0 else "failed",
        )
        if result.returncode != 0:
            record["error"] = f"case {name} exited {result.returncode}"
            dump_failure(cluster, name, out_dir, record["error"])
    except subprocess.TimeoutExpired as error:
        record.update(
            finishedAt=rounds.stamp(), exitCode=None,
            durationSeconds=round(time.monotonic() - started, 3),
            stdoutTail=(error.stdout or b"")[-4000:] if isinstance(error.stdout, bytes) else (error.stdout or "")[-4000:],
            stderrTail=(error.stderr or b"")[-4000:] if isinstance(error.stderr, bytes) else (error.stderr or "")[-4000:],
            outcome="timed-out", error=f"case {name} exceeded its {timeout}s bound",
        )
        dump_failure(cluster, name, out_dir, record["error"])
    except OSError as error:
        record.update(finishedAt=rounds.stamp(), exitCode=None, outcome="failed", error=str(error))
        dump_failure(cluster, name, out_dir, str(error))
    rounds.write_json(out_dir / "case.json", record)
    return record


# --------------------------------------------------------------- driver load


def stage_driver(cluster, root):
    cluster.exec_driver(root, "stage", data=json.dumps({
        "bench_client.py": (SCRIPTS / "bench_client.py").read_text(),
        "adapter.py": rounds.ADAPTER,
    }))


def run_load_phase(cluster, root, endpoint, workload, phase, concurrency, duration, out_dir, seed, sampler=None):
    """One load phase driven inside the bench driver, with the sampler beside it."""
    arguments = [
        "load", "--endpoint", endpoint, "--model", workload["model"],
        "--input-tokens", str(workload["inputTokens"]), "--output-tokens", str(workload["outputTokens"]),
        "--token-offset", "0", "--concurrency", str(concurrency), "--duration", str(duration),
        "--timeout", str(workload["requestTimeoutSeconds"]),
        "--out", f"{root}/{phase}.json", "--requests-out", f"{root}/{phase}_reqs.jsonl",
    ]
    log_path = Path(out_dir) / f"{phase}.log"
    with log_path.open("w") as log:
        process = subprocess.Popen(
            cluster.command(
                ["exec", DRIVER["pod"], "-c", DRIVER["container"], "--",
                 "python3", "-c", rounds.REMOTE, "run", root, phase, str(seed), *arguments],
                DRIVER["namespace"],
            ),
            stdout=subprocess.DEVNULL, stderr=log, stdin=subprocess.DEVNULL, text=True,
        )
        try:
            while process.poll() is None:
                if sampler is not None:
                    sampler()
                time.sleep(SAMPLER_INTERVAL_SECONDS)
            process.wait()
            if process.returncode != 0:
                stderr = log_path.read_text() if log_path.exists() else ""
                raise ClusterError(f"load phase {phase} failed ({process.returncode}): {stderr.strip()[-2000:]}")
            for artifact in (f"{phase}_reqs.jsonl", f"{phase}.json"):
                (Path(out_dir) / artifact).write_text(cluster.exec_driver(root, "read", artifact))
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    process.kill()


def read_request_records(out_dir, phase):
    """Parse one phase's per-request records; a malformed line is dropped, not fatal."""
    records = []
    path = Path(out_dir) / f"{phase}_reqs.jsonl"
    for line in path.read_text().splitlines():
        if not line.strip():
            continue
        try:
            records.append(json.loads(line))
        except ValueError:
            continue
    return records


# --------------------------------------------------------------- sweep


def sweep_row(records, wall_seconds):
    """Aggregate one sweep concurrency into a catalog-shaped measurement row."""
    good = [r for r in records if not r.get("error")]
    output_tokens = sum(r.get("output_tokens") or 0 for r in good)
    prompt_tokens = sum(r.get("prompt_tokens") or 0 for r in good)
    wall = wall_seconds if wall_seconds > 0 else None
    user_rates = [tokens_per_second_per_user(r) for r in good]
    user_rates = [rate for rate in user_rates if rate is not None]
    return {
        "requests": len(records),
        "completed": len(good),
        "errors": len(records) - len(good),
        "totalTokensPerSecond": round((output_tokens + prompt_tokens) / wall, 3) if wall else None,
        "inputTokensPerSecond": round(prompt_tokens / wall, 3) if wall else None,
        "outputTokensPerSecond": round(output_tokens / wall, 3) if wall else None,
        "ttftMilliseconds": percentiles([r["ttft_ms"] for r in good if r.get("ttft_ms") is not None]),
        "itlMilliseconds": percentiles([g for r in good for g in r.get("itl_ms", [])]),
        "tokensPerSecondPerUser": round(statistics.fmean(user_rates), 3) if user_rates else None,
    }


def sweep_catalog_document(rows, model=SWEEP_MODEL, engine=SWEEP_ENGINE, hardware=HARDWARE,
                           quantization=SWEEP_QUANTIZATION, gpus_per_replica=1):
    """The measured sweep as one flat 6-tuple profile: role, model, quantization, engine,
    hardware, topology widths and the measured points."""
    points = []
    for concurrency in sorted(rows):
        row = rows[concurrency]
        points.append({
            "concurrency": concurrency,
            "totalTokensPerSecond": row["totalTokensPerSecond"],
            "inputTokensPerSecond": row["inputTokensPerSecond"],
            "outputTokensPerSecond": row["outputTokensPerSecond"],
            "ttftMilliseconds": row["ttftMilliseconds"],
            "itlMilliseconds": row["itlMilliseconds"],
        })
    return {
        "apiVersion": rounds.CATALOG_API_VERSION,
        "kind": rounds.CATALOG_KIND,
        "profiles": [{
            "role": "Server",
            "model": model,
            "quantization": quantization,
            "engine": engine,
            "hardware": hardware,
            "topology": {"gpusPerReplica": gpus_per_replica, "tensorParallel": gpus_per_replica},
            "points": points,
        }],
    }


def point_within_targets(point, ttft_target_ms, itl_target_ms):
    """A point is feasible when its measured latencies sit inside both targets.

    A quantile the source did not publish cannot fail the point, so the highest
    published quantile is compared and a point without any quantile counts as
    feasible on the throughput evidence alone.
    """
    for field, target in (("ttftMilliseconds", ttft_target_ms), ("itlMilliseconds", itl_target_ms)):
        quantiles = point.get(field) or {}
        values = [value for value in quantiles.values() if isinstance(value, (int, float))]
        if values and max(values) > target:
            return False
    return True


def catalog_capacity_for(document, model, engine, hardware, quantization, gpus_per_replica,
                         ttft_target_ms, itl_target_ms, slot_gpus):
    """The tuned manual capacity for one slot width, read out of the measured catalog.

    The highest per-replica throughput whose latencies still meet the targets is the
    seed; the slot scales it by its own GPU width, which is exactly the linear
    estimate the comparison is trying to beat.
    """
    for profile in document.get("profiles", []):
        identity = (
            profile.get("model", "").lower() == model.lower(),
            profile.get("engine", "").lower() == engine.lower(),
            profile.get("hardware", "").lower() == hardware.lower(),
            profile.get("quantization", "").lower() == quantization.lower(),
        )
        topology = profile.get("topology", {})
        if not all(identity) or topology.get("gpusPerReplica") != gpus_per_replica:
            continue
        points = profile.get("points", [])
        feasible = [point for point in points if point_within_targets(point, ttft_target_ms, itl_target_ms)]
        pool = feasible or points
        if not pool:
            continue
        best = max(point["totalTokensPerSecond"] for point in pool)
        return round(best * slot_gpus / gpus_per_replica, 3)
    raise ClusterError(
        f"the catalog carries no {model}/{quantization}/{engine}/{hardware} profile at "
        f"gpusPerReplica {gpus_per_replica}; pass --tuned-capacity explicitly"
    )


def sweep_summary_markdown(rows, tuned_capacity, ttft_target_ms, itl_target_ms):
    header = (
        "| concurrency | requests | errors | total tok/s | output tok/s | ttft p50/p90/p99 ms "
        "| itl p50/p90/p99 ms | toks/s/user |\n"
        "|---|---|---|---|---|---|---|---|\n"
    )
    lines = [header]
    for concurrency in sorted(rows):
        row = rows[concurrency]
        ttft = row["ttftMilliseconds"]
        itl = row["itlMilliseconds"]

        def triple(quantiles):
            return "/".join(str(quantiles.get(name, "-")) for name in ("p50", "p90", "p99"))

        lines.append(
            f"| {concurrency} | {row['requests']} | {row['errors']} | {row['totalTokensPerSecond']} "
            f"| {row['outputTokensPerSecond']} | {triple(ttft)} | {triple(itl)} "
            f"| {row['tokensPerSecondPerUser']} |\n"
        )
    lines.append(
        f"\nTuned manual capacity under TTFT<={ttft_target_ms}ms / ITL<={itl_target_ms}ms: "
        f"{tuned_capacity} tokens/s per slot.\n"
    )
    return "".join(lines)


def suite_sweep(cluster, out_dir, options):
    """One replica of Qwen3-8B swept across the concurrency ladder, emitted as a catalog."""
    directory = Path(out_dir) / "sweep"
    directory.mkdir(parents=True, exist_ok=True)
    name = "qual-sweep"
    workload = {
        "model": options.model_path,
        "inputTokens": options.input_tokens,
        "outputTokens": options.output_tokens,
        "requestTimeoutSeconds": options.request_timeout_seconds,
    }
    deployment = server_deployment(name, "qwen3-8b", options.model_path, 1, ENGINE_IMAGES["vllm"])
    cluster.apply(deployment)
    rows = {}
    record = {"suite": "sweep", "model": SWEEP_MODEL, "engine": SWEEP_ENGINE, "hardware": HARDWARE,
              "quantization": SWEEP_QUANTIZATION, "topology": {"gpusPerReplica": 1, "tensorParallel": 1},
              "concurrencies": list(SWEEP_CONCURRENCIES), "startedAt": rounds.stamp()}
    root = None
    try:
        ensure_driver(cluster, options.case_timeout)
        wait_deployment_ready(cluster, name, options.case_timeout)
        service, port = serving_endpoint(cluster, name)
        endpoint = f"http://{service}.{namespace()}.svc:{port}"
        root = f"/tmp/gpustack-autoscaling-qual-{os.getpid()}"
        stage_driver(cluster, root)
        for concurrency in SWEEP_CONCURRENCIES:
            phase = f"c{concurrency}"
            run_load_phase(cluster, root, endpoint, workload, phase, concurrency,
                           options.phase_seconds, directory, options.seed)
            records = read_request_records(directory, phase)
            aggregate = json.loads((directory / f"{phase}.json").read_text())
            rows[concurrency] = sweep_row(records, aggregate.get("wall_seconds") or options.phase_seconds)
        document = sweep_catalog_document(rows)
        rounds.validate_catalog_document(document)
        text = rounds.dump_catalog_document(document)
        (directory / "priors.yaml").write_text(text)
        rounds.write_json(directory / "priors-configmap.json", catalog_configmap("qual-sweep-priors", text))
        tuned = catalog_capacity_for(document, SWEEP_MODEL, SWEEP_ENGINE, HARDWARE, SWEEP_QUANTIZATION,
                                     1, float(options.ttft_target_ms), float(options.itl_target_ms), 1)
        (directory / "summary.md").write_text(sweep_summary_markdown(rows, tuned, options.ttft_target_ms, options.itl_target_ms))
        record.update(rows={str(concurrency): rows[concurrency] for concurrency in sorted(rows)},
                      tunedCapacityPerReplica=tuned, outcome="succeeded")
    except (ClusterError, rounds.HarnessError, OSError, ValueError, KeyError) as error:
        record.update(outcome="failed", error=str(error))
        dump_failure(cluster, "sweep", directory, str(error))
    finally:
        if root:
            try:
                cluster.exec_driver(root, "remove")
            except (ClusterError, OSError):
                pass
        cluster.delete(MD_RESOURCE, name, namespace())
    rounds.write_json(directory / "sweep.json", record)
    return [record]


# --------------------------------------------------------------- capacity arms


def write_capacity_inputs(directory, prefix, algorithm, options, tuned_capacity, catalog_text, catalog_key):
    """The frozen/tuned/catalog input set of one rounds comparison, written to a directory."""
    directory.mkdir(parents=True, exist_ok=True)
    gpus = options.slot_gpus
    deployment = server_deployment(f"{prefix}-arms", "qwen3-8b", options.model_path, gpus, ENGINE_IMAGES["vllm"])
    throughput = {
        "serverTargetTokensPerSecond": options.frozen_capacity,
        "ttftTargetMilliseconds": str(options.ttft_target_ms),
        "itlTargetMilliseconds": str(options.itl_target_ms),
        "stableWindowSeconds": 30,
        "panicWindowSeconds": 10,
    }
    if algorithm == "TokenThroughput":
        algorithm_spec = {"type": "TokenThroughput", "tokenThroughput": copy.deepcopy(throughput)}
    else:
        algorithm_spec = {
            "type": "Predictive",
            "predictive": {
                "serverTokenCapacityPerReplica": options.frozen_capacity,
                "horizonSeconds": 300,
                "minPoints": 6,
                "tokenThroughput": copy.deepcopy(throughput),
            },
        }

    def scaler_document(capacity, prior):
        spec = copy.deepcopy(algorithm_spec)
        if capacity is None:
            if algorithm == "TokenThroughput":
                spec["tokenThroughput"].pop("serverTargetTokensPerSecond", None)
            else:
                spec["predictive"].pop("serverTokenCapacityPerReplica", None)
                spec["predictive"]["tokenThroughput"].pop("serverTargetTokensPerSecond", None)
        else:
            if algorithm == "TokenThroughput":
                spec["tokenThroughput"]["serverTargetTokensPerSecond"] = str(capacity)
            else:
                spec["predictive"]["serverTokenCapacityPerReplica"] = str(capacity)
                spec["predictive"]["tokenThroughput"]["serverTargetTokensPerSecond"] = str(capacity)
        if prior is not None:
            spec["capacityPrior"] = prior
        return {
            "apiVersion": "worker.gpustack.ai/v1",
            "kind": "ModelDeploymentScaler",
            "metadata": {"name": prefix, "namespace": namespace()},
            "spec": {
                "targetRef": {"name": prefix},
                "mode": "Active",
                "roles": [{"name": "server", "minReplicas": 1, "maxReplicas": options.max_replicas}],
                "sampling": {"intervalSeconds": 5, "pendingTimeoutSeconds": 60},
                "behavior": {"scaleDown": {"stabilizationWindowSeconds": 30}},
                "algorithm": spec,
            },
        }

    frozen = scaler_document(options.frozen_capacity, None)
    tuned = scaler_document(tuned_capacity, None)
    catalog_scaler = scaler_document(None, {"catalogRef": {"name": f"{prefix}-priors", "key": catalog_key}, "headroom": "0.75"})
    catalog_json = json.dumps(json.loads(catalog_text)) if catalog_text.lstrip().startswith("{") else None
    if catalog_json is None:
        import yaml
        catalog_json = json.dumps(yaml.safe_load(catalog_text))
    catalog = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": f"{prefix}-priors", "namespace": namespace()},
        "data": {catalog_key: catalog_json},
    }
    rounds.write_json(directory / "deployment.json", deployment)
    rounds.write_json(directory / "frozen.json", frozen)
    rounds.write_json(directory / "tuned.json", tuned)
    rounds.write_json(directory / "catalog-scaler.json", catalog_scaler)
    rounds.write_json(directory / "catalog.json", catalog)
    phases = [
        {"name": "baseline", "durationSeconds": 30, "concurrency": 2},
        {"name": "ramp", "durationSeconds": 45, "concurrency": 24},
        {"name": "sustain", "durationSeconds": 45, "concurrency": 24},
        {"name": "cooldown", "durationSeconds": 30, "concurrency": 0},
    ]
    config = {
        "schemaVersion": "1",
        "context": options.context,
        "namespace": namespace(),
        "namePrefix": prefix,
        "deploymentFile": "deployment.json",
        "scalerFiles": {"frozen-manual": "frozen.json", "tuned-manual": "tuned.json", "catalog": "catalog-scaler.json"},
        "catalogFile": "catalog.json",
        "fitRunIDs": ["sweep-" + str(options.seed)],
        "evaluationTraceID": f"arms-{algorithm}-{options.seed}",
        "clusterQueues": options.cluster_queues.split(","),
        "repeats": options.repeats,
        "order": "alternating",
        "seed": options.seed,
        "startCondition": "operator-declared-warm-cache/fresh-pods",
        "driver": {"namespace": DRIVER["namespace"], "pod": DRIVER["pod"], "container": DRIVER["container"]},
        "workload": {
            "model": options.model_path,
            "inputTokens": options.input_tokens,
            "outputTokens": options.output_tokens,
            "tokenOffset": 0,
            "requestTimeoutSeconds": options.request_timeout_seconds,
            "phases": phases,
        },
        "sampleIntervalSeconds": 2,
        "operationTimeoutSeconds": 120,
        "readinessTimeoutSeconds": 600,
        "cleanupTimeoutSeconds": 300,
        "startupAllowanceSeconds": 0,
        "retirementAllowanceSeconds": 0,
    }
    rounds.write_json(directory / "comparison.json", config)
    return directory / "comparison.json"


def suite_capacity_arms(cluster, out_dir, options):
    """Frozen-manual versus tuned-manual versus catalog, run serially per algorithm.

    The sweep catalog names the truth on this node, so the tuned arm reads its capacity
    from the measured profile and the catalog arm resolves with none stated at all. The
    TokenThroughput and Predictive comparisons run serially on the 4-GPU slot.
    """
    directory = Path(out_dir) / "capacity-arms"
    directory.mkdir(parents=True, exist_ok=True)
    if not options.catalog_configmap or not options.catalog_configmap.exists():
        record = {"suite": "capacity-arms", "outcome": "failed",
                  "error": "--catalog-configmap with the sweep's priors-configmap.json is required"}
        rounds.write_json(directory / "capacity-arms.json", record)
        return [record]
    configmap = json.loads(options.catalog_configmap.read_text())
    catalog_text = configmap["data"]["priors.yaml"]
    document = rounds.validate_catalog_document(rounds.load_catalog_document(catalog_text))
    if options.tuned_capacity:
        tuned = float(options.tuned_capacity)
    else:
        tuned = catalog_capacity_for(document, SWEEP_MODEL, SWEEP_ENGINE, HARDWARE, SWEEP_QUANTIZATION,
                                     1, float(options.ttft_target_ms), float(options.itl_target_ms), options.slot_gpus)
    for engine in ENGINE_IMAGES:
        try:
            cluster.delete("daemonsets", f"image-prefetch-{engine}", namespace())
        except (ClusterError, OSError):
            pass
    for model_name in MODELS:
        try:
            wait_prefetch_ready(cluster, model_name, options.case_timeout)
            cluster.delete("modelprefetches.worker.gpustack.ai", model_name, namespace())
        except (ClusterError, OSError):
            pass
    records = []
    for algorithm in ("TokenThroughput", "Predictive"):
        prefix = "qual-tt" if algorithm == "TokenThroughput" else "qual-pred"
        case_dir = directory / algorithm.lower()
        inputs = write_capacity_inputs(case_dir, prefix, algorithm, options, tuned, catalog_text, "priors.yaml")
        command = ["bash", str(SCRIPTS / "autoscaling_rounds.sh"),
                   "--config", str(inputs), "--out", str(case_dir / "output"), "--execute"]
        record = {"suite": "capacity-arms", "algorithm": algorithm, "command": command, "startedAt": rounds.stamp()}
        exit_code = None
        process = subprocess.Popen(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            stdout, stderr = process.communicate(timeout=options.case_timeout * 12)
            exit_code = process.returncode
        except subprocess.TimeoutExpired:
            process.kill()
            stdout, stderr = process.communicate()
            record["error"] = f"{algorithm} capacity-arms comparison exceeded its bound"
        record.update(
            finishedAt=rounds.stamp(), exitCode=exit_code,
            outcome="succeeded" if exit_code == 0 else "failed",
            stdoutTail=stdout[-4000:] if stdout else "", stderrTail=stderr[-4000:] if stderr else "",
        )
        if exit_code != 0:
            record.setdefault("error", f"{algorithm} capacity-arms comparison exited {exit_code}")
            dump_failure(cluster, f"capacity-arms-{algorithm}", case_dir, record["error"])
        records.append(record)
    rounds.write_json(directory / "capacity-arms.json", {"suite": "capacity-arms", "tunedCapacityPerSlot": tuned, "cases": records})
    return records


# --------------------------------------------------------------- sim sensitivity


def suite_sim_sensitivity(cluster, out_dir, options):
    """Proportional, TokenThroughput and Predictive through one four-phase load wave each.

    Every arm is sampled every two seconds into timeline.jsonl - what the algorithm asked
    for, what the deployment became, what the engine reported, and, for a catalog arm,
    the resolved basis and the boundary the resolution stopped at - and every request is
    scored against the true SLO so the arms compare directly against
    docs/modules/model-deployment/scaling-simulation.md.
    """
    directory = Path(out_dir) / "sim-sensitivity"
    directory.mkdir(parents=True, exist_ok=True)
    catalog_ref = None
    applied_catalog = None
    if options.catalog_configmap and options.catalog_configmap.exists():
        manifest = json.loads(options.catalog_configmap.read_text())
        cluster.apply(manifest)
        applied_catalog = manifest["metadata"]["name"]
        catalog_ref = {"name": manifest["metadata"]["name"], "key": "priors.yaml"}
    workload = {
        "model": options.model_path,
        "inputTokens": options.input_tokens,
        "outputTokens": options.output_tokens,
        "requestTimeoutSeconds": options.request_timeout_seconds,
    }
    arms = {}
    records = []
    for algorithm in ("Proportional", "TokenThroughput", "Predictive"):
        name = f"qual-sim-{algorithm.lower()}"
        case_dir = directory / algorithm.lower()
        case_dir.mkdir(parents=True, exist_ok=True)
        record = {"suite": "sim-sensitivity", "algorithm": algorithm, "startedAt": rounds.stamp()}
        deployment = server_deployment(name, "qwen3-8b", options.model_path, 1, ENGINE_IMAGES["vllm"])
        if algorithm == "Proportional":
            scaler = proportional_scaler(name, name)
        elif algorithm == "TokenThroughput":
            scaler = base_scaler(name, name, token_throughput_spec(options.sim_capacity, options.ttft_target_ms, options.itl_target_ms), None)
        else:
            prior = {"headroom": "0.75"}
            if catalog_ref:
                prior["catalogRef"] = catalog_ref
            scaler = base_scaler(name, name, predictive_spec(None, options.ttft_target_ms, options.itl_target_ms), prior)
        root = None
        try:
            ensure_driver(cluster, options.case_timeout)
            cluster.apply(deployment)
            cluster.apply(scaler)
            expect_priors = algorithm == "Predictive" and catalog_ref is not None
            wait_deployment_ready(cluster, name, options.case_timeout, expect_capacity_priors=expect_priors)
            service, port = serving_endpoint(cluster, name)
            endpoint = f"http://{service}.{namespace()}.svc:{port}"
            root = f"/tmp/gpustack-autoscaling-qual-sim-{os.getpid()}"
            stage_driver(cluster, root)
            timeline_path = case_dir / "timeline.jsonl"
            timeline_path.write_text("")

            def sampler():
                with timeline_path.open("a") as stream:
                    stream.write(json.dumps(sample_row(cluster, name, current_phase[0], algorithm)) + "\n")

            current_phase = [SIM_PHASES[0][0]]
            with timeline_path.open("a") as stream:
                stream.write(json.dumps(sample_row(cluster, name, "prepare", algorithm)) + "\n")
            for phase_name, duration, concurrency in SIM_PHASES:
                current_phase[0] = phase_name
                if concurrency:
                    run_load_phase(cluster, root, endpoint, workload, phase_name, concurrency, duration, case_dir, options.seed, sampler=sampler)
                else:
                    deadline = time.monotonic() + duration
                    while time.monotonic() < deadline:
                        sampler()
                        time.sleep(SAMPLER_INTERVAL_SECONDS)
                    (case_dir / f"{phase_name}_reqs.jsonl").write_text("")
                records_phase = read_request_records(case_dir, phase_name)
                arms.setdefault(algorithm, {})[phase_name] = true_slo_metrics(
                    records_phase, float(options.ttft_target_ms), float(options.itl_target_ms),
                )
            record.update(outcome="succeeded", phases=arms[algorithm])
        except (ClusterError, rounds.HarnessError, OSError, ValueError, KeyError) as error:
            record.update(outcome="failed", error=str(error))
            dump_failure(cluster, f"sim-sensitivity-{algorithm}", case_dir, str(error))
        finally:
            if root:
                try:
                    cluster.exec_driver(root, "remove")
                except (ClusterError, OSError):
                    pass
            cluster.delete(SCALER_RESOURCE, name, namespace())
            cluster.delete(MD_RESOURCE, name, namespace())
        records.append(record)
    if applied_catalog:
        try:
            cluster.delete("configmap", applied_catalog, namespace())
        except (ClusterError, OSError):
            pass
    rounds.write_json(directory / "sensitivity.json", {
        "schemaVersion": "1",
        "comparisonKind": "scaling-sensitivity",
        "reference": "docs/modules/model-deployment/scaling-simulation.md",
        "arms": arms,
        "cases": records,
    })
    write_sensitivity_summary(directory, arms)
    return records


def write_sensitivity_summary(directory, arms):
    header = (
        "| algorithm | phase | requests | failed | timed out | true SLO | goodput req/s "
        "| ttft p50/p90/p99 ms | itl p50/p90/p99 ms | toks/s/user | output tok/s |\n"
        "|---|---|---|---|---|---|---|---|---|---|---|\n"
    )
    lines = [header]
    for algorithm, phases in arms.items():
        for phase, metrics in phases.items():
            def triple(quantiles):
                return "/".join(str(quantiles.get(name, "-")) for name in ("p50", "p90", "p99"))

            lines.append(
                f"| {algorithm} | {phase} | {metrics['requests']} | {metrics['failed']} | {metrics['timedOut']} "
                f"| {metrics['trueSloCompliance']} | {metrics['goodputPerSecond']} | {triple(metrics['ttftMilliseconds'])} "
                f"| {triple(metrics['itlMilliseconds'])} | {metrics['tokensPerSecondPerUser']} "
                f"| {metrics['outputTokensPerSecond']} |\n"
            )
    (Path(directory) / "summary.md").write_text("".join(lines))


# --------------------------------------------------------------- builtin catalog


def suite_builtin_catalog(cluster, out_dir, options):
    """Zero-configuration capacityPrior against the embedded catalog, on gpt-oss-120b.

    The scaler states no capacity at all, so the embedded pkg/worker/scaling/priors
    catalog is the only source the resolution can use; the run asserts the priors
    resolved and measures one sustain wave under the resolved basis.
    """
    directory = Path(out_dir) / "builtin-catalog"
    directory.mkdir(parents=True, exist_ok=True)
    records = []
    widths = tuple(int(part) for part in str(options.builtin_gpus).split(",") if part.strip())
    for gpus in widths:
        name = f"qual-builtin-{gpus}gpu"
        case_dir = directory / f"{gpus}gpu"
        case_dir.mkdir(parents=True, exist_ok=True)
        record = {"suite": "builtin-catalog", "model": MODELS["gpt-oss-120b"]["repository"],
                  "quantization": MODELS["gpt-oss-120b"]["quantization"], "gpusPerReplica": gpus,
                  "startedAt": rounds.stamp()}
        workload = {
            "model": options.model_path,
            "inputTokens": options.input_tokens,
            "outputTokens": options.output_tokens,
            "requestTimeoutSeconds": options.request_timeout_seconds,
        }
        deployment = server_deployment(name, "gpt-oss-120b", options.model_path, gpus, ENGINE_IMAGES["vllm"])
        scaler = base_scaler(name, name, token_throughput_spec(None, options.ttft_target_ms, options.itl_target_ms), {})
        root = None
        try:
            ensure_driver(cluster, options.case_timeout)
            cluster.apply(deployment)
            cluster.apply(scaler)
            wait_deployment_ready(cluster, name, options.case_timeout, expect_capacity_priors=True)
            scaler_status = cluster.get_json(SCALER_RESOURCE, name, namespace())
            record["resolvedCapacityPriors"] = scaler_status.get("status", {}).get("resolvedCapacityPriors", [])
            service, port = serving_endpoint(cluster, name)
            endpoint = f"http://{service}.{namespace()}.svc:{port}"
            root = f"/tmp/gpustack-autoscaling-qual-builtin-{os.getpid()}"
            stage_driver(cluster, root)
            run_load_phase(cluster, root, endpoint, workload, "sustain", 24, options.phase_seconds, case_dir, options.seed)
            metrics = true_slo_metrics(read_request_records(case_dir, "sustain"),
                                       float(options.ttft_target_ms), float(options.itl_target_ms))
            record.update(outcome="succeeded", metrics=metrics)
        except (ClusterError, rounds.HarnessError, OSError, ValueError, KeyError) as error:
            record.update(outcome="failed", error=str(error))
            dump_failure(cluster, f"builtin-catalog-{gpus}gpu", case_dir, str(error))
        finally:
            if root:
                try:
                    cluster.exec_driver(root, "remove")
                except (ClusterError, OSError):
                    pass
            cluster.delete(SCALER_RESOURCE, name, namespace())
            cluster.delete(MD_RESOURCE, name, namespace())
        records.append(record)
    rounds.write_json(directory / "builtin-catalog.json", {"suite": "builtin-catalog", "cases": records})
    return records


# --------------------------------------------------------------- pd / epd delegation


def pd_epd_commands(legs, out_dir):
    """The scenario arms the P/D and E/P/D legs delegate to."""
    commands = []
    if legs in ("pd", "both"):
        for arm in PD_ARMS:
            commands.append((f"pd-{arm}", ["/bin/bash", str(CASES / "case-5-autoscaling.sh"), str(out_dir), "--serial", "--arm", arm]))
    if legs in ("epd", "both"):
        for arm in EPD_ARMS:
            commands.append((f"epd-{arm}", ["/bin/bash", str(CASES / "case-5-autoscaling.sh"), str(out_dir), "--serial", "--arm", arm]))
    return commands


def suite_pd_epd(cluster, out_dir, options):
    """Delegate the P/D and E/P/D scaling legs to the existing scenario case, bounded."""
    directory = Path(out_dir) / "pd-epd"
    directory.mkdir(parents=True, exist_ok=True)
    records = []
    for name, command in pd_epd_commands(options.legs, directory):
        records.append(run_case(name, command, directory / name, options.case_timeout, cluster=cluster))
    rounds.write_json(directory / "pd-epd.json", {"suite": "pd-epd", "cases": records})
    return records


# --------------------------------------------------------------- prepare


def suite_prepare(cluster, out_dir, options):
    """Image pre-pull, model pre-warm and the driver readiness gate, before any measuring."""
    directory = Path(out_dir) / "prepare"
    directory.mkdir(parents=True, exist_ok=True)
    record = {"suite": "prepare", "images": {}, "models": {}, "driver": None, "startedAt": rounds.stamp()}
    failures = []
    try:
        cluster.apply(namespace_manifest())
    except (ClusterError, OSError) as error:
        failures.append(f"namespace {namespace()}: {error}")
    for engine, image in ENGINE_IMAGES.items():
        try:
            cluster.apply(prefetch_daemonset(f"image-prefetch-{engine}", image))
        except (ClusterError, OSError) as error:
            failures.append(f"image {engine}: {error}")
            record["images"][engine] = {"image": image, "ready": False, "error": str(error)}
    try:
        cluster.apply(model_store())
        cluster.apply(model_store_binding())
    except (ClusterError, OSError) as error:
        failures.append(f"model store: {error}")
    for model_name in MODELS:
        try:
            cluster.apply(model_artifact(model_name))
            cluster.apply(model_prefetch(model_name, instance_type()))
        except (ClusterError, OSError) as error:
            failures.append(f"model {model_name}: {error}")
            record["models"][model_name] = {"repository": MODELS[model_name]["repository"], "ready": False, "error": str(error)}
    for engine, image in ENGINE_IMAGES.items():
        if record["images"].get(engine, {}).get("error"):
            continue
        try:
            wait_daemonset_ready(cluster, f"image-prefetch-{engine}", options.case_timeout)
            record["images"][engine] = {"image": image, "ready": True}
        except ClusterError as error:
            failures.append(f"image {engine}: {error}")
            record["images"][engine] = {"image": image, "ready": False, "error": str(error)}
    for model_name in MODELS:
        if record["models"].get(model_name, {}).get("error"):
            continue
        try:
            wait_prefetch_ready(cluster, model_name, options.case_timeout)
            record["models"][model_name] = {"repository": MODELS[model_name]["repository"], "ready": True}
        except (ClusterError, OSError) as error:
            failures.append(f"model {model_name}: {error}")
            record["models"][model_name] = {"repository": MODELS[model_name]["repository"], "ready": False, "error": str(error)}
    try:
        record["driver"] = ensure_driver(cluster, options.case_timeout)
    except (ClusterError, OSError) as error:
        failures.append(f"driver: {error}")
        record["driver"] = {"ready": False, "error": str(error)}
    record["outcome"] = "succeeded" if not failures else "failed"
    if failures:
        record["errors"] = failures
        dump_failure(cluster, "prepare", directory, "; ".join(failures))
    rounds.write_json(directory / "prepare.json", record)
    return [record]


# --------------------------------------------------------------- entry point


def current_context():
    """The kubectl context the run is bound to, resolved once when --context is absent."""
    try:
        result = subprocess.run(["kubectl", "config", "current-context"], text=True, capture_output=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--context", default=os.environ.get("BENCH_CONTEXT"))
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--prepare", action="store_true")
    mode.add_argument("--suite", choices=SUITES)
    parser.add_argument("--case-timeout", type=float, default=float(os.environ.get("BENCH_QUAL_CASE_TIMEOUT", "360")))
    parser.add_argument("--phase-seconds", type=float, default=90.0)
    parser.add_argument("--catalog-configmap", type=Path)
    parser.add_argument("--frozen-capacity", default="20000")
    parser.add_argument("--tuned-capacity")
    parser.add_argument("--sim-capacity", default="6000")
    parser.add_argument("--cluster-queues", default=os.environ.get("BENCH_QUAL_CLUSTER_QUEUES", f"gpustack--{HARDWARE}-linux-amd64"))
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--builtin-gpus", default="1")
    parser.add_argument("--legs", default="both", choices=("pd", "epd", "both"))
    parser.add_argument("--ttft-target-ms", default="2000")
    parser.add_argument("--itl-target-ms", default="33")
    parser.add_argument("--input-tokens", type=int, default=1024)
    parser.add_argument("--output-tokens", type=int, default=256)
    parser.add_argument("--request-timeout-seconds", type=int, default=120)
    parser.add_argument("--seed", type=int, default=71)
    parser.add_argument("--slot-gpus", type=int, default=1)
    parser.add_argument("--max-replicas", type=int, default=2)
    parser.add_argument("--model-path", default="/models")
    options = parser.parse_args()
    if options.catalog_configmap is None:
        default_sweep = options.out.parent / "sweep" / "priors-configmap.json" if options.out.name != "sweep" else None
        if default_sweep and default_sweep.exists():
            options.catalog_configmap = default_sweep
    if options.context is None:
        options.context = current_context()
    cluster = Cluster(options.context)
    if options.prepare:
        records = suite_prepare(cluster, options.out, options)
    else:
        suites = {
            "sweep": suite_sweep,
            "capacity-arms": suite_capacity_arms,
            "sim-sensitivity": suite_sim_sensitivity,
            "builtin-catalog": suite_builtin_catalog,
            "pd-epd": suite_pd_epd,
        }
        records = suites[options.suite](cluster, options.out, options)
    succeeded = all(record.get("outcome") == "succeeded" for record in records)
    rounds.write_json(options.out / "suite.json", {
        "suite": "prepare" if options.prepare else options.suite,
        "succeeded": succeeded,
        "cases": records,
    })
    return 0 if succeeded else 1


if __name__ == "__main__":
    sys.exit(main())
