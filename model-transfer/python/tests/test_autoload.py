"""The autoload bootstrap: format recognition and launch env injection."""

import os
import pathlib
import sys
from types import SimpleNamespace

import pytest

from gpustack_model_transfer import _autoload
from gpustack_model_transfer.modelexpress_route import (
    apply_launch_env,
    argv_requests_gpustack_modelexpress,
    env_requests_gpustack_modelexpress,
)

_PACKAGE_ROOT = str(pathlib.Path(__file__).resolve().parents[1])


@pytest.fixture(autouse=True)
def clean_process_env(monkeypatch):
    for name in (
        "MX_LOAD_PLUGIN",
        "MX_LOAD_PLUGIN_BEFORE",
        "MX_ARTIFACT_TRANSFER",
        "MX_SERVER_ADDRESS",
        "GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT",
        "GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY",
    ):
        monkeypatch.delenv(name, raising=False)


@pytest.fixture
def finder_calls(monkeypatch):
    calls = []
    monkeypatch.setattr(_autoload, "_install_finder", lambda: calls.append("finder"))
    monkeypatch.setattr(_autoload, "_register_engines", lambda: calls.append("vllm"))
    return calls


@pytest.mark.parametrize(
    "argv",
    [
        ["vllm", "serve", "model", "--load-format", "modelexpress"],
        ["vllm", "serve", "model", "--load-format", "mx"],
        ["vllm", "serve", "model", "--load-format=modelexpress"],
        ["vllm", "serve", "model", "--load-format=MX"],
        ["vllm", "serve", "model", "--load_format", " Modelexpress "],
        ["vllm", "serve", "model", "--load-format", "auto", "--load-format", "mx"],
    ],
)
def test_the_model_express_formats_are_recognized_on_the_command_line(argv):
    assert argv_requests_gpustack_modelexpress(argv)


@pytest.mark.parametrize(
    "argv",
    [
        [],
        ["vllm", "serve", "model"],
        ["vllm", "serve", "model", "--load-format", "auto"],
        ["vllm", "serve", "model", "--load-format", "model-transfer"],
        ["vllm", "serve", "model", "--load-format=MODELEXPRESSX"],
        ["vllm", "serve", "model", "--load-format"],
    ],
)
def test_other_formats_are_not_recognized(argv):
    assert not argv_requests_gpustack_modelexpress(argv)


