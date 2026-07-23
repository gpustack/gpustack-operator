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
  vm_name = "${var.name_prefix}-${random_string.suffix.result}"

  # Nebius returns network interface addresses as a CIDR (e.g. "1.2.3.4/32"); strip the suffix.
  public_ip  = split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].public_ip_address.address)[0]
  private_ip = split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].ip_address.address)[0]
}

resource "nebius_vpc_v1_network" "this" {
  parent_id = var.project_id
  name      = local.vm_name
}

resource "nebius_vpc_v1_subnet" "this" {
  parent_id  = var.project_id
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
  parent_id  = var.project_id
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
    # SSH is reachable from anywhere by design (a disposable test VM behind key-only auth).
    source_cidrs      = ["0.0.0.0/0"]
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
  parent_id = var.project_id
  name      = local.vm_name

  resources = {
    platform = var.instance_type.platform
    preset   = var.instance_type.preset
  }

  boot_disk = {
    attach_mode = "READ_WRITE"
    managed_disk = {
      name = "${local.vm_name}-boot"
      spec = {
        type           = var.boot_disk_type
        size_gibibytes = var.boot_disk_size_gb
        source_image_family = {
          image_family = var.instance_type.image_family
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
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
        shell: /bin/bash
        ssh_authorized_keys:
          - "${trimspace(file(pathexpand(var.ssh_public_key)))}"
  EOT
}

# Records the last SUCCESSFUL apply's inputs; Terraform auto-loads *.auto.tfvars.json on every
# command (incl. destroy), and command-line -var still overrides it on apply. A managed
# hashicorp/local local_file is not used here: it is deleted during destroy, which would strand
# a failed/interrupted destroy retry with no values for project_id (a required variable).
resource "null_resource" "last_apply" {
  depends_on = [nebius_compute_v1_instance.this]

  triggers = {
    snapshot = jsonencode({
      project_id        = var.project_id
      name_prefix       = var.name_prefix
      ssh_public_key    = var.ssh_public_key
      instance_type     = var.instance_type
      boot_disk_type    = var.boot_disk_type
      boot_disk_size_gb = var.boot_disk_size_gb
    })
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${path.module}/.last-apply.auto.tfvars.json' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }
}

