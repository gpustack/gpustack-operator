#!/usr/bin/env bash
#
# CASE 115 — Role shape edits replace complete replicas, including an initially queued group
#            (MUTATING, self-recovering; AUTO-SKIPS without a suitable accelerator pool)
#
#   E2E_C115_IT=<type> [E2E_C115_OTHER_IT=<other-pool-type>] case-115.sh <NS>
#
# Goal:        Observe size, resources, command and InstanceType edits on the same deployment.
#              A first queued group has no replacement slot. Reducing its accelerator request
#              must replace its obsolete members and Workload rather than leave it queued forever.
# Environment: A namespace watched by Kueue, an Active accelerator pool with at least one
#              whole-card unit, and onceMaxRequest=1. Other shapes SKIP the case explicitly.
#              The initially queued leg needs total capacity of at least two units. A one-card
#              pool refuses a two-card request at admission, so that leg SKIPs on such a pool.
#              The size leg needs two free units at the start; without them only that leg SKIPs.
#              The optional second type must identify a different accelerator group. Without it,
#              only the cross-pool leg SKIPs. Run with the selected pools otherwise idle.
#              A logical slice capacity enables whole-card to slice and slice percentage edits.
#              E2E_C115_CPU_IT enables a CPU P/D size and command leg; otherwise that leg SKIPs.
# Inputs:      Real ModelDeployments, Pods, Workloads and InstanceTypes. No mocked
#              hardware or edited quotas. Shell commands are placeholders; no inference is proved.
#              E2E_C115_IMAGE overrides ubuntu. E2E_C115_TIMEOUT sets each convergence bound.
#              E2E_C115_RAW_DIR optionally retains every observation, including failure evidence.
# Expected:    The initial queued Pod has no node, no admission and no replacement slot. An edit
#              gives the same deployment fresh Pod and Workload UIDs, matching template resources,
#              PodSet count and queue, and current Admitted=True. Size grows and shrinks as whole
#              groups. Command and cross-pool edits replace the members without mixing generations.
#              A P/D edit preserves the untouched role's Pod and Workload UIDs.
# Cleanup:     The finally block deletes only this case's deployment and its captured Workloads.
#              It waits for their Pods and Workloads to disappear on pass and fail. No baseline
#              setting, node, pool or device-manager configuration is changed.

set -uo pipefail
NS="${1:?usage: E2E_C115_IT=<type> case-115.sh <NS>}"
export E2E_C115_NAMESPACE="$NS"
E2E_C115_SOURCE_ROOT="$(cd "$(dirname "$0")/../../../.." && pwd)"
export E2E_C115_SOURCE_ROOT
python3 - <<'PY'
import json
import os
import pathlib
import re
import subprocess
import time
import uuid
from decimal import Decimal

NS = os.environ['E2E_C115_NAMESPACE']
IT = os.environ.get('E2E_C115_IT', '')
OTHER = os.environ.get('E2E_C115_OTHER_IT', '')
CPU_IT = os.environ.get('E2E_C115_CPU_IT', '')
NAME = 'case115-' + uuid.uuid4().hex[:8]
TIMEOUT = int(os.environ.get('E2E_C115_TIMEOUT', '180'))
RAW = pathlib.Path(os.environ['E2E_C115_RAW_DIR']) if os.environ.get('E2E_C115_RAW_DIR') else None
COMMAND = ['sh', '-c', 'sleep 86400']
ROWS = []
WORKLOADS = set()
MD_UID = None
OWNERSHIP_LABEL = 'gpustack.ai/e2e-case-115-token'
OWNERSHIP_TOKEN = 'case115-' + uuid.uuid4().hex
SOURCE = pathlib.Path(os.environ['E2E_C115_SOURCE_ROOT']) / 'pkg/nodefeature/knowns.go'
MAX_UNITS = int(re.search(r'ResourceMaxUnits\s*=\s*([0-9_]+)', SOURCE.read_text()).group(1).replace('_', ''))


def kubectl(*args, obj=None, allow_missing=False):
    result = subprocess.run(
        ['kubectl', '--request-timeout=30s', *args],
        input=json.dumps(obj) if obj is not None else None,
        capture_output=True, text=True, timeout=45,
    )
    if result.returncode:
        if allow_missing and 'NotFound' in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())
    return json.loads(result.stdout) if result.stdout.lstrip().startswith('{') else result.stdout


