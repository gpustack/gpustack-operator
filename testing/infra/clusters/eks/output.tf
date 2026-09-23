output "region" {
  description = "AWS region"
  value       = var.region
}

output "cluster_name" {
  description = "Kubernetes Cluster Name"
  value       = module.eks.cluster_name
}

output "selected_zones" {
  description = "Availability zone selected for each CPU node group, or null when it is not pinned."
  value = {
    for name, cfg in var.cpu_instance_types :
    name => cfg.availability_zone_index == null ? null : local.azs[cfg.availability_zone_index]
  }
}

output "node_group_names" {
  description = "Names of the configured EKS managed node groups."
  value       = keys(local.node_groups)
}

output "kubeconfig_command" {
  description = "Command that writes a separate kubeconfig file without changing the caller's current context."
  value       = "aws eks --region ${var.region} update-kubeconfig --name ${module.eks.cluster_name} --kubeconfig ./kubeconfig-${module.eks.cluster_name}"
}
