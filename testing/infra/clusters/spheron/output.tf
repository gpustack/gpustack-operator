output "nodes" {
  description = "Deployment IDs and API-reported connection details, keyed by stable node name."
  value       = module.node
}

output "k3s_variables" {
  description = "Save with terraform output -json k3s_variables, then pass as a var-file to the existing k3s root."
  value = {
    server              = [for node in local.servers : "${node.ssh_user}@${node.public_ip}"]
    agent               = [for node in local.agents : "${node.ssh_user}@${node.public_ip}"]
    server_ssh_port     = try(local.servers[0].ssh_port, 22)
    agent_ssh_port      = try(local.agents[0].ssh_port, 22)
    ssh_private_key     = var.ssh_private_key
    node_internal_ip    = var.node_internal_ip
    switch_kube_context = false
  }
  precondition {
    condition     = length(distinct([for node in module.node : node.public_ip])) == length(module.node)
    error_message = "The existing k3s module requires distinct host addresses for all nodes."
  }
  precondition {
    condition     = alltrue([for node in module.node : node.public_ip != "" && node.ssh_user != "" && node.ssh_port > 0 && node.ssh_port <= 65535])
    error_message = "Every node must report an IP, SSH user and valid port before exporting k3s input. Inspect the retained deployments."
  }
  precondition {
    condition     = length(distinct([for node in local.servers : node.ssh_port])) <= 1 && length(distinct([for node in local.agents : node.ssh_port])) <= 1
    error_message = "The existing k3s module requires one SSH port per role. These nodes need a per-node-port extension before k3s installation."
  }
}
