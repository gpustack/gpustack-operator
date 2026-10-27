"""Executable-mock tests for the bounded capacity-comparison lifecycle."""

import copy
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
HELPER = SCRIPTS / "autoscaling_rounds.sh"
ENTRY = SCRIPTS.parent / "cases" / "case-5-autoscaling.sh"

MOCK = r'''
import copy, fcntl, json, os, signal, sys, time
from pathlib import Path
root = Path(os.environ["MOCK_ROOT"])
fault = os.environ.get("MOCK_FAULT", "")
lock = (root / "state.lock").open("a")
fcntl.flock(lock, fcntl.LOCK_EX)
path = root / "state.json"
existed = path.exists()
state = json.loads(path.read_text()) if existed else {"objects": {}, "created": [], "active": False, "loaded": False, "gets": 0}
if not existed and fault == "preexisting":
    state["objects"]["ConfigMap/preexisting-config"] = {"kind": "ConfigMap", "metadata": {"name": "preexisting-config", "namespace": "comparison", "uid": "mock-preexisting"}}
args = sys.argv[1:]
assert args[:2] == ["--context", "explicit-test-context"]
args = args[2:]
namespace = None
if args[:1] == ["-n"]:
    namespace, args = args[1], args[2:]
assert namespace in (None, "comparison", "driver")
with (root / "calls.log").open("a") as stream:
    stream.write(json.dumps(args[:3]) + "\n")

def save():
    path.write_text(json.dumps(state))

def emit(value):
    save()
    print(json.dumps(value))
    sys.exit(0)

def kind(resource):
    if resource.startswith("modeldeploymentscalers"):
        return "ModelDeploymentScaler"
    if resource.startswith("modeldeployments"):
        return "ModelDeployment"
    if resource.startswith("serviceaccount"):
        return "ServiceAccount"
    if resource.startswith("rolebinding"):
        return "RoleBinding"
    if resource.startswith("role"):
        return "Role"
    if resource.startswith("endpointslice"):
        return "EndpointSlice"
    if resource.startswith("endpoint"):
        return "Endpoints"
    if resource.startswith("service"):
        return "Service"
    if resource.startswith("secret"):
        return "Secret"
    return "ConfigMap"

def objects(k):
    return [o for o in state["objects"].values() if o["kind"] == k]

def serving_service():
    for obj in state["objects"].values():
        if obj["kind"] == "Service":
            return obj
    return None

def rendered(obj):
    obj = copy.deepcopy(obj)
    if obj["kind"] == "ModelDeployment":
        scaler = objects("ModelDeploymentScaler")
        grown = state["active"] or (state["loaded"] and fault == "cooldown")
        count = 2 if grown else 1
        obj["spec"]["roles"][0]["replicas"] = count
        obj["status"] = {"roles": [{"name": "server", "desired": count, "ready": 0 if fault == "partial" else count}]}
        if scaler or fault == "binding":
            obj["metadata"]["annotations"] = {"modeldeployment.gpustack.ai/scaler-binding": json.dumps({"scalerName": obj["metadata"]["name"], "scalerUID": scaler[0]["metadata"]["uid"] if scaler else "old"})}
        if fault == "uid":
            obj["metadata"]["uid"] = "other-uid"
        if fault == "mutation" and len(state["created"]) >= 4:
            obj["spec"]["engine"]["image"] = "example.invalid/different-image"
    if obj["kind"] == "ModelDeploymentScaler":
        obj["status"] = {"conditions": [{"type": "Bound", "status": "True"}, {"type": "CapacityPriorsReady", "status": "True"}], "roles": [{"name": "server", "rawRecommendedReplicas": 3 if state["active"] else 1, "currentReplicas": 2 if state["active"] else 1, "readyReplicas": 2 if state["active"] else 1}], "resolvedCapacityPriors": [{"name": "server", "profileID": "pri-local", "sourceID": "local-fit", "compatibilityFingerprint": "sha256:mock"}]}
        if fault == "prior":
            obj["status"]["conditions"][-1]["status"] = "False"
    return obj

if args[0] == "create":
    obj = json.load(sys.stdin)
    obj["metadata"]["uid"] = "mock-" + str(len(state["created"]))
    if fault == "reused-uid" and len(state["created"]) >= 3:
        obj["metadata"]["uid"] = "mock-1"
    state["created"].append(copy.deepcopy(obj))
    state["objects"][obj["kind"] + "/" + obj["metadata"]["name"]] = obj
    if obj["kind"] == "ModelDeployment":
        state["loaded"] = False
        if fault != "missing-service":
            role = obj["spec"]["roles"][0]["name"]
            name = obj["metadata"]["name"] + "-" + role
            labels = {"app.kubernetes.io/name": "model-deployment",
                      "app.kubernetes.io/instance": obj["metadata"]["name"],
                      "app.kubernetes.io/component": role}
            owner = {
                "apiVersion": "worker.gpustack.ai/v1alpha1", "kind": obj["kind"],
                "name": obj["metadata"]["name"], "uid": obj["metadata"]["uid"],
                "controller": True, "blockOwnerDeletion": False,
            }
            if fault == "wrong-service-owner-uid":
                owner["uid"] = "foreign-deployment-uid"
            elif fault == "noncontroller-service-owner":
                owner["controller"] = False
            owners = [] if fault == "no-service-owner" else [owner]
            service = {"kind": "Service",
                       "metadata": {"name": name, "namespace": obj["metadata"]["namespace"],
                                    "uid": "mock-service-" + obj["metadata"]["name"], "labels": dict(labels),
                                    "ownerReferences": owners},
                       "spec": {"type": "ClusterIP", "selector": dict(labels),
                                "ports": [{"name": "http", "protocol": "TCP", "port": 8123, "targetPort": 8123}]}}
            if fault == "foreign-service":
                service["metadata"]["labels"]["app.kubernetes.io/instance"] = "another-deployment"
            state["objects"]["Service/" + name] = service
            state["objects"]["Endpoints/" + name] = {
                "kind": "Endpoints",
                "metadata": {"name": name, "namespace": obj["metadata"]["namespace"],
                             "uid": "mock-endpoints-" + obj["metadata"]["name"],
                             "ownerReferences": [{"uid": service["metadata"]["uid"]}]},
                "subsets": [] if fault == "no-endpoint" else [{"addresses": [{"ip": "10.0.0.7"}], "ports": [{"port": 8123}]}]}
            if fault == "leftover-rbac":
                state["objects"]["ServiceAccount/" + name] = {
                    "kind": "ServiceAccount",
                    "metadata": {"name": name, "namespace": obj["metadata"]["namespace"],
                                 "uid": "mock-account-" + obj["metadata"]["name"],
                                 "ownerReferences": [{"uid": obj["metadata"]["uid"]}]}}
            if fault == "foreign-object":
                state["objects"]["ConfigMap/foreign-config"] = {
                    "kind": "ConfigMap",
                    "metadata": {"name": "foreign-config", "namespace": obj["metadata"]["namespace"],
                                 "uid": "mock-foreign"}}
    emit(obj)
if args[0] == "get":
    resource = args[1]
    name = args[2] if len(args) > 2 and not args[2].startswith("-") else None
    state["gets"] += 1
    if fault == "api" or (fault == "sample" and state["active"] and resource.startswith("modeldeployments.")):
        save()
        print("mock API unavailable", file=sys.stderr)
        sys.exit(61)
    if resource == "pod":
        emit({"kind": "Pod", "metadata": {"uid": "driver-uid"}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}})
    if resource.startswith("clusterqueues"):
        count = 1 if state["active"] or (fault == "quota" and state["loaded"]) else 0
        emit({"kind": "ClusterQueue", "metadata": {"name": name, "uid": "queue-uid"}, "status": {"pendingWorkloads": 0, "reservingWorkloads": count, "admittedWorkloads": count}})
    if "," in resource:
        children = []
        for token in resource.split(","):
            base = token.split(".")[0]
            if base == "configmaps":
                children += objects("ConfigMap")
            elif base == "services":
                children += objects("Service")
            elif base == "endpoints":
                children += objects("Endpoints")
            elif base == "serviceaccounts":
                children += objects("ServiceAccount")
            elif base == "roles":
                children += objects("Role")
            elif base == "rolebindings":
                children += objects("RoleBinding")
            elif base == "pods" and state["active"]:
                children += [{"kind": "Pod", "metadata": {"uid": "child-uid"}, "status": {"reason": "Evicted" if fault == "preempt" else "Running"}}, {"kind": "Workload", "metadata": {"uid": "workload-uid"}, "status": {"admission": {"clusterQueue": "isolated-queue"}}}]
        emit({"items": children})
    if name:
        obj = state["objects"].get(kind(resource) + "/" + name)
        if obj:
            emit(rendered(obj))
        save()
        sys.exit(0)
    emit({"items": [rendered(o) for o in objects(kind(resource))]})
if args[0] == "delete":
    if "--raw" not in args:
        raise AssertionError("delete must carry a UID precondition")
    uri = args[args.index("--raw") + 1]
    body = json.loads(sys.stdin.read())
    with (root / "deletes.log").open("a") as stream:
        stream.write(json.dumps({"uri": uri, "body": body}) + "\n")
    name = uri.rsplit("/", 1)[-1]
    if "modeldeploymentscalers/" in uri:
        k = "ModelDeploymentScaler"
    elif "modeldeployments/" in uri:
        k = "ModelDeployment"
    elif "configmaps/" in uri:
        k = "ConfigMap"
    else:
        raise AssertionError("unexpected delete uri " + uri)
    if fault == "delete-error" and k == "ModelDeploymentScaler":
        print("mock delete rejected", file=sys.stderr)
        sys.exit(61)
    key = k + "/" + name
    stored = state["objects"].get(key)
    if fault == "replace" and stored is not None and not state.get("replaced"):
        stored["metadata"]["uid"] = "mock-replaced"
        state["replaced"] = True
        save()
    precondition = body.get("preconditions", {}).get("uid")
    if stored is None:
        print('Error from server (NotFound): {0} "{1}" not found'.format(k.lower(), name), file=sys.stderr)
        sys.exit(1)
    if precondition != stored["metadata"]["uid"]:
        print('Error from server (Conflict): Operation cannot be fulfilled on {0} "{1}": the UID in the precondition ({2}) does not match the UID in record ({3})'.format(k.lower(), name, precondition, stored["metadata"]["uid"]), file=sys.stderr)
        sys.exit(1)
    if fault == "delete-timeout" and k == "ModelDeploymentScaler":
        emit({})
    removed = state["objects"].pop(key, None)
    if removed and removed.get("kind") == "ModelDeployment":
        role = removed["spec"]["roles"][0]["name"]
        state["objects"].pop("Service/" + removed["metadata"]["name"] + "-" + role, None)
        state["objects"].pop("Endpoints/" + removed["metadata"]["name"] + "-" + role, None)
    emit({})
if args[0] == "exec":
    split = args.index("--")
    action, directory, *remote_args = args[split + 4:]
    remote = root / Path(directory).name
    if action == "stage":
        remote.mkdir()
        for filename, content in json.load(sys.stdin).items():
            (remote / filename).write_text(content)
    elif action == "run":
        phase = remote_args[0]
        options = remote_args[2:]
        if options[0] != "load":
            raise AssertionError("unexpected driver invocation")
        service = serving_service()
        if service is None:
            raise AssertionError("no serving Service to load against")
        expected = "http://{0}.{1}.svc:{2}".format(service["metadata"]["name"], service["metadata"]["namespace"], service["spec"]["ports"][0]["port"])
        endpoint = options[options.index("--endpoint") + 1]
        if endpoint != expected:
            raise AssertionError("load endpoint {0} is not the serving Service {1}".format(endpoint, expected))
        state["active"] = True
        state["loaded"] = True
        save()
        fcntl.flock(lock, fcntl.LOCK_UN)
        record = {"ttft_ms": 1, "itl_ms": [1, 2], "tpot_ms": 1.5, "e2e_ms": 5, "prompt_tokens": 32, "output_tokens": 4, "started_at": time.time()}
        (remote / (phase + "_reqs.jsonl")).write_text("bad json\n" if fault == "requests" else json.dumps(record) + "\n")
        (remote / (phase + ".json")).write_text(json.dumps({"mock": True}))
        if fault == "interrupt":
            os.kill(os.getppid(), signal.SIGINT)
        time.sleep(.35)
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = json.loads(path.read_text())
        state["active"] = False
    elif action == "read":
        if fault == "artifact" and remote_args[0].endswith("_reqs.jsonl"):
            print("mock artifact unavailable", file=sys.stderr)
            sys.exit(61)
        print((remote / remote_args[0]).read_text(), end="")
    elif action == "stop":
        state["active"] = False
        save()
        if fault == "stop":
            print("mock stop rejected", file=sys.stderr)
            sys.exit(61)
    elif action == "remove":
        state["removed"] = state.get("removed", 0) + 1
    else:
        raise AssertionError(action)
    save()
    sys.exit(0)
raise AssertionError(args)
'''


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def fixture(root, algorithm="TokenThroughput"):
    deployment = {
        "apiVersion": "worker.gpustack.ai/v1", "kind": "ModelDeployment",
        "metadata": {"name": "template", "namespace": "comparison", "annotations": {"identity.example/model": "qualified-identity"}},
        "spec": {
            "model": {"name": "local-model", "artifactRef": {"name": "local-model"}},
            "engine": {"name": "vLLM", "image": "example.invalid/qualified-engine@sha256:" + "a" * 64},
            "roles": [{"name": "server", "kind": "Server", "replicas": 1,
                       "size": 1, "instanceType": "local-small", "resources": {"accelerator": "1"}}],
        },
    }
    throughput = {
        "serverTargetTokensPerSecond": "300", "ttftTargetMilliseconds": "2000",
        "itlTargetMilliseconds": "100", "stableWindowSeconds": 30, "panicWindowSeconds": 10,
    }
    algorithm_spec = (
        {"type": "TokenThroughput", "tokenThroughput": copy.deepcopy(throughput)}
        if algorithm == "TokenThroughput"
        else {
            "type": "Predictive",
            "predictive": {
                "serverTokenCapacityPerReplica": "300", "horizonSeconds": 300, "minPoints": 6,
                "tokenThroughput": copy.deepcopy(throughput),
            },
        }
    )
    scaler = {
        "apiVersion": "worker.gpustack.ai/v1", "kind": "ModelDeploymentScaler",
        "metadata": {"name": "template", "namespace": "comparison"},
        "spec": {
            "targetRef": {"name": "template"}, "mode": "Active",
            "roles": [{"name": "server", "minReplicas": 1, "maxReplicas": 2}],
            "sampling": {"intervalSeconds": 10, "pendingTimeoutSeconds": 60},
            "behavior": {"scaleDown": {"stabilizationWindowSeconds": 1}},
            "algorithm": algorithm_spec,
        },
    }
    frozen, tuned, catalog_scaler = (copy.deepcopy(scaler) for _ in range(3))
    if algorithm == "TokenThroughput":
        tuned["spec"]["algorithm"]["tokenThroughput"]["serverTargetTokensPerSecond"] = "400"
        del catalog_scaler["spec"]["algorithm"]["tokenThroughput"]["serverTargetTokensPerSecond"]
    else:
        tuned["spec"]["algorithm"]["predictive"]["serverTokenCapacityPerReplica"] = "250"
        tuned["spec"]["algorithm"]["predictive"]["tokenThroughput"]["serverTargetTokensPerSecond"] = "250"
        del catalog_scaler["spec"]["algorithm"]["predictive"]["serverTokenCapacityPerReplica"]
        del catalog_scaler["spec"]["algorithm"]["predictive"]["tokenThroughput"]["serverTargetTokensPerSecond"]
    catalog_scaler["spec"]["algorithm"]["capacityPrior"] = {
        "catalogRef": {"name": "offline-prior", "key": "priors.json"},
        "roles": [{"name": "server", "profileID": "pri-local"}],
    }
    catalog = {
        "schemaVersion": "gpustack.ai/v1", "digest": "sha256:" + "b" * 64,
        "sources": [{"id": "local-fit", "revision": "fit-run-1", "outcome": "Accepted",
                     "artifacts": [{"path": "fit-results.json", "sha256": "c" * 64}]}],
        "profiles": [{"id": "pri-local", "sourceID": "local-fit", "digest": "sha256:" + "d" * 64,
                      "role": "Server", "workload": {"id": "local-workload", "revision": "fit-run-1"}}],
    }
    cm = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "offline-prior", "namespace": "comparison"},
          "data": {"priors.json": json.dumps(catalog)}}
    for name, value in (("deployment.json", deployment), ("frozen.json", frozen),
                        ("tuned.json", tuned), ("catalog-scaler.json", catalog_scaler), ("catalog.json", cm)):
        write_json(root / name, value)
    config = {
        "schemaVersion": "1", "context": "explicit-test-context", "namespace": "comparison",
        "namePrefix": "capacity", "deploymentFile": "deployment.json",
        "scalerFiles": {"frozen-manual": "frozen.json", "tuned-manual": "tuned.json", "catalog": "catalog-scaler.json"},
        "catalogFile": "catalog.json", "fitRunIDs": ["fit-run-1"], "evaluationTraceID": "evaluation-2",
        "clusterQueues": ["isolated-queue"], "repeats": 3, "order": "alternating", "seed": 71,
        "startCondition": "operator-declared-warm-cache/fresh-pods",
        "driver": {"namespace": "driver", "pod": "bench-driver", "container": "driver"},
        "workload": {"model": "local-model", "inputTokens": 32, "outputTokens": 4, "tokenOffset": 0,
                     "requestTimeoutSeconds": 1, "phases": [
                         {"name": "load", "durationSeconds": .05, "concurrency": 1},
                         {"name": "cooldown", "durationSeconds": 1, "concurrency": 0}]},
        "sampleIntervalSeconds": .02, "operationTimeoutSeconds": 15,
        "readinessTimeoutSeconds": 5, "cleanupTimeoutSeconds": 5,
        "startupAllowanceSeconds": 0, "retirementAllowanceSeconds": 0,
    }
    write_json(root / "comparison.json", config)
    return config


