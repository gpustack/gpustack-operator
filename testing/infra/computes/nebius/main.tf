provider "nebius" {
  service_account = {
    private_key_file_env = "NEBIUS_AUTHKEY_PRIVATE_PATH"
    public_key_id_env    = "NEBIUS_AUTHKEY_PUBLIC_ID"
    account_id_env       = "NEBIUS_SA_ID"
  }
}

resource "random_string" "suffix" {
  length  = 8
  special = false
  upper   = false
}

locals {
  vm_name = "${var.vm_name_prefix}-${random_string.suffix.result}"

  # Nebius returns network interface addresses as a CIDR (e.g. "1.2.3.4/32"); strip the suffix.
  public_ip  = split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].public_ip_address.address)[0]
  private_ip = split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].ip_address.address)[0]
}

resource "nebius_vpc_v1_network" "this" {
  parent_id = var.parent_id
  name      = local.vm_name
}

resource "nebius_vpc_v1_subnet" "this" {
  parent_id  = var.parent_id
  name       = local.vm_name
  network_id = nebius_vpc_v1_network.this.id

  ipv4_private_pools = {
    use_network_pools = true
  }
  ipv4_public_pools = {
    use_network_pools = true
  }
}

resource "nebius_vpc_v1_security_group" "this" {
  parent_id  = var.parent_id
  name       = local.vm_name
  network_id = nebius_vpc_v1_network.this.id
}

resource "nebius_vpc_v1_security_rule" "ssh_ingress" {
  parent_id = nebius_vpc_v1_security_group.this.id
  name      = "ssh-ingress"
  access    = "ALLOW"
  protocol  = "TCP"
  priority  = 100
  type      = "STATEFUL"

  ingress = {
    source_cidrs      = var.ssh_source_cidrs
    destination_ports = [22]
  }
}

resource "nebius_vpc_v1_security_rule" "egress" {
  parent_id = nebius_vpc_v1_security_group.this.id
  name      = "egress"
  access    = "ALLOW"
  protocol  = "ANY"
  priority  = 200
  type      = "STATEFUL"

  egress = {
    destination_cidrs = ["0.0.0.0/0"]
  }
}

resource "nebius_compute_v1_instance" "this" {
  parent_id = var.parent_id
  name      = local.vm_name

  resources = {
    platform = var.platform_preset_image.platform
    preset   = var.platform_preset_image.preset
  }

  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "${local.vm_name}-boot"
      spec = {
        type           = var.boot_disk_type
        size_gibibytes = var.boot_disk_size_gibibytes
        source_image_family = {
          image_family = var.platform_preset_image.image_family
        }
      }
    }
  }

  network_interfaces = [{
    name              = "eth0"
    subnet_id         = nebius_vpc_v1_subnet.this.id
    ip_address        = {}
    public_ip_address = {}
    security_groups   = [{ id = nebius_vpc_v1_security_group.this.id }]
  }]

  cloud_init_user_data = <<-EOT
    #cloud-config
    users:
      - name: ${var.ssh_username}
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        ssh_authorized_keys:
          - "${trimspace(file(pathexpand(var.ssh_public_key)))}"
  EOT
}

