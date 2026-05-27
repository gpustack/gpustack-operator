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

resource "kubernetes_persistent_volume_claim_v1" "nfs_server" {
  metadata {
    name      = "nfsserver-pvc"
    namespace = data.kubernetes_namespace_v1.gpustack_testing_system.metadata[0].name
  }
  spec {
    access_modes = ["ReadWriteMany"]
    resources {
      requests = {
        storage = "100Gi"
      }
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
            value = "/nfs-share"
          }
          env {
            name = "NFS_DOMAIN"
            value = "*"
          }
          env {
            name  = "NFS_OPTION"
            value = "fsid=0,rw,sync,insecure,all_squash,anonuid=65534,anongid=65534,no_subtree_check,nohide"
          }
          volume_mount {
            mount_path = "/nfs-share"
            name       = "nfs-data-dir"
          }
          security_context {
            privileged = true
            capabilities {
              add = [
                "SYS_ADMIN",
                "SETPCAP",
              ]
            }
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
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim_v1.nfs_server.metadata[0].name
          }
        }
      }
    }
  }
}
