provider "aws" {
  region = var.region
}

data "aws_availability_zones" "available" {
  exclude_zone_ids = ["us-east-1e"]
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

resource "random_string" "suffix" {
  length  = 8
  special = false
}

locals {
  eks_name = "${var.eks_name_prefix}-${random_string.suffix.result}"

  azs = slice(data.aws_availability_zones.available.names, 0, 3)
  public_subnets = [
    for i in range(length(local.azs)) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnets = [
    for i in range(3) : cidrsubnet(var.vpc_cidr, 8, i + length(local.azs))
  ]
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.6.1"

  name = local.eks_name
  cidr = var.vpc_cidr

  azs             = local.azs
  private_subnets = local.private_subnets
  public_subnets  = local.public_subnets

  enable_nat_gateway     = true
  single_nat_gateway     = true
  one_nat_gateway_per_az = false

  enable_dns_hostnames = true

  public_subnet_tags = {
    "kubernetes.io/role/elb" = 1
    Environment              = "testing"
    Terraform                = "true"
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = 1
    Environment                       = "testing"
    Terraform                         = "true"
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

resource "aws_key_pair" "accessor" {
  key_name   = "${local.eks_name}-accessor"
  public_key = file("~/.ssh/id_rsa.pub")
}

locals {
  node_groups = merge(
    {
      cpu = {
        # https://docs.aws.amazon.com/eks/latest/APIReference/API_Nodegroup.html#AmazonEKS-Type-Nodegroup-amiType
        ami_type       = "AL2023_x86_64_STANDARD"
        max_size       = 1
        min_size       = 1
        instance_types = var.eks_cpu_instance_types
        disk_size      = 100
        key_name       = aws_key_pair.accessor.key_name
        network_interfaces = [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = {
          xvda = {
            device_name = "/dev/xvda"
            ebs = {
              volume_size           = 100
              volume_type           = "gp3"
              iops                  = 3000
              throughput            = 125
              delete_on_termination = true
            }
          }
        }
      }
    },
    {
      for idx, it in var.eks_gpu_instance_types :
      "gpu-${idx}" => {
        ami_type       = "AL2023_x86_64_NVIDIA"
        max_size       = 1
        min_size       = 0
        instance_types = it
        disk_size      = 100
        key_name       = aws_key_pair.accessor.key_name
        network_interfaces = [
          {
            associate_public_ip_address = true
          }
        ]
        block_device_mappings = {
          xvda = {
            device_name = "/dev/xvda"
            ebs = {
              volume_size           = 100
              volume_type           = "gp3"
              iops                  = 3000
              throughput            = 125
              delete_on_termination = true
            }
          }
        }
      }
    }
  )
}

module "ebs_csi_driver_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts"
  version = "6.6.0"

  name                  = "ebs-csi"
  attach_ebs_csi_policy = true

  oidc_providers = {
    this = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["kube-system:ebs-csi-controller-sa"]
    }
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

module "efs_csi_driver_irsa" {
  source  = "terraform-aws-modules/iam/aws//modules/iam-role-for-service-accounts"
  version = "6.6.0"

  name                  = "efs-csi"
  attach_efs_csi_policy = true

  oidc_providers = {
    this = {
      provider_arn               = module.eks.oidc_provider_arn
      namespace_service_accounts = ["kube-system:efs-csi-controller-sa"]
    }
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "21.17.1"

  name               = local.eks_name
  kubernetes_version = var.eks_version

  addons = {
    cert-manager              = {}
    coredns                   = {}
    eks-node-monitoring-agent = {}
    external-dns              = {}
    kube-proxy                = {}
    metrics-server            = {}
    aws-ebs-csi-driver = {
      service_account_role_arn = module.ebs_csi_driver_irsa.arn
    }
    aws-efs-csi-driver = {
      service_account_role_arn = module.efs_csi_driver_irsa.arn
    }
    aws-ec2-local-instance-store-csi-driver = {}
    eks-pod-identity-agent = {
      before_compute = true
    }
    vpc-cni = {
      before_compute = true
    }
  }

  endpoint_public_access                   = true
  enable_cluster_creator_admin_permissions = true

  compute_config = {
    enabled = false
  }

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.public_subnets

  eks_managed_node_groups = {
    for ng_name, ng_config in local.node_groups :
    ng_name => ng_config
  }

  node_security_group_additional_rules = {
    cluster-to-node-tcp = {
      description                   = "Cluster to node ingress on ephemeral tcp ports"
      protocol                      = "tcp"
      from_port                     = 1025
      to_port                       = 65535
      source_cluster_security_group = true
    }
    cluster-to-node-udp = {
      description                   = "Cluster to node ingress on ephemeral udp ports"
      protocol                      = "udp"
      from_port                     = 1025
      to_port                       = 65535
      source_cluster_security_group = true
    }
    allow-ssh-tcp = {
      description = "Allow ingress access to the nodes on SSH port"
      protocol    = "tcp"
      from_port   = 22
      to_port     = 22
      cidr_blocks = ["0.0.0.0/0"]
    }
    allow-ephemeral-tcp = {
      description = "Allow ingress access to the nodes on ephemeral tcp ports"
      protocol    = "tcp"
      from_port   = 30000
      to_port     = 32767
      cidr_blocks = ["0.0.0.0/0"]
    }
    allow-ephemeral-udp = {
      description = "Allow ingress access to the nodes on ephemeral udp ports"
      protocol    = "udp"
      from_port   = 30000
      to_port     = 32767
      cidr_blocks = ["0.0.0.0/0"]
    }
  }

  tags = {
    Environment = "testing"
    Terraform   = "true"
  }
}

resource "null_resource" "update_kubeconfig" {
  depends_on = [module.eks]

  provisioner "local-exec" {
    command = "aws eks --region ${var.region} update-kubeconfig --name ${module.eks.cluster_name} --alias ${module.eks.cluster_name} --user-alias ${module.eks.cluster_name}"
  }
}

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--region", var.region, "--cluster-name", module.eks.cluster_name]
  }
}

provider "kubectl" {
  apply_retry_count      = 15
  load_config_file       = false
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--region", var.region, "--cluster-name", module.eks.cluster_name]
  }
}

resource "kubectl_manifest" "gpustack_system" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  yaml_body = yamlencode(
    {
      apiVersion = "v1"
      kind       = "Namespace"
      metadata = {
        name = "gpustack-operator-system"
        labels = {
          "app.kubernetes.io/part-of" = "gpustack-operator"
        }
      }
    }
  )
}

data "kubernetes_namespace_v1" "gpustack_system" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  metadata {
    name = yamldecode(kubectl_manifest.gpustack_system[0].yaml_body)["metadata"]["name"]
  }
}

resource "kubernetes_service_account_v1" "gpustack_worker" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  metadata {
    name      = "gpustack-operator-worker"
    namespace = data.kubernetes_namespace_v1.gpustack_system[0].metadata[0].name
    labels = {
      "app.kubernetes.io/part-of" = "gpustack-operator-worker"
    }
  }
}