def record(status, check, detail):
    ROWS.append((status, check, detail))
    print(' | '.join(ROWS[-1]), flush=True)


def discover_owned_deployment():
    global MD_UID
    current = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json',
                      allow_missing=True)
    if current is None:
        return None
    labels = current.get('metadata', {}).get('labels', {})
    if labels.get(OWNERSHIP_LABEL) != OWNERSHIP_TOKEN:
        return None
    MD_UID = current['metadata']['uid']
    return current


def create_owned_deployment(obj):
    global MD_UID
    try:
        created = kubectl('create', '-f', '-', '-o', 'json', obj=obj)
    except Exception:
        # The API may have persisted the object before the client lost its response.
        # Recover its UID only after checking the case-specific ownership token.
        discover_owned_deployment()
        raise
    if created.get('metadata', {}).get('labels', {}).get(OWNERSHIP_LABEL) != OWNERSHIP_TOKEN:
        raise RuntimeError('created deployment is missing the case ownership token')
    MD_UID = created['metadata']['uid']
    return created


def delete_owned_deployment(uid):
    options = {'apiVersion': 'v1', 'kind': 'DeleteOptions',
               'preconditions': {'uid': uid}}
    path = ('/apis/worker.gpustack.ai/v1/namespaces/' + NS
            + '/modeldeployments/' + NAME)
    kubectl('delete', '--raw', path, '-f', '-', obj=options)


def has_condition(obj, name):
    return any(c['type'] == name and c['status'] == 'True'
               for c in obj.get('status', {}).get('conditions', []))


def owns_workload(workload):
    return any(o.get('kind') == 'Pod' and o.get('name', '').startswith(NAME + '-')
               for o in workload['metadata'].get('ownerReferences', []))


def snapshot(label):
    md = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json')
    pods = kubectl('-n', NS, 'get', 'pods', '-l', 'app.kubernetes.io/instance=' + NAME, '-o', 'json')['items']
    pods = [p for p in pods if any(o['uid'] == md['metadata']['uid']
                                 for o in p['metadata'].get('ownerReferences', []))]
    uids = {p['metadata']['uid'] for p in pods}
    workloads = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io', '-o', 'json')['items']
    workloads = [w for w in workloads if owns_workload(w) or any(o['uid'] in uids
                                           for o in w['metadata'].get('ownerReferences', []))]
    WORKLOADS.update(w['metadata']['name'] for w in workloads)
    result = {'deployment': md, 'pods': pods, 'workloads': workloads}
    if RAW:
        RAW.mkdir(parents=True, exist_ok=True)
        with (RAW / (NAME + '-' + label.replace('/', '-') + '.jsonl')).open('a') as stream:
            stream.write(json.dumps(result) + '\n')
    return result


def wait(label, predicate):
    deadline = time.monotonic() + TIMEOUT
    while True:
        state = snapshot(label)
        if predicate(state):
            return state
        if time.monotonic() >= deadline:
            raise RuntimeError(label + ' did not converge; observation retained in E2E_C115_RAW_DIR')
        time.sleep(2)


def main_container(pod):
    return next(c for c in pod['spec']['containers'] if c['name'] == 'main')


def accelerator_matches(container, role, inst_type):
    accelerated = inst_type['spec'].get('acceleratable', False)
    resources = role['resources']
    memory = resources.get('acceleratorSlicedMemoryPercentage', 0)
    cores = resources.get('acceleratorSlicedCoresPercentage', 0)
    for kind in ('requests', 'limits'):
        actual = container['resources'][kind]
        if memory or cores:
            expected_memory, expected_cores = memory or cores, cores or memory
            expected = {'nvidia.com/gpu.sliced': str(resources['accelerator']),
                        'nvidia.com/gpu.sliced.memory-percentage': str(expected_memory),
                        'nvidia.com/gpu.sliced.cores-percentage': str(expected_cores)}
            if not accelerated or any(actual.get(key) != value for key, value in expected.items()):
                return False
            units = actual.get('nvidia.com/gpu.sliced.units', '')
            if not units:
                return False
            scale = {'k': 1000, 'M': 1000000, 'G': 1000000000}.get(units[-1], 1)
            value = Decimal(units[:-1] if scale != 1 else units) * scale
            if value != expected_memory * MAX_UNITS // 100 or 'nvidia.com/gpu' in actual:
                return False
        else:
            expected = str(resources['accelerator']) if accelerated else None
            if actual.get('nvidia.com/gpu') != expected or 'nvidia.com/gpu.sliced' in actual:
                return False
    return True


