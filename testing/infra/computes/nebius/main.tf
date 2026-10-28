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
  fabric_requested = var.instance_type.gpu_cluster_id != null || var.instance_type.infiniband_fabric != null
  binding          = try(var.nebius_resolved.bindings.vm, null)
  gpu_cluster_id   = var.instance_type.gpu_cluster_id != null ? var.instance_type.gpu_cluster_id : (var.instance_type.infiniband_fabric != null ? (try(local.binding.cluster_id, null) != null ? local.binding.cluster_id : try(nebius_compute_v1_gpu_cluster.this[0].id, null)) : null)
  effective_price  = var.instance_type.spot_price_from_history ? try(var.nebius_resolved.prices.vm.max_price, "0.001") : var.instance_type.spot_max_price
  vm_name          = "${var.name_prefix}-${random_string.suffix.result}"

  # Nebius returns network interface addresses as a CIDR (e.g. "1.2.3.4/32"); strip the suffix.
  public_ip  = var.public_ip ? try(split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].public_ip_address.address)[0], null) : null
  private_ip = try(split("/", nebius_compute_v1_instance.this.status.network_interfaces[0].ip_address.address)[0], null)
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

# Resolve image_family from the platform against Nebius' live public-image catalogue, and validate the
# platform and preset against the region's platform list in the same call -- one script, one answer, no
# partial results. Gated on count: an explicitly given image_family skips the CLI entirely, so plan,
# apply AND destroy need no `nebius` binary at all.
#
# NOTE the bargain a data source makes: unpinned, this runs on every plan, every apply and every
# destroy, and it needs the CLI authenticated each time. See the README.
data "external" "image" {
  count = var.instance_type.image_family == null ? 1 : 0

  program = ["bash", "-c", <<-EOT
    set -euo pipefail
    # Both tools named before either is used: a command substitution inside eval reports a missing jq
    # as "PROJECT: unbound variable", which names neither the tool nor this data source.
    for tool in jq nebius; do
      command -v "$tool" >/dev/null || { echo "$tool is required to resolve instance_type.image_family; install it, or pin image_family to skip the lookup" >&2; exit 1; }
    done
    eval "$(jq -r '@sh "PROJECT=\(.project_id) PLAT=\(.platform) PRESET=\(.preset) WANTS=\(.wants_cluster)"')"

    region="$(nebius iam project get --id "$PROJECT" --format json | jq -er '.spec.region')"

    # The region's platforms, and this platform's presets. Checked in the same call that resolves the
    # family, so a typo in either fails at PLAN with the valid values listed, rather than at apply with
    # a provider error that names none of them.
    platforms="$(nebius compute platform list --parent-id "$PROJECT" --format json)"
    if ! presets="$(printf '%s' "$platforms" | jq -er --arg p "$PLAT" '.items[] | select(.metadata.name == $p) | [.spec.presets[].name] | join(", ")')"; then
      echo "platform '$PLAT' is not available in region $region; available: $(printf '%s' "$platforms" | jq -r '[.items[].metadata.name] | sort | join(", ")')" >&2
      exit 1
    fi
    # Exact membership, asked of the JSON array rather than of the joined string: $PRESET expanded
    # into a shell `case` pattern is a glob, so a value of "*" would match every platform's list.
    if ! printf '%s' "$platforms" | jq -e --arg p "$PLAT" --arg preset "$PRESET" \
      '.items[] | select(.metadata.name == $p) | [.spec.presets[].name] | index($preset) != null' >/dev/null; then
      echo "preset '$PRESET' is not offered by platform '$PLAT'; available: $presets" >&2
      exit 1
    fi

    # A preset whose allow_gpu_clustering is false cannot join a GPU cluster at all: an
    # instance_type.gpu_cluster_id against one fails here, at plan, naming the presets that can,
    # instead of at apply with a provider error that names none of them.
    if [ "$WANTS" = true ]; then
      allow="$(printf '%s' "$platforms" | jq -r --arg plat "$PLAT" --arg preset "$PRESET" '
          [.items[] | select(.metadata.name == $plat) | .spec.presets[] | select(.name == $preset)]
          | if length == 0 then "unknown" else (.[0].allow_gpu_clustering // false | tostring) end
        ')"
      if [ "$allow" != true ]; then
        allowed="$(printf '%s' "$platforms" | jq -r --arg plat "$PLAT" '
            [.items[] | select(.metadata.name == $plat)
             | .spec.presets[] | select(.allow_gpu_clustering == true) | .name] | sort | join(", ")
          ')"
        if [ "$allow" = unknown ]; then
          echo "preset '$PRESET' was not found on platform '$PLAT', so it cannot be attached to a GPU cluster; presets of '$PLAT' with allow_gpu_clustering=true: $${allowed:-none}" >&2
        else
          echo "preset '$PRESET' has allow_gpu_clustering=false, so it cannot join an InfiniBand GPU cluster; presets of '$PLAT' with allow_gpu_clustering=true: $${allowed:-none}" >&2
        fi
        exit 1
      fi
    fi

    # The family whose recommended_platforms carries this platform; newest by created_at when more than
    # one claims it. Every value is a string and stdout carries nothing else: data.external accepts one
    # JSON object of strings and nothing more, so the choice is surfaced as an OUTPUT rather than
    # printed here, and every diagnostic above goes to stderr (which Terraform shows on failure).
    images="$(nebius compute image list-public --region "$region" --format json)"
    if ! printf '%s' "$images" | jq -e --arg p "$PLAT" --arg region "$region" '
          [.items[] | select((.spec.recommended_platforms // []) | index($p))]
          | sort_by(.metadata.created_at) | last
          # `last` of an empty array is null, and an object built from null carries null fields and
          # is still truthy, so jq -e would report success and hand the provider a schema error
          # instead of the diagnostic below. Dropped here, jq -e produces no output and exits 4.
          | select(. != null)
          | {
              image_family:        .spec.image_family,
              cpu_architecture:    .spec.cpu_architecture,
              min_disk_size_bytes: (.status.min_disk_size_bytes | tostring),
              region:              $region
            }
        '; then
      echo "no public image in $region recommends platform '$PLAT'; families that recommend any platform: $(printf '%s' "$images" | jq -r '[.items[] | select((.spec.recommended_platforms // []) | length > 0) | .spec.image_family] | unique | join(", ")')" >&2
      exit 1
    fi
  EOT
  ]

  query = {
    project_id    = var.project_id
    platform      = var.instance_type.platform
    preset        = var.instance_type.preset
    wants_cluster = var.instance_type.gpu_cluster_id != null ? "true" : "false"
  }
}

locals {
  # Lazily, NOT with coalesce(): coalesce evaluates every argument, and indexing a zero-count data
  # source is an error -- so the pinned path, the one that needs no CLI at all, would fail at plan.
  resolved = one(data.external.image[*].result)

  image_family = var.instance_type.image_family != null ? var.instance_type.image_family : local.resolved.image_family

  # Bytes to GiB, rounded UP: a floor would under-check by up to 1 GiB and let a disk one byte too
  # small through. Null on the pinned path, where there is no minimum to check against.
  image_min_disk_gib = local.resolved == null ? null : ceil(tonumber(local.resolved.min_disk_size_bytes) / 1073741824)
}

resource "nebius_compute_v1_instance" "this" {
  parent_id = var.project_id
  name      = local.vm_name

  preemptible         = var.instance_type.preemptible ? { on_preemption = "STOP" } : null
  recovery_policy     = var.instance_type.preemptible ? "FAIL" : null
  follows_spot_price  = var.instance_type.preemptible && local.effective_price == null && var.instance_type.spot_pricing_policy_id == null ? {} : null
  spot_pricing_policy = var.instance_type.spot_pricing_policy_id != null ? { id = data.nebius_billing_v1_pricing_policy.external[0].id } : (local.effective_price != null ? { id = nebius_billing_v1_pricing_policy.this[0].id } : null)

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
          image_family = local.image_family
        }
      }
    }
  }

  # The binding comes from the explicit ID or the GPU cluster selected during preparation.
  # Nebius only permits attaching the VM at creation.
  gpu_cluster = local.gpu_cluster_id != null ? {
    id = local.gpu_cluster_id
  } : null

  lifecycle {
    replace_triggered_by = [null_resource.price]
    precondition {
      condition     = (!var.instance_type.spot_price_from_history && !local.fabric_requested) || (local.snapshot_valid && (!var.instance_type.spot_price_from_history || can(var.nebius_resolved.prices.vm.max_price)) && (!local.fabric_requested || can(var.nebius_resolved.bindings.vm.fabric)))
      error_message = "Run terraform apply -target=null_resource.prepare with the same variables before planning history pricing or fabric selection."
    }
    precondition {
      condition     = var.instance_type.spot_pricing_policy_id == null ? true : (data.nebius_billing_v1_pricing_policy.external[0].parent_id == var.project_id && data.nebius_billing_v1_pricing_policy.external[0].compute_instance_spec.v1.platform == var.instance_type.platform)
      error_message = "The pricing policy must belong to this project and platform."
    }
    # The image publishes its own minimum (10 GiB driverless, 40 GiB CUDA) and the provider only
    # reports it as an apply-time error. Short-circuited with || so the pinned path -- where there is
    # nothing to compare against -- is not an error in itself.
    precondition {
      condition     = local.image_min_disk_gib == null || var.boot_disk_size_gb >= local.image_min_disk_gib
      error_message = "boot_disk_size_gb (${var.boot_disk_size_gb} GiB) is below the minimum the resolved image requires."
    }
  }

  network_interfaces = [{
    name              = "eth0"
    subnet_id         = nebius_vpc_v1_subnet.this.id
    ip_address        = {}
    public_ip_address = var.public_ip ? {} : null
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


resource "null_resource" "price" {
  triggers = {
    max_price   = local.effective_price
    preemptible = var.instance_type.preemptible
    policy_id   = var.instance_type.spot_pricing_policy_id
    platform    = local.effective_price == null ? null : var.instance_type.platform
  }
}

resource "nebius_billing_v1_pricing_policy" "this" {
  count                 = local.effective_price != null ? 1 : 0
  parent_id             = var.project_id
  name                  = "${local.vm_name}-spot"
  compute_instance_spec = { v1 = { platform = var.instance_type.platform } }
  pricing               = { max_price_v1 = { max_price = local.effective_price } }
  lifecycle {
    replace_triggered_by = [null_resource.price]
    precondition {
      condition     = !var.instance_type.spot_price_from_history || (local.snapshot_valid && try(var.nebius_resolved.prices.vm.max_price != null && var.nebius_resolved.prices.vm.max_price != "", false))
      error_message = "Run terraform apply -target=null_resource.prepare with the same variables before planning history pricing."
    }
  }
}

data "nebius_billing_v1_pricing_policy" "external" {
  count = var.instance_type.spot_pricing_policy_id != null ? 1 : 0
  id    = var.instance_type.spot_pricing_policy_id
}

resource "nebius_compute_v1_gpu_cluster" "this" {
  count             = var.instance_type.infiniband_fabric != null && try(var.nebius_resolved.bindings.vm.cluster_id, null) == null ? 1 : 0
  parent_id         = var.project_id
  name              = "${local.vm_name}-gpu"
  infiniband_fabric = try(var.nebius_resolved.bindings.vm.fabric, null)
  lifecycle {
    precondition {
      condition     = local.snapshot_valid && try(var.nebius_resolved.bindings.vm.fabric != null && var.nebius_resolved.bindings.vm.fabric != "", false)
      error_message = "Run terraform apply -target=null_resource.prepare with the same variables before planning fabric selection."
    }
  }
}
