locals {
  prepare_input       = { project_id = var.project_id, groups = { vm = { platform = var.instance_type.platform, preset = var.instance_type.preset, preemptible = var.instance_type.preemptible, infiniband_fabric = var.instance_type.infiniband_fabric, gpu_cluster_id = var.instance_type.gpu_cluster_id, reuse_gpu_cluster = var.instance_type.reuse_gpu_cluster, fabric_selection_revision = var.instance_type.fabric_selection_revision, spot_price_from_history = var.instance_type.spot_price_from_history, spot_price_ceiling = var.instance_type.spot_price_ceiling } } }
  prepare_fingerprint = sha256(jsonencode(local.prepare_input))
  snapshot_valid      = try(var.nebius_resolved.fingerprint == local.prepare_fingerprint, false)
}

resource "null_resource" "prepare" {
  count = anytrue([for cfg in values(local.prepare_input.groups) : cfg.spot_price_from_history || cfg.infiniband_fabric != null || cfg.gpu_cluster_id != null]) ? 1 : 0
  triggers = {
    input       = base64encode(jsonencode(local.prepare_input))
    fingerprint = local.prepare_fingerprint
  }
  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    environment = {
      NEBIUS_PREPARE_INPUT       = self.triggers.input
      NEBIUS_PREPARE_FINGERPRINT = self.triggers.fingerprint
      NEBIUS_PREPARE_PATH        = "${path.root}/.nebius-resolved.auto.tfvars.json"
    }
    command = <<-SCRIPT
      python3 - <<'PYTHON'
      import base64
      import datetime as dt
      import decimal
      import json
      import hashlib
      import os
      import subprocess
      import sys
      import tempfile
      import urllib.parse

      D = decimal.Decimal
      config = json.loads(base64.b64decode(os.environ['NEBIUS_PREPARE_INPUT']))
      fingerprint = os.environ['NEBIUS_PREPARE_FINGERPRINT']
      path = os.environ['NEBIUS_PREPARE_PATH']

      def run(args):
          result = subprocess.run(args, capture_output=True, text=True, timeout=45)
          if result.returncode:
              raise RuntimeError('command failed: ' + ' '.join(args[:4]) + ': ' + result.stderr.strip())
          return json.loads(result.stdout, parse_float=D)

      def cli(*args):
          return run(['nebius', '--no-browser', '--no-check-update', '--retries', '1', '--timeout', '15s', '--auth-timeout', '15s', *args, '--format', 'json'])

      def field(obj, snake, camel=None):
          return obj.get(snake, obj.get(camel or snake))

      def rest(endpoint, params):
          items, token = [], None
          seen = set()
          while True:
              query = dict(params, pageSize='100')
              if token:
                  query['pageToken'] = token
              url = 'https://api.nebius.cloud/billing/v1alpha1/spot-price' + endpoint + '?' + urllib.parse.urlencode(query)
              data = run(['curl', '--fail', '--silent', '--show-error', '--retry', '2', '--max-time', '15', url])
              items.extend(data.get('items', []))
              token = field(data, 'next_page_token', 'nextPageToken')
              if not token:
                  return items
              if token in seen:
                  raise RuntimeError('repeated spot price page token')
              seen.add(token)

      def timestamp(value):
          return dt.datetime.fromisoformat(value.replace('Z', '+00:00'))

      def money(value):
          result = D(str(value))
          if not result.is_finite() or result <= 0:
              raise ValueError('invalid spot price')
          return result

      try:
          project = cli('iam', 'project', 'get', '--id', config['project_id'])
          region = project['spec']['region']
          old = {}
          if os.path.exists(path):
              with open(path) as source:
                  old = json.load(source).get('nebius_resolved', {})
          bindings = {}
          fabric_groups = {k: v for k, v in config['groups'].items() if v.get('infiniband_fabric') or v.get('gpu_cluster_id')}
          if fabric_groups:
              platforms = cli('compute', 'platform', 'list', '--parent-id', config['project_id'], '--all').get('items', [])
              tenant = field(project.get('metadata', {}), 'parent_id', 'parentId')
              advice = None
              probe = subprocess.run(['nebius', 'capacity', 'resource-advice', 'list', '--help'], capture_output=True, text=True, timeout=20)
              if probe.returncode == 0 and '--parent-id' in probe.stdout:
                  try:
                      advice = cli('capacity', 'resource-advice', 'list', '--parent-id', tenant, '--all').get('items', [])
                  except Exception as error:
                      print('Resource Advice unavailable: ' + str(error), file=sys.stderr)
              clusters = None
              own_ids = set()
              proc = subprocess.run(['terraform', 'state', 'pull'], capture_output=True, text=True, timeout=30)
              if proc.returncode:
                  raise RuntimeError('cannot inspect Terraform state before GPU cluster reuse: ' + proc.stderr.strip())
              if proc.stdout.strip():
                  state = json.loads(proc.stdout)
                  for resource in state.get('resources', []):
                      if resource.get('type') == 'nebius_compute_v1_gpu_cluster' and resource.get('name') == 'this':
                          own_ids.update(i.get('attributes', {}).get('id') for i in resource.get('instances', []))
              for name, group in fabric_groups.items():
                  presets = [p for platform in platforms if platform.get('metadata', {}).get('name') == group['platform'] for p in platform.get('spec', {}).get('presets', []) if p.get('name') == group['preset']]
                  if len(presets) != 1 or not field(presets[0], 'allow_gpu_clustering', 'allowGpuClustering'):
                      raise ValueError('platform/preset does not allow GPU clustering for ' + name)
                  identity = {k: group.get(k) for k in ('platform', 'preset', 'infiniband_fabric', 'gpu_cluster_id', 'reuse_gpu_cluster', 'preemptible', 'fabric_selection_revision')}
                  identity['project_id'] = config['project_id']
                  key = hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()
                  previous = old.get('bindings', {}).get(name, {}) if old.get('project_id') == config['project_id'] and old.get('region') == region else {}
                  if previous.get('key') == key:
                      bindings[name] = previous
                      continue
                  fabric, selected = group.get('infiniband_fabric'), group.get('gpu_cluster_id')
                  if selected:
                      cluster = cli('compute', 'gpu-cluster', 'get', '--id', selected)
                      if field(cluster.get('metadata', {}), 'parent_id', 'parentId') != config['project_id']:
                          raise ValueError('GPU cluster belongs to another project for ' + name)
                      fabric = field(cluster['spec'], 'infiniband_fabric', 'infinibandFabric')
                  matching = []
                  if advice is not None:
                      for row in advice:
                          spec = row.get('spec', {})
                          instance = field(spec, 'compute_instance', 'computeInstance') or {}
                          if spec.get('region') != region or instance.get('platform') != group['platform'] or (instance.get('preset') or {}).get('name') != group['preset']:
                              continue
                          status = row.get('status', {})
                          availability = status.get('preemptible', {}) if group.get('preemptible') else field(status, 'on_demand', 'onDemand') or {}
                          matching.append((spec.get('fabric'), availability))
                  if fabric == 'auto':
                      if advice is None:
                          raise RuntimeError('automatic fabric selection requires Resource Advice; upgrade Nebius CLI and check tenant access')
                      candidates = [(f, a) for f, a in matching if f and field(a, 'data_state', 'dataState') == 'DATA_STATE_FRESH' and a.get('available') is not None and int(a['available']) > 0]
                      if not candidates:
                          raise ValueError('no fresh positive capacity for automatic fabric selection for ' + name)
                      fabric = sorted(candidates, key=lambda pair: (-int(pair[1]['available']), pair[0]))[0][0]
                  elif advice is None or not any(f == fabric for f, _ in matching):
                      print('Resource Advice could not verify fabric ' + str(fabric) + ' for ' + name, file=sys.stderr)
                  if not selected and group.get('reuse_gpu_cluster'):
                      if clusters is None:
                          clusters = cli('compute', 'gpu-cluster', 'list', '--parent-id', config['project_id'], '--all').get('items', [])
                      candidates = [c.get('metadata', {}).get('id') for c in clusters if field(c.get('spec', {}), 'infiniband_fabric', 'infinibandFabric') == fabric and c.get('metadata', {}).get('id') not in own_ids]
                      selected = sorted(x for x in candidates if x)[0] if candidates else None
                  selected_advice = next((a for f, a in matching if f == fabric), {})
                  bindings[name] = {'key': key, 'fabric': fabric, 'cluster_id': selected, 'advice_state': field(selected_advice, 'data_state', 'dataState') or 'UNKNOWN', 'advice_available': selected_advice.get('available'), 'advice_effective_at': field(selected_advice, 'effective_at', 'effectiveAt'), 'advice_limit': selected_advice.get('limit'), 'advice_level': field(selected_advice, 'availability_level', 'availabilityLevel'), 'queried_at': dt.datetime.now(dt.timezone.utc).isoformat()}
          history_groups = {k: v for k, v in config['groups'].items() if v.get('spot_price_from_history')}
          results = {}
          if history_groups:
              probe = subprocess.run(['nebius', 'billing', 'v1alpha1', 'spot-price', 'list-series', '--help'], capture_output=True, text=True, timeout=20)
              list_probe = subprocess.run(['nebius', 'billing', 'v1alpha1', 'spot-price', 'list', '--help'], capture_output=True, text=True, timeout=20)
              has_cli = probe.returncode == 0 and '--aggregation-granularity' in probe.stdout and list_probe.returncode == 0 and '--all' in list_probe.stdout
              now = dt.datetime.now(dt.timezone.utc)
              start = now - dt.timedelta(hours=6)
              start_s, end_s = (x.isoformat(timespec='seconds').replace('+00:00', 'Z') for x in (start, now))
              current = cli('billing', 'v1alpha1', 'spot-price', 'list', '--all')['items'] if has_cli else rest('', {})
              for name, group in history_groups.items():
                  matches = [x for x in current if x.get('region') == region and field(x, 'compute_instance', 'computeInstance',).get('platform') == group['platform']]
                  if len(matches) != 1:
                      raise ValueError('expected exactly one spot SKU for ' + name)
                  row = matches[0]
                  sku = field(row, 'sku_id', 'skuId')
                  price = row['price']
                  if str(price.get('currency', '')).upper() != 'USD' or 'gpu' not in field(price, 'unit_title', 'unitTitle').lower():
                      raise ValueError('unexpected spot currency or unit for ' + name)
                  if has_cli:
                      series = cli('billing', 'v1alpha1', 'spot-price', 'list-series', '--filters-sku-ids', sku, '--filters-start-time', start_s, '--filters-end-time', end_s, '--aggregation-granularity', 'granularity_minute_15', '--aggregation-method', 'method_last_price', '--all').get('items', [])
                  else:
                      series = rest('/list-series', {'filters.skuIds': sku, 'filters.startTime': start_s, 'filters.endTime': end_s, 'aggregation.granularity': 'GRANULARITY_MINUTE_15', 'aggregation.method': 'METHOD_LAST_PRICE'})
                  series = [x for x in series if field(x, 'sku_id', 'skuId') == sku and x.get('region') == region and field(x, 'compute_instance', 'computeInstance').get('platform') == group['platform']]
                  if len(series) != 1 or str(series[0].get('currency', '')).upper() != 'USD' or 'gpu' not in field(series[0], 'unit_title', 'unitTitle').lower():
                      raise ValueError('missing or mismatched spot price series for ' + name)
                  points = sorted((timestamp(x['timestamp']), money(x['value'])) for x in series[0].get('points', []))
                  if not points or points[0][0] > start + dt.timedelta(minutes=15) or points[-1][0] < now - dt.timedelta(minutes=15) or any(b[0] - a[0] > dt.timedelta(minutes=15) for a, b in zip(points, points[1:])):
                      raise ValueError('incomplete six-hour spot price series for ' + name)
                  high = max(money(price['value']), *(v for _, v in points))
                  bid = (high * D('1.10')).quantize(D('.001'), rounding=decimal.ROUND_CEILING)
                  if group.get('spot_price_ceiling') is not None:
                      cap = money(group['spot_price_ceiling']).quantize(D('.001'), rounding=decimal.ROUND_FLOOR)
                      if cap <= 0:
                          raise ValueError('spot price ceiling is too low')
                      bid = min(bid, cap)
                  results[name] = {'max_price': str(bid), 'sku_id': sku, 'current': str(price['value']), 'sampled_high': str(max(v for _, v in points)), 'start': start_s, 'end': end_s}
          snapshot = {'fingerprint': fingerprint, 'project_id': config['project_id'], 'region': region, 'prices': results, 'bindings': bindings}
          directory = os.path.dirname(path) or '.'
          fd, temp = tempfile.mkstemp(prefix='.nebius-resolved-', dir=directory)
          try:
              with os.fdopen(fd, 'w') as out:
                  json.dump({'nebius_resolved': snapshot}, out, indent=2)
                  out.write('\n')
              os.replace(temp, path)
          finally:
              if os.path.exists(temp):
                  os.unlink(temp)
      except (Exception, KeyboardInterrupt) as error:
          print('Nebius prepare failed: ' + str(error), file=sys.stderr)
          sys.exit(1)
      PYTHON
    SCRIPT
  }
}
