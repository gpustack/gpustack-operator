# Spheron nodes and k3s

This root composes [standalone Spheron compute](../../computes/spheron/README.md) with the
existing [k3s root](../k3s/README.md) in two stages. First it creates named instances and
exports SSH inventory. Then k3s installs the cluster using that resolved inventory.
The k3s resources currently use host addresses as keys, so a single apply with new,
unknown instance IPs is not supported by that root.

## Configure and deploy

Build the [local provider](../../providers/spheron/README.md), export `SPHERON_API_KEY`,
and use its session-local Terraform configuration. Follow the
[machine selection workflow](../../computes/spheron/README.md#machine-selection) for all
requested nodes, including instance-disk requirements and quantity checks. Prefer Spot GPU
offers for temporary tests and set `preemptible = true` explicitly. Define `nodes` in `terraform.tfvars`:

```hcl
nodes = {
  control = {
    role        = "server"
    request_key = "unique-control-allocation-001"
    instance_type = {
      provider         = "copy-from-catalog"
      offer_id         = "copy-cpu-offer"
      gpu_type         = "CPU"
      gpu_count        = 0
      region           = "copy-one-catalog-region"
      operating_system = "copy-one-catalog-os-option"
    }
  }
  source = {
    role        = "agent"
    request_key = "unique-source-allocation-001"
    instance_type = {
      provider         = "copy-from-catalog"
      offer_id         = "copy-gpu-offer"
      gpu_type         = "copy-from-catalog"
      gpu_count        = 8
      region           = "copy-one-catalog-region"
      operating_system = "copy-one-catalog-os-option"
      preemptible      = true
    }
  }
}
```

Add another named agent for a cross-node target. Each node needs its own request key.
Use `name_prefix`, `team_id`, `ssh_public_key` and `ssh_private_key` to match the account
and local SSH setup. A GPU node can also serve as the server when no suitable CPU offer exists.

From this directory, after reviewing offers and costs:

```bash
terraform init
terraform plan -out=nodes.tfplan
terraform apply -parallelism=1 nodes.tfplan
terraform output -json k3s_variables > k3s.tfvars.json
terraform -chdir=../k3s init
terraform -chdir=../k3s plan -var-file=../spheron/k3s.tfvars.json -out=k3s.tfplan
terraform -chdir=../k3s apply k3s.tfplan
```

Spheron creates one deployment per team at a time. Keep node creation serial with
`-parallelism=1`; concurrent creates can return `deployment_in_progress`.

The export preserves API-reported SSH users and ports. The existing k3s module requires a
common port within each role; the export fails if nodes within one role have different ports
or missing connection details. The instances stay recorded in Terraform state for inspection.
Supply `node_internal_ip` only when actual cluster addresses differ from SSH addresses;
it does not create a private network. Review the k3s networking prerequisites before install.
The generated configuration keeps `switch_kube_context = false`.

Each root owns separate state. Destroy k3s while the nodes are reachable, then destroy the
Spheron nodes. After node replacement, regenerate the inventory and review the k3s plan;
changed addresses can replace installation resources. The provider never tears down k3s
implicitly.

## Cross-node fabric evidence

- Spheron's [official offering](https://www.spheron.ai/) advertises provider-dependent
  multi-node InfiniBand and custom clusters with requested InfiniBand configurations.
- Its [networking article](https://www.spheron.network/blog/gpu-networking-infiniband-roce-spectrum-x-guide/)
  distinguishes NVLink within a node from RoCEv2 or InfiniBand across nodes, depending on the cluster.
- [Reserved GPUs](https://docs.spheron.ai/reserved-gpus) are explicit multi-node capacity.
  Published-cluster bidding requires invitation and uses monthly terms; a custom request is
  a separate procurement flow.
- The [documented deployment API](https://docs.spheron.ai/api-reference) has no published
  fabric ID, shared-fabric attachment, placement-group or network-interface selection field.
  The catalog's `clusters` field supplies the deployment region; it is not proof of fabric membership.
  Even the documented `CLUSTER` example reports NVLink with `extras.networking_type = "ethernet"`.
- SXM5/NVLink metadata describes connectivity within a node. Catalog availability does not
  reserve capacity or guarantee shared fabric between instances.

Therefore the platform advertises cross-node fabric, but two ordinary API-created instances
are not established to share it. This module provides no `fabric_id` that the API cannot honor.
For test planning, treat immediate cross-node RDMA as unavailable through this module until
a specific supported cluster arrangement is established. Reserved GPUs are a separate
procurement path with monthly commitments, not an automatic fallback for a short Spot test.
Before an RDMA run, obtain the exact provider/region, two-node placement guarantee, network
type and speed, HCA/VF exposure, and the supported API attachment procedure or supplier-arranged
cluster. Then verify the actual nodes with device/link inspection, RDMA bandwidth tests, and
the NIXL test. Ordinary IP reachability and successful k3s installation do not qualify RDMA.
