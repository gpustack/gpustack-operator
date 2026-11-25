output "context_name" {
  description = "The kubectl context merged into ~/.kube/config for this cluster."
  value       = local.context_name
}

output "cluster_name" {
  description = "The Kind cluster name on the remote Docker host."
  value       = var.cluster_name
}

output "host" {
  description = "The remote Docker host running the Kind cluster."
  value       = local.target_host
}

output "node_image" {
  description = "The effective kindest/node image reference used for all cluster nodes."
  value       = local.effective_node_image
}

output "node_count" {
  description = "Total number of Kind nodes (control-plane + worker)."
  value       = local.node_count
}

output "kubeconfig_path" {
  description = "Path to the standalone rewritten kubeconfig for the cluster."
  value       = local.kubeconfig_path
}

output "kind_config_yaml" {
  description = "Rendered kind.x-k8s.io/v1alpha4 Cluster configuration YAML."
  value       = local.kind_cluster_config
}
