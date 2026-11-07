"""Explicit CPU-only fixture for real Go broker and Python client integration.

Only JSON over stdin/stdout is a control channel; vendor APIs are never imported.
The shared-memory target is a test primitive and does not model GPU/NPU semantics.
"""

import base64
import ctypes
import hashlib
import json
import queue
import sys
import threading
import time
import uuid
from multiprocessing import resource_tracker, shared_memory
from pathlib import Path
from types import SimpleNamespace

from gpustack_model_transfer.client import EngineReader
from gpustack_model_transfer.source.backend import SourceRegistration
from gpustack_model_transfer.source.control import BootstrapClient, ControlClient
from gpustack_model_transfer.source.memory import HostBlock
from gpustack_model_transfer.source.supervisor import SourceSupervisor
from gpustack_model_transfer.source.transfer_service import PrivateServer

FIXTURE = Path(__file__).resolve().parents[3] / "contracts/fixtures/tiny_dense"


def emit(value):
    print(json.dumps(value, separators=(",", ":")), flush=True)


def prepare(root):
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    original = json.loads((FIXTURE / "manifest.json").read_bytes())
    checkpoint = (FIXTURE / "checkpoint.safetensors").read_bytes()
    config = b'{"model_type":"fixture"}'
    canonical_entries = [
        {
            "path": "checkpoint.safetensors",
            "size": len(checkpoint),
            "digest": "sha256:" + hashlib.sha256(checkpoint).hexdigest(),
        },
        {
            "path": "config.json",
            "size": len(config),
            "digest": "sha256:" + hashlib.sha256(config).hexdigest(),
        },
    ]
    canonical = b"gpustack-manifest v1\n" + b"".join(
        f"{entry['digest']} {entry['size']} {entry['path']}\n".encode()
        for entry in sorted(canonical_entries, key=lambda item: item["path"])
    )
    digest = "sha256:" + hashlib.sha256(canonical).hexdigest()
    listing = json.dumps(
        {"digest": digest, "entries": canonical_entries}, separators=(",", ":")
    ).encode()
    header_length = int.from_bytes(checkpoint[:8], "little")
    header = checkpoint[: 8 + header_length]
    records = []
    for suffix in ("a", "b"):
        uid = "artifact-" + suffix
        tree = root / suffix / "tree"
        view = root / suffix / "view"
        tree.mkdir(parents=True)
        view.mkdir(parents=True)
        (tree / "checkpoint.safetensors").write_bytes(checkpoint)
        (tree / "config.json").write_bytes(config)
        manifest = json.loads(json.dumps(original))
        manifest["artifact_uid"] = uid
        manifest["files"].append(
            {
                "path": "config.json",
                "size": len(config),
            }
        )
        raw = json.dumps(manifest, separators=(",", ":")).encode()
        files = []
        for name, payload in {
            "checkpoint.safetensors": header,
            "config.json": config,
            "manifest.json": listing,
        }.items():
            (view / name).write_bytes(payload)
            entry = {
                "name": name,
                "size": len(payload),
                "sha256": hashlib.sha256(payload).hexdigest(),
            }
            if name.endswith(".safetensors"):
                entry["truncated"] = True
            files.append(entry)
        (view / "view.json").write_text(
            json.dumps(
                {
                    "kind": "gpustack-model-transfer-metadata-view/v1",
                    "artifactDigest": digest,
                    "incomplete": True,
                    "files": files,
                }
            )
        )
        tensor = manifest["tensors"][0]
        tensor_bytes = checkpoint[tensor["data_begin"] : tensor["data_end"]]
        records.append(
            {
                "namespace": "tenant-" + suffix,
                "artifactName": "model-" + suffix,
                "artifactUID": uid,
                "sourceGeneration": 1,
                "digest": digest,
                "manifestSHA256": hashlib.sha256(raw).hexdigest(),
                "manifestJSON": raw.decode(),
                "treePath": str(tree.resolve()),
                "modelPath": str(view.resolve()),
                "totalBytes": len(checkpoint),
                "registrations": 1,
                "expectedFirstTensorSHA256": hashlib.sha256(tensor_bytes).hexdigest(),
            }
        )
    return {"event": "prepared", "fixtures": records}


