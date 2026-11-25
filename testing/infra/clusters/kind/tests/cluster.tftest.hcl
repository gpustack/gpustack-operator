run "default_multi_node_plan" {
  command = plan

  variables {
    host = "frank@192.168.50.17"
  }

  assert {
    condition     = output.context_name == "kind-gpustack-kind-192-168-50-17"
    error_message = "context_name must include cluster_name and sanitized host."
  }

  assert {
    condition     = output.node_image == "kindest/node:v1.34.11"
    error_message = "default node_image must pair kindest/node with default release v1.34.11."
  }

  assert {
    condition     = output.node_count == 4
    error_message = "default node_count must be 1 control-plane + 3 workers."
  }

  assert {
    condition     = strcontains(output.kind_config_yaml, "enable-aggregator-routing") && strcontains(output.kind_config_yaml, "192.168.50.17") && strcontains(output.kind_config_yaml, "topology.gpustack.ai/rack")
    error_message = "rendered Kind config must carry aggregator routing, host TLS SAN, and nested TAS topology labels."
  }
}

run "arbitrary_k8s_version_and_scale" {
  command = plan

  variables {
    host                    = "ubuntu@10.0.0.50"
    cluster_name            = "scale-v135"
    release                 = "v1.35.8"
    workers                 = 8
    topology_zones          = 2
    topology_racks_per_zone = 2
    api_server_port         = 16443
    mirror                  = "cn"
    switch_kube_context     = false
  }

  assert {
    condition     = output.node_image == "kindest/node:v1.35.8"
    error_message = "arbitrary Kubernetes release must resolve to kindest/node:<release>."
  }

  assert {
    condition     = output.node_count == 9
    error_message = "node_count must reflect 1 control-plane + 8 workers."
  }

  assert {
    condition     = yamldecode(output.kind_config_yaml).networking.apiServerPort == 16443 && strcontains(output.kind_config_yaml, "rack-4")
    error_message = "rendered Kind config must wire custom apiServerPort and 4 strictly nested racks across 2 zones."
  }
}

run "node_image_override_and_disabled_topology" {
  command = plan

  variables {
    host            = "10.0.0.51"
    node_image      = "registry.internal/kindest/node:v1.32.11@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    workers         = 2
    topology_region = ""
  }

  assert {
    condition     = output.node_image == "registry.internal/kindest/node:v1.32.11@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    error_message = "explicit node_image must override release and node_image_repository."
  }

  assert {
    condition     = !strcontains(output.kind_config_yaml, "topology.kubernetes.io/region")
    error_message = "empty topology_region must omit topology labels from worker nodes."
  }
}

run "rejects_invalid_release" {
  command = plan

  variables {
    host    = "ubuntu@10.0.0.50"
    release = "1.34.11"
  }

  expect_failures = [
    var.release,
  ]
}
