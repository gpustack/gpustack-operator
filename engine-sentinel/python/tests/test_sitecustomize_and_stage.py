"""Tests for stage_build.py, dormant/active sitecustomize.py, anti-recursion, and zero VLLM_PLUGINS mutation."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

REPO_PYTHON_ROOT = Path(__file__).resolve().parents[1]


def test_stage_build_flat_layout_and_dormant_sitecustomize_zero_vllm_plugins(
    tmp_path: Path,
):
    stage_script = REPO_PYTHON_ROOT / "stage_build.py"
    assert stage_script.is_file(), "stage_build.py must exist"

    # 1. Stage flat unpacked layout locked to the operator release version.
    staged_dir = tmp_path / "engine-sentinel"
    build_res = subprocess.run(
        [
            sys.executable,
            str(stage_script),
            "--output-dir",
            str(staged_dir),
            "--version",
            "v0.12.3",
        ],
        cwd=REPO_PYTHON_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert build_res.returncode == 0, build_res.stdout + build_res.stderr

    assert staged_dir.is_dir()
    assert (staged_dir / "sitecustomize.py").is_file()
    assert (staged_dir / "gpustack_engine_sentinel" / "__init__.py").is_file()
    assert (
        staged_dir / "gpustack_engine_sentinel-0.12.3.dist-info" / "METADATA"
    ).is_file()
    assert (
        staged_dir / "gpustack_engine_sentinel-0.12.3.dist-info" / "RECORD"
    ).is_file()
    assert not list(staged_dir.rglob("__pycache__"))
    assert not list(staged_dir.rglob("*.pyc"))

    # 2. Verify non-PEP440 dev versions fall back cleanly to 0.0.0.
    dev_dir = tmp_path / "engine-sentinel-dev"
    dev_res = subprocess.run(
        [
            sys.executable,
            str(stage_script),
            "--output-dir",
            str(dev_dir),
            "--version",
            "main-dirty-abc123",
        ],
        cwd=REPO_PYTHON_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert dev_res.returncode == 0, dev_res.stdout + dev_res.stderr
    assert (dev_dir / "gpustack_engine_sentinel-0.0.0.dist-info" / "METADATA").is_file()

    # 3. Verify dormant mode when GPUSTACK_ENGINE_SENTINEL_ENABLE is unset + downstream chaining!
    downstream_dir = tmp_path / "downstream"
    downstream_dir.mkdir()
    (downstream_dir / "sitecustomize.py").write_text(
        "import os\nos.environ['_DOWNSTREAM_RAN'] = 'yes'\n",
        encoding="utf-8",
    )

    env = {
        k: v
        for k, v in os.environ.items()
        if not k.startswith("GPUSTACK_ENGINE_SENTINEL") and k != "VLLM_PLUGINS"
    }
    env["PYTHONPATH"] = f"{staged_dir}{os.pathsep}{downstream_dir}"

    probe_code = """
import importlib.metadata as m
import json, os, sys
eps_ep = [e.name for e in m.entry_points(group="vllm.endpoint_plugins")]
eps_gen = [e.name for e in m.entry_points(group="vllm.general_plugins")]
eps_omni_gen = [e.name for e in m.entry_points(group="vllm_omni.general_plugins")]
eps_omni_plat = [e.name for e in m.entry_points(group="vllm_omni.platform_plugins")]
print(json.dumps({
    "imported": "gpustack_engine_sentinel" in sys.modules,
    "downstream_chained": os.environ.get("_DOWNSTREAM_RAN"),
    "vllm_plugins_env": os.environ.get("VLLM_PLUGINS"),
    "dist_version": m.version("gpustack-engine-sentinel"),
    "endpoint_plugins": eps_ep,
    "general_plugins": eps_gen,
    "omni_general_plugins": eps_omni_gen,
    "omni_platform_plugins": eps_omni_plat,
}))
"""
    dormant = subprocess.run(
        [sys.executable, "-c", probe_code],
        env=env,
        capture_output=True,
        text=True,
        check=True,
    )
    payload = json.loads(dormant.stdout.strip())
    assert payload["imported"] is False
    assert payload["downstream_chained"] == "yes"
    assert payload["vllm_plugins_env"] is None
    assert payload["dist_version"] == "0.12.3"
    assert payload["endpoint_plugins"] == []
    assert payload["general_plugins"] == []
    assert payload["omni_general_plugins"] == []
    assert payload["omni_platform_plugins"] == []


def test_sitecustomize_anti_recursion_with_duplicate_copies_and_cyclic_downstream(
    tmp_path: Path,
):
    copy1 = tmp_path / "copy1"
    copy2 = tmp_path / "copy2"
    cyclic_downstream = tmp_path / "cyclic_downstream"
    copy1.mkdir()
    copy2.mkdir()
    cyclic_downstream.mkdir()

    src_sc = REPO_PYTHON_ROOT / "sitecustomize.py"
    shutil.copy2(src_sc, copy1 / "sitecustomize.py")
    shutil.copy2(src_sc, copy2 / "sitecustomize.py")

    # Downstream sitecustomize attempts to re-exec copy1/sitecustomize.py and copy2/sitecustomize.py!
    (cyclic_downstream / "sitecustomize.py").write_text(
        f"""
