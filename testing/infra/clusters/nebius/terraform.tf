terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "0.6.30"
    }
    external = {
      source  = "hashicorp/external"
      version = "2.3.4"
    }
    null = {
      source  = "hashicorp/null"
      version = "3.3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.9.0"
    }
  }
}
