# Spheron standalone compute

Create one GPU/CPU instance through the [local provider](../../providers/spheron/README.md)
and its Python API client. Export `SPHERON_API_KEY`, build the provider, and set the
session's `TF_CLI_CONFIG_FILE` as described there.

## Machine selection

Use this workflow when selecting machines manually or through a coding agent. Start with
the requested node count, GPUs per node, exact GPU variant, instance-disk capacity, provider
or region restrictions, and total hourly budget. Prefer Spot for temporary tests. Select
Dedicated only when the task permits it; do not silently change the purchase type, GPU
variant or budget to obtain capacity. Selection is read-only and does not authorize deployment.

Use these endpoints from the [Spheron API reference](https://docs.spheron.ai/api-reference):

| Check | API | Use |
|---|---|---|
| Provider IDs | `GET https://app.spheron.ai/api/providers` | Discover supported provider names |
| Offers and inventory | `GET https://app.spheron.ai/api/gpu-offers?computeType=gpu&search=h100&page=1&limit=100` | Read all pages through `totalPages`; change the search/model as needed |
| Owned instances | `GET https://app.spheron.ai/api/deployments` | Record the baseline and reconcile allocation outcomes |
| Deletion eligibility | `GET https://app.spheron.ai/api/deployments/{id}/can-terminate` | Read `canTerminate`, `minimumRuntime` and `timeRemaining` before cleanup |

Authenticate through `SPHERON_API_KEY`. The local Python client adds the bearer header;
do not copy credentials into Terraform variables or saved requests. From the repository root,
inspect the providers and raw catalog:

```bash
python3 testing/infra/providers/spheron/python/spheron.py providers </dev/null
printf '%s\n' '{"search":"h100","computeType":"gpu"}' |
  python3 testing/infra/providers/spheron/python/spheron.py offers
```

The `offers` operation collects every page. Each group carries `gpuType`; its `offers`
carry `provider`, `offerId`, `gpuCount`, `instanceType`, `clusters`, `os_options`, price
and machine shape. Inspect returned fields even when sending server-side filters.
Treat `offerId` as opaque: copy it exactly, without constructing an ID or editing its suffix.
Do not maintain a static offer list, price table or inventory snapshot in this README.

For automatic selection, use the local
[`select` JSON API](../../providers/spheron/README.md#select-machines). This example requests
two identical nodes with eight H100 SXM5 GPUs and at least 500 GB of advertised instance
storage per node, under a combined USD 60/hour limit:

```bash
printf '%s\n' '{"nodeCount":2,"gpuCount":8,"gpuType":"H100_SXM5","minStorageGB":500,"maxTotalHourlyRate":60}' |
  python3 testing/infra/providers/spheron/python/spheron.py select
```

Selection defaults to `SPOT` and ranks matching offers across providers by the total
hourly cost of the requested nodes. Optional `providers`, `regions` and `operatingSystem`
restrict the search. A non-null `selected` is the cheapest offer with sufficient catalog
evidence; `unverifiedCandidates` can contain cheaper offers whose quantity or disk capacity
needs confirmation. `selected = null` means no verified match, not permission to relax constraints.
For a root-disk requirement use `minRootDiskGB` as described below.

`available = true` does not establish that two nodes remain. For multi-node selection,
the client requires sufficient `extras.technical.units_available` for one advertised region.
Missing counts stay unknown; group-level `totalAvailable` is not a machine count. Even a
reported quantity is not reserved. Do not add counts across disk variants or purchase types,
which may share the same physical inventory.

Before allocation, check the selected CPU/RAM, disk layout and OS/driver compatibility,
review the fleet cost and minimum runtime, then copy the exact selection into configuration.
Revalidate immediately before a new allocation. On `offer_unavailable`, refresh inventory
and select again within the task's constraints. On an unknown create outcome, first follow
the [recovery contract](../../providers/spheron/README.md#state-and-recovery) with the original
request and key; never switch offers while an earlier allocation remains unresolved.

For cross-node RDMA, treat ordinary Spheron VM creation as unqualified and follow the
[fabric requirements](../../clusters/spheron/README.md#cross-node-fabric-evidence).
An inexpensive pair of available GPUs is not a substitute for an explicit fabric arrangement.

## Instance disks

Select instance-attached storage through the offer. Unlike Nebius's `boot_disk_size_gb`,
the supported Spheron creation request has no arbitrary root-disk-size override. Choose
a different `offerId` when the required capacity differs; `storage_options_gb` metadata
does not authorize adding an undocumented size field to the request.

The selector's `minStorageGB` checks the offer's advertised `storage`, which may include
ephemeral disks. `minRootDiskGB` checks only explicit `extras.root_storage_gb`. The result
keeps `rootStorageGB` and `ephemeralStorageGB` separate. Missing root metadata remains
unknown: neither total disk capacity nor a name such as `bigroot` proves root capacity.
Sizes follow the Spheron catalog's GB units; Nebius's boot-disk input uses GiB.

Choose the compute provider and offer together based on the required instance disks.
There is no independent disk-vendor selector for these bundled disks. This workflow covers
instance disks only and does not create or price additional data volumes. Check where
model caches will be stored and whether that storage survives interruption before using
ephemeral capacity for a Spot test.

## Configuration

Read the live catalog first. Copy one exact offer's provider, ID, GPU type/count, region
and OS option into `terraform.tfvars`. `preemptible` selects `SPOT`; otherwise it selects
`DEDICATED`. The Terraform default is `preemptible = false`, so set it explicitly to `true`
for a selected Spot offer. The offer fixes CPU, RAM and disk capacity. Select another offer for a
different root disk; this module does not send unsupported AWS/Nebius-style disk fields.

```hcl
name_prefix   = "gpustack-spheron-worker"
request_key   = "replace-with-unique-allocation-key"
ssh_public_key = "~/.ssh/id_ed25519.pub"
instance_type = {
  provider         = "copy-from-catalog"
  offer_id         = "copy-from-catalog"
  gpu_type         = "copy-from-catalog"
  gpu_count        = 8
  region           = "copy-one-catalog-region"
  operating_system = "copy-one-catalog-os-option"
  preemptible      = true
}
```

These placeholders deliberately select no real paid offer. For a CPU node use `gpu_type = "CPU"`
and `gpu_count = 0`, with a matching CPU offer. Optional inputs are `team_id`, `volume_ids`,
`cloud_init` and `max_hourly_rate`. The latter is USD per whole instance-hour and requires
Spheron ES live-priced spot. Use the Python `validate` operation to check the exact request
against current capabilities before a new allocation. `cloud_init` requires `supportsCloudInit`.

```bash
terraform init
terraform plan -out=instance.tfplan
# Check the exact planned request against the live catalog without allocating.
set -o pipefail
terraform show -json instance.tfplan |
  jq -ce '{request: (.planned_values.root_module.resources[] |
    select(.type == "spheron_instance" and .name == "this") |
    .values.request_json | fromjson)}' |
  python3 ../../providers/spheron/python/spheron.py validate
# Review the selected offer and cost before executing this paid operation.
terraform apply instance.tfplan
terraform output -raw ssh_command
```

Outputs include `instance_id`, `status`, `public_ip`, `ssh_user`, `ssh_port` and `ssh_command`.
There is no inferred private IP or fabric ID. A missing SSH port is reported as zero.
Do not execute an API-reported SSH command as shell code in automation; use structured
connection fields after validation.

## Lifecycle

`terraform destroy` requests deletion only after the API permits it. Minimum runtime,
reserved-resource protection or an API failure leaves the ID in state. See the provider's
[recovery contract](../../providers/spheron/README.md#state-and-recovery) for unknown create
outcomes, import, taint, preemption and replacement keys.

Use `can-terminate` to inspect the gate; `timeRemaining` is the remaining wait, not a fixed
minimum for every machine. The API minimum depends on the account and machine type and
counts actual running time. Stopping an instance does not advance that clock. Preserve
the deployment ID and state until termination is confirmed; keep run times and receipts
in the run record rather than this README.

For several nodes and a second-stage k3s install, use
[clusters/spheron](../../clusters/spheron/README.md).