def mock_environment(root):
    binary = root / "bin"
    binary.mkdir()
    for name, target in (("python3", sys.executable), ("bash", "/bin/bash"), ("dirname", "/usr/bin/dirname")):
        (binary / name).symlink_to(target)
    kubectl = binary / "kubectl"
    kubectl.write_text(f"#!{sys.executable}\nimport os,sys\nfrom pathlib import Path\n"
                       "with Path(os.environ['MOCK_CALLS']).open('a') as f: f.write(repr(sys.argv)+'\\n')\n"
                       "sys.exit(61)\n")
    kubectl.chmod(0o755)
    env = dict(os.environ, PATH=str(binary), PYTHONDONTWRITEBYTECODE="1", MOCK_CALLS=str(root / "calls.log"))
    return env


def lifecycle_environment(root, fault):
    env = mock_environment(root)
    (root / "bin" / "kubectl").write_text(f"#!{sys.executable}\n" + MOCK)
    env.update(MOCK_ROOT=str(root), MOCK_FAULT=fault)
    return env


class LifecycleTests(unittest.TestCase):
    def test_actual_entrypoint_lifecycle(self):
        cases = [
            ("complete", "", 0, 9, None),
            ("API error", "api", 1, 0, "API unavailable"),
            ("delete rejection", "delete-error", 1, 1, "delete rejected"),
            ("deletion timeout", "delete-timeout", 1, 1, "timeout waiting for deletion"),
            ("binding release timeout", "binding", 1, 1, "binding and retirement release"),
            ("quota release timeout", "quota", 1, 1, "children and quota release"),
            ("UID mismatch", "uid", 1, 1, "UID mismatch"),
            ("UID reuse", "reused-uid", 1, 2, "reused UID"),
            ("admission mutation", "mutation", 1, 2, "admitted manifests differ"),
            ("partial readiness", "partial", 1, 1, "exact baseline and binding"),
            ("catalog not resolved", "prior", 1, 3, "exact baseline and binding"),
            ("sampling error", "sample", 1, 1, "sampling failed"),
            ("preemption", "preempt", 130, 1, "preempted or evicted"),
            ("interruption", "interrupt", 130, 1, "comparison interrupted"),
            ("incomplete cooldown", "cooldown", 1, 1, "incomplete cooldown"),
            ("malformed request artifact", "requests", 1, 1, "malformed_requests"),
            ("foreign serving Service", "foreign-service", 1, 1, "not owned by the target"),
            ("missing serving Service", "missing-service", 1, 1, "missing serving Service"),
            ("no ready endpoint", "no-endpoint", 1, 1, "ready endpoint behind the serving Service"),
        ]
        for name, fault, expected_code, expected_rounds, expected_reason in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                result = subprocess.run(
                    ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--execute"],
                    env=lifecycle_environment(root, fault), text=True, capture_output=True, timeout=60,
                )
                self.assertEqual(result.returncode, expected_code, result.stdout + result.stderr)
                summary = json.loads((root / "output" / "comparison.json").read_text())
                self.assertEqual(len(summary["runs"]), expected_rounds, summary.get("failure"))
                self.assertEqual(summary["valid"], not fault)
                if fault in ("delete-error", "delete-timeout", "binding", "quota", "uid"):
                    self.assertFalse(summary["runs"][0]["cleanup"])
                if not fault:
                    state = json.loads((root / "state.json").read_text())
                    self.assertEqual(state["objects"], {})
                    self.assertEqual(state["removed"], 9)
                    for obj in state["created"]:
                        if obj["kind"] == "ModelDeployment":
                            self.assertEqual(obj["metadata"]["annotations"]["identity.example/model"], "qualified-identity")
                    for arm in summary["arms"].values():
                        self.assertEqual(arm["validRepeats"], 3)
                    run = summary["runs"][0]
                    self.assertEqual(run["report"]["timeline"]["roles"]["server"]["requestedMax"], 2)
                    raw = [sample["roles"]["server"]["rawRecommendedReplicas"] for sample in run["report"]["timeline"]["evidence"]]
                    self.assertEqual(max(raw), 3)
                    self.assertTrue(run["report"]["cooldown"]["complete"])
                else:
                    self.assertTrue(summary["failure"])
                    self.assertIn(expected_reason, summary["failure"])


