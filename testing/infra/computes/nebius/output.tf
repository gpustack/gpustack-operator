output "vm_name" {
  description = "Name of the VM instance"
  value       = nebius_compute_v1_instance.this.name
}

output "public_ip" {
  description = "Public IPv4 address of the VM"
  value       = nebius_compute_v1_instance.this.status.network_interfaces[0].public_ip_address.address
}

output "private_ip" {
  description = "Private IPv4 address of the VM"
  value       = nebius_compute_v1_instance.this.status.network_interfaces[0].ip_address.address
}

output "ssh_command" {
  description = "Ready-to-run SSH command to reach the VM"
  value       = "ssh ${var.ssh_username}@${nebius_compute_v1_instance.this.status.network_interfaces[0].public_ip_address.address}"
}