class FakeBlock(HostBlock):
    def __init__(self, name, size, owner):
        super().__init__(name, size, owner)
        self.storage = ctypes.create_string_buffer(size)

    def data_ptr(self):
        if self.storage is None:
            raise RuntimeError("released")
        return ctypes.addressof(self.storage)


class FakeAllocator:
    def allocate(self, name, size):
        return FakeBlock(name, size, self)

    def release(self, block):
        block.storage = None


class FakeSourceBackend:
    def __init__(self, mode="normal"):
        self.mode = mode
        self.peers = []

    def start(self):
        pass

    def register(self, blocks):
        return SourceRegistration(
            "nixl",
            {name: uuid.uuid4().hex for name in blocks},
            dict(blocks),
            agent=self,
        )

    def disconnect(self, registration):
        pass

    def revoke(self, registration):
        registration.handles.clear()

    def post_write(self, registration, name, offset, length, target):
        region = shared_memory.SharedMemory(
            name=base64.b64decode(target["metadata"], validate=True).decode()
        )
        resource_tracker.unregister(region._name, "shared_memory")
        self.peers.append(region)
        region.buf[:length] = registration.blocks[name].view(offset, length)
        if self.mode == "ambiguous":
            raise RuntimeError("injected may-have-posted outcome")
        return region

    def poll_write(self, operation):
        return "succeeded"

    def close_write(self, operation):
        operation.close()
        self.peers.remove(operation)


class FakeWindow:
    def __init__(self, backend, pod_ip, size, *, memory_mode="host", device=None):
        if backend != "nixl":
            raise ValueError("the CPU fixture supports only the injected fake")
        if memory_mode != "host":
            raise ValueError("the CPU fixture supports only the host window")
        self.size = min(size, 16)
        self.shared = shared_memory.SharedMemory(create=True, size=self.size)
        self.block = self
        self.registration_id = uuid.uuid4().hex
        self.window_id = uuid.uuid4().hex
        self.closed = False
        self.copies = 0
        self.syncs = 0

    def start(self):
        pass

    def view(self, offset, length):
        return self.shared.buf[offset : offset + length]

    def descriptor(self):
        address = ctypes.addressof(ctypes.c_char.from_buffer(self.shared.buf))
        return {
            "backend": "nixl",
            "address": address,
            "length": self.size,
            "metadata": base64.b64encode(self.shared.name.encode()).decode(),
            "endpoint": "",
        }

    def copy_to(self, target, offset, length):
        target.data[offset : offset + length] = self.shared.buf[:length]
        self.copies += 1
        self.syncs += 1

    def synchronize(self, target):
        self.syncs += 1

    def close(self):
        self.shared.close()
        self.shared.unlink()
        self.closed = True


class FixtureEngine:
    def __init__(self, env):
        self.reader = EngineReader(
            env,
            native_factory=FakeWindow,
            target_validator=lambda descriptor, target: True,
        )
        self.model = SimpleNamespace(model=env["GPUSTACK_MODEL_TRANSFER_MODEL_PATH"])
        self.descriptors = None
        self.session = None
        self.target = None

    def command(self, message):
        op = message.get("op")
        if op == "probe":
            self.descriptors = self.reader.probe(self.model)
            return {"ok": True, "tensorCount": len(self.descriptors)}
        if op == "start":
            self.session = self.reader.start(self.model)
            return {
                "ok": True,
                "sessionID": self.session.session_id,
                "targetInstanceID": self.reader.target_instance_id,
            }
        if op == "fill":
            index = message.get("tensorIndex", 0)
            descriptor = self.descriptors[index]
            tensor = self.reader.manifest.tensor(descriptor.name)
            self.target = SimpleNamespace(
                data=bytearray(tensor.byte_size), device=SimpleNamespace(type="cuda")
            )
            self.session.fill(descriptor, self.target)
            return {
                "ok": True,
                "sha256": hashlib.sha256(self.target.data).hexdigest(),
                "bytes": len(self.target.data),
                "copies": self.session.native.copies,
                "syncs": self.session.native.syncs,
            }
        if op == "close":
            self.session.close()
            return {"ok": True, "closed": self.session.closed}
        if op == "abort":
            self.session.abort()
            return {"ok": True, "retainedUnknown": self.session.unknown}
        if op == "inspect":
            return {
                "ok": True,
                "hasSession": self.session is not None,
                "unknown": bool(self.session and self.session.unknown),
                "inflight": bool(self.session and self.session.inflight),
                "windowClosed": bool(
                    self.session and self.session.native and self.session.native.closed
                ),
            }
        if op == "exit":
            if self.session is not None and not self.session.closed:
                return {"ok": False, "kind": "session-held"}
            return {"ok": True, "exit": True}
        return {"ok": False, "kind": "invalid-command"}