def converged(state, old=None, changed='server'):
    md = state['deployment']
    roles = md['spec']['roles']
    pods, workloads = state['pods'], state['workloads']
    if md['metadata']['uid'] != MD_UID or len(pods) != sum(r['size'] for r in roles) or len(workloads) != len(roles):
        return False
    if md['metadata'].get('annotations', {}).get('modeldeployment.gpustack.ai/replacement-slots'):
        return False
    if not all(has_condition(p, 'Ready') and not p['metadata'].get('deletionTimestamp') for p in pods):
        return False
    for role in roles:
        members = [p for p in pods if p['metadata']['labels']['app.kubernetes.io/component'] == role['name']]
        if len(members) != role['size']:
            return False
        member_uids = {p['metadata']['uid'] for p in members}
        owned = [w for w in workloads if any(o['uid'] in member_uids for o in w['metadata'].get('ownerReferences', []))]
        if len(owned) != 1:
            return False
        workload = owned[0]
        if not has_condition(workload, 'Admitted') or not workload.get('status', {}).get('admission'):
            return False
        pod_set = workload['spec']['podSets']
        if len(pod_set) != 1 or pod_set[0]['name'] != role['name'] or pod_set[0]['count'] != role['size']:
            return False
        owners = {o['uid'] for o in workload['metadata'].get('ownerReferences', []) if o['kind'] == 'Pod'}
        if owners != member_uids:
            return False
        inst_type = kubectl('get', 'instancetypes.worker.gpustack.ai', role['instanceType'], '-o', 'json')
        entrance = inst_type['status']['entrance']
        if workload['spec']['queueName'] != entrance:
            return False
        for pod in members:
            container = main_container(pod)
            if container.get('command') != role['command']:
                return False
            if pod['metadata']['labels'].get('kueue.x-k8s.io/queue-name') != entrance:
                return False
            if inst_type['spec'].get('acceleratable', False):
                node = pod['spec'].get('nodeName')
                if not node:
                    return False
                devices = kubectl('get', 'devices.worker.gpustack.ai', node, '-o', 'json')
                offered = {a['id'] for group in devices['spec']['groups']
                           if group['manufacturer'] + '-' + group['id'] == inst_type['spec']['acceleratorGroup']
                           for a in group.get('accelerators', [])}
                allocation = json.loads(pod['metadata'].get('annotations', {}).get(
                    'device.gpustack.ai/accelerator.allocated', '{}'))
                groups = allocation.get('main', {}).get('devices', {}).get('groups', [])
                assigned = {a['id'] for group in groups for a in group.get('accelerators', [])}
                if not groups or not assigned or not assigned.issubset(offered):
                    return False
                if any(group['manufacturer'] + '-' + group['id'] != inst_type['spec']['acceleratorGroup']
                       for group in groups):
                    return False
            if not accelerator_matches(container, role, inst_type):
                return False
            if main_container(pod_set[0]['template']).get('resources') != container.get('resources'):
                return False
            if main_container(pod_set[0]['template']).get('command') != container.get('command'):
                return False
    if old:
        changed_pods = [p for p in old['pods'] if p['metadata']['labels']['app.kubernetes.io/component'] == changed]
        old_pods = {p['metadata']['uid'] for p in changed_pods}
        if old_pods.intersection(p['metadata']['uid'] for p in pods):
            return False
        for pod_before in changed_pods:
            current = kubectl('-n', NS, 'get', 'pods', pod_before['metadata']['name'],
                              '-o', 'json', allow_missing=True)
            if current and current['metadata']['uid'] == pod_before['metadata']['uid']:
                return False
        for workload_before in old['workloads']:
            if not any(o['uid'] in old_pods for o in workload_before['metadata'].get('ownerReferences', [])):
                continue
            current = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io',
                              workload_before['metadata']['name'], '-o', 'json', allow_missing=True)
            if current and current['metadata']['uid'] == workload_before['metadata']['uid']:
                return False
        for role in roles:
            if role['name'] == changed:
                continue
            def identity(view):
                uids = {p['metadata']['uid'] for p in view['pods']
                        if p['metadata']['labels']['app.kubernetes.io/component'] == role['name']}
                work = {w['metadata']['uid'] for w in view['workloads']
                        if any(o['uid'] in uids for o in w['metadata'].get('ownerReferences', []))}
                return uids, work
            if identity(state) != identity(old):
                return False
    return True


