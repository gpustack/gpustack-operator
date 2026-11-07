#!/usr/bin/env python3
"""Build the local provider and write a session-scoped Terraform CLI configuration."""

import json
import os
import subprocess
from pathlib import Path


def main():
    root = Path(__file__).resolve().parent
    env = dict(os.environ)
    env.pop("GODEBUG", None)
    target = subprocess.check_output(
        ["go", "env", "GOOS", "GOARCH"], env=env, text=True
    ).split()
    mirror = root / ".terraform" / "mirror"
    directory = mirror / "gpustack.local/infra/spheron/0.1.0" / "_".join(target)
    directory.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        [
            "go",
            "build",
            "-o",
            str(directory / "terraform-provider-spheron_v0.1.0"),
            ".",
        ],
        cwd=root,
        env=env,
        check=True,
    )
    config = root / ".terraform" / "dev.tfrc"
    config.write_text(
        "provider_installation {\n"
        "  dev_overrides {\n"
        f'    "gpustack.local/infra/spheron" = {json.dumps(str(directory))}\n'
        "  }\n"
        "  filesystem_mirror {\n"
        f"    path = {json.dumps(str(mirror))}\n"
        '    include = ["gpustack.local/infra/spheron"]\n'
        "  }\n"
        "  direct {\n"
        '    exclude = ["gpustack.local/infra/spheron"]\n'
        "  }\n"
        "}\n",
        encoding="utf-8",
    )
    print(config)


if __name__ == "__main__":
    main()
