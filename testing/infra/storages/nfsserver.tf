resource "kubernetes_service_v1" "nfs_server" {
  metadata {
    name      = "nfsserver-svc"
    namespace = data.kubernetes_namespace_v1.gpustack_testing_system.metadata[0].name
    labels = {
      "app.kubernetes.io/part-of" = "nfs-server"
    }
  }
  spec {
    type = "ClusterIP"
    selector = {
      "app.kubernetes.io/part-of" = "nfs-server"
    }
    port {
      name     = "tcp-2049"
      port     = 2049
      protocol = "TCP"
    }
    port {
      name     = "udp-111"
      port     = 111
      protocol = "UDP"
    }
  }
}

resource "kubernetes_deployment_v1" "nfs_server" {
  metadata {
    name      = "nfsserver"
    namespace = data.kubernetes_namespace_v1.gpustack_testing_system.metadata[0].name
  }

  spec {
    replicas = 1
    selector {
      match_labels = {
        "app.kubernetes.io/part-of"   = "nfs-server"
        "app.kubernetes.io/component" = "server"
        "app.kubernetes.io/name"      = "nfs-server"
      }
    }
    template {
      metadata {
        labels = {
          "app.kubernetes.io/part-of"   = "nfs-server"
          "app.kubernetes.io/component" = "server"
          "app.kubernetes.io/name"      = "nfs-server"
        }
      }
      spec {
        node_selector = {
          "kubernetes.io/os" = "linux"
        }
        container {
          name  = "main"
          image = "gists/nfs-server:2.6.4"

          env {
            name  = "NFS_DIR"
            value = "/share"
          }
          volume_mount {
            mount_path = "/share"
            name       = "nfs-data-dir"
          }
          security_context {
            privileged = true
          }
          port {
            name           = "tcp-2049"
            container_port = 2049
            protocol       = "TCP"
          }
          port {
            name           = "udp-111"
            container_port = 111
            protocol       = "UDP"
          }
        }
        volume {
          name = "nfs-data-dir"
          empty_dir {}
        }
      }
    }
  }
}