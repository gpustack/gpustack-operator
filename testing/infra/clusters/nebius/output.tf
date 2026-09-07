output "context_name" {
  description = "The kubectl context merged into ~/.kube/config for this cluster."
  value       = local.context_name
}

output "cluster_id" {
  description = "Nebius mk8s cluster ID."
  value       = nebius_mk8s_v1_cluster.this.id
}

output "public_endpoint" {
  description = "Public Kubernetes API endpoint."
  value       = nebius_mk8s_v1_cluster.this.status.control_plane.endpoints.public_endpoint
}

output "node_group_names" {
  description = "Names of the provisioned node groups."
  value       = [for ng in nebius_mk8s_v1_node_group.this : ng.name]
}

# Which groups are reachable is read off the CREATED node group, never off the flag that asked for
# one. Two things are being avoided here. A hardcoded "the cpu group has none" would still read as
# true after cpu_instance_types.public_ip turns one on, because nothing in that sentence depends on
# the flag. And a value derived from var.*_instance_types.public_ip would report what was REQUESTED:
# public_ip_address is optional-and-computed, so turning the flag off on a group that already exists
# plans no change and the node keeps its address (see the README's public-addresses section), and
# the note would then list a group as unreachable while SSH still works on it. The attribute in
# state is the only one of the three that says what is actually there.
#
# The lookup is deliberately NOT wrapped in try(). main.tf always writes exactly one interface, so
# index 0 is always there; what try() would actually absorb is a mistyped attribute name, and it
# would absorb it into null -- reporting every group as unreachable, quietly, which is the same
# class of wrong answer this local exists to remove. `terraform validate` does not check this path
# (it accepts a nonexistent attribute name here without complaint), so a loud failure is the only
# gate left.
locals {
  ssh_addressed = [
    for k, ng in nebius_mk8s_v1_node_group.this :
    k if ng.template.network_interfaces[0].public_ip_address != null
  ]
  ssh_unaddressed = [
    for k, ng in nebius_mk8s_v1_node_group.this :
    k if ng.template.network_interfaces[0].public_ip_address == null
  ]
}

output "ssh_note" {
  description = "How to reach individual nodes over SSH (node groups don't surface per-node IPs in Terraform state)."
  value = format(
    "kubectl --context %s get nodes -o wide, then ssh ubuntu@<ExternalIP>. Groups with an address: %s. Groups without: %s.",
    local.context_name,
    length(local.ssh_addressed) > 0 ? join(", ", local.ssh_addressed) : "none",
    length(local.ssh_unaddressed) > 0 ? join(", ", local.ssh_unaddressed) : "none",
  )
}
