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

  # Platforms whose cards can be partitioned in hardware (NVIDIA MIG). MIG is a datacenter-SXM
  # feature: an L40S or an RTX PRO 6000 has none, and a card without MIG must not be treated as if
  # it had it — see the `mig` flag in local.node_groups for what that gating actually protects.
  # A platform missing from this list is treated as non-partitionable; override it per group with
  # `mig` in var.gpu_instance_types rather than waiting for this list to catch up.
  mig_platforms = ["gpu-h100-sxm", "gpu-h200-sxm", "gpu-b200-sxm", "gpu-b200-sxm-a", "gpu-b300-sxm"]

  node_groups = merge(
    # A CPU group that should not exist is an absent key in cpu_instance_types. Some regions
    # sell accelerator capacity alone and hold compute.instance.non-gpu.vcpu at zero; there a
    # plain node is not a small extra cost but a failed apply, and the accelerator tests never
    # needed one.
    {
      for name, cfg in var.cpu_instance_types : name => {
        instance_type = { platform = cfg.platform, preset = cfg.preset }
        os            = cfg.os
        preemptible   = false
        mig           = false
        # Defaults to false: the accelerator tests drive GPU nodes, not this one, and a public
        # address is a quota'd resource (vpc.ipv4-address.public.count). Turn it on for the one
        # workflow that needs inbound reach -- building images on the node itself.
        public_ip         = cfg.public_ip
        node_count        = cfg.node_count
        gpu               = null
        infiniband_fabric = null
        # A CPU group never joins a fabric.
        gpu_cluster_id = null
        # The CPU group pulls no engine images, so it never outgrows the module-wide default.
        boot_disk_size_gb = null
      }
    },
    {
      for name, cfg in var.gpu_instance_types : name => {
        instance_type = { platform = cfg.platform, preset = cfg.preset }
        os            = coalesce(cfg.os, data.external.gpu_compat[name].result.os)
        preemptible   = cfg.preemptible
        # This flag gates the DCGM masking half of local.gpu_node_prep: a platform that cannot
        # be partitioned keeps its GPU telemetry. The NebiusGPUError auto-repair rule is not
        # gated on it — that is disabled on every GPU node group (each.value.gpu != null),
        # because non-MIG presets trip the same condition in their own way; see the auto_repair
        # block on the node group resource below.
        mig = coalesce(cfg.mig, contains(local.mig_platforms, cfg.platform))
        # A GPU node is driven from outside the cluster over SSH — the hardware-partition tests
        # toggle MIG on the card that way, and they take the node's address as an input rather than
        # guess it — and that inbound reach is what the public address buys. Turn it off per group
        # (`public_ip = false`) only for a GPU group nobody has to log in to.
        public_ip = cfg.public_ip
        # Nodes per GPU group. Adding a key to gpu_instance_types and raising a group's
        # node_count are different purchases: a key buys a group with its own platform and
        # preset, the count buys more nodes of the shape this group already names. Accelerator
        # quota is granted per platform and preset, so a count above one can be refused where a
        # second key would not be.
        node_count = cfg.node_count
        gpu        = { drivers_preset = coalesce(cfg.drivers_preset, data.external.gpu_compat[name].result.drivers_preset) }
        # Null unless the group asked for a fabric. A fabric is what puts RDMA devices on the node,
        # and it can only be joined at creation time, so this cannot be added to a running group.
        infiniband_fabric = cfg.infiniband_fabric
        # The GPU cluster to join, resolved once in local.gpu_cluster_ids below: an explicit
        # gpu_cluster_id, an existing project cluster reused by fabric, or the cluster this module
        # creates. Null leaves the group without RDMA.
        gpu_cluster_id = local.gpu_cluster_ids[name]
        # Null falls through to var.node_boot_disk_size_gb in the boot_disk block below.
        boot_disk_size_gb = cfg.boot_disk_size_gb
      }
    },
  )

  # The GPU clusters this module creates, keyed by the group that asked for one. Groups are read
  # through this map rather than by indexing the resource, so a group that ends up reusing an
  # existing cluster -- no entry here -- never touches a resource address that does not exist.
  created_gpu_cluster_ids = { for name, cluster in nebius_compute_v1_gpu_cluster.this : name => cluster.id }

  # Existing project GPU clusters by fabric, from data.external.gpu_clusters above. {} when the
  # lookup is switched off (no group asked for reuse), and a miss behaves the same either way
  # through lookup()'s default.
  reusable_gpu_clusters = coalesce(one(data.external.gpu_clusters[*].result), {})

  # The GPU cluster each GPU group joins, resolved once and consumed by both the node-group
  # template and the creation decision below. First match wins: an explicit gpu_cluster_id; else,
  # when a fabric is named and reuse_gpu_cluster is true, an existing project cluster on that
  # fabric; else the cluster this module creates. Null when the group asked for neither, which is
  # what keeps a plain group's template exactly as it was before reuse existed.
  #
  # Note: reuse_gpu_cluster picks the lexicographically smallest existing cluster ID on the fabric
  # at plan time, assuming no concurrent out-of-band creation or deletion of lower-ID clusters on
  # the same fabric (pin gpu_cluster_id explicitly when sharing a project with concurrent writers,
  # because changing a node group's gpu_cluster forces replacement).
  gpu_cluster_ids = {
    for name, cfg in var.gpu_instance_types : name =>
    cfg.gpu_cluster_id != null ? cfg.gpu_cluster_id :
    (cfg.infiniband_fabric != null ? coalesce(
      cfg.reuse_gpu_cluster ? lookup(local.reusable_gpu_clusters, cfg.infiniband_fabric, null) : null,
      lookup(local.created_gpu_cluster_ids, name, null),
    ) : null)
  }

  # Node preparation, per GPU node group — keyed by group because half of it is MIG-only. The
  # managed GPU image ships two things GPUStack must not compete with: a vendor device plugin as a
  # STATIC Pod (it advertises the same accelerator resource the GPUStack device plugin does, and
  # cannot be removed with `kubectl delete` because the kubelet owns it), and a DCGM telemetry
  # service holding driver handles that make a MIG mode switch fail.
  #
  # The device plugin is moved aside on every GPU node. DCGM is masked only where the card can be
  # partitioned at all: elsewhere there is nothing to unblock, and the node would lose its GPU
  # telemetry for nothing.
  #
  # This runs as a boot-time unit rather than a one-shot cloud-init command, for two reasons: the
  # static manifest is written by the node bootstrap, which has not necessarily finished when
  # cloud-init runs; and the provider reboots a node whose GPU health check fails — which putting a
  # card into MIG mode is enough to cause — so anything merely stopped would come back.
  gpu_node_prep = {
    for name, group in local.node_groups : name => <<-EOT
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
            # Everything below is MIG-only; the unit passes MIG_CAPABLE per node group.
            [ "$${MIG_CAPABLE:-false}" = true ] || exit 0
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
            Description=Disable the vendor device plugin (and DCGM where MIG applies) so GPUStack owns the accelerators
            After=network-online.target
            [Service]
            Type=oneshot
            RemainAfterExit=yes
            Environment=MIG_CAPABLE=${group.mig}
            ExecStart=/usr/local/sbin/gpustack-node-prep.sh
            [Install]
            WantedBy=multi-user.target
      runcmd:
        - [ systemctl, daemon-reload ]
        - [ systemctl, enable, --now, gpustack-node-prep.service ]
    EOT
    if group.gpu != null
  }
}