def edit(label, fields):
    old = snapshot(label + '-before')
    patch = [{'op': 'add', 'path': '/spec/roles/0/' + key, 'value': value}
             for key, value in fields.items()]
    kubectl('-n', NS, 'patch', 'modeldeployments.worker.gpustack.ai', NAME,
            '--type=json', '-p', json.dumps(patch))
    changed = old['deployment']['spec']['roles'][0]['name']
    wait(label, lambda state: converged(state, old, changed))
    record('PASS', label, 'same deployment; fresh complete group and Admitted Workload')


def cleanup():
    if MD_UID is None:
        discover_owned_deployment()
    if MD_UID is None:
        return
    current = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json',
                      allow_missing=True)
    if current is None:
        return
    labels = current.get('metadata', {}).get('labels', {})
    if (labels.get(OWNERSHIP_LABEL) != OWNERSHIP_TOKEN
            or current.get('metadata', {}).get('uid') != MD_UID):
        raise RuntimeError('refusing to delete a deployment without matching case ownership')
    delete_owned_deployment(MD_UID)
    for name in sorted(WORKLOADS):
        kubectl('-n', NS, 'delete', 'workloads.kueue.x-k8s.io', name,
                '--ignore-not-found', '--wait=false')
    deadline = time.monotonic() + TIMEOUT
    while time.monotonic() < deadline:
        pods = kubectl('-n', NS, 'get', 'pods', '-l', 'app.kubernetes.io/instance=' + NAME, '-o', 'json')['items']
        md = kubectl('-n', NS, 'get', 'modeldeployments.worker.gpustack.ai', NAME, '-o', 'json', allow_missing=True)
        workloads = kubectl('-n', NS, 'get', 'workloads.kueue.x-k8s.io', '-o', 'json')['items']
        discovered = {w['metadata']['name'] for w in workloads if owns_workload(w)}
        for name in discovered - WORKLOADS:
            kubectl('-n', NS, 'delete', 'workloads.kueue.x-k8s.io', name,
                    '--ignore-not-found', '--wait=false')
        WORKLOADS.update(discovered)
        remaining = [w for w in workloads if w['metadata']['name'] in WORKLOADS]
        if not pods and md is None and not remaining:
            record('PASS', 'cleanup', 'owned deployment, Pods and Workloads absent')
            return
        time.sleep(2)
    raise RuntimeError('owned deployment, Pods or Workloads remain after cleanup')


