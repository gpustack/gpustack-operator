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

# Which groups are reachable is DERIVED from local.node_groups, never spelled out. An output that
# restates a fact it could compute is a second copy that nothing keeps in sync: a hardcoded "the
# cpu group has none" would still read as true after cpu_instance_types.public_ip turns one on,
# because nothing in that sentence depends on the flag.
locals {
  ssh_addressed   = [for k, v in local.node_groups : k if v.public_ip]
  ssh_unaddressed = [for k, v in local.node_groups : k if !v.public_ip]
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
