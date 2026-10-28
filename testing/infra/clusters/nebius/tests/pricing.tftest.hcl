mock_provider "nebius" {}
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

run "history_requires_snapshot" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "1gpu-16vcpu-200gb", node_count = 1, preemptible = true, spot_price_from_history = true } }
  }
  expect_failures = [nebius_mk8s_v1_node_group.this["gpu"]]
}

run "fabric_requires_snapshot" {
  command = plan
  variables {
    gpu_instance_types = { gpu = { platform = "gpu-h100-sxm", preset = "8gpu-128vcpu-1600gb", node_count = 1, infiniband_fabric = "auto" } }
  }
  expect_failures = [nebius_mk8s_v1_node_group.this["gpu"]]
}
