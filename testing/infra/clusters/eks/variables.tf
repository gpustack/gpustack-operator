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
  # Keyed by group name so each GPU node group has a stable key (gpu-<name>).
  # Adding a key is a +create only; editing a key's instance-type list replaces
  # just that group, never rotating the others.
  description = "Instance types per EKS GPU node group, keyed by group name; check with https://docs.aws.amazon.com/dlami/latest/devguide/gpu.html and https://aws.amazon.com/ec2/pricing/on-demand/"
  type        = map(list(string))
  default     = { g4dn = ["g4dn.xlarge", "g4dn.12xlarge"] }
  # default     = { xlarge = ["g4dn.xlarge", "g5.xlarge"], xlarge-alt = ["g5.xlarge", "g6.xlarge"], large = ["g4dn.12xlarge", "g5.12xlarge"] }
  # default     = { small = ["g4dn.xlarge", "g4dn.12xlarge", "g6.xlarge"], large = ["g4dn.12xlarge", "g5.12xlarge", "g6.12xlarge"] }
  # default     = { g4dn = ["g4dn.xlarge", "g4dn.12xlarge"], g5 = ["g5.xlarge", "g5.12xlarge"], g6 = ["g6.xlarge", "g6.12xlarge"] }
}
