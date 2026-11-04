terraform {
  required_version = ">= 1.9.0"
  required_providers {
    spheron = {
      source  = "gpustack.local/infra/spheron"
      version = "0.1.0"
    }
  }
}
