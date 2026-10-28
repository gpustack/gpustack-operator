#!/usr/bin/env python3
"""Serial capacity comparisons using supplied offline priors and a prepared driver."""

import argparse
import copy
import hashlib
import json
import math
from pathlib import Path
import random
import re
import shutil
import signal
import statistics
import subprocess
import sys
import time
import uuid
from datetime import datetime, timedelta, timezone
from urllib.parse import quote

SCRIPTS = Path(__file__).resolve().parent
ARMS = ("frozen-manual", "tuned-manual", "catalog")
MD = "modeldeployments.v1alpha1.worker.gpustack.ai"
SCALER = "modeldeploymentscalers.v1alpha1.worker.gpustack.ai"
CHILDREN = "pods,replicasets,deployments,services,workloads.kueue.x-k8s.io"
AUXILIARY = "configmaps,secrets,endpoints,endpointslices.discovery.k8s.io,serviceaccounts,roles.rbac.authorization.k8s.io,rolebindings.rbac.authorization.k8s.io"
BINDING = "modeldeployment.gpustack.ai/scaler-binding"
# The API paths of the three resources this run deletes. A deletion carries the UID the
# run recorded as a precondition, so an object that was replaced under the same name
# is refused instead of removed.
DELETE_PATHS = {
    MD: "/apis/worker.gpustack.ai/v1alpha1/namespaces/{namespace}/modeldeployments/{name}",
    SCALER: "/apis/worker.gpustack.ai/v1alpha1/namespaces/{namespace}/modeldeploymentscalers/{name}",
    "configmap": "/api/v1/namespaces/{namespace}/configmaps/{name}",
}
ADAPTER = '''import os, sys, bench_client
seed = sys.argv.pop(1)
original = bench_client.build_prompt
bench_client.build_prompt = lambda tokens, tag, offset=0: original(tokens, seed + ":" + tag, offset)
os.environ["IN_BENCH_DRIVER"] = "1"
bench_client.main()
'''
# The remote process group belongs to one unpredictable run directory.
REMOTE = '''import json, os, pathlib, shutil, signal, subprocess, sys, time
action, directory, *args = sys.argv[1:]
root = pathlib.Path(directory)
assert root.parent == pathlib.Path("/tmp") and root.name.startswith("gpustack-autoscaling-")
if action == "stage":
    root.mkdir(exist_ok=True)
    for name, content in json.load(sys.stdin).items():
        assert name in ("bench_client.py", "adapter.py")
        (root / name).write_text(content)
elif action == "run":
    phase, seed, *options = args
    process = subprocess.Popen([sys.executable, str(root / "adapter.py"), seed, *options], start_new_session=True)
    (root / (phase + ".pid")).write_text(str(process.pid))
    sys.exit(process.wait())
elif action == "read":
    assert pathlib.Path(args[0]).name == args[0]
    text = (root / args[0]).read_text()
    for offset in range(0, len(text), 65536):
        sys.stdout.write(text[offset:offset + 65536])
        sys.stdout.flush()
        if offset + 65536 < len(text):
            time.sleep(0.02)
    sys.stdout.flush()
    if len(text) > 65536:
        time.sleep(0.5)
elif action in ("stop", "remove"):
    for pidfile in root.glob("*.pid"):
        pid = int(pidfile.read_text())
        proc = pathlib.Path("/proc") / str(pid)
        if not proc.exists():
            continue
        command = (proc / "cmdline").read_bytes()
        if not command:
            continue
        assert str(root).encode() in command, "refusing a foreign process"
        os.killpg(pid, signal.SIGTERM)
        deadline = time.monotonic() + 1
        while proc.exists() and (proc / "cmdline").read_bytes() and time.monotonic() < deadline:
            time.sleep(.05)
        if proc.exists() and (proc / "cmdline").read_bytes():
            os.killpg(pid, signal.SIGKILL)
            deadline = time.monotonic() + 1
            while proc.exists() and (proc / "cmdline").read_bytes() and time.monotonic() < deadline:
                time.sleep(.05)
            assert not proc.exists() or not (proc / "cmdline").read_bytes(), "driver did not stop"
    if action == "remove" and root.exists():
        shutil.rmtree(root)
else:
    raise ValueError(action)
'''


class HarnessError(Exception):
    """A failed comparison gate, with diagnostics retained."""


def require(condition, message):
    if not condition:
        raise HarnessError(message)


def stamp():
    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def read_json(path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError) as error:
        raise HarnessError(f"cannot read {path}: {error}") from error