# Resolve each GPU group's os + drivers_preset from Nebius' live compatibility matrix for
# var.release, picking the newest available driver preset (per-group overrides in
# var.gpu_instance_types take precedence via coalesce above). The same script reads the region's
# platform list, so a group that asks to join a GPU cluster -- by fabric or by ID -- fails at PLAN
# when its preset has allow_gpu_clustering=false, with the presets that do allow it named, instead
# of at apply with a provider error that names none of them. Groups that ask for no GPU cluster
# skip that check: a preset that cannot join a fabric is perfectly fine on its own.
# NOTE: as a data source this runs on every plan, apply AND destroy, so the `nebius` CLI
# (authenticated) and `jq` must be available whenever Terraform runs against this module —
# including `terraform destroy`.
data "external" "gpu_compat" {
  for_each = var.gpu_instance_types

  program = ["bash", "-c", <<-EOT
    set -euo pipefail
    for tool in jq nebius; do
      command -v "$tool" >/dev/null || { echo "$tool is required to resolve os/drivers_preset and check allow_gpu_clustering; install it" >&2; exit 1; }
    done
    eval "$(jq -r '@sh "PROJECT=\(.project_id) REL=\(.release) PLAT=\(.platform) PRESET=\(.preset) WANTS=\(.wants_cluster)"')"
    matrix="$(nebius mk8s node-group get-compatibility-matrix \
      --cluster-kubernetes-version "$REL" --platform "$PLAT" --format json)"
    platforms="$(nebius compute platform list --parent-id "$PROJECT" --format json)"
    os_drivers="$(printf '%s' "$matrix" | jq -e '
        .versions[0].items
        | map(select(.drivers_preset != null and .drivers_preset != ""))
        | sort_by(.drivers_preset) | last
        | {os: .os, drivers_preset: .drivers_preset}
      ')"
    # "unknown" means the platform or the preset answered nothing, which is its own failure mode:
    # an attachment request against a preset that does not exist should say so, not just "no".
    allow="$(printf '%s' "$platforms" | jq -r --arg plat "$PLAT" --arg preset "$PRESET" '
        [.items[] | select(.metadata.name == $plat) | .spec.presets[] | select(.name == $preset)]
        | if length == 0 then "unknown" else (.[0].allow_gpu_clustering // false | tostring) end
      ')"
    if [ "$WANTS" = true ] && [ "$allow" != true ]; then
      allowed="$(printf '%s' "$platforms" | jq -r --arg plat "$PLAT" '
          [.items[] | select(.metadata.name == $plat)
           | .spec.presets[] | select(.allow_gpu_clustering == true) | .name] | sort | join(", ")
        ')"
      if [ "$allow" = unknown ]; then
        echo "preset '$PRESET' was not found on platform '$PLAT', so it cannot be attached to a GPU cluster; presets of '$PLAT' with allow_gpu_clustering=true: $${allowed:-none}" >&2
      else
        echo "preset '$PRESET' has allow_gpu_clustering=false, so it cannot join an InfiniBand fabric or an existing GPU cluster; presets of '$PLAT' with allow_gpu_clustering=true: $${allowed:-none}" >&2
      fi
      exit 1
    fi
    jq -n --argjson od "$os_drivers" --arg allow "$allow" '$od + {allow_gpu_clustering: $allow}'
  EOT
  ]

  query = {
    project_id    = var.project_id
    release       = var.release
    platform      = each.value.platform
    preset        = each.value.preset
    wants_cluster = (each.value.infiniband_fabric != null || each.value.gpu_cluster_id != null) ? "true" : "false"
  }
}

# Lists the GPU clusters already present in the project as a flat fabric->id map, so a group that
# names a fabric with reuse_gpu_cluster = true (the default) attaches to an existing cluster and
# leaves a compute.gpucluster.count unit unspent. When several clusters share a fabric the map
# keeps the lowest cluster ID, so the pick is stable across runs. data.external accepts one flat
# object of strings and nothing else, which is exactly what a fabric->id map is. Gated on count:
# a run that names no fabric, or pins every group by gpu_cluster_id, pays no CLI round trip.
# NOTE the same bargain gpu_compat makes: when live, this runs on every plan, apply and destroy,
# and needs the CLI authenticated each time.
data "external" "gpu_clusters" {
  count = anytrue([
    for cfg in values(var.gpu_instance_types) :
    cfg.infiniband_fabric != null && cfg.gpu_cluster_id == null && cfg.reuse_gpu_cluster
  ]) ? 1 : 0

  program = ["bash", "-c", <<-EOT
    set -euo pipefail
    for tool in jq nebius; do
      command -v "$tool" >/dev/null || { echo "$tool is required to look up reusable GPU clusters; install it, or set reuse_gpu_cluster = false on every fabric group to skip the lookup" >&2; exit 1; }
    done
    eval "$(jq -r '@sh "PROJECT=\(.project_id) STATE_PATH=\(.state_path)"')"
    # Exclude GPU clusters already managed in this module's Terraform state (local terraform.tfstate
    # by default, or `terraform state pull` when a non-default backend is configured) so a second
    # plan/apply never treats the module's own created cluster as an external reusable one.
    OWN_IDS="[]"
    if [ -s "$STATE_PATH" ]; then
      OWN_IDS="$(jq -c '[.resources[]? | select(.type == "nebius_compute_v1_gpu_cluster" and .name == "this") | .instances[]?.attributes.id // empty]' "$STATE_PATH")"
    elif command -v terraform >/dev/null; then
      OWN_IDS="$(terraform -chdir="$(dirname "$STATE_PATH")" state pull 2>/dev/null | jq -c '[.resources[]? | select(.type == "nebius_compute_v1_gpu_cluster" and .name == "this") | .instances[]?.attributes.id // empty]' 2>/dev/null || echo '[]')"
    fi
    nebius compute gpu-cluster list --parent-id "$PROJECT" --format json \
    | jq -e --argjson own_ids "$OWN_IDS" '
        [ (.items // [])[]
          | select((.spec.infiniband_fabric // "") != "")
          | select((.metadata.id as $id | $own_ids | index($id)) | not)
          | { key: .spec.infiniband_fabric, value: .metadata.id } ]
        | sort_by(.value)
        # First assignment per fabric wins -- the map keeps the lowest cluster ID, which is what
        # makes the pick stable across runs; setpath alone would let the highest overwrite it.
        | reduce .[] as $e ({}; if has($e.key) then . else setpath([$e.key]; $e.value) end)
      '
  EOT
  ]

  query = {
    project_id = var.project_id
    state_path = "${path.root}/terraform.tfstate"
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

# One GPU cluster per node group that named a fabric AND will not reuse one that already exists --
# an explicit gpu_cluster_id, or reuse_gpu_cluster = true (the default) finding a cluster already on
# that fabric in the project, both attach without spending a compute.gpucluster.count unit. The
# cluster resource is the handle that binds nodes to a physical InfiniBand fabric, and joining one
# is what puts RDMA devices on the node -- without it the accelerators are there but the
# interconnect is not. Every node in a cluster shares its fabric, so a group that wants a different
# one gets its own cluster rather than a shared one.
#
# The resource itself allocates no hardware -- the nodes attached to it do -- but it is quota'd
# per region by compute.gpucluster.count, which counts clusters rather than nodes. A fabric name
# the region does not have is rejected outright ("no infiniband fabric found with name"), so a
# typo fails the apply rather than producing a group without RDMA.
resource "nebius_compute_v1_gpu_cluster" "this" {
  for_each = {
    for name, cfg in var.gpu_instance_types : name => cfg
    if cfg.infiniband_fabric != null &&
    !(cfg.reuse_gpu_cluster && lookup(local.reusable_gpu_clusters, cfg.infiniband_fabric, null) != null)
  }

  parent_id         = var.project_id
  name              = "${local.cluster_name}-${each.key}"
  infiniband_fabric = each.value.infiniband_fabric
}

resource "nebius_mk8s_v1_node_group" "this" {
  for_each  = local.node_groups
  parent_id = nebius_mk8s_v1_cluster.this.id
  name      = each.key

  fixed_node_count = each.value.node_count

  template = {
    resources = {
      platform = each.value.instance_type.platform
      preset   = each.value.instance_type.preset
    }

    os = each.value.os

    boot_disk = {
      type           = var.node_boot_disk_type
      size_gibibytes = coalesce(each.value.boot_disk_size_gb, var.node_boot_disk_size_gb)
    }

    network_interfaces = [{
      subnet_id = nebius_vpc_v1_subnet.this.id
      # An empty object is how the API asks for a public IPv4; null leaves the node private-only.
      # This buys INBOUND reach (SSH), not egress: the network's default route table carries a
      # 0.0.0.0/0 route through Nebius' default egress gateway, which NATs a node that has no
      # public address behind a dynamic one from a region-wide pool — so a private-only node still
      # joins the cluster and pulls images. Each address is charged against the
      # vpc.ipv4-address.public.count quota, which counts allocations rather than running instances
      # and is charged per node, so a group nothing logs in to must not take one. That quota's
      # allowance varies by region and is readable only on the tenant (see README).
      public_ip_address = each.value.public_ip ? {} : null
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
      each.value.gpu != null ? local.gpu_node_prep[each.key] : "",
    ]))

    # An empty object is how the API asks for preemptible nodes; null keeps the group on-demand.
    # Preemptible capacity is cheaper, and the platform may reclaim a node at any time. Nebius now
    # requires a spot pricing model on preemptible templates: without follows_spot_price the mk8s
    # API rejects the generated NebiusMachineTemplate with "spec.pricing_model is invalid".
    preemptible        = each.value.preemptible ? {} : null
    follows_spot_price = each.value.preemptible ? {} : null

    gpu_settings = each.value.gpu != null ? {
      drivers_preset = each.value.gpu.drivers_preset
    } : null

    # Attaching the group to its fabric. The API accepts this only at creation, and only for a
    # preset whose allow_gpu_clustering is true -- a single-card preset carves up a host whose
    # interconnect stays with the host, so it cannot be attached however the fabric is named
    # (checked at plan in data.external.gpu_compat). The ID is whatever local.gpu_cluster_ids
    # resolved to: the cluster created above, an existing project cluster reused by fabric, or the
    # explicit gpu_cluster_id the group named.
    gpu_cluster = each.value.gpu_cluster_id != null ? {
      id = each.value.gpu_cluster_id
    } : null
  }

  # Two different failures raise the NebiusGPUError node condition, and the default auto-repair
  # rule for it cordons the node and then shuts it down — so either one kills the very node the
  # test runs on. (a) A hardware partitioning mode (MIG) fails the provider's own GPU health
  # check: its NVLink topology probe cannot read a partitioned card. (b) Non-MIG presets such as
  # gpu-l40s-d / 2gpu-64vcpu-384gb trip node-problem-detector-gpu itself: it hands its
  # health-checker a platform name the checker does not know (Invalid platform "2xL40") and exits
  # 1, so the detector raises NebiusGPUError=True (reason: GPUHealthCheckFailedMK8S) on every
  # node of the group ~30 seconds after boot. Either way MK8s auto-repair (nebiusgpuerror_true)
  # cordons and stops healthy nodes in a loop, leaving the group stuck in PROVISIONING, unless
  # the rule below disables it. Turning the rule off leaves the condition reported and visible,
  # but no longer node-fatal. Every other auto-repair rule (kernel deadlock, disk IO, container
  # runtime) keeps its default.
  #
  # status = "TRUE" is what makes the disable match the nebiusgpuerror_true trigger — without it
  # the API stores CONDITION_STATUS_UNSPECIFIED, which matches nothing, and the rule stays live.
  auto_repair = each.value.gpu != null ? {
    conditions = [{
      type     = "NebiusGPUError"
      status   = "TRUE"
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
    # Tracked so flipping the flag re-runs get-credentials, instead of taking effect
    # only the next time the cluster itself changes.
    switch_kube_context = var.switch_kube_context
  }

  provisioner "local-exec" {
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      set -euo pipefail
      # Read before the write: get-credentials has no flag to keep the current context
      # (only --context-name, which names the one it adds), so keeping it means putting
      # this one back afterwards. Empty when there is no kubeconfig yet.
      previous="$(kubectl config current-context 2>/dev/null || true)"
      nebius mk8s cluster get-credentials --id ${self.triggers.id} --external --force --context-name ${self.triggers.context}
      if [ '${var.switch_kube_context}' = 'false' ] && [ -n "$previous" ]; then
        kubectl config use-context "$previous" >/dev/null
        echo "current context left at $previous"
      fi
    EOT
  }

  # Delete the context AND its cluster/user entries by their real names, so destroy leaves
  # no orphaned cluster/user piling up in ~/.kube/config across runs. Deleting the context does
  # NOT clear current-context when it names that context -- kubectl leaves the field pointing at
  # a context that no longer exists -- and that dangling value is exactly what makes the next
  # apply fail under switch_kube_context = false (it reads the current context to restore it).
  # So clear it too, which also restores the "no current context" state a fresh kubeconfig had.
  provisioner "local-exec" {
    when        = destroy
    on_failure  = continue
    interpreter = ["/bin/bash", "-c"]
    command     = <<-EOT
      kubectl config delete-context '${self.triggers.context}' || true
      kubectl config delete-cluster '${self.triggers.kubeconfig_entry}' || true
      kubectl config unset 'users.${self.triggers.kubeconfig_entry}' || true
      if [ "$(kubectl config current-context 2>/dev/null || true)" = '${self.triggers.context}' ]; then
        kubectl config unset current-context >/dev/null || true
      fi
    EOT
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
