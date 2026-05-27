variable "region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "172.31.0.0/16"
}

variable "eks_name_prefix" {
  description = "Prefix for the EKS cluster name"
  type        = string
  default     = "gpustack-eks"
}

variable "eks_version" {
  description = "EKS version"
  type        = string
  default     = "1.34"
}

variable "eks_cpu_instance_types" {
  description = "Instance types for EKS CPU node group list, check with https://aws.amazon.com/ec2/pricing/on-demand/"
  type        = list(string)
  default     = ["c6a.4xlarge", "c7a.4xlarge"]
}

variable "eks_gpu_instance_types" {
  description = "Instance types for EKS GPU node group list, check with https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html and https://aws.amazon.com/ec2/pricing/on-demand/"
  type        = list(list(string))
  default     = [["g4dn.xlarge", "g4dn.12xlarge"], ["g5.xlarge", "g5.12xlarge"]]
  # default     = [["g4dn.xlarge", "g4dn.12xlarge"], ["g5.xlarge", "g5.12xlarge"], ["g6.xlarge", "g6.12xlarge"]]
}

variable "image" {
  description = "Container image to deploy for testing"
  type        = string
  default     = ""
}
