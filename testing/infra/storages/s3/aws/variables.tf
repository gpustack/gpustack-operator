variable "region" {
  description = "AWS region"
  type        = string
  default     = "ap-east-1"
}

variable "s3_name_prefix" {
  description = "Prefix for the S3 name"
  type        = string
  default     = "gpustack-s3"
}

