mock_provider "nebius" {
  mock_data "nebius_billing_v1_pricing_policy" {
    defaults = {
      parent_id             = "project-e00bnr403eb6dxpbkt"
      compute_instance_spec = { v1 = { platform = "gpu-h100-sxm" } }
    }
  }
}
mock_provider "external" {
  mock_data "external" {
    defaults = { result = { os = "ubuntu24.04", drivers_preset = "test", allow_gpu_clustering = "true" } }
  }
}
mock_provider "null" {}
mock_provider "random" {}

variables {
  project_id         = "project-e00bnr403eb6dxpbkt"
  ssh_public_key     = "terraform.tf"
  cpu_instance_types = {}
  price_cases = {
    whole      = { input = "4.000", expected = "4" }
    fractional = { input = "3.500", expected = "3.5" }
    minimum    = { input = "0.001", expected = "0.001" }
    integer    = { input = 4, expected = "4" }
    leading    = { input = "0004.000", expected = "4" }
    tens       = { input = "10.000", expected = "10" }
    exact      = { input = "${join("", [for i in range(160) : "1"])}.001", expected = "${join("", [for i in range(160) : "1"])}.001" }
  }
}

run "legacy_preemptible" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true } }
  }
  assert {
    condition     = nebius_mk8s_v1_node_group.this["gpu"].template.follows_spot_price != null && length(nebius_billing_v1_pricing_policy.this) == 0
    error_message = "An existing preemptible group must follow the market without a policy."
  }
}

run "explicit_price" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_max_price = "1.234", boot_disk_type = "NETWORK_HDD", boot_disk_size_gb = 200 } }
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this["gpu"].pricing.max_price_v1.max_price == "1.234" && nebius_mk8s_v1_node_group.this["gpu"].template.follows_spot_price == null && nebius_mk8s_v1_node_group.this["gpu"].template.boot_disk.type == "NETWORK_HDD"
    error_message = "The group must use its price policy and disk override."
  }
}

run "decimal_price_normalization" {
  command = plan
  variables {
    gpu_instance_types = { for name, price in var.price_cases : name => { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_max_price = price.input } }
  }
  assert {
    condition     = alltrue([for name, price in var.price_cases : nebius_billing_v1_pricing_policy.this[name].pricing.max_price_v1.max_price == price.expected])
    error_message = "Resource prices must remove insignificant zeros without changing exact limits."
  }
}

run "history_price_normalization" {
  command = plan
  variables {
    gpu_instance_types = { for name, price in var.price_cases : name => { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_price_from_history = true, spot_price_ceiling = tostring(price.input) } }
    nebius_resolved = {
      fingerprint = sha256(jsonencode({
        project_id = var.project_id
        groups = { for name, price in var.price_cases : name => {
          platform                  = "gpu-h100-sxm"
          preset                    = "1gpu-16vcpu-200gb"
          node_count                = 1
          preemptible               = true
          infiniband_fabric         = null
          gpu_cluster_id            = null
          reuse_gpu_cluster         = true
          fabric_selection_revision = ""
          spot_price_from_history   = true
          spot_price_ceiling        = tostring(price.input)
        } }
      }))
      prices = { for name, price in var.price_cases : name => { max_price = tostring(price.input) } }
    }
  }
  assert {
    condition     = alltrue([for name, price in var.price_cases : nebius_billing_v1_pricing_policy.this[name].pricing.max_price_v1.max_price == price.expected])
    error_message = "Snapshot prices must reach the resource normalized without changing the capped value."
  }
}

run "external_policy" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_pricing_policy_id = "pricingpolicy-e00bnr403eb6dxpbkt000" } }
  }
  assert {
    condition     = length(nebius_billing_v1_pricing_policy.this) == 0 && nebius_mk8s_v1_node_group.this["gpu"].template.spot_pricing_policy.id == "pricingpolicy-e00bnr403eb6dxpbkt000" && nebius_mk8s_v1_node_group.this["gpu"].template.follows_spot_price == null
    error_message = "An external policy must remain attached without creating a local price policy."
  }
}

run "zero_price_rejected" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_max_price = "0.000" } }
  }
  expect_failures = [var.gpu_instance_types]
}

run "excess_precision_rejected" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_max_price = "3.5000" } }
  }
  expect_failures = [var.gpu_instance_types]
}

run "history_requires_snapshot" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_price_from_history = true } }
  }
  expect_failures = [nebius_billing_v1_pricing_policy.this["gpu"]]
}

run "fabric_requires_snapshot" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "8gpu-128vcpu-1600gb", node_count = 1, infiniband_fabric = "auto" } }
  }
  expect_failures = [nebius_compute_v1_gpu_cluster.this["gpu"]]
}

run "targeted_policy_requires_snapshot" {
  command = plan
  plan_options {
    target = [nebius_billing_v1_pricing_policy.this["gpu"]]
  }
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_price_from_history = true } }
  }
  expect_failures = [nebius_billing_v1_pricing_policy.this["gpu"]]
}

mock_provider "external" {
  alias = "insufficient_capacity"
  mock_data "external" {
    defaults = {
      result = {
        os                   = "ubuntu24.04"
        drivers_preset       = "test"
        allow_gpu_clustering = "true"
        capacity_ok          = "false"
        capacity_message     = "Insufficient live Nebius preemptible capacity for group 'gpu'"
      }
    }
  }
}

run "insufficient_capacity_fails_at_plan" {
  command = plan
  providers = {
    external = external.insufficient_capacity
  }
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "8gpu-128vcpu-1600gb", node_count = 2, preemptible = true } }
  }
  expect_failures = [
    nebius_vpc_v1_network.this,
  ]
}
