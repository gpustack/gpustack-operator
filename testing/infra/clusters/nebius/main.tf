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
  cluster_name = "${var.name_prefix}-${random_string.suffix.result}"
  context_name = local.cluster_name

  node_groups = merge(
    {
      cpu = {
        instance_type = { platform = var.cpu_instance_types.platform, preset = var.cpu_instance_types.preset }
        os            = var.cpu_instance_types.os
        gpu           = null
      }
    },
    {
      for name, cfg in var.gpu_instance_types :
      "gpu-${name}" => {
        instance_type = { platform = cfg.platform, preset = cfg.preset }
        os            = coalesce(cfg.os, data.external.gpu_compat[name].result.os)
        gpu           = { drivers_preset = coalesce(cfg.drivers_preset, data.external.gpu_compat[name].result.drivers_preset) }
      }
    },
  )
}

# Resolve each GPU group's os + drivers_preset from Nebius' live compatibility matrix for
# var.release, picking the newest available driver preset (per-group overrides in
# var.gpu_instance_types take precedence via coalesce above). NOTE: as a data source this runs on
# every plan, apply AND destroy, so the `nebius` CLI (authenticated) and `jq` must be available
# whenever Terraform runs against this module — including `terraform destroy`.
data "external" "gpu_compat" {
  for_each = var.gpu_instance_types

  program = ["bash", "-c", <<-EOT
    set -euo pipefail
    eval "$(jq -r '@sh "REL=\(.release) PLAT=\(.platform)"')"
    nebius mk8s node-group get-compatibility-matrix \
      --cluster-kubernetes-version "$REL" --platform "$PLAT" --format json \
    | jq -e '
        .versions[0].items
        | map(select(.drivers_preset != null and .drivers_preset != ""))
        | sort_by(.drivers_preset) | last
        | {os: .os, drivers_preset: .drivers_preset}
      '
  EOT
  ]

  query = {
    release  = var.release
    platform = each.value.platform
  }
}

resource "nebius_vpc_v1_network" "this" {
  parent_id = var.project_id
  name      = local.cluster_name
}

resource "nebius_vpc_v1_subnet" "this" {
  parent_id  = var.project_id
  name       = local.cluster_name
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
  name       = local.cluster_name
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
    # SSH is reachable from anywhere by design (disposable test nodes behind key-only auth).
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

resource "nebius_mk8s_v1_cluster" "this" {
  parent_id = var.project_id
  name      = local.cluster_name

  control_plane = {
    subnet_id         = nebius_vpc_v1_subnet.this.id
    version           = var.release
    etcd_cluster_size = 1

    endpoints = {
      public_endpoint = {}
    }
  }
}

resource "nebius_mk8s_v1_node_group" "this" {
  for_each  = local.node_groups
  parent_id = nebius_mk8s_v1_cluster.this.id
  name      = each.key

  fixed_node_count = 1

  template = {
    resources = {
      platform = each.value.instance_type.platform
      preset   = each.value.instance_type.preset
    }

    os = each.value.os

    boot_disk = {
      type           = var.node_boot_disk_type
      size_gibibytes = var.node_boot_disk_size_gb
    }

    network_interfaces = [{
      subnet_id         = nebius_vpc_v1_subnet.this.id
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

    gpu_settings = each.value.gpu != null ? {
      drivers_preset = each.value.gpu.drivers_preset
    } : null
  }
}

# Merges the cluster into ~/.kube/config as a new context (mirrors clusters/eks's
# update_kubeconfig); on destroy it removes that context/cluster/user.
resource "null_resource" "kubeconfig" {
  depends_on = [nebius_mk8s_v1_cluster.this]

  triggers = {
    id      = nebius_mk8s_v1_cluster.this.id
    context = local.context_name
    # get-credentials names the CONTEXT "<context>" but the cluster/user entries
    # "nebius-mk8s-<context>-<clusterId>", where <clusterId> is the cluster id with its
    # "mk8scluster-" resource-type prefix stripped. Precompute the full entry name here so
    # the destroy provisioner (which may reference only self) deletes the real names.
    kubeconfig_entry = "nebius-mk8s-${local.context_name}-${trimprefix(nebius_mk8s_v1_cluster.this.id, "mk8scluster-")}"
  }

  provisioner "local-exec" {
    command = "nebius mk8s cluster get-credentials --id ${self.triggers.id} --external --force --context-name ${self.triggers.context}"
  }

  # Delete the context AND its cluster/user entries by their real names, so destroy leaves
  # no orphaned cluster/user piling up in ~/.kube/config across runs.
  provisioner "local-exec" {
    when       = destroy
    on_failure = continue
    command    = "kubectl config delete-context '${self.triggers.context}' || true; kubectl config delete-cluster '${self.triggers.kubeconfig_entry}' || true; kubectl config unset 'users.${self.triggers.kubeconfig_entry}' || true"
  }
}

# Records the last SUCCESSFUL apply's inputs; Terraform auto-loads *.auto.tfvars.json on every
# command (incl. destroy), and command-line -var still overrides it on apply. A managed
# hashicorp/local local_file is not used here: it is deleted during destroy, which would strand
# a failed/interrupted destroy retry with no values for project_id (a required variable).
resource "null_resource" "last_apply" {
  depends_on = [
    nebius_mk8s_v1_cluster.this,
    nebius_mk8s_v1_node_group.this,
    null_resource.kubeconfig,
  ]

  triggers = {
    snapshot = jsonencode({
      project_id             = var.project_id
      name_prefix            = var.name_prefix
      release                = var.release
      ssh_public_key         = var.ssh_public_key
      node_boot_disk_size_gb = var.node_boot_disk_size_gb
      node_boot_disk_type    = var.node_boot_disk_type
      cpu_instance_types     = var.cpu_instance_types
      gpu_instance_types     = var.gpu_instance_types
    })
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = "cat > '${path.module}/.last-apply.auto.tfvars.json' <<'EOF'\n${self.triggers.snapshot}\nEOF"
  }
}
