terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.42.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "3.8.1"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.2.1"
    }
    null = {
      source  = "hashicorp/null"
      version = "3.2.4"
    }
  }
}