import importlib.util, os
os.environ["_CYCLIC_COUNT"] = str(int(os.environ.get("_CYCLIC_COUNT", "0")) + 1)
for target in ({str(copy1 / "sitecustomize.py")!r}, {str(copy2 / "sitecustomize.py")!r}):
    spec = importlib.util.spec_from_file_location("sitecustomize", target)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
""",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env.pop("VLLM_PLUGINS", None)
    env["PYTHONPATH"] = os.pathsep.join(
        [
            str(copy1),
            str(copy1),
            str(copy2),
            str(cyclic_downstream),
            str(REPO_PYTHON_ROOT),
        ]
    )

    code = """
import json, os, sys
finders = [f for f in sys.meta_path if type(f).__name__.endswith("PostImportFinder")]
print(json.dumps({
    "cyclic_count": os.environ.get("_CYCLIC_COUNT"),
    "visited_len": len(getattr(sys, "_gpustack_sitecustomize_visited", set())),
    "finder_count": len(finders),
    "vllm_plugins": os.environ.get("VLLM_PLUGINS"),
}))
"""
    res = subprocess.run(
        [sys.executable, "-c", code],
        env=env,
        capture_output=True,
        text=True,
        check=True,
    )
    info = json.loads(res.stdout.strip())
    assert info["cyclic_count"] == "1"
    assert info["visited_len"] >= 3
    assert info["finder_count"] == 1
    assert info["vllm_plugins"] is None


def test_zero_vllm_plugins_mutation_preserves_omni_ascend_and_model_transfer(
    tmp_path: Path,
):
    # Create fake installed distributions registering vllm-ascend, model-transfer,
    # gpustack_modelexpress, vllm_omni.general_plugins, and vllm_omni.platform_plugins,
    # plus a fake sglang_omni.serve.openai_api module to verify sglang-omni stays untouched.
    fake_site = tmp_path / "fake_site"
    dist_info = fake_site / "ecosystem_plugins-1.0.0.dist-info"
    dist_info.mkdir(parents=True)
    (dist_info / "METADATA").write_text(
        "Metadata-Version: 2.1\nName: ecosystem-plugins\nVersion: 1.0.0\n",
        encoding="utf-8",
    )
    (dist_info / "entry_points.txt").write_text(
        """[vllm.platform_plugins]
ascend = fake_ecosystem:ascend_plugin

[vllm.general_plugins]
model_transfer = fake_ecosystem:model_transfer_plugin
gpustack_modelexpress = fake_ecosystem:modelexpress_plugin

[vllm_omni.general_plugins]
omni_core = fake_ecosystem:omni_general_plugin

[vllm_omni.platform_plugins]
omni_npu = fake_ecosystem:omni_platform_plugin
""",
        encoding="utf-8",
    )
    (fake_site / "fake_ecosystem.py").write_text(
        """