resource "kubernetes_cluster_role_binding_v1" "gpustack_worker" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  metadata {
    name = "gpustack-operator-worker"
    labels = {
      "app.kubernetes.io/part-of" = "gpustack-operator-worker"
    }
  }
  subject {
    kind      = "ServiceAccount"
    name      = kubernetes_service_account_v1.gpustack_worker[0].metadata[0].name
    namespace = kubernetes_service_account_v1.gpustack_worker[0].metadata[0].namespace
  }
  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "ClusterRole"
    name      = "cluster-admin"
  }
}

resource "kubernetes_service_v1" "gpustack_worker" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  metadata {
    name      = "gpustack-operator-worker"
    namespace = data.kubernetes_namespace_v1.gpustack_system[0].metadata[0].name
    annotations = {
      "prometheus.io/scrape" = "true"
      "prometheus.io/port"   = "443"
      "prometheus.io/path"   = "/metrics"
      "prometheus.io/scheme" = "https"
    }
    labels = {
      "app.kubernetes.io/part-of" = "gpustack-operator-worker"
    }
  }
  spec {
    selector = {
      "app.kubernetes.io/part-of"   = "gpustack-operator"
      "app.kubernetes.io/component" = "worker"
      "app.kubernetes.io/name"      = "gpustack-operator-worker"
    }
    session_affinity = "ClientIP"
    port {
      name        = "https"
      port        = 443
      target_port = "https"
    }
  }
}

resource "kubectl_manifest" "gpustack_worker_cert_issuer" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  yaml_body = yamlencode(
    {
      apiVersion = "cert-manager.io/v1"
      kind       = "Issuer"
      metadata = {
        name      = "gpustack-operator-worker-selfsigned-issuer"
        namespace = data.kubernetes_namespace_v1.gpustack_system[0].metadata[0].name
        labels = {
          "app.kubernetes.io/part-of" = "gpustack-operator-worker"
        }
      }
      spec = {
        selfSigned = {}
      }
    }
  )
}

resource "kubectl_manifest" "gpustack_worker_cert" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  yaml_body = yamlencode(
    {
      apiVersion = "cert-manager.io/v1"
      kind       = "Certificate"
      metadata = {
        name      = "gpustack-operator-worker-cert"
        namespace = data.kubernetes_namespace_v1.gpustack_system[0].metadata[0].name
        labels = {
          "app.kubernetes.io/part-of" = "gpustack-operator-worker"
        }
      }
      spec = {
        secretName = "gpustack-operator-worker-cert"
        issuerRef = {
          name = "gpustack-operator-worker-selfsigned-issuer"
          kind = "Issuer"
        }
        commonName = "gpustack-operator-worker"
        dnsNames = [
          "${kubernetes_service_v1.gpustack_worker[0].metadata[0].name}.${kubernetes_service_v1.gpustack_worker[0].metadata[0].namespace}.svc.cluster.local",
          "${kubernetes_service_v1.gpustack_worker[0].metadata[0].name}.${kubernetes_service_v1.gpustack_worker[0].metadata[0].namespace}.svc",
          "${kubernetes_service_v1.gpustack_worker[0].metadata[0].name}.${kubernetes_service_v1.gpustack_worker[0].metadata[0].namespace}",
          kubernetes_service_v1.gpustack_worker[0].metadata[0].name,
        ]
      }
    }
  )
}

