"""The broker transport's capacity refusal carries its named wait."""

from gpustack_model_transfer.client_transport import (
    WirePreAdmission,
    _retry_after_seconds,
)


def test_retry_after_header_wins_over_the_body_figure():
    assert _retry_after_seconds("7", 5) == 7.0


def test_retry_after_body_figure_is_used_without_a_header():
    assert _retry_after_seconds(None, 5) == 5.0
    assert _retry_after_seconds("", 1.5) == 1.5


def test_retry_after_garbage_answers_none():
    assert _retry_after_seconds(None, None) is None
    assert _retry_after_seconds("soon", "later") is None
    assert _retry_after_seconds("0", 0) is None
    assert _retry_after_seconds("-3", -3) is None
    assert _retry_after_seconds(True, True) is None
    assert _retry_after_seconds("99999", "99999") is None


def test_pre_admission_carries_the_wait_and_the_kind():
    exc = WirePreAdmission(
        "broker refused before admission",
        authoritative=True,
        retry_after=5.0,
        kind="capacity-exhausted",
    )
    assert exc.authoritative
    assert exc.retry_after == 5.0
    assert exc.kind == "capacity-exhausted"


def test_a_plain_refusal_defaults_the_wait_and_the_kind():
    exc = WirePreAdmission("broker refused before admission", authoritative=True)
    assert exc.retry_after is None
    assert exc.kind is None


def test_projected_token_rotation_with_same_size_and_mtime(tmp_path):
    import os

    from gpustack_model_transfer.client_transport import BrokerTransport

    path = tmp_path / "token"
    path.write_text("first")
    original = path.stat()
    transport = BrokerTransport.__new__(BrokerTransport)
    transport._token_file = path
    assert transport._read_token() == "first"
    replacement = tmp_path / "replacement"
    replacement.write_text("other")
    os.utime(replacement, ns=(original.st_atime_ns, original.st_mtime_ns))
    replacement.replace(path)
    assert transport._read_token() == "other"
