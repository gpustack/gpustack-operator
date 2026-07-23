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

output "ssh_note" {
  description = "How to reach individual nodes over SSH (node groups don't surface per-node IPs in Terraform state)."
  value       = "kubectl --context ${local.context_name} get nodes -o wide, then ssh ubuntu@<ExternalIP>"
}
