import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]
NEBIUS = r"""#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
if '--help' in args:
    print('--parent-id --all')
    sys.exit(0)
if 'get-compatibility-matrix' in args:
    result = {'versions': [{'items': [{'os': 'ubuntu22.04', 'drivers_preset': 'cuda12'}]}]}
elif 'platform' in args:
    result = {'items': [{'metadata': {'name': 'gpu-test'}, 'spec': {'presets': [{'name': '8gpu', 'allow_gpu_clustering': True}]}}]}
elif 'project' in args:
    result = {'metadata': {'parent_id': 'tenant-test'}, 'spec': {'region': 'eu-north1'}}
elif 'resource-advice' in args:
    result = {'items': [{'spec': {'region': 'eu-north1', 'fabric': 'fabric-2', 'compute_instance': {'platform': 'gpu-test', 'preset': {'name': '8gpu'}}}, 'status': {'preemptible': {'data_state': 'DATA_STATE_FRESH', 'available': 0}}}]}
elif 'instance' in args:
    result = {'items': json.loads(os.environ['TEST_INSTANCES'])}
else:
    sys.exit('unexpected command: ' + str(args))
print(json.dumps(result))
"""


def instance(group, state="RUNNING"):
    return {
        "metadata": {"name": group + "-node"},
        "status": {"state": state},
        "spec": {
            "resources": {"platform": "gpu-test", "preset": "8gpu"},
            "preemptible": {},
            "gpu_cluster": {"id": "fabric-cluster"},
        },
    }


def terraform_state(project="project-test"):
    return {
        "resources": [
            {
                "mode": "managed",
                "type": "nebius_mk8s_v1_cluster",
                "name": "this",
                "instances": [
                    {"attributes": {"id": "cluster-own", "parent_id": project}},
                ],
            },
            {
                "mode": "managed",
                "type": "nebius_mk8s_v1_node_group",
                "name": "this",
                "instances": [
                    {
                        "index_key": "gpu",
                        "attributes": {
                            "id": "mk8snodegroup-own",
                            "parent_id": "cluster-own",
                        },
                    },
                    {
                        "index_key": "other",
                        "attributes": {
                            "id": "mk8snodegroup-other",
                            "parent_id": "cluster-own",
                        },
                    },
                ],
            },
        ]
    }


class CapacityTest(unittest.TestCase):
    def test_only_current_group_reduces_new_capacity(self):
        cases = [
            ("own node", [instance("mk8snodegroup-own")], terraform_state(), 1, "true"),
            (
                "foreign group",
                [instance("mk8snodegroup-other")],
                terraform_state(),
                1,
                "false",
            ),
            (
                "foreign cluster",
                [instance("mk8snodegroup-foreign")],
                terraform_state(),
                1,
                "false",
            ),
            (
                "mixed groups",
                [instance("mk8snodegroup-own"), instance("mk8snodegroup-other")],
                terraform_state(),
                2,
                "false",
            ),
            (
                "stopped own node",
                [instance("mk8snodegroup-own", "STOPPED")],
                terraform_state(),
                1,
                "false",
            ),
            ("no state", [instance("mk8snodegroup-own")], {}, 1, "false"),
            (
                "other project state",
                [instance("mk8snodegroup-own")],
                terraform_state("other-project"),
                1,
                "false",
            ),
        ]
        source = (
            (ROOT / "main.tf").read_text().split('data "external" "gpu_compat"', 1)[1]
        )
        script = textwrap.dedent(
            source.split("<<-EOT\n", 1)[1].split("\n  EOT", 1)[0]
        ).replace("$${", "${")
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            for name, content in {
                "nebius": NEBIUS,
                "terraform": '#!/bin/sh\nprintf "%s" "$TEST_TERRAFORM_STATE"\n',
            }.items():
                binary = work / name
                binary.write_text(content)
                binary.chmod(0o755)
            for name, instances, state, count, expected in cases:
                with self.subTest(name=name):
                    query = {
                        "project_id": "project-test",
                        "release": "1.31",
                        "group_name": "gpu",
                        "platform": "gpu-test",
                        "preset": "8gpu",
                        "preemptible": "true",
                        "node_count": str(count),
                        "infiniband_fabric": "fabric-2",
                        "gpu_cluster_id": "fabric-cluster",
                        "wants_cluster": "true",
                    }
                    env = dict(
                        os.environ,
                        PATH=directory + os.pathsep + os.environ["PATH"],
                        TEST_INSTANCES=json.dumps(instances),
                        TEST_TERRAFORM_STATE=json.dumps(state),
                    )
                    result = subprocess.run(
                        ["bash", "-c", script],
                        input=json.dumps(query),
                        env=env,
                        cwd=work,
                        capture_output=True,
                        text=True,
                        check=True,
                    )
                    self.assertEqual(json.loads(result.stdout)["capacity_ok"], expected)


if __name__ == "__main__":
    unittest.main()
