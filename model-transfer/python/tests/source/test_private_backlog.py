"""The private listener can buffer a finite handler-sized connection burst."""

import socket

from gpustack_model_transfer.source.transfer_service import (
    PrivateHandler,
    PrivateServer,
)


def test_private_listener_queues_64_connections_without_accepting_native_work(tmp_path):
    token = tmp_path / "private-token"
    token.write_text("test-token")
    server = PrivateServer(None, str(token), ("127.0.0.1", 0))
    connections = []
    try:
        for _ in range(64):
            connections.append(
                socket.create_connection(server.server_address, timeout=0.2)
            )
        assert len(connections) == 64
        assert PrivateHandler.protocol_version == "HTTP/1.0"
        for _ in range(64):
            assert server.slots.acquire(blocking=False)
        assert not server.slots.acquire(blocking=False)
    finally:
        for connection in connections:
            connection.close()
        server.server_close()
