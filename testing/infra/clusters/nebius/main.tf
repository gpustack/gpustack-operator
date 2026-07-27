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

  # Node preparation for a GPU node group. The managed GPU image ships two things GPUStack must not
  # compete with: a vendor device plugin as a STATIC Pod (it advertises the same accelerator resource
  # the GPUStack device plugin does, and cannot be removed with `kubectl delete` because the kubelet
  # owns it), and a DCGM telemetry service holding driver handles that make a MIG mode switch fail.
  #
  # This runs as a boot-time unit rather than a one-shot cloud-init command, for two reasons: the
  # static manifest is written by the node bootstrap, which has not necessarily finished when
  # cloud-init runs; and the provider reboots a node whose GPU health check fails — which putting a
  # card into MIG mode is enough to cause — so anything merely stopped would come back.
  gpu_node_prep = <<-EOT
    write_files:
      - path: /usr/local/sbin/gpustack-node-prep.sh
        permissions: "0755"
        content: |
          #!/usr/bin/env bash
          # Idempotent, and re-applied on every boot.
          set -u
          disabled=/etc/kubernetes/manifests.disabled
          mkdir -p "$disabled"
          shopt -s nullglob
          # Wait out the node bootstrap: the manifests directory may not be populated yet.
          for _ in $(seq 1 60); do
            found=(/etc/kubernetes/manifests/*device-plugin*.yaml /etc/kubernetes/manifests/*device_plugin*.yaml)
            if [ $${#found[@]} -gt 0 ]; then
              mv -f "$${found[@]}" "$disabled"/
              break
            fi
            sleep 10
          done
          # Mask rather than disable: DCGM holds driver handles that make a MIG mode switch fail,
          # and a disabled unit is still pulled back in by the vendor's own units and timers — it
          # was observed running again hours after a clean disable. Masking is what actually
          # keeps it down. Wait for the units to exist: the driver preset installs them after the
          # first boot, so an early mask would silently no-op.
          for _ in $(seq 1 60); do
            systemctl list-unit-files 'nvidia-dcgm*' 2>/dev/null | grep -q nvidia-dcgm && break
            sleep 10
          done
          systemctl mask --now nvidia-dcgm nvidia-dcgm-exporter 2>/dev/null || true
      - path: /etc/systemd/system/gpustack-node-prep.service
        content: |
          [Unit]
          Description=Disable the vendor device plugin and DCGM so GPUStack owns the accelerators
          After=network-online.target
          [Service]
          Type=oneshot
          RemainAfterExit=yes
          ExecStart=/usr/local/sbin/gpustack-node-prep.sh
          [Install]
          WantedBy=multi-user.target
    runcmd:
      - [ systemctl, daemon-reload ]
      - [ systemctl, enable, --now, gpustack-node-prep.service ]
  EOT

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

resource "nebius_vpc_v1_security_rule" "ephemeral_tcp_ingress" {
  parent_id = nebius_vpc_v1_security_group.this.id
  name      = "ephemeral-tcp-ingress"
  access    = "ALLOW"
  protocol  = "TCP"
  priority  = 110
  type      = "STATEFUL"

  # Mirrors clusters/eks's NodePort ingress (30000-32767), so services reachable via a NodePort
  # in e2e can be reached here too. Nebius security rules cap at 8 discrete ports per rule (no
  # from/to range like AWS), so the full TCP range is opened instead of just the NodePort span.
  ingress = {
    source_cidrs = ["0.0.0.0/0"]
  }
}

resource "nebius_vpc_v1_security_rule" "ephemeral_udp_ingress" {
  parent_id = nebius_vpc_v1_security_group.this.id
  name      = "ephemeral-udp-ingress"
  access    = "ALLOW"
  protocol  = "UDP"
  priority  = 111
  type      = "STATEFUL"

  ingress = {
    source_cidrs = ["0.0.0.0/0"]
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

    cloud_init_user_data = join("\n", compact([
      <<-EOT
        #cloud-config
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            shell: /bin/bash
            ssh_authorized_keys:
              - "${trimspace(file(pathexpand(var.ssh_public_key)))}"
      EOT
      ,
      each.value.gpu != null ? local.gpu_node_prep : "",
    ]))

    gpu_settings = each.value.gpu != null ? {
      drivers_preset = each.value.gpu.drivers_preset
    } : null
  }

  # Putting a card into a hardware partitioning mode fails the provider's own GPU health check —
  # its NVLink topology probe cannot read a partitioned card — which raises the NebiusGPUError
  # node condition. The default auto-repair rule for that condition cordons the node and then
  # shuts it down, so a partitioning test kills the very node it runs on. Turning the rule off
  # leaves the condition reported and visible, but no longer node-fatal. Every other auto-repair
  # rule (kernel deadlock, disk IO, container runtime) keeps its default.
  auto_repair = each.value.gpu != null ? {
    conditions = [{
      type     = "NebiusGPUError"
      disabled = true
    }]
  } : null
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

# Carries project_id — the one variable with no default — across a failed or interrupted destroy
# retry. Terraform auto-loads *.auto.tfvars.json on every command (incl. destroy), and
# command-line -var still overrides it on apply. A managed hashicorp/local local_file is not used
# here: it is deleted during destroy, which is exactly when the value is still needed.
#
# Snapshot NOTHING that has a default. Auto-loading is indiscriminate — it feeds `apply` just as
# readily as `destroy` — so a variable recorded here silently overrides its own default on every
# later apply in this directory, with nothing on the command line to hint at it. Recording the
# cluster's shape here once made a plain `apply` keep rebuilding an 8-GPU node group long after
# the default had been reduced to a single card.
resource "null_resource" "last_apply" {
  depends_on = [
    nebius_mk8s_v1_cluster.this,
    nebius_mk8s_v1_node_group.this,
    null_resource.kubeconfig,
  ]

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