resource "kubernetes_deployment_v1" "gpustack_worker" {
  count      = length(var.image) > 0 ? 1 : 0
  depends_on = [null_resource.update_kubeconfig]

  wait_for_rollout = false

  metadata {
    name      = "gpustack-operator-worker"
    namespace = data.kubernetes_namespace_v1.gpustack_system[0].metadata[0].name
    labels = {
      "app.kubernetes.io/part-of"   = "gpustack-operator"
      "app.kubernetes.io/component" = "worker"
      "app.kubernetes.io/name"      = "gpustack-operator-worker"
    }
  }
  spec {
    replicas = 1
    selector {
      match_labels = {
        "app.kubernetes.io/part-of"   = "gpustack-operator"
        "app.kubernetes.io/component" = "worker"
        "app.kubernetes.io/name"      = "gpustack-operator-worker"
      }
    }
    template {
      metadata {
        labels = {
          "app.kubernetes.io/part-of"   = "gpustack-operator"
          "app.kubernetes.io/component" = "worker"
          "app.kubernetes.io/name"      = "gpustack-operator-worker"
        }
      }
      spec {
        affinity {
          pod_anti_affinity {
            preferred_during_scheduling_ignored_during_execution {
              weight = 100
              pod_affinity_term {
                topology_key = "kubernetes.io/hostname"
                label_selector {
                  match_expressions {
                    key      = "app.kubernetes.io/part-of"
                    operator = "In"
                    values   = ["gpustack-operator"]
                  }
                  match_expressions {
                    key      = "app.kubernetes.io/component"
                    operator = "In"
                    values   = ["worker"]
                  }
                  match_expressions {
                    key      = "app.kubernetes.io/name"
                    operator = "In"
                    values   = ["gpustack-operator-worker"]
                  }
                }
              }
            }
          }
        }
        restart_policy       = "Always"
        service_account_name = kubernetes_service_account_v1.gpustack_worker[0].metadata[0].name
        container {
          name              = "main"
          image             = var.image
          image_pull_policy = "Always"
          args = [
            "gpustack",
            "worker",
            "-v=2",
            "--disable-peer",
            "--cert-dir=/var/run/gpustack/certs",
          ]
          resources {
            limits = {
              cpu    = 4
              memory = "8Gi"
            }
            requests = {
              cpu    = "500m"
              memory = "512Mi"
            }
          }
          env {
            name = "KUBERNETES_POD_IP"
            value_from {
              field_ref {
                field_path = "status.podIP"
              }
            }
          }
          env {
            name = "KUBERNETES_POD_NAME"
            value_from {
              field_ref {
                field_path = "metadata.name"
              }
            }
          }
          env {
            name = "KUBERNETES_POD_NAMESPACE"
            value_from {
              field_ref {
                field_path = "metadata.namespace"
              }
            }
          }
          env {
            name = "KUBERNETES_NODE_NAME"
            value_from {
              field_ref {
                field_path = "spec.nodeName"
              }
            }
          }
          env {
            name  = "KUBERNETES_SERVICE_NAME"
            value = "gpustack-operator-worker"
          }
          port {
            name           = "https"
            container_port = 31443
          }
          startup_probe {
            failure_threshold = 10
            period_seconds    = 5
            http_get {
              scheme = "HTTPS"
              port   = "https"
              path   = "/readyz"
            }
          }
          readiness_probe {
            failure_threshold = 3
            timeout_seconds   = 5
            period_seconds    = 5
            http_get {
              scheme = "HTTPS"
              port   = "https"
              path   = "/readyz"
            }
          }
          liveness_probe {
            failure_threshold = 10
            timeout_seconds   = 5
            period_seconds    = 10
            http_get {
              scheme = "HTTPS"
              port   = "https"
              path   = "/livez"
            }
          }
          volume_mount {
            name       = "gpustack-data-dir"
            mount_path = "/var/lib/gpustack"
          }
          volume_mount {
            name       = "gpustack-cert-dir"
            mount_path = "/var/run/gpustack/certs"
            read_only  = true
          }
        }
        volume {
          name = "gpustack-data-dir"
          empty_dir {}
        }
        volume {
          name = "gpustack-cert-dir"
          secret {
            secret_name = "gpustack-operator-worker-cert"
          }
        }
      }
    }
  }
}