def ascend_plugin(): return "ascend"
def model_transfer_plugin(): return "model_transfer"
def modelexpress_plugin(): return "modelexpress"
def omni_general_plugin(): return "omni_core"
def omni_platform_plugin(): return "omni_npu"
""",
        encoding="utf-8",
    )
    sglang_omni_dir = fake_site / "sglang_omni" / "serve"
    sglang_omni_dir.mkdir(parents=True)
    (fake_site / "sglang_omni" / "__init__.py").write_text("", encoding="utf-8")
    (sglang_omni_dir / "__init__.py").write_text("", encoding="utf-8")
    (sglang_omni_dir / "openai_api.py").write_text(
        "class DummySGLangOmniApp:\n    pass\napp = DummySGLangOmniApp()\n",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env.pop("VLLM_PLUGINS", None)
    env["PYTHONPATH"] = os.pathsep.join([str(REPO_PYTHON_ROOT), str(fake_site)])

    probe = """
import importlib.metadata as m
import json, os
import sglang_omni.serve.openai_api as sgl_omni

# Simulate vllm_omni/plugins/__init__.py load_omni_plugins_by_group:
allowed_plugins = os.environ.get("VLLM_PLUGINS")
def visible(group):
    eps = [ep.name for ep in m.entry_points(group=group)]
    if allowed_plugins is not None:
        allow = [p.strip() for p in allowed_plugins.split(",") if p.strip()]
        eps = [n for n in eps if n in allow]
    return sorted(eps)

print(json.dumps({
    "vllm_plugins": os.environ.get("VLLM_PLUGINS"),
    "vllm_platform": visible("vllm.platform_plugins"),
    "vllm_general": visible("vllm.general_plugins"),
    "omni_general": visible("vllm_omni.general_plugins"),
    "omni_platform": visible("vllm_omni.platform_plugins"),
    "sglang_omni_untouched": not hasattr(sgl_omni.app, "_gpustack_sentinel_installed"),
}))
"""
    res = subprocess.run(
        [sys.executable, "-c", probe],
        env=env,
        capture_output=True,
        text=True,
        check=True,
    )
    out = json.loads(res.stdout.strip())
    assert out["vllm_plugins"] is None
    assert out["vllm_platform"] == ["ascend"]
    assert out["vllm_general"] == ["gpustack_modelexpress", "model_transfer"]
    assert out["omni_general"] == ["omni_core"]
    assert out["omni_platform"] == ["omni_npu"]
    assert out["sglang_omni_untouched"] is True


def test_sitecustomize_symlinked_downstream_does_not_hang(tmp_path: Path):
    symlink_dir = tmp_path / "symlink_dir"
    symlink_dir.mkdir()
    os.symlink(
        REPO_PYTHON_ROOT / "sitecustomize.py",
        symlink_dir / "sitecustomize.py",
    )
    tail_dir = tmp_path / "tail_dir"
    tail_dir.mkdir()
    (tail_dir / "sitecustomize.py").write_text(
        "import os\nos.environ['_TAIL_RAN'] = '1'\n",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env.pop("GPUSTACK_ENGINE_SENTINEL_ENABLE", None)
    env["PYTHONPATH"] = os.pathsep.join(
        [str(REPO_PYTHON_ROOT), str(symlink_dir), str(tail_dir)]
    )
    res = subprocess.run(
        [sys.executable, "-c", "import os; print(os.environ.get('_TAIL_RAN', ''))"],
        env=env,
        capture_output=True,
        text=True,
        timeout=10,
        check=True,
    )
    assert res.stdout.strip() == "1"


def test_sitecustomize_broken_downstream_does_not_abort_bootstrap(tmp_path: Path):
    broken_dir = tmp_path / "broken_dir"
    broken_dir.mkdir()
    (broken_dir / "sitecustomize.py").write_text(
        "raise RuntimeError('boom in downstream sitecustomize')\n",
        encoding="utf-8",
    )

    env = dict(os.environ)
    env["GPUSTACK_ENGINE_SENTINEL_ENABLE"] = "1"
    env["PYTHONPATH"] = os.pathsep.join([str(REPO_PYTHON_ROOT), str(broken_dir)])
    probe = (
        "import json, sys; "
        "finders = [f for f in sys.meta_path if type(f).__name__.endswith('PostImportFinder')]; "
        "print(json.dumps({'finders': len(finders)}))"
    )
    res = subprocess.run(
        [sys.executable, "-c", probe],
        env=env,
        capture_output=True,
        text=True,
        timeout=10,
        check=True,
    )
    assert json.loads(res.stdout.strip())["finders"] == 1
    assert "boom in downstream sitecustomize" in res.stderr
