output "region" {
  description = "AWS region"
  value       = var.region
}

output "cluster_name" {
  description = "Kubernetes Cluster Name"
  value       = module.eks.cluster_name
}

# Retrieve kubeconfig for the EKS cluster
# aws eks --region $(terraform output -raw region) update-kubeconfig --name $(terraform output -raw cluster_name)