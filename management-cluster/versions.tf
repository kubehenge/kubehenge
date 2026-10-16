terraform {
  # Ephemeral resources and write-only arguments.
  required_version = ">= 1.11"

  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.116"
    }
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.12"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
  }
}

locals {
  versions = merge(local.tested_versions, var.version_overrides)

  # The tested set. Raising one of these upgrades that component on the next apply.
  tested_versions = {
    talos           = "v1.14.4"
    kubernetes      = "v1.37.1"
    cert_manager    = "v1.21.2"
    capi_operator   = "0.29.0"
    cluster_api     = "v1.14.3"
    capmox          = "v0.9.1"
    rke2            = "v0.26.0"
    k3s             = "v0.4.0"
    ipam_in_cluster = "v1.1.1"
    helm_addon      = "v0.6.4"
  }

  # capmox up to v0.9.1 refuses the changed machine template of a kubeadm cluster's upgrade (#735);
  # the fix (#832) is on its main branch but in no release yet. Until one carries it, capmox v0.9.1
  # runs as built from that commit, on v0.9.1's own manifests: main has changed no CRD, RBAC or
  # webhook since. hack/capmox-image.sh builds it reproducibly, and a release publishes it only when
  # it comes out with this digest. It applies only while capmox is at `release`.
  capmox_build = {
    release = "v0.9.1"
    commit  = "edd7461c2a2f335cd4fc1839171671681f4216c6"
    # capmox's own image is built with Go 1.26 on distroless static.
    go         = "go1.26.9"
    base_image = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
    image      = "ghcr.io/kubehenge/cluster-api-provider-proxmox@sha256:07d0062f41980afce99c9e5adec5472d60340de42c798124542d8bffaa8ebdbe"
  }
}
