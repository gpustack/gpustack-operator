output "vm_name" {
  description = "Name of the VM instance"
  value       = nebius_compute_v1_instance.this.name
}

output "public_ip" {
  description = "Public IPv4 address of the VM"
  value       = local.public_ip
}

output "private_ip" {
  description = "Private IPv4 address of the VM"
  value       = local.private_ip
}

output "ssh_command" {
  description = "Ready-to-run SSH command to reach the VM"
  value       = "ssh -i ${trimsuffix(var.ssh_public_key, ".pub")} ubuntu@${local.public_ip}"
}
