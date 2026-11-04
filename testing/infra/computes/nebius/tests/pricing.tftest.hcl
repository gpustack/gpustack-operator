mock_provider "nebius" {
  mock_data "nebius_billing_v1_pricing_policy" {
    defaults = {
      parent_id             = "project-e00bnr403eb6dxpbkt"
      compute_instance_spec = { v1 = { platform = "gpu-h100-sxm" } }
    }
  }
}
mock_provider "external" {}
mock_provider "null" {}
mock_provider "random" {}

variables {
  project_id     = "project-e00bnr403eb6dxpbkt"
  ssh_public_key = "terraform.tf"
  price_instance = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test", preemptible = true }
  history_group = {
    platform                  = "gpu-h100-sxm"
    preset                    = "1gpu-16vcpu-200gb"
    preemptible               = true
    infiniband_fabric         = null
    gpu_cluster_id            = null
    reuse_gpu_cluster         = true
    fabric_selection_revision = ""
    spot_price_from_history   = true
    spot_price_ceiling        = "4.000"
  }
}

run "legacy_on_demand" {
  command = plan
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test" }
  }
  assert {
    condition     = nebius_compute_v1_instance.this.preemptible == null && length(nebius_billing_v1_pricing_policy.this) == 0
    error_message = "The default instance must remain on-demand without a policy."
  }
}

run "follows_market" {
  command = plan
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test", preemptible = true }
  }
  assert {
    condition     = nebius_compute_v1_instance.this.follows_spot_price != null && length(nebius_billing_v1_pricing_policy.this) == 0
    error_message = "Preemptible instances without a policy must follow the market."
  }
}

run "explicit_price" {
  command = plan
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test", preemptible = true, spot_max_price = "1.234" }
    public_ip     = false
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "1.234" && nebius_compute_v1_instance.this.follows_spot_price == null && nebius_compute_v1_instance.this.network_interfaces[0].public_ip_address == null
    error_message = "The instance must use the specified policy and omit the public IP."
  }
}

run "decimal_price_normalization" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "4.000" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "4"
    error_message = "The resource price must match the API's decimal representation."
  }
}

run "fractional_price_normalization" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "3.500" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "3.5"
    error_message = "The resource must remove trailing fractional zeros."
  }
}

run "minimum_price_unchanged" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "0.001" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "0.001"
    error_message = "The resource must preserve the minimum positive price."
  }
}

run "integer_price" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = 4 })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "4"
    error_message = "Integer input must retain the existing string conversion."
  }
}

run "leading_zeros_normalization" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "0004.000" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "4"
    error_message = "The resource must remove insignificant leading zeros."
  }
}

run "integer_zeros_preserved" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "10.000" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "10"
    error_message = "The resource must retain significant integer zeros."
  }
}

run "exact_price_preserved" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "${join("", [for i in range(160) : "1"])}.001" })
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "${join("", [for i in range(160) : "1"])}.001"
    error_message = "Normalization must not round any digits of an accepted decimal string."
  }
}

run "history_whole_price_normalization" {
  command = plan
  variables {
    instance_type = merge(var.history_group, { image_family = "test" })
    nebius_resolved = {
      fingerprint = sha256(jsonencode({ project_id = var.project_id, groups = { vm = var.history_group } }))
      prices      = { vm = { max_price = "4.000" } }
    }
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "4"
    error_message = "The resource must normalize a whole-dollar snapshot price."
  }
}

run "history_fractional_price_normalization" {
  command = plan
  variables {
    instance_type = merge(var.history_group, { image_family = "test" })
    nebius_resolved = {
      fingerprint = sha256(jsonencode({ project_id = var.project_id, groups = { vm = var.history_group } }))
      prices      = { vm = { max_price = "3.500" } }
    }
  }
  assert {
    condition     = nebius_billing_v1_pricing_policy.this[0].pricing.max_price_v1.max_price == "3.5"
    error_message = "The resource must normalize a fractional snapshot price without changing its value."
  }
}

run "external_policy" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_pricing_policy_id = "pricingpolicy-e00bnr403eb6dxpbkt000" })
  }
  assert {
    condition     = length(nebius_billing_v1_pricing_policy.this) == 0 && nebius_compute_v1_instance.this.spot_pricing_policy.id == "pricingpolicy-e00bnr403eb6dxpbkt000" && nebius_compute_v1_instance.this.follows_spot_price == null
    error_message = "An external policy must remain attached without creating a local price policy."
  }
}

run "zero_price_rejected" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "0.000" })
  }
  expect_failures = [var.instance_type]
}

run "excess_precision_rejected" {
  command = plan
  variables {
    instance_type = merge(var.price_instance, { spot_max_price = "3.5000" })
  }
  expect_failures = [var.instance_type]
}

run "history_requires_snapshot" {
  command = plan
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test", preemptible = true, spot_price_from_history = true }
  }
  expect_failures = [nebius_billing_v1_pricing_policy.this[0]]
}

run "fabric_requires_snapshot" {
  command = plan
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "8gpu-128vcpu-1600gb", image_family = "test", infiniband_fabric = "auto" }
  }
  expect_failures = [nebius_compute_v1_gpu_cluster.this[0]]
}

run "targeted_policy_requires_snapshot" {
  command = plan
  plan_options {
    target = [nebius_billing_v1_pricing_policy.this[0]]
  }
  variables {
    instance_type = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", image_family = "test", preemptible = true, spot_price_from_history = true }
  }
  expect_failures = [nebius_billing_v1_pricing_policy.this[0]]
}
