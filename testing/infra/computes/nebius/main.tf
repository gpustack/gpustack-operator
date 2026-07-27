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

# Carries project_id — the one variable with no default — across a failed or interrupted destroy
# retry. Terraform auto-loads *.auto.tfvars.json on every command (incl. destroy), and
# command-line -var still overrides it on apply. A managed hashicorp/local local_file is not used
# here: it is deleted during destroy, which is exactly when the value is still needed. A
# destroy-time provisioner on this resource has the same defect: last_apply depends on the
# instances, so it is destroyed FIRST, and the file would be gone before they were.
#
# Snapshot NOTHING that has a default. Auto-loading is indiscriminate — it feeds `apply` just as
# readily as `destroy` — so a variable recorded here silently overrides its own default on every
# later apply in this directory, with nothing on the command line to hint at it.
resource "null_resource" "last_apply" {
  depends_on = [nebius_compute_v1_instance.this]

  triggers = {
    snapshot = jsonencode({
      project_id = var.project_id
    })
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${path.module}/.last-apply.auto.tfvars.json' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }
}

