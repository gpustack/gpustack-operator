module "node" {
  source   = "../../computes/spheron"
  for_each = var.nodes

  name_prefix     = "${var.name_prefix}-${each.key}"
  request_key     = each.value.request_key
  team_id         = var.team_id
  instance_type   = each.value.instance_type
  ssh_public_key  = var.ssh_public_key
  volume_ids      = each.value.volume_ids
  max_hourly_rate = each.value.max_hourly_rate
}

locals {
  servers = [for name, node in module.node : node if var.nodes[name].role == "server"]
  agents  = [for name, node in module.node : node if var.nodes[name].role == "agent"]
}