@pytest.mark.parametrize(
    "environ",
    [
        {"MX_LOAD_PLUGIN": "gpustack"},
        {"MX_LOAD_PLUGIN": " gpustack "},
        {"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "modelexpress"},
        {"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "mx"},
        {"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "MODELEXPRESS"},
    ],
)
def test_the_plugin_hook_variables_are_recognized(environ):
    assert env_requests_gpustack_modelexpress(environ)


@pytest.mark.parametrize(
    "environ",
    [
        {},
        {"MX_LOAD_PLUGIN": "model-transfer"},
        {"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "model-transfer"},
        {"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "gpustack-modelexpress"},
    ],
)
def test_other_environments_are_not_recognized(environ):
    assert not env_requests_gpustack_modelexpress(environ)


def test_apply_launch_env_names_the_plugin_and_its_insertion_point():
    env = {}
    apply_launch_env(env)
    assert env["MX_LOAD_PLUGIN"] == "gpustack"
    assert env["MX_LOAD_PLUGIN_BEFORE"] == "server-cache"
    assert "MX_ARTIFACT_TRANSFER" not in env


def test_apply_launch_env_enables_artifact_transfer_with_a_server():
    env = {"MX_SERVER_ADDRESS": "md-modelexpress.ns.svc:8001"}
    apply_launch_env(env)
    assert env["MX_ARTIFACT_TRANSFER"] == "1"


@pytest.mark.parametrize("server", ["", "   "])
def test_a_blank_server_keeps_artifact_transfer_off(server):
    env = {"MX_SERVER_ADDRESS": server}
    apply_launch_env(env)
    assert "MX_ARTIFACT_TRANSFER" not in env


def test_apply_launch_env_never_overrides_set_values():
    env = {
        "MX_LOAD_PLUGIN": "other",
        "MX_LOAD_PLUGIN_BEFORE": "rdma",
        "MX_ARTIFACT_TRANSFER": "0",
        "MX_SERVER_ADDRESS": "mx:8001",
    }
    apply_launch_env(env)
    assert env["MX_LOAD_PLUGIN"] == "other"
    assert env["MX_LOAD_PLUGIN_BEFORE"] == "rdma"
    assert env["MX_ARTIFACT_TRANSFER"] == "0"


def test_the_real_process_arms_the_route_for_the_command_line(
    monkeypatch, finder_calls
):
    monkeypatch.setattr(sys, "argv", ["vllm", "--load-format", "modelexpress"])
    requested = _autoload.bootstrap()
    assert requested
    assert finder_calls == ["finder", "vllm"]
    assert os.environ.get("MX_LOAD_PLUGIN") == "gpustack"


def test_the_real_process_applies_the_launch_env(monkeypatch, finder_calls):
    monkeypatch.setattr(sys, "argv", ["vllm", "--load-format", "mx"])
    monkeypatch.setenv("MX_SERVER_ADDRESS", "mx:8001")
    requested = _autoload.bootstrap()
    assert requested
    assert os.environ.get("MX_LOAD_PLUGIN") == "gpustack"
    assert os.environ.get("MX_LOAD_PLUGIN_BEFORE") == "server-cache"
    assert os.environ.get("MX_ARTIFACT_TRANSFER") == "1"


def test_the_real_process_recognizes_the_plugin_hook_alone(monkeypatch, finder_calls):
    monkeypatch.setattr(sys, "argv", [])
    monkeypatch.setenv("MX_LOAD_PLUGIN", "gpustack")
    requested = _autoload.bootstrap()
    assert requested
    assert finder_calls == ["finder", "vllm"]


def test_the_real_process_arms_the_finder_for_the_model_transfer_route(
    monkeypatch, finder_calls
):
    monkeypatch.setattr(sys, "argv", ["python", "-m", "source_worker"])
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT", "model-transfer")
    requested = _autoload.bootstrap()
    assert not requested
    assert finder_calls == ["finder"]


def test_a_hypothetical_command_line_is_a_pure_query(finder_calls):
    """An injected argv or environ gets the answer, never the arming."""
    env = {}
    requested = _autoload.bootstrap(argv=["vllm", "--load-format", "mx"], environ=env)
    assert requested
    assert finder_calls == []
    assert env == {}
    assert os.environ.get("MX_LOAD_PLUGIN") is None


def test_a_hypothetical_probe_arms_nothing(finder_calls):
    requested = _autoload.bootstrap(argv=[], environ={"MX_LOAD_PLUGIN": "gpustack"})
    assert requested
    assert finder_calls == []


def test_a_hypothetical_model_transfer_route_arms_nothing(finder_calls):
    requested = _autoload.bootstrap(
        argv=["python", "-m", "source_worker"],
        environ={"GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT": "model-transfer"},
    )
    assert not requested
    assert finder_calls == []


def test_bootstrap_leaves_a_plain_process_untouched(finder_calls):
    requested = _autoload.bootstrap(argv=["vllm"], environ={})
    assert not requested
    assert finder_calls == []


def test_a_require_bootstrap_arm_failure_propagates(monkeypatch, caplog):
    """Under Require a failed arm must not leave the route unenforced."""
    import logging

    from gpustack_model_transfer.adapters.modelexpress import registration

    def broken():
        raise TypeError("incompatible base layout")

    monkeypatch.setattr(registration, "register_strategy_route", broken)
    monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
    with caplog.at_level(logging.ERROR):
        with pytest.raises(TypeError):
            _autoload._register_engines()
    assert any(
        "under the Require policy" in record.getMessage() for record in caplog.records
    )


def test_an_unreadable_policy_keeps_the_start_advisory(monkeypatch, caplog):
    """A policy import broken by the same failure must not crash startup."""
    import logging
    import sys

    from gpustack_model_transfer.adapters.modelexpress import registration

    def broken():
        raise ImportError("the adapter package is broken")

    monkeypatch.setattr(registration, "register_strategy_route", broken)
    monkeypatch.setitem(
        sys.modules,
        "gpustack_model_transfer.adapters.modelexpress.policy",
        None,
    )
    with caplog.at_level(logging.WARNING):
        # Prefer: advisory, so a swallowed arm stays a warning.
        _autoload._register_engines()
        # The policy itself cannot be read, so even Require stays
        # advisory instead of raising from inside the failure handler.
        monkeypatch.setenv("GPUSTACK_MODEL_TRANSFER_DELIVERY_POLICY", "Require")
        _autoload._register_engines()
    assert any(
        "ModelExpress registration failed at bootstrap" in record.getMessage()
        for record in caplog.records
    )


def test_the_bootstrap_arms_the_strategy_route_not_the_engine_registries(monkeypatch):
    """The vLLM registry step stays on vLLM's own general_plugins entry.

    Registering it at .pth start would import vllm.model_executor.model_loader
    into every requested process — including an SGLang engine, where the
    import is pure cost.
    """
    from gpustack_model_transfer.adapters.modelexpress import registration

    calls = []
    monkeypatch.setattr(
        registration, "register_strategy_route", lambda: calls.append("route")
    )
    monkeypatch.setattr(
        registration, "register_vllm_load_formats", lambda: calls.append("vllm")
    )
    monkeypatch.setattr(registration, "register", lambda: calls.append("entry"))
    _autoload._register_engines()
    assert calls == ["route"]


def test_a_bootstrap_registration_failure_warns_with_the_traceback(monkeypatch, caplog):
    """A .pth swallow at debug level hides a broken route from the operator."""
    import logging

    from gpustack_model_transfer.adapters.modelexpress import registration

    def broken():
        raise TypeError("incompatible base layout")

    monkeypatch.setattr(registration, "register_strategy_route", broken)
    with caplog.at_level(logging.WARNING, logger="gpustack_model_transfer._autoload"):
        # Prefer: the outer bootstrap handler stays advisory at
        # interpreter start (the route's own handler enforces Require).
        _autoload._register_engines()
    records = [record for record in caplog.records if record.exc_info is not None]
    assert records
    assert records[-1].levelno == logging.WARNING
    assert "ModelExpress registration failed at bootstrap" in records[-1].getMessage()


def test_a_mid_list_choice_failure_does_not_abort_the_remaining_values(caplog):
    """One refused value must not stop the values after it registering."""
    import logging
    import types

    attempts = []

    def add_choices(value):
        for item in value if isinstance(value, list) else [value]:
            attempts.append(item)
            if item == "modelexpress":
                raise TypeError("modelexpress refused")

    module = types.ModuleType("fake_choices")
    module.add_load_format_choices = add_choices
    with caplog.at_level(logging.DEBUG):
        _autoload._patch_server_args(module)
    assert attempts == [
        # The batch attempt, refused partway through.
        "model-transfer",
        "modelexpress",
        # The per-value retry reaches past the refusal.
        "model-transfer",
        "modelexpress",
        "mx",
    ]


def test_install_runs_the_bootstrap(monkeypatch):
    seen = SimpleNamespace(argv=None, environ="unset")

    def fake_bootstrap(argv=None, environ=None):
        seen.argv = argv
        seen.environ = environ
        return True

    monkeypatch.setattr(_autoload, "bootstrap", fake_bootstrap)
    assert _autoload.install() is None
    # install() delegates without arguments: bootstrap itself defaults
    # argv to sys.argv and environ to os.environ.
    assert seen.argv is None
    assert seen.environ is None
