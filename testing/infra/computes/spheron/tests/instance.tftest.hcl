mock_provider "spheron" {}

variables {
  request_key    = "offline-allocation-001"
  ssh_public_key = "tests/fixture.pub"
  instance_type = {
    provider         = "spheron-es"
    offer_id         = "catalog-offer"
    gpu_type         = "h100-sxm5"
    gpu_count        = 8
    region           = "EU North 1"
    operating_system = "Ubuntu 22.04"
    preemptible      = true
  }
}

run "spot_request" {
  command = plan
  variables {
    max_hourly_rate = 20
  }
  assert {
    condition     = jsondecode(spheron_instance.this.request_json).instanceType == "SPOT" && jsondecode(spheron_instance.this.request_json).maxHourlyRate == 20
    error_message = "Spot mode and whole-instance price cap must reach the API request."
  }
  assert {
    condition     = !contains(keys(jsondecode(spheron_instance.this.request_json)), "teamId") && !contains(keys(jsondecode(spheron_instance.this.request_json)), "cloudInit")
    error_message = "Unset optional fields must be omitted."
  }
}

run "reject_bad_key" {
  command = plan
  variables {
    request_key = "bad key"
  }
  expect_failures = [var.request_key]
}

run "startup_optional_fields" {
  command = plan
  variables {
    cloud_init = {
      writeFiles = [{ path = "/etc/motd", content = "test" }]
    }
  }
  assert {
    condition     = jsondecode(spheron_instance.this.request_json).cloudInit == { writeFiles = [{ path = "/etc/motd", content = "test" }] }
    error_message = "Unset startup fields must be omitted instead of sent as null."
  }
}

run "reject_fractional_gpu" {
  command = plan
  variables {
    instance_type = {
      provider         = "spheron-ai"
      offer_id         = "catalog-offer"
      gpu_type         = "h100"
      gpu_count        = 0.5
      region           = "CANADA-1"
      operating_system = "Ubuntu 22.04"
    }
  }
  expect_failures = [var.instance_type]
}

run "cpu_request" {
  command = plan
  variables {
    instance_type = {
      provider         = "data-crunch"
      offer_id         = "cpu-offer"
      gpu_type         = "CPU"
      gpu_count        = 0
      region           = "Finland 1"
      operating_system = "Ubuntu 22.04"
    }
  }
  assert {
    condition     = jsondecode(spheron_instance.this.request_json).gpuCount == 0 && jsondecode(spheron_instance.this.request_json).instanceType == "DEDICATED"
    error_message = "CPU nodes must retain zero GPUs and dedicated mode."
  }
}