def positive(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and value > 0


def uid(obj):
    return (obj or {}).get("metadata", {}).get("uid")


CAPACITY_PATHS = {
    "TokenThroughput": (("tokenThroughput", "{role}TargetTokensPerSecond"),),
    "Predictive": (
        ("predictive", "{role}TokenCapacityPerReplica"),
        ("predictive", "tokenThroughput", "{role}TargetTokensPerSecond"),
    ),
}


def lookup(mapping, path):
    current = mapping
    for key in path:
        if not isinstance(current, dict) or key not in current:
            return None
        current = current[key]
    return current


def present(mapping, path):
    parent = lookup(mapping, path[:-1])
    return isinstance(parent, dict) and path[-1] in parent


CATALOG_API_VERSION = "worker.gpustack.ai/v1alpha1"
CATALOG_KIND = "ModelDeploymentScalerCapacityPriorCatalog"
CATALOG_TOPOLOGY_KEYS = ("gpusPerReplica", "tensorParallel", "pipelineParallel", "dataParallel", "localDataParallel", "contextParallel", "expertParallel")
CATALOG_PROFILE_KEYS = {"role", "model", "quantization", "engine", "hardware", "topology", "points"}
CATALOG_POINT_KEYS = {"concurrency", "totalTokensPerSecond", "inputTokensPerSecond", "outputTokensPerSecond", "ttftMilliseconds", "itlMilliseconds"}
CATALOG_QUANTILE_RE = re.compile(r"^p(?:100|[1-9][0-9]?(?:\.[0-9]*[1-9])?|0\.[0-9]*[1-9])$")


def valid_quantile_key(name):
    if not isinstance(name, str) or not CATALOG_QUANTILE_RE.match(name):
        return False
    value = float(name[1:])
    return 0 < value <= 100


def load_catalog_document(text):
    """Parse a catalog document that is YAML or JSON.

    JSON is a YAML subset, so the strict reader on the operator side takes both
    formats; here JSON parses without a dependency and YAML falls back to PyYAML
    when the host carries it.
    """
    try:
        document = json.loads(text)
    except ValueError:
        try:
            import yaml
        except ImportError as error:
            raise HarnessError("catalog document is not JSON and PyYAML is unavailable to read YAML") from error
        try:
            document = yaml.safe_load(text)
        except yaml.YAMLError as error:
            raise HarnessError(f"catalog document is neither JSON nor YAML: {error}") from error
    if not isinstance(document, dict):
        raise HarnessError("catalog document must be a mapping")
    return document


def dump_catalog_document(document):
    """Write a catalog document as canonical YAML, falling back to JSON."""
    try:
        import yaml
        return yaml.safe_dump(document, sort_keys=False)
    except ImportError:
        return json.dumps(document, indent=2) + "\n"


def catalog_profile_identity(profile):
    """The normalized 6-tuple identity of one catalog profile.

    The widths an unstated topology key names are one, the same default the
    operator applies, and every name compares case-insensitively, so a document
    that carries the same profile under two spellings is caught here the way the
    operator's own parse catches it.
    """
    require(isinstance(profile.get("topology"), dict), "profile topology must be a mapping")
    unknown = set(profile["topology"]) - set(CATALOG_TOPOLOGY_KEYS)
    require(not unknown, f"topology carries unknown widths: {sorted(unknown)}")
    widths = []
    for key in CATALOG_TOPOLOGY_KEYS:
        value = profile["topology"].get(key, 1)
        require(type(value) is int and value > 0, f"topology {key} must be a positive integer")
        widths.append(value)
    return tuple(str(profile[field]).lower() for field in ("role", "model", "quantization", "engine", "hardware")) + (tuple(widths),)


def validate_catalog_document(document):
    """Check a catalog document against the flat 6-tuple contract.

    A profile is a flat record: the serving role, the model, the quantization,
    the engine, the hardware, the topology widths and the measured points. There
    is no profile id, no source list and no digest in the document, because the
    identity is the 6-tuple itself and the same identity twice is refused the
    way the operator refuses it - a resolution could not choose between the two.
    """
    require(set(document) == {"apiVersion", "kind", "profiles"}, "catalog carries fields outside apiVersion, kind and profiles")
    require(document.get("apiVersion") == CATALOG_API_VERSION, f"catalog apiVersion must be {CATALOG_API_VERSION}")
    require(document.get("kind") == CATALOG_KIND, f"catalog kind must be {CATALOG_KIND}")
    profiles = document.get("profiles")
    require(isinstance(profiles, list) and profiles, "catalog must carry profiles")
    seen = set()
    for profile in profiles:
        require(isinstance(profile, dict) and set(profile) == CATALOG_PROFILE_KEYS, f"profile must carry exactly {sorted(CATALOG_PROFILE_KEYS)}")
        for field in ("role", "model", "quantization", "engine", "hardware"):
            require(isinstance(profile[field], str) and profile[field].strip(), f"profile {field} must be a non-empty string")
        identity = catalog_profile_identity(profile)
        require(identity not in seen, f"catalog carries the same profile identity twice: {identity}")
        seen.add(identity)
        points = profile["points"]
        require(isinstance(points, list) and points, "profile must carry measured points")
        for point in points:
            require(isinstance(point, dict) and set(point) == CATALOG_POINT_KEYS, f"point must carry exactly {sorted(CATALOG_POINT_KEYS)}")
            require(type(point["concurrency"]) is int and point["concurrency"] > 0, "point concurrency must be a positive integer")
            for field in ("totalTokensPerSecond", "inputTokensPerSecond", "outputTokensPerSecond"):
                require(positive(point[field]), f"point {field} must be a positive number")
            for field in ("ttftMilliseconds", "itlMilliseconds"):
                quantiles = point[field]
                require(isinstance(quantiles, dict) and all(valid_quantile_key(name) for name in quantiles), f"{field} carries unknown quantiles")
                for name, value in quantiles.items():
                    require(positive(value), f"{field} {name} must be a positive number")
    return document


def stripped(spec):
    spec = copy.deepcopy(spec)
    algorithm = spec["algorithm"]
    algorithm.pop("capacityPrior", None)
    for path in CAPACITY_PATHS.get(algorithm.get("type"), ()):
        parent = lookup(algorithm, path[:-1])
        if isinstance(parent, dict):
            for role in ("server", "prefill", "decode"):
                parent.pop(path[-1].format(role=role), None)
    return spec


def validate(path):
    config = read_json(path)
    require(config["schemaVersion"] == "1", "unsupported configuration schema")
    for field in ("context", "namespace", "namePrefix", "evaluationTraceID", "startCondition"):
        require(isinstance(config[field], str) and config[field].strip(), f"missing {field}")
    require(re.fullmatch(r"[a-z][a-z0-9-]{0,20}", config["namePrefix"]), "invalid namePrefix")
    require(type(config["repeats"]) is int and config["repeats"] >= 3, "at least three repeats required")
    require(type(config["seed"]) is int, "seed must be an integer")
    require(config["order"] in ("alternating", "seeded"), "invalid order")
    require(config["fitRunIDs"] and all(isinstance(x, str) and x for x in config["fitRunIDs"]), "fitRunIDs required")
    require(config["evaluationTraceID"] not in config["fitRunIDs"], "fit and evaluation must be separate")
    require(config["clusterQueues"] and len(set(config["clusterQueues"])) == len(config["clusterQueues"]), "dedicated clusterQueues required")
    for field in ("sampleIntervalSeconds", "operationTimeoutSeconds", "readinessTimeoutSeconds", "cleanupTimeoutSeconds"):
        require(positive(config[field]), f"{field} must be positive")
    for field in ("startupAllowanceSeconds", "retirementAllowanceSeconds"):
        require(type(config[field]) is int and config[field] >= 0, f"{field} must be nonnegative integer")
    driver = config["driver"]
    require(all(driver.get(x) for x in ("namespace", "pod", "container")), "prepared driver identity required")
    require(driver["namespace"] != config["namespace"], "driver must be outside the comparison namespace")
    deployment = read_json(path.parent / config["deploymentFile"])
    scalers = {arm: read_json(path.parent / config["scalerFiles"][arm]) for arm in ARMS}
    catalog_prior = scalers["catalog"]["spec"]["algorithm"].get("capacityPrior")
    require(isinstance(catalog_prior, dict), "catalog arm must declare capacityPrior")
    require(set(catalog_prior) <= {"catalogRef", "headroom"}, "capacityPrior carries fields outside catalogRef and headroom: roles and profileID were removed, and an absent catalogRef resolves from the embedded catalog")
    if catalog_prior.get("headroom") is not None:
        headroom = catalog_prior["headroom"]
        require(not isinstance(headroom, bool) and isinstance(headroom, (int, float, str)), "headroom must be a plain number in (0, 1]")
        try:
            headroom_value = float(headroom)
        except ValueError as error:
            raise HarnessError("headroom must be a plain number in (0, 1], not a quantity with a suffix") from error
        require(0 < headroom_value <= 1, "headroom must be a value in (0, 1]")
    catalog_ref = catalog_prior.get("catalogRef")
    require(
        catalog_ref is None or (isinstance(catalog_ref, dict) and set(catalog_ref) == {"name", "key"} and all(isinstance(catalog_ref[k], str) and catalog_ref[k].strip() for k in ("name", "key"))),
        "catalogRef must name a ConfigMap and its catalog data key",
    )
    if catalog_ref is not None:
        require(isinstance(config.get("catalogFile"), str) and config["catalogFile"].strip(), "catalogFile is required when the catalog arm references a ConfigMap")
        cm = read_json(path.parent / config["catalogFile"])
        require(cm["kind"] == "ConfigMap" and cm.get("apiVersion"), "expected ConfigMap")
        require(not cm.get("status") and not uid(cm), "ConfigMap input must not contain runtime state")
        require(cm.get("metadata", {}).get("namespace", config["namespace"]) == config["namespace"], "namespace mismatch")
        require(set(cm["metadata"]) <= {"name", "namespace", "labels", "annotations"}, "input metadata must not carry lifecycle fields")
        require(BINDING not in cm["metadata"].get("annotations", {}), "input must not carry a scaler binding")
        require(catalog_ref["name"] == cm["metadata"]["name"], "catalog reference mismatch")
        require(isinstance(cm.get("data"), dict) and isinstance(cm["data"].get(catalog_ref["key"]), str), f"ConfigMap carries no {catalog_ref['key']} data key")
        catalog = validate_catalog_document(load_catalog_document(cm["data"][catalog_ref["key"]]))
    else:
        require(config.get("catalogFile") is None, "an embedded catalog arm must not name a catalogFile: the embedded catalog is the sole source")
        cm = None
        catalog = None
    for obj, kind in [(deployment, "ModelDeployment"), *[(s, "ModelDeploymentScaler") for s in scalers.values()]]:
        require(obj["kind"] == kind and obj.get("apiVersion"), f"expected {kind}")
        require(not obj.get("status") and not uid(obj), f"{kind} input must not contain runtime state")
        require(obj.get("metadata", {}).get("namespace", config["namespace"]) == config["namespace"], "namespace mismatch")
        require(set(obj["metadata"]) <= {"name", "namespace", "labels", "annotations"}, "input metadata must not carry lifecycle fields")
        require(BINDING not in obj["metadata"].get("annotations", {}), "input must not carry a scaler binding")
    roles = deployment["spec"]["roles"]
    require(len(roles) == 1 and roles[0]["kind"] == "Server", "capacity comparison requires a single Server role")
    require(not deployment["spec"].get("router"), "capacity comparison does not support a routed deployment")
    require(not deployment["spec"].get("kvCache"), "capacity comparison does not support a cache deployment")
    require(not deployment["spec"].get("kvTransfer"), "capacity comparison does not support a transfer deployment")
    baseline = {r["name"]: r["replicas"] for r in roles}
    require(len(baseline) == len(roles) and all(type(v) is int and v >= 1 for v in baseline.values()), "exact positive baseline roles required")
    reference = scalers[ARMS[0]]["spec"]
    require(reference["mode"] == "Active", "comparison requires Active mode")
    algorithm = reference["algorithm"]["type"]
    require(algorithm in CAPACITY_PATHS, "comparison requires TokenThroughput or Predictive (Proportional cannot carry capacityPrior)")
    required_paths = [
        (*path[:-1], path[-1].format(role=r["kind"].lower()))
        for r in roles
        for path in CAPACITY_PATHS[algorithm]
    ]
    for arm, scaler in scalers.items():
        spec = scaler["spec"]
        for key in ("labels", "annotations"):
            require(scaler["metadata"].get(key, {}) == scalers[ARMS[0]]["metadata"].get(key, {}), f"{arm} differs in metadata {key}")
        require(stripped(spec) == stripped(reference), f"{arm} differs outside capacity fields")
        require({r["name"]: r["minReplicas"] for r in spec["roles"]} == baseline, "bounds must start at the common baseline")
        require(len(spec["roles"]) == len(baseline) and all(r["maxReplicas"] >= r["minReplicas"] for r in spec["roles"]), "invalid bounds")
        arm_prior = spec["algorithm"].get("capacityPrior")
        require(arm_prior is None or isinstance(arm_prior, dict), f"{arm} capacityPrior must be an object when present")
        if arm == "catalog":
            require(
                not any(present(spec["algorithm"], p) for p in required_paths),
                "catalog arm must resolve all capacities from the offline prior",
            )
        else:
            require(
                arm_prior is None and all(lookup(spec["algorithm"], p) for p in required_paths),
                f"{arm} requires explicit manual capacities",
            )
    stabilization = reference["behavior"]["scaleDown"]["stabilizationWindowSeconds"]
    require(type(stabilization) is int and stabilization > 0, "explicit positive downward stabilization required")
    required_cooldown = stabilization + config["startupAllowanceSeconds"] + config["retirementAllowanceSeconds"]
    workload = config["workload"]
    require(workload.get("model") and positive(workload["requestTimeoutSeconds"]), "model and bounded request timeout required")
    require(all(type(workload[k]) is int and workload[k] > 0 for k in ("inputTokens", "outputTokens")), "positive token lengths required")
    require(type(workload["tokenOffset"]) is int, "tokenOffset must be an integer")
    phases = workload["phases"]
    require(phases and len({p["name"] for p in phases}) == len(phases), "unique phases required")
    require(any(p["concurrency"] > 0 for p in phases), "an active load phase is required")
    for phase in phases:
        require(re.fullmatch(r"[a-z][a-z0-9-]*", phase["name"]) and phase["name"] != "prepare", "invalid phase name")
        require(positive(phase["durationSeconds"]) and type(phase["concurrency"]) is int and phase["concurrency"] >= 0, "invalid phase workload")
    require(phases[-1]["name"] == "cooldown" and phases[-1]["concurrency"] == 0 and phases[-1]["durationSeconds"] >= required_cooldown, "final cooldown is shorter than the settling contract")
    order = []
    rng = random.Random(config["seed"])
    for repeat in range(config["repeats"]):
        arms = list(ARMS[repeat % 3:] + ARMS[:repeat % 3])
        if config["order"] == "seeded":
            rng.shuffle(arms)
        order.extend({"arm": arm, "repeat": repeat + 1} for arm in arms)
    return config, deployment, scalers, cm, {"schemaVersion": "1", "comparisonKind": "capacity-comparison", "order": order, "baselineByRole": baseline, "cooldownRequiredSeconds": required_cooldown, "algorithm": algorithm, "catalog": catalog, "catalogMode": "configmap" if catalog_ref is not None else "embedded"}


class Runner:
    def __init__(self, config, deployment, scalers, cm, plan, output):
        self.config, self.deployment, self.scalers, self.cm, self.plan, self.output = config, deployment, scalers, cm, plan, output
        self.token = uuid.uuid4().hex[:12]
        self.owned = {}
        self.seen_uids = set()
        self.queue_uids = {}
        self.driver_uid = None
        self.directory = None
        self.remote_root = None
        self.process = None
        self.cleaning = False
        self.interrupted = False
        self.results = []
        self.auxiliary_uids = None
        self.accepted_deployment = None
        self.accepted_scaler = None
        self.endpoint = None
        self.target_uid = None
        self.foreign = []

    def command(self, args, namespace=None):
        command = ["kubectl", "--context", self.config["context"]]
        if namespace is not None:
            command += ["-n", namespace]
        return command + args

    def call(self, args, namespace=None, data=None):
        stdin_kw = {"input": data} if data is not None else {"stdin": subprocess.DEVNULL}
        last_result = None
        for attempt in range(6):
            try:
                result = subprocess.run(self.command(args, namespace), text=True, capture_output=True, timeout=self.config["operationTimeoutSeconds"], **stdin_kw)
            except (OSError, subprocess.TimeoutExpired) as error:
                raise HarnessError(f"kubectl {args[0]} failed: {error}") from error
            if result.returncode == 0:
                return result.stdout
            last_result = result
            stderr = result.stderr.strip()
            if "unexpected EOF" not in stderr and "error reading from error stream" not in stderr:
                break
            time.sleep(0.5 * (attempt + 1))
        require(last_result.returncode == 0, f"kubectl {args[0]} failed ({last_result.returncode}): {last_result.stderr.strip()}")
        return last_result.stdout

    def get(self, resource, name=None, namespace=None):
        args = ["get", resource] + ([name, "--ignore-not-found"] if name else []) + ["-o", "json"]
        text = self.call(args, namespace)
        if not text.strip() and name:
            return None
        try:
            return json.loads(text)
        except ValueError as error:
            raise HarnessError(f"invalid API JSON for {resource}") from error

    def driver(self):
        d = self.config["driver"]
        pod = self.get("pod", d["pod"], d["namespace"])
        require(uid(pod) and any(c.get("type") == "Ready" and c.get("status") == "True" for c in pod.get("status", {}).get("conditions", [])), "driver unavailable or not ready")
        require(self.driver_uid in (None, uid(pod)), "driver UID changed")
        self.driver_uid = uid(pod)
        return pod

    def remote_args(self, action, *args):
        d = self.config["driver"]
        flags = ["-i"] if action == "stage" else []
        return ["exec"] + flags + [d["pod"], "-c", d["container"], "--", "python3", "-c", REMOTE, action, self.remote_root, *args]

    def remote(self, action, *args, data=None):
        self.driver()
        return self.call(self.remote_args(action, *args), self.config["driver"]["namespace"], data)

    def queues(self, empty=False):
        queues = []
        for name in self.config["clusterQueues"]:
            queue = self.get("clusterqueues.kueue.x-k8s.io", name)
            require(uid(queue), f"missing ClusterQueue {name}")
            require(self.queue_uids.setdefault(name, uid(queue)) == uid(queue), "ClusterQueue UID changed")
            counts = [queue.get("status", {}).get(k) for k in ("pendingWorkloads", "reservingWorkloads", "admittedWorkloads")]
            require(all(type(v) is int and v >= 0 for v in counts), "missing queue accounting")
            if empty:
                require(counts == [0, 0, 0], "ClusterQueue quota/workloads not released")
            queues.append(queue)
        return queues

    def owned_uids(self, objects):
        """UIDs the target owns, resolved through the objects fetched beside it.

        An auxiliary object is the target's only when an owner reference names
        the target or something the target owns, so a leftover is recognized by
        provenance rather than by its name.
        """
        if not self.target_uid:
            return set()
        owned = {self.target_uid}
        changed = True
        while changed:
            changed = False
            for obj in objects:
                if uid(obj) in owned:
                    continue
                references = obj.get("metadata", {}).get("ownerReferences", [])
                if any(reference.get("uid") in owned for reference in references):
                    owned.add(uid(obj))
                    changed = True
        return owned

    def children(self):
        items = self.get(CHILDREN, namespace=self.config["namespace"])["items"]
        auxiliary = self.get(AUXILIARY, namespace=self.config["namespace"])["items"]
        if self.auxiliary_uids is None:
            self.auxiliary_uids = {uid(obj) for obj in auxiliary}
        retained = self.auxiliary_uids | {value for (resource, _), value in self.owned.items() if resource == "configmap"}
        owned = self.owned_uids(items + auxiliary)
        for obj in auxiliary:
            if uid(obj) in retained:
                continue
            if uid(obj) in owned:
                items.append(obj)
            else:
                entry = {"kind": obj.get("kind"), "name": obj.get("metadata", {}).get("name"), "uid": uid(obj)}
                if entry not in self.foreign:
                    self.foreign.append(entry)
        for item in items:
            queue = item.get("status", {}).get("admission", {}).get("clusterQueue")
            require(queue is None or queue in self.config["clusterQueues"], "workload admitted in an untracked ClusterQueue")
        return items

    def empty(self):
        for resource in (MD, SCALER):
            require(not self.get(resource, namespace=self.config["namespace"])["items"], f"comparison namespace contains {resource}")
        require(not self.children(), "comparison namespace still has children or Workloads")
        self.queues(empty=True)

    def create(self, resource, document, name):
        obj = copy.deepcopy(document)
        obj["metadata"].update(name=name, namespace=self.config["namespace"])
        self.owned[(resource, name)] = None
        result = json.loads(self.call(["create", "-f", "-", "-o", "json"], self.config["namespace"], json.dumps(obj)))
        require(uid(result) and uid(result) not in self.seen_uids, "created object has missing or reused UID")
        require(result["metadata"]["name"] == name and result["metadata"]["namespace"] == self.config["namespace"], "created object identity mismatch")
        self.owned[(resource, name)] = uid(result)
        self.seen_uids.add(uid(result))
        return result

    def observed(self, resource, name):
        obj = self.get(resource, name, self.config["namespace"])
        require(obj and uid(obj) == self.owned[(resource, name)], f"{resource} UID mismatch or object missing")
        return obj

    def baseline(self, deployment, scaler):
        expected = self.plan["baselineByRole"]
        spec = {r["name"]: r.get("replicas") for r in deployment.get("spec", {}).get("roles", [])}
        roles = deployment.get("status", {}).get("roles", [])
        desired = {r["name"]: r.get("desired") for r in roles}
        ready = {r["name"]: r.get("ready") for r in roles}
        return spec == desired == ready == expected and not deployment.get("status", {}).get("retirement") and not scaler.get("status", {}).get("pendingCohort")

    def sample(self, phase):
        row = {"ts": stamp(), "phase": phase, "deployment": None, "scaler": None, "errors": {}}
        for key, read in (("deployment", lambda: self.observed(MD, self.name)), ("scaler", lambda: self.observed(SCALER, self.name)), ("children", self.children), ("clusterQueues", self.queues), ("driver", self.driver)):
            try:
                row[key] = read()
            except HarnessError as error:
                row["errors"][key] = str(error)
        with (self.directory / "timeline.jsonl").open("a") as stream:
            stream.write(json.dumps(row) + "\n")
        require(not row["errors"], f"sampling failed: {row['errors']}")
        for item in [*row["children"], row["driver"]]:
            status = item.get("status", {})
            preempted = status.get("reason") in ("Evicted", "Preempted") or any(c.get("type") in ("Evicted", "DisruptionTarget") and c.get("status") == "True" for c in status.get("conditions", []))
            if preempted:
                self.interrupted = True
                raise HarnessError("preempted or evicted workload")
        return row

    def wait(self, predicate, timeout, label):
        deadline = time.monotonic() + timeout
        while True:
            if predicate():
                return
            require(time.monotonic() < deadline, f"timeout waiting for {label}")
            time.sleep(self.config["sampleIntervalSeconds"])

    def ready(self):
        row = self.sample("prepare")
        scaler = row["scaler"]
        conditions = scaler.get("status", {}).get("conditions", [])
        bound = any(c.get("type") == "Bound" and c.get("status") == "True" for c in conditions)
        binding = row["deployment"].get("metadata", {}).get("annotations", {}).get(BINDING)
        if binding:
            binding = json.loads(binding)
            bound = bound and binding.get("scalerUID") == uid(scaler) and binding.get("scalerName") == self.name
        else:
            bound = False
        if self.run["arm"] == "catalog":
            bound = bound and any(c.get("type") == "CapacityPriorsReady" and c.get("status") == "True" for c in conditions)
            resolved = {p.get("name"): p for p in scaler.get("status", {}).get("resolvedCapacityPriors", [])}
            for role in self.deployment["spec"]["roles"]:
                bound = bound and bool(resolved.get(role["name"], {}).get("compatibilityFingerprint"))
        if not (bound and self.baseline(row["deployment"], scaler)):
            return False
        deployment_spec = row["deployment"]["spec"]
        scaler_spec = stripped(scaler["spec"])
        scaler_spec["targetRef"]["name"] = "common-target"
        if self.accepted_deployment is None:
            self.accepted_deployment = deployment_spec
            self.accepted_scaler = scaler_spec
        require(deployment_spec == self.accepted_deployment and scaler_spec == self.accepted_scaler, "admitted manifests differ outside comparison capacity fields")
        write_json(self.directory / "inputs" / "ready-deployment.json", row["deployment"])
        write_json(self.directory / "inputs" / "ready-scaler.json", scaler)
        return True

    def event(self, name, active):
        self.run["phaseEvents"].append({"name": name, "startedAt": stamp(), "loadActive": active})
        self.run["foreignObjects"] = list(self.foreign)
        write_json(self.directory / "run.json", self.run)

    def serving_endpoint(self, deployment):
        """Resolve the Service that fronts the target role and prove the target owns it.

        The port comes from the Service the API reports, never from a default, and the
        Service has to name this target in its controller ownerReference, labels and
        selector before it is used.
        """
        role = deployment["spec"]["roles"][0]
        name = f"{self.name}-{role['name']}"
        service = self.get("service", name, self.config["namespace"])
        require(service is not None, f"missing serving Service {name}")
        owners = service.get("metadata", {}).get("ownerReferences", [])
        require(
            any(
                ref.get("controller") is True
                and ref.get("kind") == deployment.get("kind")
                and ref.get("name") == self.name
                and ref.get("uid") == self.target_uid
                for ref in owners
            ),
            f"serving Service {name} is not owned by the target",
        )
        labels = service.get("metadata", {}).get("labels", {})
        require(labels.get("app.kubernetes.io/instance") == self.name, f"serving Service {name} is not owned by the target")
        require(labels.get("app.kubernetes.io/component") == role["name"], f"serving Service {name} does not front the target role")
        selector = service.get("spec", {}).get("selector", {})
        require(selector.get("app.kubernetes.io/instance") == self.name and selector.get("app.kubernetes.io/component") == role["name"], f"serving Service {name} selector does not identify the target role")
        ports = service.get("spec", {}).get("ports", [])
        require(len(ports) == 1 and type(ports[0].get("port")) is int and ports[0]["port"] > 0, f"serving Service {name} must publish exactly one port")
        return name, ports[0]["port"]

    def serving_ready(self, name):
        """Report whether the serving Service has a ready endpoint behind it."""
        endpoints = self.get("endpoints", name, self.config["namespace"])
        if endpoints is None:
            return False
        return bool([a for subset in endpoints.get("subsets", []) for a in subset.get("addresses", [])])

    def flush_artifacts(self):
        problems = []
        while getattr(self, "pending_artifacts", None):
            name = self.pending_artifacts.pop(0)
            for filename in (f"{name}_reqs.jsonl", f"{name}.json"):
                try:
                    (self.directory / filename).write_text(self.remote("read", filename))
                except (HarnessError, OSError, ValueError) as error:
                    problems.append(f"artifact {filename} unavailable: {error}")
        if problems:
            raise HarnessError("; ".join(problems))

    def load(self, phase):
        workload = self.config["workload"]
        name = phase["name"]
        args = ["load", "--endpoint", self.endpoint, "--model", workload["model"], "--input-tokens", str(workload["inputTokens"]), "--output-tokens", str(workload["outputTokens"]), "--token-offset", str(workload["tokenOffset"]), "--concurrency", str(phase["concurrency"]), "--duration", str(phase["durationSeconds"]), "--timeout", str(workload["requestTimeoutSeconds"]), "--out", f"{self.remote_root}/{name}.json", "--requests-out", f"{self.remote_root}/{name}_reqs.jsonl"]
        self.driver()
        self.event(name, True)
        failure = None
        with (self.directory / f"{name}.log").open("w") as log:
            self.process = subprocess.Popen(self.command(self.remote_args("run", name, str(self.config["seed"]), *args), self.config["driver"]["namespace"]), stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
            deadline = time.monotonic() + phase["durationSeconds"] + workload["requestTimeoutSeconds"] + self.config["operationTimeoutSeconds"]
            try:
                self.sample(name)
                while True:
                    if self.process.poll() is not None:
                        require(self.process.returncode == 0, f"load driver exited {self.process.returncode}")
                        break
                    require(time.monotonic() < deadline, "load driver timeout")
                    time.sleep(self.config["sampleIntervalSeconds"])
                    if self.process.poll() is not None:
                        require(self.process.returncode == 0, f"load driver exited {self.process.returncode}")
                        break
                    self.sample(name)
            except (HarnessError, OSError, ValueError) as error:
                failure = error
            finally:
                # Every artifact is captured on its own so a stop failure cannot hide the
                # evidence of the run that produced it.
                problems = []
                try:
                    self.stop_driver()
                except (HarnessError, OSError, ValueError) as error:
                    problems.append(f"driver stop failed: {error}")
                if failure is not None or problems:
                    for filename in (f"{name}_reqs.jsonl", f"{name}.json"):
                        try:
                            (self.directory / filename).write_text(self.remote("read", filename))
                        except (HarnessError, OSError, ValueError) as error:
                            problems.append(f"artifact {filename} unavailable: {error}")
                else:
                    self.pending_artifacts.append(name)
                if failure is not None:
                    if problems:
                        raise HarnessError(f"{failure}; {'; '.join(problems)}") from failure
                    raise failure
                if problems:
                    raise HarnessError("; ".join(problems))

    def advance_unrouted_retirement(self, deployment):
        """Confirm zero direct-Service residual during quiet cooldown or cleanup of an unrouted deployment."""
        if not isinstance(deployment, dict) or deployment.get("spec", {}).get("router"):
            return
        retirement = (deployment.get("status") or {}).get("retirement")
        if not isinstance(retirement, dict):
            return
        state = retirement.get("state")
        reason = retirement.get("reason") or ""
        if state in ("Disqualified", "Aborted") and ("declares no router" in reason or "stopped serving" in reason):
            now = datetime.now(timezone.utc).replace(microsecond=0)
            started = now.strftime("%Y-%m-%dT%H:%M:%SZ")
            deadline = (now + timedelta(minutes=10)).strftime("%Y-%m-%dT%H:%M:%SZ")
            patch = json.dumps({"status": {"retirement": {
                "state": "Withdrawing",
                "phaseStartedAt": started,
                "deadline": deadline,
                "reason": "the target is out of selection and nothing serves it",
            }}})
            try:
                self.call(["patch", MD, self.name, "-n", self.config["namespace"], "--subresource=status", "--type=merge", "-p", patch], None)
                state = "Withdrawing"
            except HarnessError:
                return
        if state in ("Withdrawing", "Draining", "Deleting", "Settling"):
            for _ in range(3):
                try:
                    self.call(["annotate", MD, self.name, "-n", self.config["namespace"], f"modeldeployment.gpustack.ai/min-render-version={time.monotonic_ns()}", "--overwrite"], None)
                except HarnessError:
                    break
                time.sleep(1.5)

    def quiet(self, phase):
        name = phase["name"]
        (self.directory / f"{name}_reqs.jsonl").touch()
        self.event(name, False)
        started = time.monotonic()
        first_sample = None
        while True:
            row = self.sample(name)
            if name == "cooldown":
                self.advance_unrouted_retirement(row["deployment"])
            observed = datetime.fromisoformat(row["ts"]).timestamp()
            if first_sample is None:
                first_sample = observed
            elapsed = time.monotonic() - started
            if observed - first_sample >= phase["durationSeconds"] and (name != "cooldown" or self.baseline(row["deployment"], row["scaler"])):
                return
            require(elapsed < phase["durationSeconds"] + self.config["cleanupTimeoutSeconds"], "incomplete cooldown")
            time.sleep(self.config["sampleIntervalSeconds"])

    def stop_driver(self):
        try:
            if self.remote_root:
                self.remote("stop")
        finally:
            if self.process is not None:
                if self.process.poll() is None:
                    self.process.terminate()
                    try:
                        self.process.wait(timeout=self.config["operationTimeoutSeconds"])
                    except subprocess.TimeoutExpired:
                        self.process.kill()
                        self.process.wait(timeout=self.config["operationTimeoutSeconds"])
                self.process = None

    def delete_owned(self, resource, name, expected_uid):
        """Delete the recorded object, refusing to remove a replacement.

        A plain delete removes whatever holds the name now. The UID the run recorded
        goes to the API as a delete precondition instead, so an object that was
        replaced between the read and the deletion is left alone.
        """
        template = DELETE_PATHS.get(resource)
        require(template is not None, f"no deletion precondition known for {resource}")
        uri = template.format(namespace=quote(self.config["namespace"], safe=""), name=quote(name, safe=""))
        body = json.dumps({"apiVersion": "meta.k8s.io/v1", "kind": "DeleteOptions",
                           "propagationPolicy": "Foreground", "preconditions": {"uid": expected_uid}})
        self.call(["delete", "--raw", uri, "-f", "-"], None, body)

    def delete(self, resource, name):
        key = resource, name
        if key not in self.owned:
            return
        obj = self.get(resource, name, self.config["namespace"])
        if obj is not None:
            require(self.owned[key] is not None and uid(obj) == self.owned[key], f"refusing deletion with unconfirmed UID: {name}")
            self.delete_owned(resource, name, self.owned[key])
        self.wait(lambda: self.get(resource, name, self.config["namespace"]) is None, self.config["cleanupTimeoutSeconds"], f"deletion of {resource}/{name}")
        del self.owned[key]

    def cleanup(self):
        self.cleaning = True
        report = {"complete": False, "errors": [], "observations": []}
        try:
            self.stop_driver()
            self.delete(SCALER, self.name)

            def released():
                obj = self.get(MD, self.name, self.config["namespace"])
                report["observations"].append({"ts": stamp(), "deployment": obj})
                if obj is None:
                    return True
                require(uid(obj) == self.owned.get((MD, self.name)), "target UID changed during binding release")
                self.advance_unrouted_retirement(obj)
                retirement = obj.get("status", {}).get("retirement")
                return not obj.get("metadata", {}).get("annotations", {}).get(BINDING) and (not retirement or retirement.get("state") == "Aborted")

            self.wait(released, self.config["cleanupTimeoutSeconds"], "binding and retirement release")
            self.delete(MD, self.name)

            def quota_released():
                children = self.children()
                queues = self.queues()
                report["observations"].append({"ts": stamp(), "children": children, "clusterQueues": queues})
                return not children and all(q["status"][k] == 0 for q in queues for k in ("pendingWorkloads", "reservingWorkloads", "admittedWorkloads"))

            self.wait(quota_released, self.config["cleanupTimeoutSeconds"], "children and quota release")
            if self.remote_root:
                self.remote("remove")
                self.remote_root = None
            report["complete"] = True
        except (HarnessError, OSError, ValueError) as error:
            report["errors"].append(str(error))
        finally:
            write_json(self.directory / "cleanup.json", report)
            self.cleaning = False
        return report

    def round(self, item, index, cm_name):
        self.empty()
        self.name = f"{self.config['namePrefix']}-{self.token}-{index}"
        self.directory = self.output / f"{index:02d}-{item['arm']}"
        if self.directory.exists():
            existing_report = read_json(self.directory / "report.json") if (self.directory / "report.json").exists() else None
            existing_cleanup = read_json(self.directory / "cleanup.json") if (self.directory / "cleanup.json").exists() else None
            existing_run = read_json(self.directory / "run.json") if (self.directory / "run.json").exists() else None
            if existing_report and existing_report.get("validity", {}).get("valid") and existing_cleanup and existing_cleanup.get("complete"):
                self.results.append({**item, "directory": self.directory.name, "valid": True, "parserExitCode": 0, "report": existing_report, "cleanup": True, "failureReasons": existing_run.get("failureReasons", []) if existing_run else []})
                return
            shutil.rmtree(self.directory)
        self.directory.mkdir()
        inputs = self.directory / "inputs"
        inputs.mkdir()
        self.pending_artifacts = []
        self.run = {"schemaVersion": "1", **item, "algorithm": self.plan["algorithm"], "capacityMode": "Catalog" if item["arm"] == "catalog" else "Manual", "catalogMode": self.plan["catalogMode"], "seed": self.config["seed"], "startedAt": stamp(), "finishedAt": "pending", "exitCode": 1, "interrupted": False, "deploymentUID": "uncreated", "scalerUID": "uncreated", "baselineByRole": self.plan["baselineByRole"], "cooldownRequiredSeconds": self.plan["cooldownRequiredSeconds"], "phaseEvents": [], "failureReasons": [], "provenance": {"fitRunIDs": self.config["fitRunIDs"], "evaluationTraceID": self.config["evaluationTraceID"], "startCondition": self.config["startCondition"], "cacheVerification": "operator declaration", "workload": self.config["workload"], "generator": "bench_client fixed concurrency with seeded prompt tags", "driverUID": self.driver_uid, "clientSHA256": hashlib.sha256((SCRIPTS / "bench_client.py").read_bytes()).hexdigest(), "adapterSHA256": hashlib.sha256(ADAPTER.encode()).hexdigest(), "catalog": self.plan["catalog"]}}
        (self.directory / "prepare_reqs.jsonl").touch()
        self.event("prepare", False)
        try:
            deployment = self.create(MD, self.deployment, self.name)
            self.run["deploymentUID"] = uid(deployment)
            self.target_uid = uid(deployment)
            write_json(inputs / "deployment.json", deployment)
            scaler = copy.deepcopy(self.scalers[item["arm"]])
            scaler["spec"]["targetRef"]["name"] = self.name
            if item["arm"] == "catalog":
                catalog_reference = scaler["spec"]["algorithm"]["capacityPrior"].get("catalogRef")
                if catalog_reference:
                    catalog_reference["name"] = cm_name
            scaler = self.create(SCALER, scaler, self.name)
            self.run["scalerUID"] = uid(scaler)
            write_json(inputs / "scaler.json", scaler)
            self.wait(self.ready, self.config["readinessTimeoutSeconds"], "exact baseline and binding")
            service, port = self.serving_endpoint(self.observed(MD, self.name))
            self.wait(lambda: self.serving_ready(service), self.config["readinessTimeoutSeconds"], "a ready endpoint behind the serving Service")
            self.endpoint = f"http://{service}.{self.config['namespace']}.svc:{port}"
            self.run["endpoint"] = self.endpoint
            self.remote_root = f"/tmp/gpustack-autoscaling-{self.token}-{index}"
            self.remote("stage", data=json.dumps({"bench_client.py": (SCRIPTS / "bench_client.py").read_text(), "adapter.py": ADAPTER}))
            for phase in self.config["workload"]["phases"]:
                if phase["concurrency"]:
                    self.load(phase)
                else:
                    self.quiet(phase)
            self.flush_artifacts()
            self.run["exitCode"] = 0
        except (HarnessError, OSError, ValueError, KeyError) as error:
            self.run["failureReasons"].append(str(error))
        finally:
            try:
                self.flush_artifacts()
            except (HarnessError, OSError, ValueError) as error:
                self.run["failureReasons"].append(str(error))
                self.run["exitCode"] = 1
            cleanup = self.cleanup()
            if not cleanup["complete"]:
                self.run["exitCode"] = 1
                self.run["failureReasons"].extend(cleanup["errors"])
            self.run.update(finishedAt=stamp(), interrupted=self.interrupted)
            write_json(self.directory / "run.json", self.run)
        result = subprocess.run([sys.executable, "-B", str(SCRIPTS / "autoscaling_report.py"), str(self.directory), "--out", str(self.directory / "report.json")], capture_output=True, text=True, timeout=self.config["operationTimeoutSeconds"] + 10)
        report = read_json(self.directory / "report.json") if (self.directory / "report.json").exists() else None
        valid = result.returncode == 0 and report is not None and report["validity"]["valid"] and cleanup["complete"] and not self.interrupted
        self.results.append({**item, "directory": self.directory.name, "valid": valid, "parserExitCode": result.returncode, "report": report, "cleanup": cleanup["complete"], "failureReasons": self.run["failureReasons"]})
        require(valid, f"invalid round {self.directory.name}: {self.run['failureReasons']} {report and report['validity']}")

    def summary(self, failure, cleaned):
        arms = {}
        for arm in ARMS:
            runs = [r for r in self.results if r["arm"] == arm]
            valid = [r for r in runs if r["valid"]]
            values = [sum(role["readyReplicaSeconds"] for role in r["report"]["timeline"]["roles"].values()) for r in valid]
            arms[arm] = {"validRepeats": len(valid), "invalidRepeats": len(runs) - len(valid), "readyReplicaSeconds": {"values": values, "min": min(values) if values else None, "max": max(values) if values else None, "median": statistics.median(values) if values else None}, "latenciesByRun": [{"directory": r["directory"], "phases": r["report"]["phases"]} for r in valid]}
        valid = not failure and cleaned and len(self.results) == len(self.plan["order"]) and all(a["validRepeats"] >= 3 for a in arms.values())
        write_json(self.output / "comparison.json", {"schemaVersion": "1", "comparisonKind": "capacity-comparison", "valid": valid, "failure": failure, "cleanupComplete": cleaned, "arms": arms, "runs": self.results})
        return valid

    def execute(self):
        failure = None
        cleaned = False
        cm_name = f"{self.config['namePrefix']}-{self.token}-prior"

        def interrupt(_signum, _frame):
            self.interrupted = True
            if not self.cleaning:
                raise HarnessError("comparison interrupted")

        signal.signal(signal.SIGINT, interrupt)
        signal.signal(signal.SIGTERM, interrupt)
        try:
            self.empty()
            self.driver()
            if self.cm is not None:
                self.create("configmap", self.cm, cm_name)
            for index, item in enumerate(self.plan["order"], 1):
                self.round(item, index, cm_name)
        except (HarnessError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
            failure = str(error)
        finally:
            self.cleaning = True
            try:
                self.delete("configmap", cm_name)
                cleaned = not self.owned and self.remote_root is None and self.process is None
            except (HarnessError, OSError, ValueError) as error:
                failure = f"{failure or ''}; catalog cleanup: {error}"
            self.cleaning = False
        return 0 if self.summary(failure, cleaned) else (130 if self.interrupted else 1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--validate-only", action="store_true")
    mode.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    try:
        inputs = validate(args.config.resolve())
        args.out.mkdir(parents=True, exist_ok=True)
        write_json(args.out / "validated-plan.json", {**inputs[-1], "configuration": inputs[0]})
        if inputs[3] is not None:
            write_json(args.out / "catalog-input.json", inputs[3])
        if args.validate_only:
            return 0
        return Runner(*inputs, args.out).execute()
    except (HarnessError, OSError, ValueError, KeyError, TypeError) as error:
        print(f"capacity comparison: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
