mock_provider "nebius" {}
mock_provider "external" {}
mock_provider "null" {}
mock_provider "random" {}

variables {
  project_id     = "project-e00bnr403eb6dxpbkt"
  ssh_public_key = "terraform.tf"
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
