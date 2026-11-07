"""Two-process private control test using explicit shared-memory fakes only."""

import base64
import ctypes
import http.client
import json
import multiprocessing
import runpy
import threading
import time
from multiprocessing import shared_memory
from pathlib import Path

from gpustack_model_transfer.source.transfer_service import PrivateServer


def source_process(pipe, bootstrap):
    helpers = runpy.run_path(
        str(Path(__file__).resolve().parents[1] / "source/test_native_source.py")
    )
    source, controls, backends, _ = helpers["supervisor"]([helpers["order"]()])
    source.poll_once()
    helpers["eventually"](lambda: source.workers["generation-1"].wait_until_filled(0))
    worker = source.workers["generation-1"]
    worker.control.lease_id = "lease"
    source.worker_instance_id = "instance-1"
    backend = backends["generation-1"]

    def post(registration, name, offset, length, target):
        shared_name = base64.b64decode(target["metadata"]).decode()
        region = shared_memory.SharedMemory(name=shared_name)
        region.buf[:length] = registration.blocks[name].data[offset : offset + length]
        return region

    backend.post_write = post
    backend.poll_write = lambda op: "succeeded"
    backend.close_write = lambda op: op.close()
    server = PrivateServer(source.operations, bootstrap, ("127.0.0.1", 0))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    tensor = worker.assignment.manifest.tensors[0]
    pipe.send((server.server_port, tensor.name, tensor.file, tensor.data_begin))
    pipe.recv()
    controls["generation-1"].action = "drain"
    helpers["eventually"](lambda: worker.state == "drained")
    server.shutdown()
    server.server_close()
    thread.join(timeout=3)
    pipe.close()


def post(port, path, body, token="secret-token"):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
    try:
        connection.request("POST", path, json.dumps(body), {"Authorization": token})
        response = connection.getresponse()
        result = json.loads(response.read())
        assert response.status == 200
        return result
    finally:
        connection.close()


def wait_status(port, common, expected):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        result = post(port, "/v1/operations/status", common)
        if result["state"] == expected:
            return result
        time.sleep(0.02)
    raise AssertionError("operation did not reach expected state")


def test_two_process_owned_window_and_control_lifetime(tmp_path):
    bootstrap = tmp_path / "bootstrap"
    bootstrap.write_text("secret-token")
    context = multiprocessing.get_context("spawn")
    parent, child = context.Pipe()
    process = context.Process(target=source_process, args=(child, str(bootstrap)))
    process.start()
    child.close()
    region = None
    try:
        assert parent.poll(10)
        port, tensor, filename, file_offset = parent.recv()
        region = shared_memory.SharedMemory(create=True, size=8)
        addr = ctypes.addressof(ctypes.c_char.from_buffer(region.buf))
        common = {
            "sourcePodUID": "pod-uid",
            "workerInstanceID": "instance-1",
            "generation": "generation-1",
            "leaseID": "lease",
            "sessionID": "session",
            "requestID": "operation",
        }
        owner = {
            "targetPodUID": "target-pod",
            "targetInstanceID": "target-process",
            "registrationID": "registration",
            "windowID": "window",
        }
        target = {
            "backend": "nixl",
            "address": addr,
            "length": 8,
            "metadata": base64.b64encode(region.name.encode()).decode(),
            "endpoint": "",
        }
        body = (
            common
            | owner
            | {
                "tensor": tensor,
                "file": filename,
                "fileOffset": file_offset,
                "tensorOffset": 0,
                "length": 8,
                "target": target,
            }
        )
        assert post(port, "/v1/operations", body)["state"] == "dispatching"
        assert wait_status(port, common, "terminal")["verdict"] == "succeeded"
        fixture = (
            Path(__file__).resolve().parents[3]
            / "contracts/fixtures/tiny_dense"
            / filename
        )
        assert (
            bytes(region.buf[:8]) == fixture.read_bytes()[file_offset : file_offset + 8]
        )
        post(port, "/v1/operations/ack", common | owner)
        wait_status(port, common, "acknowledged")
        assert (
            post(port, "/v1/sessions/close", common | {"requestID": "close"})["state"]
            == "closed"
        )
        parent.send("stop")
        process.join(timeout=10)
        assert process.exitcode == 0
    finally:
        if region is not None:
            region.close()
            region.unlink()
        if process.is_alive():
            process.terminate()
            process.join(timeout=3)
        parent.close()
