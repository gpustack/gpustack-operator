"""Behavioral regression checks for the build pipeline, without external services."""

import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(sys.argv.pop(1)).resolve()


class BuildPipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="gpustack-build-check-")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.tree = self.work / "tree"
        shutil.copytree(ROOT / "hack/lib", self.tree / "hack/lib")
        for name in ("build.sh", "test.sh", "package.sh"):
            shutil.copy(ROOT / "hack" / name, self.tree / "hack" / name)
        self.bin = self.work / "bin"
        self.bin.mkdir()
        self.trace = self.work / "trace.jsonl"
        self.env = {
            k: v
            for k, v in os.environ.items()
            if not k.startswith(("GIT_", "BUILD_", "CGO_", "PACKAGE_", "E2E_"))
            and k not in ("VERSION", "COMMIT", "CC", "GOFLAGS", "GOOS", "GOARCH")
        }
        self.env.update(
            PATH=f"{self.bin}:{os.environ['PATH']}",
            BUILD_CHECK_TRACE=str(self.trace),
            LOG_COLORFUL="false",
        )
        self.stub(
            "go",
            """
if sys.argv[1] == "env":
    print({"GOHOSTOS": "linux", "GOHOSTARCH": "amd64", "GOOS": "linux", "GOARCH": "amd64", "CC": "gcc"}[sys.argv[2]])
elif sys.argv[1] == "build":
    output = next(a[3:] for a in sys.argv if a.startswith("-o="))
    Path(output).write_text("#!/bin/sh\\nexit " + os.environ.get("BUILD_CHECK_VERSION_EXIT", "0") + "\\n")
    Path(output).chmod(0o755)
elif sys.argv[1] == "list":
    print("example/first\\nexample/skip\\nexample/last")
""",
        )
        shutil.copytree(
            ROOT / ".agents/skills/_e2e-lib/scripts",
            self.tree / ".agents/skills/_e2e-lib/scripts",
        )
        self.stub(
            "readelf",
            """
print(os.environ.get("BUILD_CHECK_DYNAMIC", "Dynamic section: (NEEDED) libc.so"))
print(" ".join(["padding"] * int(os.environ.get("BUILD_CHECK_PADDING", "0"))))
sys.exit(int(os.environ.get("BUILD_CHECK_READELF_EXIT", "0")))
""",
        )

    def stub(self, name, code):
        path = self.bin / name
        path.write_text(
            f"#!{sys.executable}\n"
            + """
import json, os, sys
from pathlib import Path
with open(os.environ["BUILD_CHECK_TRACE"], "a") as trace:
    trace.write(json.dumps({"command": Path(sys.argv[0]).name, "args": sys.argv[1:],
                           "env": {k: v for k, v in os.environ.items() if k.startswith(("CGO_", "PACKAGE_"))}}) + "\\n")
"""
            + code
        )
        path.chmod(0o755)

    def run_script(self, script, *args, **env):
        return subprocess.run(
            ["bash", str(script), *args],
            cwd=self.tree,
            env=self.env | env,
            text=True,
            capture_output=True,
        )

    def calls(self, name):
        if not self.trace.exists():
            return []
        return [
            r
            for line in self.trace.read_text().splitlines()
            if (r := json.loads(line))["command"] == name
        ]

    def test_version_without_git(self):
        cases = [
            ({"GIT_COMMIT": "a" * 40, "GIT_VERSION": "v1.2.3"}, "a" * 40, "v1.2.3"),
            (
                {
                    "COMMIT": "b" * 40,
                    "VERSION": "v2.3.4",
                    "GIT_COMMIT": "a" * 40,
                    "GIT_VERSION": "v1.2.3",
                },
                "b" * 40,
                "v2.3.4",
            ),
            ({}, "unknown", "unknown"),
        ]
        for env, commit, version in cases:
            with self.subTest(env=env):
                result = subprocess.run(
                    [
                        "bash",
                        "-eu",
                        "-c",
                        'source "$1"; gpustack::version::get_version_vars; printf "%s %s" "$GIT_COMMIT" "$GIT_VERSION"',
                        "test",
                        str(self.tree / "hack/lib/version.sh"),
                    ],
                    cwd=self.tree,
                    env=self.env | env,
                    text=True,
                    capture_output=True,
                )
                assert result.returncode == 0, result.stderr
                assert result.stdout == f"{commit} {version}"

    def test_build_flags_and_stamps(self):
        result = self.run_script(
            self.tree / "hack/build.sh",
            BUILD_TAGS="first second",
            GIT_COMMIT="a" * 40,
            GIT_VERSION="v1.2.3",
            CGO_CFLAGS="-O3",
            CGO_LDFLAGS="-Wl,-z,now",
        )
        assert result.returncode == 0, result.stderr
        build = next(c for c in self.calls("go") if c["args"][0] == "build")
        assert "-tags=first second" in build["args"]
        assert "-buildvcs=false" in build["args"]
        flags = next(a for a in build["args"] if a.startswith("-ldflags="))
        assert "Version=v1.2.3" in flags
        assert "GitCommit=" + "a" * 40 in flags
        assert "-extldflags=-Wl,-z,lazy" in flags
        assert build["env"]["CGO_CFLAGS"] == "-O3 -std=gnu17"
        assert build["env"]["CGO_LDFLAGS"].endswith("-Wl,-z,lazy")

    def test_elf_and_native_execution_failures(self):
        cases = [
            {"BUILD_CHECK_DYNAMIC": "(BIND_NOW)", "BUILD_CHECK_PADDING": "50000"},
            {"BUILD_CHECK_DYNAMIC": "(FLAGS) BIND_NOW"},
            {"BUILD_CHECK_DYNAMIC": "(FLAGS_1) Flags: NOW PIE"},
            {"BUILD_CHECK_READELF_EXIT": "1"},
            {"BUILD_CHECK_VERSION_EXIT": "1"},
        ]
        for env in cases:
            with self.subTest(env=env):
                result = self.run_script(self.tree / "hack/build.sh", **env)
                assert result.returncode != 0, result.stdout + result.stderr

    def test_exclusions_keep_tags_and_run_each_package_once(self):
        result = self.run_script(
            self.tree / "hack/test.sh", "skip", BUILD_TAGS="first second"
        )
        assert result.returncode == 0, result.stderr
        calls = self.calls("go")
        listing = next(c for c in calls if c["args"][0] == "list")
        assert "-tags=first second" in listing["args"]
        tests = [c for c in calls if c["args"][0] == "test"]
        assert len(tests) == 1
        assert tests[0]["args"][-2:] == ["example/first", "example/last"]

    def test_native_default_optimization_flags(self):
        result = self.run_script(self.tree / "hack/build.sh")
        assert result.returncode == 0, result.stderr
        build = next(c for c in self.calls("go") if c["args"][0] == "build")
        assert "-O2" in build["env"]["CGO_CFLAGS"]

    def test_non_login_path_discovers_toolchain_and_installed_tools(self):
        for name in ("toolchain", "tools", "selected"):
            folder = self.work / name
            folder.mkdir()
            command = folder / ("formatter" if name == "tools" else "go")
            command.write_text("#!/bin/sh\nexit 0\n")
            command.chmod(0o755)
        empty_path = self.work / "empty-path"
        empty_path.mkdir()
        for selected in (False, True):
            with self.subTest(selected=selected):
                initial = str(self.work / "selected") if selected else str(empty_path)
                script = """
source "$1"
gpustack::build::add_go_paths "$2" "$3"
gpustack::build::add_go_paths "$2" "$3"
command -v go
command -v formatter
printf '%s' "$PATH"
"""
                result = subprocess.run(
                    [
                        "/bin/bash",
                        "-eu",
                        "-c",
                        script,
                        "test",
                        str(ROOT / "hack/lib/build.sh"),
                        str(self.work / "toolchain"),
                        str(self.work / "tools"),
                    ],
                    env=self.env | {"PATH": initial},
                    text=True,
                    capture_output=True,
                )
                assert result.returncode == 0, result.stderr
                lines = result.stdout.splitlines()
                assert lines[0] == str(
                    self.work / ("selected" if selected else "toolchain") / "go"
                )
                assert lines[1] == str(self.work / "tools/formatter")
                assert lines[2].split(":").count(str(self.work / "tools")) == 1

    def test_generated_binding_link_contract(self):
        for root, pattern in (("gen/binding", "*/config.yaml"), ("binding", "*/*.go")):
            for path in (ROOT / root).glob(pattern):
                for line in path.read_text().splitlines():
                    if "--unresolved-symbols=ignore-in-object-files" in line:
                        with self.subTest(path=str(path.relative_to(ROOT))):
                            assert "-Wl,-z,lazy" not in line
                            assert "linux" in line
        for name in (
            "binding/hgml/hgml.go",
            "binding/hgml/const.go",
            "gen/binding/hgml/config.yaml",
        ):
            with self.subTest(path=name):
                assert "-std=gnu17" in (ROOT / name).read_text()

    def test_binding_macro_generation_is_not_pointer_valued(self):
        for package, macro, go_name in (
            ("amdsmi", "AMDSMI_LIB_VERSION_STRING", "LIB_VERSION_STRING"),
            ("rsmi", "RSMI_MAX_NUM_POWER_PROFILES", "MAX_NUM_POWER_PROFILES"),
            ("cndev", "CNDEV_EXPORT", "EXPORT"),
        ):
            with self.subTest(package=package):
                config = (ROOT / "gen/binding" / package / "config.yaml").read_text()
                assert f'action: ignore, from: "^{macro}$"' in config
                generated = (ROOT / "binding" / package / "const.go").read_text()
                assert not re.search(rf"\b{go_name}\s*=", generated)
                if package != "cndev":
                    authored = ROOT / "gen/binding" / package / "macro_constants.go"
                    copied = ROOT / "binding" / package / "macro_constants.go"
                    assert authored.read_bytes() == copied.read_bytes()
                    assert f"const {go_name} = C.{macro}" in authored.read_text()

    def test_wheel_version_without_git(self):
        for name in ("gpustack-operator", "model-transfer"):
            dockerfile = (ROOT / "pack" / name / "Dockerfile").read_text()
            code = re.search(
                r'version=".*?export PIP_BREAK_SYSTEM_PACKAGES=1', dockerfile, re.DOTALL
            )
            assert code is not None, name
            for supplied, want in (
                ("v1.2.3", "1.2.3"),
                ("0.9.3.dev38", "0.9.3.dev38"),
                ("v1.2.3-rc1", "1.2.3rc1"),
                ("dev", "0.0.0"),
                ("", "0.0.0"),
            ):
                with self.subTest(image=name, version=supplied):
                    result = subprocess.run(
                        [
                            "bash",
                            "-eu",
                            "-c",
                            code[0]
                            + '\nprintf "%s\\n%s" "$SETUPTOOLS_SCM_PRETEND_VERSION" "$SETUPTOOLS_SCM_PRETEND_VERSION_FOR_GPUSTACK_MODEL_TRANSFER"',
                        ],
                        env=os.environ | {"GPUSTACK_GIT_VERSION": supplied},
                        text=True,
                        capture_output=True,
                    )
                    assert result.returncode == 0, result.stderr
                    assert result.stdout.splitlines() == [want, want]

    def test_real_elf_link_mode(self):
        compiler = shutil.which("aarch64-linux-gnu-gcc")
        target_arch = "arm64"
        if sys.platform == "linux":
            compiler = shutil.which("cc")
            target_arch = "arm64" if os.uname().machine == "aarch64" else "amd64"
        reader = shutil.which("readelf") or shutil.which("llvm-readelf")
        if compiler is None or reader is None:
            self.skipTest(
                "a Linux compiler and readelf are required for the ELF fixture"
            )
        source = self.work / "main.c"
        source.write_text(
            '#include <stdio.h>\nint main(void) { puts("fixture version 1"); return 0; }\n'
        )
        for mode in ("lazy", "now"):
            with self.subTest(mode=mode):
                binary = self.work / mode
                build = subprocess.run(
                    [compiler, str(source), f"-Wl,-z,{mode}", "-o", str(binary)],
                    text=True,
                    capture_output=True,
                )
                assert build.returncode == 0, build.stderr
                result = subprocess.run(
                    [
                        "bash",
                        "-eu",
                        "-c",
                        'source "$1"; source "$2"; gpustack::build::verify_binary "$3" linux "$4"',
                        "test",
                        str(ROOT / "hack/lib/log.sh"),
                        str(ROOT / "hack/lib/build.sh"),
                        str(binary),
                        target_arch,
                    ],
                    env=self.env | {"READELF": reader},
                    text=True,
                    capture_output=True,
                )
                assert (result.returncode == 0) == (mode == "lazy"), (
                    result.stdout + result.stderr
                )

    def test_real_cgo_binding_flags(self):
        compiler = shutil.which("aarch64-linux-gnu-gcc")
        target_arch = "arm64"
        if sys.platform == "linux":
            compiler = shutil.which("cc")
            target_arch = "arm64" if os.uname().machine == "aarch64" else "amd64"
        reader = shutil.which("readelf") or shutil.which("llvm-readelf")
        if compiler is None or reader is None or shutil.which("go") is None:
            self.skipTest("a Linux compiler, Go and readelf are required for CGO")
        directives = set()
        for path in (ROOT / "binding").glob("*/*.go"):
            directives.update(
                line
                for line in path.read_text().splitlines()
                if line.startswith("#cgo linux")
                and ("--unresolved-symbols=" in line or "-std=gnu17" in line)
            )
        binary = self.work / "cgo-fixture"
        source = binary.with_suffix(".go")
        source.write_text(
            "package main\n/*\n"
            + "\n".join(sorted(directives))
            + '\n*/\nimport "C"\nfunc main() {}\n'
        )
        result = subprocess.run(
            [
                "bash",
                "-eu",
                "-c",
                """source "$1"; source "$2"
gpustack::build::setup_env linux "$4"
GOOS=linux GOARCH="$4" go build -buildvcs=false -ldflags=-extldflags=-Wl,-z,lazy -o "$3" "$3.go"
gpustack::build::verify_binary "$3" linux "$4"
""",
                "test",
                str(ROOT / "hack/lib/log.sh"),
                str(ROOT / "hack/lib/build.sh"),
                str(binary),
                target_arch,
            ],
            cwd=self.work,
            env=self.env
            | {"PATH": os.environ["PATH"], "CC": compiler, "READELF": reader},
            text=True,
            capture_output=True,
        )
        assert result.returncode == 0, result.stdout + result.stderr

    def test_remote_command_quoting_failures_and_digest(self):
        (self.tree / "tracked.go").write_text("package fixture\n")
        self.stub(
            "git",
            """
args = sys.argv[1:]
if args[0] == "-C": args = args[2:]
if args[0] == "rev-parse": print("a" * 40)
elif args[0] == "ls-files" and "--deleted" not in args: sys.stdout.write("tracked.go\\0")
elif args[0] == "bundle": Path(args[2]).write_text("bundle")
elif args[0] == "diff": sys.stdout.write("removed file.go\\0")
""",
        )
        self.stub("rsync", "")
        self.stub("scp", "")
        self.stub(
            "ssh",
            """
import shlex, subprocess
command = sys.argv[-1]
if command.startswith("mktemp "): print("/tmp/gpustack-e2e-head.fixture")
elif "xargs -0" in command:
    assert sys.stdin.buffer.read() == b"removed file.go\\0"
    if os.environ.get("BUILD_CHECK_REMOTE_FAIL") == "delete": sys.exit(1)
elif "git rev-parse" in command: print("a" * 40)
elif command.startswith("bash -c "):
    parts = shlex.split(command)
    assert len(parts) == 3
    script = parts[2]
    subprocess.run(["bash", "-n", "-c", script], check=True)
    if "make package" in script:
        if os.environ.get("BUILD_CHECK_REMOTE_FAIL") == "package": sys.exit(1)
    elif "imagetools inspect" in script:
        print("" if os.environ.get("BUILD_CHECK_REMOTE_FAIL") == "digest" else "sha256:" + "a" * 64)
""",
        )
        for failure in ("", "delete", "package", "digest"):
            with self.subTest(failure=failure):
                result = self.run_script(
                    self.tree / ".agents/skills/_e2e-lib/scripts/build-load.sh",
                    "test",
                    E2E_BUILDER_SSH="user@fixture",
                    E2E_BUILDER_DIR="/home/user/a builder's repo",
                    E2E_IMAGE_NAMESPACE="registry:5000/team",
                    GIT_VERSION="v1.2.3",
                    BUILD_CHECK_REMOTE_FAIL=failure,
                )
                if failure:
                    assert result.returncode != 0, result.stdout + result.stderr
                    assert "built & pushed:" not in result.stdout
                else:
                    assert result.returncode == 0, result.stdout + result.stderr
                    assert (
                        "registry:5000/team/gpustack-operator@sha256:" in result.stdout
                    )
        commands = [
            shlex.split(c["args"][-1])[2]
            for c in self.calls("ssh")
            if c["args"][-1].startswith("bash -c ") and "make package" in c["args"][-1]
        ]
        assert commands
        assert "/home/user/a builder's repo" in shlex.split(commands[0])
        assert "VERSION=v1.2.3" in shlex.split(commands[0])

    def test_cross_build_copy_failure_stops_before_building(self):
        self.stub("ssh", "sys.stdin.buffer.read()\n")
        result = subprocess.run(
            [
                "bash",
                "-c",
                'source "$1"; xput "$2" /tmp/fixture',
                "test",
                str(
                    ROOT
                    / ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/lib.sh"
                ),
                str(self.work / "missing"),
            ],
            env=self.env | {"XB_MODE": "ssh", "XB_HOST": "fixture"},
            text=True,
            capture_output=True,
        )
        assert result.returncode != 0, result.stdout + result.stderr

    def test_cross_build_copy_quotes_paths(self):
        self.stub(
            "ssh",
            "import subprocess\nsys.exit(subprocess.run(['bash', '-c', sys.argv[-1]]).returncode)\n",
        )
        source = self.work / "source"
        source.write_bytes(b"fixture\x00contents")
        for mode, command in (
            ("ssh", 'xput "$2" "$3"'),
            ("pty", 'commands=$(_xb_pty_put_cmds "$2" "$3"); bash -eu -c "$commands"'),
        ):
            with self.subTest(mode=mode):
                destination = (
                    self.work / "builder's $directory" / f"{mode} file with spaces"
                )
                result = subprocess.run(
                    [
                        "bash",
                        "-c",
                        'source "$1"; ' + command,
                        "test",
                        str(
                            ROOT
                            / ".agents/skills/gpustack-operator-xbuild-and-verify/scripts/lib.sh"
                        ),
                        str(source),
                        str(destination),
                    ],
                    env=self.env | {"XB_MODE": mode, "XB_HOST": "fixture"},
                    text=True,
                    capture_output=True,
                )
                assert result.returncode == 0, result.stdout + result.stderr
                assert destination.read_bytes() == source.read_bytes()

    def test_cross_compile_requires_compiler_and_elf_reader(self):
        for env in ({"BUILD_ARCH": "unsupported"}, {"READELF": "/nonexistent/readelf"}):
            with self.subTest(env=env):
                result = self.run_script(self.tree / "hack/build.sh", **env)
                assert result.returncode != 0, result.stdout + result.stderr

    def test_local_load_success_and_explicit_namespace(self):
        self.stub("make", "")
        self.stub("kind", "")
        self.stub(
            "kubectl",
            "print('docker-desktop' if 'config' in sys.argv else 'kind://docker/local/node')",
        )
        result = self.run_script(
            self.tree / ".agents/skills/_e2e-lib/scripts/build-load.sh",
            "test",
            E2E_IMAGE_NAMESPACE="registry:5000/team",
            PACKAGE_PUSH="true",
        )
        assert result.returncode == 0, result.stdout + result.stderr
        assert (
            "built & loaded: registry:5000/team/gpustack-operator:test" in result.stdout
        )
        assert self.calls("make")[0]["env"]["PACKAGE_PUSH"] == "false"
        assert (
            "registry:5000/team/gpustack-operator:test" in self.calls("kind")[0]["args"]
        )

    def test_operator_image_build_preserves_failures(self):
        dockerfile = (ROOT / "pack/gpustack-operator/Dockerfile").read_text()
        script = dockerfile.split("# Build gpustack-operator\n", 1)[1]
        script = script.split("<<EOF\n", 1)[1].split("\nEOF", 1)[0]
        script = script.replace(
            "/out/bin/gpustack-operator", str(self.bin / "gpustack-operator")
        )
        path = self.work / "image-build.sh"
        path.write_text(script)
        (self.tree / "staging").mkdir()
        self.stub("make", 'sys.exit(int(os.environ["BUILD_CHECK_MAKE_EXIT"]))\n')
        self.stub("cp", 'sys.exit(int(os.environ["BUILD_CHECK_COPY_EXIT"]))\n')
        self.stub("gpustack-operator", "")
        cases = [
            ("ci succeeds", True, "linux/amd64", "ci", 0, 0),
            ("archive succeeds", False, "linux/amd64", "build", 0, 0),
            ("cross build succeeds", True, "linux/arm64", "build", 0, 0),
            ("ci fails", True, "linux/amd64", "ci", 23, 0),
            ("archive fails", False, "linux/amd64", "build", 23, 0),
            ("cross build fails", True, "linux/arm64", "build", 23, 0),
            ("copy fails", True, "linux/amd64", "ci", 0, 24),
        ]
        for name, git_dir, target, make_target, make_rc, copy_rc in cases:
            with self.subTest(name=name):
                git = self.tree / ".git"
                if git_dir:
                    git.mkdir(exist_ok=True)
                elif git.exists():
                    git.rmdir()
                self.trace.unlink(missing_ok=True)
                result = self.run_script(
                    path,
                    GPUSTACK_WORKSPACE=str(self.tree),
                    GPUSTACK_GIT_COMMIT="",
                    GPUSTACK_GIT_VERSION="",
                    BUILDPLATFORM="linux/amd64",
                    TARGETPLATFORM=target,
                    TARGETOS="linux",
                    TARGETARCH=target.split("/")[1],
                    BUILD_CHECK_MAKE_EXIT=str(make_rc),
                    BUILD_CHECK_COPY_EXIT=str(copy_rc),
                )
                assert result.returncode == (make_rc or copy_rc), (
                    result.stdout + result.stderr
                )
                assert self.calls("make")[0]["args"] == [make_target]
                commands = [
                    json.loads(line)["command"]
                    for line in self.trace.read_text().splitlines()
                ]
                expected = ["make"]
                if make_rc == 0:
                    expected.append("cp")
                    if copy_rc == 0:
                        expected.append("gpustack-operator")
                assert commands == expected

    def test_package_stamps_and_local_image_load(self):
        folder = self.tree / "pack/gpustack-operator"
        folder.mkdir(parents=True)
        (folder / "Dockerfile").write_text("FROM scratch\n")
        self.stub("docker", "")
        result = self.run_script(
            self.tree / "hack/package.sh", GIT_VERSION="v1.2.3", GIT_COMMIT="a" * 40
        )
        assert result.returncode == 0, result.stderr
        build = next(
            c for c in self.calls("docker") if c["args"][:2] == ["buildx", "build"]
        )
        assert "GPUSTACK_GIT_VERSION=v1.2.3" in build["args"]
        assert "GPUSTACK_GIT_COMMIT=" + "a" * 40 in build["args"]
        assert "--load" in build["args"]
        assert "--push" not in build["args"]
        result = self.run_script(self.tree / "hack/package.sh", "no-such-image")
        assert result.returncode != 0

    def test_local_load_failures(self):
        for code, env in [
            ("sys.exit(1)", {}),
            ("print('remote-cluster')", {}),
            ("print('local-k3s')", {"BUILD_CHECK_IMPORT_FAIL": "1"}),
        ]:
            with self.subTest(code=code, env=env):
                self.stub("make", "")
                self.stub("kubectl", code)
                self.stub("docker", "print('image')")
                self.stub("sudo", "sys.exit(1)")
                result = self.run_script(
                    self.tree / ".agents/skills/_e2e-lib/scripts/build-load.sh",
                    "test",
                    **env,
                )
                assert result.returncode != 0, result.stdout + result.stderr
                assert "built & loaded:" not in result.stdout


if __name__ == "__main__":
    unittest.main(verbosity=2)
