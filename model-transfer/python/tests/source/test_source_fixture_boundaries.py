"""Unknown control endpoints cannot masquerade as valid lease responses."""

import pytest
import sourceutil


@pytest.mark.parametrize(
    "path,status",
    [
        ("/v1/register", 200),
        ("/v1/native-start", 200),
        ("/v1/heartbeat", 200),
        ("/v1/filled", 200),
        ("/v1/drain-ack", 200),
        ("/v1/quarantine", 200),
        ("/v1/heartbeet", 404),
    ],
)
def test_fake_source_broker_rejects_unknown_path(path, status):
    broker = sourceutil.FakeBroker()
    try:
        actual, response = broker.reply(path, {})
        assert actual == status
        if status == 404:
            assert response == {"kind": "unknown-path"}
    finally:
        broker.close()
