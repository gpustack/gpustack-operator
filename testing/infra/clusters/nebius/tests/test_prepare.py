import base64
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[1]

NEBIUS = r'''#!/usr/bin/env python3
import datetime as dt, json, os, sys
args = sys.argv[1:]
if '--help' in args:
    if 'list-series' in args and os.environ.get('OLD_CLI'):
        sys.exit(1)
    print('--aggregation-granularity --parent-id --all')
    sys.exit(0)
if 'project' in args:
    obj = {'spec': {'region': 'eu-north1'}, 'metadata': {'parent_id': 'tenant-test'}}
elif 'platform' in args:
    obj = {'items': [{'metadata': {'name': 'gpu-test'}, 'spec': {'presets': [{'name': '8gpu', 'allow_gpu_clustering': True}]}}]}
elif 'resource-advice' in args:
    obj = {'items': [{'spec': {'region': 'eu-north1', 'fabric': f, 'compute_instance': {'platform': 'gpu-test', 'preset': {'name': '8gpu'}}}, 'status': {'preemptible': {'data_state': 'DATA_STATE_FRESH', 'available': n}}} for f, n in [('fabric-2', 2), ('fabric-3', int(os.environ.get('CAPACITY', '3')))]]}
elif 'gpu-cluster' in args and 'list' in args:
    obj = {'items': json.loads(os.environ.get('CLUSTERS', '[]'))}
elif 'spot-price' in args and 'list-series' in args:
    now = dt.datetime.now(dt.timezone.utc).replace(second=0, microsecond=0)
    now -= dt.timedelta(minutes=now.minute % 15)
    points = [{'timestamp': (now - dt.timedelta(minutes=15*i)).isoformat(), 'value': '1.2' if i == 4 else '1.0'} for i in range(25)]
    if os.environ.get('INCOMPLETE'): points = points[:1]
    obj = {'items': [{'sku_id': 'sku-test', 'region': 'eu-north1', 'compute_instance': {'platform': 'gpu-test'}, 'currency': 'USD', 'unit_title': 'GPU hour', 'points': points}]}
elif 'spot-price' in args:
    obj = {'items': [{'sku_id': 'sku-test', 'region': 'eu-north1', 'compute_instance': {'platform': 'gpu-test'}, 'price': {'value': '1.1', 'currency': 'USD', 'unit_title': 'GPU hour'}}]}
else:
    print('unexpected command', args, file=sys.stderr); sys.exit(1)
print(json.dumps(obj))
'''

CURL = r'''#!/usr/bin/env python3
import datetime as dt, json, sys, urllib.parse
url = sys.argv[-1]
query = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
if 'list-series' not in url:
    if 'pageToken' not in query:
        obj = {'items': [], 'nextPageToken': 'next'}
    else:
        obj = {'items': [{'skuId': 'sku-test', 'region': 'eu-north1', 'computeInstance': {'platform': 'gpu-test'}, 'price': {'value': '1.1', 'currency': 'usd', 'unitTitle': 'GPU hour'}}]}
else:
    now = dt.datetime.now(dt.timezone.utc).replace(second=0, microsecond=0)
    now -= dt.timedelta(minutes=now.minute % 15)
    points = [{'timestamp': (now - dt.timedelta(minutes=15*i)).isoformat(), 'value': '1.2'} for i in range(25)]
    obj = {'items': [{'skuId': 'sku-test', 'region': 'eu-north1', 'computeInstance': {'platform': 'gpu-test'}, 'currency': 'usd', 'unitTitle': 'GPU hour', 'points': points}]}
print(json.dumps(obj))
'''

class PrepareTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        binary = self.work / 'nebius'
        binary.write_text(NEBIUS)
        binary.chmod(0o755)
        binary = self.work / 'terraform'
        binary.write_text('#!/bin/sh\nprintf "%s" "$TEST_TERRAFORM_STATE"\n')
        binary.chmod(0o755)
        binary = self.work / 'curl'
        binary.write_text(CURL)
        binary.chmod(0o755)
        source = (ROOT / 'prepare.tf').read_text()
        self.script = textwrap.dedent(source.split("      python3 - <<'PYTHON'\n", 1)[1].split('\n      PYTHON\n', 1)[0])
        self.snapshot = self.work / '.nebius-resolved.auto.tfvars.json'
        self.group = {'platform': 'gpu-test', 'preset': '8gpu', 'preemptible': True, 'infiniband_fabric': 'auto', 'gpu_cluster_id': None, 'reuse_gpu_cluster': True, 'fabric_selection_revision': '', 'spot_price_from_history': True, 'spot_price_ceiling': '1.250'}

    def prepare(self, **extra):
        config = {'project_id': 'project-test', 'owned_prefix': 'own-', 'groups': {'gpu': self.group}}
        env = dict(os.environ, PATH=str(self.work) + os.pathsep + os.environ['PATH'], NEBIUS_PREPARE_INPUT=base64.b64encode(json.dumps(config).encode()).decode(), NEBIUS_PREPARE_FINGERPRINT='fingerprint', NEBIUS_PREPARE_PATH=str(self.snapshot), TEST_TERRAFORM_STATE=getattr(self, 'terraform_state', ''), **extra)
        return subprocess.run([sys.executable, '-c', self.script], cwd=self.work, env=env, capture_output=True, text=True)

    def test_quote_cap_and_binding_stability(self):
        first = self.prepare()
        self.assertEqual(first.returncode, 0, first.stderr)
        snapshot = json.loads(self.snapshot.read_text())['nebius_resolved']
        self.assertEqual(snapshot['prices']['gpu']['max_price'], '1.250')
        self.assertEqual(snapshot['bindings']['gpu']['fabric'], 'fabric-3')
        second = self.prepare(CAPACITY='1')
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(json.loads(self.snapshot.read_text())['nebius_resolved']['bindings']['gpu']['fabric'], 'fabric-3')
        self.group['fabric_selection_revision'] = 'new'
        third = self.prepare(CAPACITY='1')
        self.assertEqual(third.returncode, 0, third.stderr)
        self.assertEqual(json.loads(self.snapshot.read_text())['nebius_resolved']['bindings']['gpu']['fabric'], 'fabric-2')

    def test_old_cli_uses_paginated_rest(self):
        result = self.prepare(OLD_CLI='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        snapshot = json.loads(self.snapshot.read_text())['nebius_resolved']
        self.assertEqual(snapshot['prices']['gpu']['max_price'], '1.250')

    def test_reuse_excludes_owned_cluster(self):
        self.group['infiniband_fabric'] = 'fabric-2'
        (self.work / 'terraform.tfstate').write_text(json.dumps({'resources': []}))
        self.terraform_state = json.dumps({'resources': [{'type': 'nebius_compute_v1_gpu_cluster', 'name': 'this', 'instances': [{'attributes': {'id': 'computegpucluster-owned'}}]}]})
        clusters = [{'metadata': {'id': x}, 'spec': {'infiniband_fabric': 'fabric-2'}} for x in ['computegpucluster-owned', 'computegpucluster-z', 'computegpucluster-a']]
        result = self.prepare(CLUSTERS=json.dumps(clusters))
        self.assertEqual(result.returncode, 0, result.stderr)
        binding = json.loads(self.snapshot.read_text())['nebius_resolved']['bindings']['gpu']
        self.assertEqual(binding['cluster_id'], 'computegpucluster-a')

    def test_incomplete_history_preserves_snapshot(self):
        self.snapshot.write_text('{"sentinel": true}')
        before = self.snapshot.read_bytes()
        result = self.prepare(INCOMPLETE='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.snapshot.read_bytes(), before)

if __name__ == '__main__':
    unittest.main()