def source(config):
    bootstrap_file, address = config["bootstrapFile"], config["controlAddress"]
    worker_id, instance = config["workerID"], config["workerInstanceID"]
    supervisor = SourceSupervisor(
        BootstrapClient(address, bootstrap_file),
        control_factory=lambda assignment: ControlClient(
            address,
            worker_id,
            assignment.generation,
            assignment.worker_secret,
            worker_instance_id=instance,
        ),
        backend_factory=lambda assignment: FakeSourceBackend(
            config.get("postMode", "normal")
        ),
        allocator_factory=lambda assignment: FakeAllocator(),
        worker_id=worker_id,
        backend="nixl",
        poll_interval=0.01,
    )
    supervisor.worker_instance_id = instance
    server = PrivateServer(supervisor.operations, bootstrap_file, ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    commands = queue.Queue()

    def input_loop():
        for line in sys.stdin:
            commands.put(json.loads(line))
        commands.put({"op": "eof"})

    threading.Thread(target=input_loop, daemon=True).start()
    emit(
        {
            "event": "ready",
            "privateAddress": f"127.0.0.1:{server.server_port}",
            "workerID": worker_id,
            "workerInstanceID": instance,
        }
    )
    failures = 0
    while True:
        try:
            supervisor.poll_once()
            failures = 0
        except Exception:
            failures += 1
            if failures >= 100 and not supervisor.workers:
                emit({"event": "control-error", "kind": "unavailable"})
                break
        try:
            command = commands.get(timeout=0.05)
        except queue.Empty:
            continue
        op = command.get("op")
        if op == "status":
            emit({"ok": True, "workers": supervisor.statuses()})
        elif op == "stop":
            supervisor.operations.freeze()
            if all(
                w.state in ("drained", "stopped") for w in supervisor.workers.values()
            ):
                emit({"ok": True, "stopped": True})
                break
            emit({"ok": False, "kind": "native-state-held"})
        elif op == "eof":
            # A vanished controller is not a native release fence.
            supervisor.operations.freeze()
            if all(
                w.state in ("drained", "stopped") for w in supervisor.workers.values()
            ):
                break
        else:
            emit({"ok": False, "kind": "invalid-command"})
    server.shutdown()
    server.server_close()
    thread.join(timeout=3)


def engine(config):
    holder = FixtureEngine(config["env"])
    emit({"event": "ready", "mode": "engine"})
    for line in sys.stdin:
        try:
            result = holder.command(json.loads(line))
        except BaseException as exc:
            result = {"ok": False, "kind": type(exc).__name__}
        emit(result)
        if result.get("exit"):
            return
    # Unexpected EOF must not turn unknown state into a fake native release.
    if holder.session is not None and not holder.session.closed:
        while True:
            time.sleep(60)


def main():
    first = json.loads(sys.stdin.readline())
    if first["mode"] == "prepare":
        emit(prepare(first["root"]))
    elif first["mode"] == "source":
        source(first)
    elif first["mode"] == "engine":
        engine(first)
    else:
        raise ValueError("unknown fixture mode")


if __name__ == "__main__":
    main()
