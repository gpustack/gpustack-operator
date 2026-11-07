"""Fixture protocol failures remain explicit and observable."""

import http.client
import json
import runpy
import ssl
from pathlib import Path

import pytest


@pytest.mark.parametrize(
    "body,status",
    [
        (b"{", 409),
        (b"{}", 409),
        (b'{"podUID":"p","targetInstanceID":"i","targetReleased":true}', 200),
    ],
)
def test_fake_broker_delete_reports_and_records(body, status, tmp_path):
    broker_type = runpy.run_path(str(Path(__file__).with_name("go_fake_broker.py")))[
        "FakeTLSBroker"
    ]
    broker = broker_type(tmp_path, [], "127.0.0.1:1", "secret")
    broker.sessions["session"] = {"podUID": "p", "targetInstanceID": "i", "suffix": "a"}
    connection = http.client.HTTPSConnection(
        *broker.server.server_address,
        context=ssl._create_unverified_context(),
        timeout=3,
    )
    try:
        connection.request(
            "DELETE", "/v1/sessions/session", body, {"Authorization": "Bearer token-a"}
        )
        response = connection.getresponse()
        payload = json.loads(response.read())
        assert response.status == status
        assert payload == (
            {"state": "closed"} if status == 200 else {"kind": "fixture-error"}
        )
        if body != b"{":
            assert broker.calls == [("/v1/sessions/session", json.loads(body))]
    finally:
        connection.close()
        broker.close()
