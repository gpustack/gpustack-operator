terraform {
  required_providers {
    nebius = {
      source  = "nebius/nebius"
      version = "0.6.30"
    }
    # Pinned rather than inferred by init: this module declares its provider set explicitly, and an
    # unpinned external provider would contradict that. Same version as clusters/nebius, which already
    # uses this pattern.
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
