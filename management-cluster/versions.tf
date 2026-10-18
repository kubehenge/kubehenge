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
    # renovate: datasource=github-releases depName=siderolabs/talos
    talos = "v1.14.4"
    # renovate: datasource=github-releases depName=kubernetes/kubernetes
    kubernetes = "v1.37.1"
    # renovate: datasource=github-releases depName=cert-manager/cert-manager
    cert_manager = "v1.21.2"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-operator extractVersion=^v(?<version>.+)$
    capi_operator = "0.29.0"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api
    cluster_api = "v1.14.3"
    # renovate: datasource=github-releases depName=ionos-cloud/cluster-api-provider-proxmox
    capmox = "v0.9.1"
    # renovate: datasource=github-releases depName=rancher/cluster-api-provider-rke2
    rke2 = "v0.26.0"
    # renovate: datasource=github-releases depName=k3s-io/cluster-api-k3s
    k3s = "v0.4.0"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    ipam_in_cluster = "v1.1.1"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-addon-provider-helm
    helm_addon = "v0.6.4"
  }

  # capmox up to v0.9.1 refuses the changed machine template of a kubeadm cluster's upgrade (#735);
  # the fix (#832) is on its main branch but in no release yet. Until one carries it, capmox v0.9.1
  # runs as built from that commit, on v0.9.1's own manifests: main has changed no CRD, RBAC or
  # webhook since. The build also carries scripts/capmox-patches: capmox starts a VM's disk resize
  # without waiting for it, and a reconcile that read the machine before the resize's task reference
  # was written clears that reference, so the bootstrap ISO's attach can time out on the VM's lock,
  # which capmox takes as a VMProvisionFailed it never retries. One patch waits for the resize, the
  # other for any task still running on the VM. scripts/capmox-image.sh builds it reproducibly,
  # and a release publishes it only when it comes out with this digest. It applies only while capmox
  # is at `release`.
  capmox_build = {
    release = "v0.9.1"
    commit  = "edd7461c2a2f335cd4fc1839171671681f4216c6"
    # capmox's own image is built with Go 1.26 on distroless static.
    go         = "go1.26.9"
    base_image = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
    image      = "ghcr.io/kubehenge/cluster-api-provider-proxmox@sha256:762ce6cf397a023c63dea6c9fa4864b687cc8dea42f7cf84ddb8f4eb38360c5b"
  }
}