class FailureDiagnosticsTests(unittest.TestCase):
    def test_load_failure_retains_artifacts(self):
        cases = [
            ("driver stop failure", "stop", "driver stop failed", ["load_reqs.jsonl", "load.json"], []),
            ("missing request artifact", "artifact", "artifact load_reqs.jsonl unavailable", ["load.json"], ["load_reqs.jsonl"]),
        ]
        for name, fault, expected_reason, captured, missing in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                result = subprocess.run(
                    ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--execute"],
                    env=lifecycle_environment(root, fault), text=True, capture_output=True, timeout=60,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                summary = json.loads((root / "output" / "comparison.json").read_text())
                self.assertFalse(summary["valid"])
                self.assertIn(expected_reason, summary["failure"])
                self.assertEqual(len(summary["runs"]), 1, summary.get("failure"))
                round_directory = root / "output" / "01-frozen-manual"
                for artifact in captured:
                    self.assertTrue((round_directory / artifact).exists(), artifact)
                for artifact in missing:
                    self.assertFalse((round_directory / artifact).exists(), artifact)


class DeletionTests(unittest.TestCase):
    def test_deletion_carries_uid_precondition(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            result = subprocess.run(
                ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                 "--out", str(root / "output"), "--execute"],
                env=lifecycle_environment(root, ""), text=True, capture_output=True, timeout=180,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            deletes = [json.loads(line) for line in (root / "deletes.log").read_text().splitlines()]
            self.assertTrue(deletes)
            for entry in deletes:
                self.assertEqual(entry["body"]["apiVersion"], "meta.k8s.io/v1")
                self.assertEqual(entry["body"]["kind"], "DeleteOptions")
                self.assertEqual(entry["body"]["propagationPolicy"], "Foreground")
                self.assertTrue(entry["body"]["preconditions"]["uid"])
                self.assertTrue(entry["uri"].startswith(("/api/v1/namespaces/comparison/", "/apis/worker.gpustack.ai/v1alpha1/namespaces/comparison/")), entry["uri"])

    def test_uid_replacement_between_get_and_delete_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            result = subprocess.run(
                ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                 "--out", str(root / "output"), "--execute"],
                env=lifecycle_environment(root, "replace"), text=True, capture_output=True, timeout=180,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            summary = json.loads((root / "output" / "comparison.json").read_text())
            self.assertEqual(len(summary["runs"]), 1, summary.get("failure"))
            self.assertIn("does not match", summary["failure"])
            state = json.loads((root / "state.json").read_text())
            self.assertTrue([obj for obj in state["objects"].values() if obj["metadata"].get("uid") == "mock-replaced"])


class InventoryTests(unittest.TestCase):
    def test_auxiliary_inventory_ownership(self):
        cases = [
            ("leftover router RBAC", "leftover-rbac", 1, "children and quota release", []),
            ("foreign namespace object", "foreign-object", 0, None, ["foreign-config"]),
            ("preexisting namespace object", "preexisting", 0, None, []),
        ]
        for name, fault, expected_code, expected_reason, foreign in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                result = subprocess.run(
                    ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--execute"],
                    env=lifecycle_environment(root, fault), text=True, capture_output=True, timeout=120,
                )
                self.assertEqual(result.returncode, expected_code, result.stdout + result.stderr)
                summary = json.loads((root / "output" / "comparison.json").read_text())
                if expected_reason:
                    self.assertIn(expected_reason, summary["failure"])
                run = json.loads((root / "output" / "01-frozen-manual" / "run.json").read_text())
                self.assertEqual([entry["name"] for entry in run.get("foreignObjects", [])], foreign)
                state = json.loads((root / "state.json").read_text())
                if fault == "preexisting":
                    self.assertIn("ConfigMap/preexisting-config", state["objects"])


class ValidationTests(unittest.TestCase):
    def test_validation_does_not_call_cluster_or_driver(self):
        cases = [
            ("complete local configuration", "TokenThroughput", None, None, None, True),
            ("predictive configuration", "Predictive", None, None, None, True),
            ("missing prior", "TokenThroughput", "comparison.json", ("catalogFile",), "absent.json", False),
            ("too few repeats", "TokenThroughput", "comparison.json", ("repeats",), 2, False),
            ("reused fit trace", "TokenThroughput", "comparison.json", ("evaluationTraceID",), "fit-run-1", False),
            ("different pacing", "TokenThroughput", "tuned.json", ("spec", "behavior", "scaleDown", "stabilizationWindowSeconds"), 2, False),
            ("different slo", "TokenThroughput", "tuned.json", ("spec", "algorithm", "tokenThroughput", "ttftTargetMilliseconds"), "3000", False),
            ("predictive catalog explicit forecast", "Predictive", "catalog-scaler.json", ("spec", "algorithm", "predictive", "serverTokenCapacityPerReplica"), "300", False),
            ("predictive catalog explicit fallback", "Predictive", "catalog-scaler.json", ("spec", "algorithm", "predictive", "tokenThroughput", "serverTargetTokensPerSecond"), "300", False),
            ("predictive manual missing forecast", "Predictive", "tuned.json", ("spec", "algorithm", "predictive", "serverTokenCapacityPerReplica"), None, False),
            ("predictive manual missing fallback", "Predictive", "tuned.json", ("spec", "algorithm", "predictive", "tokenThroughput", "serverTargetTokensPerSecond"), None, False),
            ("predictive different horizon", "Predictive", "tuned.json", ("spec", "algorithm", "predictive", "horizonSeconds"), 180, False),
            ("predictive different fallback slo", "Predictive", "tuned.json", ("spec", "algorithm", "predictive", "tokenThroughput", "ttftTargetMilliseconds"), "3000", False),
            ("different baseline", "TokenThroughput", "deployment.json", ("spec", "roles"), [{"name": "server", "kind": "Server", "replicas": 2}], False),
            ("different metadata", "TokenThroughput", "tuned.json", ("metadata", "annotations"), {"identity": "different"}, False),
            ("existing binding", "TokenThroughput", "deployment.json", ("metadata", "annotations"), {"modeldeployment.gpustack.ai/scaler-binding": "{}"}, False),
            ("unaccepted source", "TokenThroughput", "catalog.json", ("data", "priors.json"), "{}", False),
            ("driver shares namespace", "TokenThroughput", "comparison.json", ("driver", "namespace"), "comparison", False),
            ("short cooldown", "TokenThroughput", "comparison.json", ("startupAllowanceSeconds",), 2, False),
            ("routed deployment", "TokenThroughput", "deployment.json", ("spec", "router"), {"name": "llm-d-router"}, False),
            ("cache deployment", "TokenThroughput", "deployment.json", ("spec", "kvCache"), {"name": "pool"}, False),
            ("transfer deployment", "TokenThroughput", "deployment.json", ("spec", "kvTransfer"), {"name": "leg"}, False),
            ("seeded order", "TokenThroughput", "comparison.json", ("order",), "seeded", True),
        ]
        for name, algorithm, filename, path, value, valid in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root, algorithm)
                if filename:
                    document = json.loads((root / filename).read_text())
                    target = document
                    for part in path[:-1]:
                        target = target[part]
                    if value is None:
                        target.pop(path[-1], None)
                    else:
                        target[path[-1]] = value
                    write_json(root / filename, document)
                result = subprocess.run(
                    ["/bin/bash", str(HELPER), "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--validate-only"],
                    env=mock_environment(root), text=True, capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
                self.assertFalse((root / "calls.log").exists(), "validation contacted the cluster")
                if valid:
                    plan = json.loads((root / "output" / "validated-plan.json").read_text())
                    self.assertEqual(len(plan["order"]), 9)
                    self.assertEqual(plan["comparisonKind"], "capacity-comparison")
                    self.assertEqual(plan["algorithm"], algorithm)

    def test_predictive_mock_execution(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root, "Predictive")
            result = subprocess.run(
                ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                 "--out", str(root / "output"), "--execute"],
                env=lifecycle_environment(root, ""), text=True, capture_output=True, timeout=60,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            plan = json.loads((root / "output" / "validated-plan.json").read_text())
            self.assertEqual(plan["algorithm"], "Predictive")
            summary = json.loads((root / "output" / "comparison.json").read_text())
            self.assertTrue(summary["valid"])
            self.assertEqual(len(summary["runs"]), 9)
            for arm in summary["arms"].values():
                self.assertEqual(arm["validRepeats"], 3)

    def test_serving_service_controller_ownership(self):
        cases = [
            ("missing Service ownerReference", "no-service-owner"),
            ("different UID Service ownerReference", "wrong-service-owner-uid"),
            ("noncontroller Service ownerReference", "noncontroller-service-owner"),
        ]
        for name, fault in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                result = subprocess.run(
                    ["/bin/bash", str(ENTRY), "--capacity-comparison", "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--execute"],
                    env=lifecycle_environment(root, fault), text=True, capture_output=True, timeout=60,
                )
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                summary = json.loads((root / "output" / "comparison.json").read_text())
                self.assertFalse(summary["valid"])
                self.assertEqual(len(summary["runs"]), 1, summary.get("failure"))
                self.assertIn("not owned by the target", summary["failure"])
                self.assertFalse((root / "output" / "01-frozen-manual" / "load_reqs.jsonl").exists())
                state = json.loads((root / "state.json").read_text())
                self.assertFalse(state["loaded"])

    def test_capacity_prior_shape_validation(self):
        """capacityPrior carries no policy: absent is manual, present resolves offline."""
        cases = [
            ("catalog capacityPrior without policy field", None, True),
            ("manual arm with capacityPrior", "manual-prior", False),
            ("removed policy field is rejected", "policy-field", False),
            ("catalog scaler without capacityPrior", "no-prior", False),
        ]
        for name, shape, valid in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                catalog_scaler = json.loads((root / "catalog-scaler.json").read_text())
                algorithm = catalog_scaler["spec"]["algorithm"]
                if shape is None:
                    algorithm["capacityPrior"] = {
                        "catalogRef": {"name": "offline-prior", "key": "priors.json"},
                        "roles": [{"name": "server", "profileID": "pri-local"}],
                    }
                elif shape == "policy-field":
                    algorithm["capacityPrior"]["policy"] = "Catalog"
                elif shape == "no-prior":
                    algorithm.pop("capacityPrior", None)
                write_json(root / "catalog-scaler.json", catalog_scaler)
                if shape == "manual-prior":
                    frozen = json.loads((root / "frozen.json").read_text())
                    frozen["spec"]["algorithm"]["capacityPrior"] = {
                        "catalogRef": {"name": "offline-prior", "key": "priors.json"},
                        "roles": [{"name": "server", "profileID": "pri-local"}],
                    }
                    write_json(root / "frozen.json", frozen)
                result = subprocess.run(
                    ["/bin/bash", str(HELPER), "--config", str(root / "comparison.json"),
                     "--out", str(root / "output"), "--validate-only"],
                    env=mock_environment(root), text=True, capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode == 0, valid, result.stdout + result.stderr)
                self.assertFalse((root / "calls.log").exists(), "validation contacted the cluster")

    def test_prompt_seed_does_not_change_length_correction(self):
        spec = importlib.util.spec_from_file_location("autoscaling_rounds", SCRIPTS / "autoscaling_rounds.py")
        runner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runner)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "adapter.py").write_text(runner.ADAPTER)
            (root / "bench_client.py").write_text(
                "import json, os, sys\n"
                "def build_prompt(tokens, tag, offset=0): return [tokens, tag, offset]\n"
                "def main():\n"
                " print(json.dumps({'prompt': build_prompt(512, 'load-0-1', 7), 'argv': sys.argv[1:], 'driver': os.environ['IN_BENCH_DRIVER']}))\n"
            )
            result = subprocess.run([sys.executable, "-B", str(root / "adapter.py"), "71", "load"], capture_output=True, text=True, timeout=5, check=True)
            observed = json.loads(result.stdout)
            self.assertEqual(observed, {"prompt": [512, "71:load-0-1", 7], "argv": ["load"], "driver": "1"})


if __name__ == "__main__":
    unittest.main()
