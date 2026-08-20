output "context_name" {
  description = "The kubectl context merged into ~/.kube/config for this cluster."
  value       = local.context_name
}

output "server_hosts" {
  description = "The RKE2 server (control-plane) host addresses."
  value       = [for s in local.servers : s.host]
}

output "agent_hosts" {
  description = "The RKE2 agent (worker) host addresses."
  value       = [for a in local.agents : a.host]
}

output "kubeconfig_path" {
  description = "Path to the standalone rewritten kubeconfig for the cluster."
  value       = local.kubeconfig_path
}
