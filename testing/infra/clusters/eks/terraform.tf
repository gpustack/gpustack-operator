terraform {
  # This module needs Terraform 1.9 for cross-variable validation. The other cluster modules
  # have no such validation, so they do not need a matching version floor.
  required_version = ">= 1.9.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.54.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.9.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.3.0"
    }
    cloudinit = {
      source  = "hashicorp/cloudinit"
      version = "2.4.0"
    }
    null = {
      source  = "hashicorp/null"
      version = "3.3.0"
    }
  }
}