try:
    if not IT:
        record('SKIP', 'role shape updates', 'E2E_C115_IT is unset; NOTHING WAS VERIFIED')
    else:
        inst_type = kubectl('get', 'instancetypes.worker.gpustack.ai', IT, '-o', 'json')
        resource = inst_type['status']['accelerator']
        suitable = (inst_type['status']['phase'] == 'Active'
                    and inst_type['status']['detail']['manufacturer'] == 'nvidia'
                    and resource.get('onceMaxRequest') == '1'
                    and int(resource['remaining']) >= 1)
        if not suitable:
            record('SKIP', 'role shape updates', 'needs a free NVIDIA whole-card unit and onceMaxRequest=1; NOTHING WAS VERIFIED')
        else:
            obj = {'apiVersion': 'worker.gpustack.ai/v1', 'kind': 'ModelDeployment',
                   'metadata': {'name': NAME, 'namespace': NS}, 'spec': {
                       'model': {'name': 'Qwen/Qwen2.5-0.5B-Instruct'},
                       'engine': {'name': 'vLLM', 'version': '0.29.0'}, 'roles': [{
                           'name': 'server', 'kind': 'Server', 'replicas': 1, 'size': 1,
                           'instanceType': IT, 'image': os.environ.get('E2E_C115_IMAGE', 'ubuntu:24.04'),
                           'command': COMMAND, 'resources': {'accelerator': '2' if int(resource['capacity']) >= 2 else '1'},
                           'terminationGracePeriodSeconds': 15}]}}
            obj['metadata']['labels'] = {OWNERSHIP_LABEL: OWNERSHIP_TOKEN}
            created = create_owned_deployment(obj)
            def initially_queued(state):
                if len(state['pods']) != 1 or len(state['workloads']) != 1:
                    return False
                pod, workload = state['pods'][0], state['workloads'][0]
                return (not pod['spec'].get('nodeName') and not has_condition(workload, 'Admitted')
                        and any(g['name'] == 'kueue.x-k8s.io/admission' for g in pod['spec'].get('schedulingGates', []))
                        and not state['deployment']['metadata'].get('annotations', {}).get('modeldeployment.gpustack.ai/replacement-slots'))
            if int(resource['capacity']) >= 2:
                wait('initially queued without a slot', initially_queued)
                record('PASS', 'initially queued without a slot', 'real Pod and owning Workload; no admission or node')
                edit('queued resources two to one', {'resources': {'accelerator': '1'}})
            else:
                record('SKIP', 'initially queued resources two to one', 'one-card pool refuses the initial request; queued resource behavior remains UNVERIFIED')
                wait('one-card baseline', converged)
            if int(resource['remaining']) >= 2:
                edit('size one to two', {'size': 2})
                edit('size two to one', {'size': 1})
            else:
                record('SKIP', 'size one to two and back', 'fewer than two free units at the start; multi-member replacement remains UNVERIFIED')
            edit('takeover command replacement', {'command': ['sh', '-c', 'true; sleep 86400']})
            if int(inst_type['status'].get('acceleratorSliced', {}).get('capacity') or 0) > 0:
                edit('whole card to half slice', {'resources': {'accelerator': '1',
                     'acceleratorSlicedMemoryPercentage': 50, 'acceleratorSlicedCoresPercentage': 50}})
                edit('slice fifty to forty percent', {'resources': {'accelerator': '1',
                     'acceleratorSlicedMemoryPercentage': 40, 'acceleratorSlicedCoresPercentage': 40}})
                edit('slice to whole card', {'resources': {'accelerator': '1'}})
            else:
                record('SKIP', 'logical slice resource updates', 'no logical slice capacity; slice replacement remains UNVERIFIED')
            if OTHER:
                second = kubectl('get', 'instancetypes.worker.gpustack.ai', OTHER, '-o', 'json')
                if second['spec']['acceleratorGroup'] == inst_type['spec']['acceleratorGroup']:
                    raise RuntimeError('E2E_C115_OTHER_IT names the same accelerator group; it cannot prove a cross-pool update')
                edit('InstanceType cross-pool replacement', {'instanceType': OTHER})
            else:
                record('SKIP', 'InstanceType cross-pool replacement', 'E2E_C115_OTHER_IT is unset; cross-pool behavior remains UNVERIFIED')
            if CPU_IT:
                cleanup()
                MD_UID = None
                WORKLOADS.clear()
                NAME += '-pd'
                obj['metadata']['name'] = NAME
                obj['spec']['kvTransfer'] = {'protocol': 'TCP'}
                obj['spec']['roles'] = [{
                    'name': kind.lower(), 'kind': kind, 'replicas': 1, 'size': 1,
                    'instanceType': CPU_IT, 'image': os.environ.get('E2E_C115_IMAGE', 'ubuntu:24.04'),
                    'command': COMMAND, 'resources': {'accelerator': '0'},
                    'terminationGracePeriodSeconds': 15,
                } for kind in ('Prefill', 'Decode')]
                created = create_owned_deployment(obj)
                wait('P/D baseline', converged)
                edit('prefill size one to two preserves decode', {'size': 2})
                edit('prefill size two to one preserves decode', {'size': 1})
                edit('prefill command replacement preserves decode', {'command': ['sh', '-c', 'true; sleep 86400']})
            else:
                record('SKIP', 'P/D sibling preservation', 'E2E_C115_CPU_IT is unset; P/D size and command behavior remains UNVERIFIED')
except Exception as error:
    record('FAIL', 'role shape updates', str(error))
finally:
    try:
        cleanup()
    except Exception as error:
        record('FAIL', 'cleanup', str(error))

print('\nSTATUS | CHECK | OBJECT')
for row in ROWS:
    print(' | '.join(row))
raise SystemExit(1 if any(row[0] == 'FAIL' for row in ROWS) else 0)
PY
