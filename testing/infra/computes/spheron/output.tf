output "instance_id" {
  description = "Spheron deployment ID, also retained when provisioning fails after creation."
  value       = spheron_instance.this.id
}

output "public_ip" {
  description = "API-reported address, without an inferred private or RDMA address."
  value       = spheron_instance.this.public_ip
}

output "ssh_user" {
  description = "API-reported SSH user."
  value       = spheron_instance.this.ssh_user
}

output "ssh_port" {
  description = "API-reported SSH port; zero means it was not reported."
  value       = spheron_instance.this.ssh_port
}

output "ssh_command" {
  description = "API-reported SSH command."
  value       = spheron_instance.this.ssh_command
}

output "status" {
  description = "Latest deployment status."
  value       = spheron_instance.this.status
}
