provider "aws" {
  region = var.region
}

resource "random_string" "suffix" {
  length  = 8
  special = false
}

locals {
  s3_name = "${var.s3_name_prefix}-${random_string.suffix.result}"
}

module "s3_bucket" {
  source = "terraform-aws-modules/s3-bucket/aws"

  bucket = lower(local.s3_name)
  region = var.region

  force_destroy = true

  control_object_ownership = true
  object_ownership         = "ObjectWriter"
}
