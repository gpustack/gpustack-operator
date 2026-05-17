provider "kubectl" {
  config_path = "~/.kube/config"
}

resource "kubectl_manifest" "gpustack_testing_system" {
  yaml_body = yamlencode(
    {
      apiVersion = "v1"
      kind       = "Namespace"
      metadata = {
        name = "gpustack-testing-system"
      }
    }
  )
}

provider "kubernetes" {
  config_path = "~/.kube/config"
}

data "kubernetes_namespace_v1" "gpustack_testing_system" {
  metadata {
    name = yamldecode(kubectl_manifest.gpustack_testing_system.yaml_body)["metadata"]["name"]
  }
}

provider "helm" {
  kubernetes = {
    config_path = "~/.kube/config"
  }
}

# resource "helm_release" "traefik_ingress" {
#   name       = "traefik"
#   repository = "https://traefik.github.io/charts"
#   chart      = "traefik"
#   version    = "40.2.0"
#
#   atomic          = true
#   cleanup_on_fail = true
#   namespace       = data.kubernetes_namespace_v1.gpustack_testing_system.metadata[0].name
# }

resource "helm_release" "rustfs" {
  name       = "rustfs"
  repository = "https://charts.rustfs.com"
  chart      = "rustfs"
  version    = "0.3.0"

  atomic          = true
  cleanup_on_fail = true
  namespace       = data.kubernetes_namespace_v1.gpustack_testing_system.metadata[0].name

  set = [
    {
      name  = "config.rustfs.log_level"
      value = "warn"
    },
    {
      name  = "config.rustfs.region"
      value = "us-east-1"
    },
    {
      name  = "ingress.enabled"
      value = "false"
    },
    {
      name  = "mode.standalone.enabled"
      value = "true"
    },
    {
      name  = "mode.distributed.enabled"
      value = "false"
    },
    # {
    #   name  = "ingress.className"
    #   value = "traefik"
    # },
    {
      name  = "secret.allowInsecureDefaults"
      value = "true"
    },
    # {
    #   name  = "secret.rustfs.access_key"
    #   value = "rustfsadmin"
    # },
    # {
    #   name  = "secret.rustfs.secret_key"
    #   value = "rustfsadmin"
    # },
    {
      name  = "storageclass.name"
      value = ""
    },
    {
      name  = "storageclass.dataStorageSize"
      value = "100Gi"
    }
  ]
}
