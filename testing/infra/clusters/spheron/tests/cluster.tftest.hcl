mock_provider "spheron" {
  mock_resource "spheron_instance" {
    defaults = {
      public_ip   = "192.0.2.10"
      ssh_user    = "ubuntu"
      ssh_port    = 22
      status      = "running"
      ssh_command = "ssh ubuntu@192.0.2.10"
    }
  }
}

variables {
  ssh_public_key = "../../computes/spheron/tests/fixture.pub"
  nodes = {
    control = {
      role        = "server"
      request_key = "control-offline-001"
      instance_type = {
        provider         = "data-crunch"
        offer_id         = "cpu-offer"
        gpu_type         = "CPU"
        gpu_count        = 0
        region           = "Finland 1"
        operating_system = "Ubuntu 22.04"
      }
    }
    gpu = {
      role        = "agent"
      request_key = "gpu-offline-001"
      instance_type = {
        provider         = "data-crunch"
        offer_id         = "gpu-offer"
        gpu_type         = "h100"
        gpu_count        = 8
        region           = "Finland 1"
        operating_system = "Ubuntu 22.04"
      }
    }
  }
}

run "stable_node_plan" {
  command = plan
  assert {
    condition     = toset(keys(module.node)) == toset(["control", "gpu"])
    error_message = "Node resource keys must come from configuration, not unknown IP addresses."
  }
}

run "export_k3s" {
  command = apply
  override_resource {
    target = module.node["gpu"].spheron_instance.this
    values = {
      public_ip   = "192.0.2.11"
      ssh_user    = "ubuntu"
      ssh_port    = 22
      status      = "running"
      ssh_command = "ssh ubuntu@192.0.2.11"
    }
  }
  assert {
    condition     = output.k3s_variables.server == ["ubuntu@192.0.2.10"] && output.k3s_variables.agent == ["ubuntu@192.0.2.11"]
    error_message = "Connection inventory must preserve server/agent roles."
  }
  assert {
    condition     = output.k3s_variables.switch_kube_context == false && length(output.k3s_variables.node_internal_ip) == 0
    error_message = "Export must not infer private addresses or switch the active context."
  }
}
