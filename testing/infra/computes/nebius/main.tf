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
    eval "$(jq -r '@sh "PROJECT=\(.project_id) PLAT=\(.platform) PRESET=\(.preset)"')"

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
    project_id = var.project_id
    platform   = var.instance_type.platform
    preset     = var.instance_type.preset
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

  lifecycle {
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

