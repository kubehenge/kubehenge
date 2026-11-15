terraform {
  # Ephemeral resources and write-only arguments.
  required_version = ">= 1.11"

  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.122"
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
    talos = "v1.14.6"
    # renovate: datasource=github-releases depName=kubernetes/kubernetes
    kubernetes = "v1.37.2"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api
    cluster_api = "v1.14.4"
    # renovate: datasource=github-releases depName=ionos-cloud/cluster-api-provider-proxmox
    capmox = "v0.9.2"
    # renovate: datasource=github-releases depName=rancher/cluster-api-provider-rke2
    rke2 = "v0.27.2"
    # renovate: datasource=github-releases depName=k3s-io/cluster-api-k3s
    k3s = "v0.5.0"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    ipam_in_cluster = "v1.1.1"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-addon-provider-helm
    helm_addon = "v0.6.4"
    # Talos's providers come from Kubehenge's forks of them: upstream's releases are on Cluster
    # API's v1beta1 contract, which Cluster API's core removes, and its control-plane provider has
    # no TalosControlPlaneTemplate, which a ClusterClass's control plane needs.
    # renovate: datasource=github-releases depName=kubehenge/cluster-api-bootstrap-provider-talos
    talos_bootstrap = "v0.7.0"
    # renovate: datasource=github-releases depName=kubehenge/cluster-api-control-plane-provider-talos
    talos_control_plane = "v0.6.0"
  }

  # The charts the management cluster runs beneath its providers, with their images, by digest.
  # They are part of the tested set but cannot be held back: nothing outside the management cluster
  # depends on their versions.
  cert_manager = {
    # renovate: datasource=docker depName=quay.io/jetstack/charts/cert-manager
    chart = "v1.21.3@sha256:e482675aec32862d323a9218293ff7de81192337998615e4378c56498473f240"
    images = {
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-controller
      controller = "v1.21.3@sha256:9f4f96a33c7774cef24466fcd85031997531a03a8d117f151126baa8d1290dcc"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-webhook
      webhook = "v1.21.3@sha256:055ac8e9de6a0ba6763794838fe38944acc650f68ebe1b01dd8be079e580f458"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-cainjector
      cainjector = "v1.21.3@sha256:083d0e906f8eb44e255fe4ff817ae54e49f9ed82e22f1011bb4e9e4f4b8a641f"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-startupapicheck
      startupapicheck = "v1.21.3@sha256:1c519d4afca1413d1e4ccd78bdc756184418ab55a34569a3b3de39d32f9974e1"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-acmesolver
      acmesolver = "v1.21.3@sha256:c8c1225bb2fe8ab37a330b77f0d8c2ee6a41210fcb54fc07ca606ab9a022b8b7"
    }
  }
  capi_operator = {
    # The chart's release and the SHA-256 of its archive there, which the build fetches beside the
    # module (charts/, `make capi-operator-chart`) and the module checks again before installing:
    # upstream publishes the chart only there and in a Helm repository, whose index Helm checks no
    # archive against.
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-operator
    chart        = "v0.30.0"
    chart_sha256 = "d91da9022021a8c9d48830047314ac5cb73bb38a19f450d5a3186cc2c3b570d5"
    # renovate: datasource=docker depName=registry.k8s.io/capi-operator/cluster-api-operator
    image = "v0.30.0@sha256:2dd577d7bd9cf65deb45b8cd6444f5c89c3de3237ed8e150bd3d34df24fbce10"
  }

  # Where the providers of each component in the tested set are released.
  provider_repositories = {
    cluster_api         = "kubernetes-sigs/cluster-api"
    rke2                = "rancher/cluster-api-provider-rke2"
    k3s                 = "k3s-io/cluster-api-k3s"
    capmox              = "ionos-cloud/cluster-api-provider-proxmox"
    ipam_in_cluster     = "kubernetes-sigs/cluster-api-ipam-provider-in-cluster"
    helm_addon          = "kubernetes-sigs/cluster-api-addon-provider-helm"
    talos_bootstrap     = "kubehenge/cluster-api-bootstrap-provider-talos"
    talos_control_plane = "kubehenge/cluster-api-control-plane-provider-talos"
  }

  # The SHA-256 of each file the operator installs a provider of the tested set from, by its address
  # under https://github.com/. A version held back brings its own in provider_manifests.
  tested_manifests = {
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.4/metadata.yaml" = "c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.4/core-components.yaml" = "0030264eb0a0d90ebacc9da8f6bbb717bee32e45ffadc4a74784fb5ad7b9bb0b"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.4/bootstrap-components.yaml" = "cf805775f24d197581b06e938bcd0eb9a9ed49d675f67e0f689ce7b71e5e397c"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.4/control-plane-components.yaml" = "7fecb41843dcff7e05ab539dbe497c050a9aa7c888d4fb68be12d3c1805be438"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.2/metadata.yaml" = "dc75670ce2d929e00c1b6cf1f2f91e0ad7c0e9b4637df101b2bb884ef8c74e0d"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.2/bootstrap-components.yaml" = "0ba4c73bfe335bfa1b8a277b4df5f35e9a475f47f93038809010a1dc111063a2"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.2/control-plane-components.yaml" = "f47336d0d34e6c96797d633470c7566973ba5b86925791fa68cbf60add607378"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.5.0/metadata.yaml" = "c53705b9d79ab9fc70c95b19cd4104bc07a65305e4c8f752ca42153c84225cb6"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.5.0/bootstrap-components.yaml" = "ed311227fd133dbdfccaad643f0c68c12c06f9bfec75221d57dffcefc8936a26"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.5.0/control-plane-components.yaml" = "9032b67bb1a2642f967c00763ca3706ee0eaea34e855e646f0fec452e838b624"
    # renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
    "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.2/metadata.yaml" = "df2916a79503ac053f6a9866a8de025e780e24d6d472f00f59f84898b5a1c572"
    # renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
    "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.2/infrastructure-components.yaml" = "6817322435b42cfda07a8f94006bacb709afb83445ce24545af253c7904755d8"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    "kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/metadata.yaml" = "eb280ab315f478324973a7b65739a655d57e3c22a1356efd9f5127534d3502b7"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    "kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/ipam-components.yaml" = "8a0656a39d9e85ee07219702254669b7e543d09957e86315f3d20b4bf6aebfa5"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
    "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/metadata.yaml" = "d50a44ee0db2f0368699d15ff011b9b5a61d9969483de1c4f6e4e6046390530f"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
    "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/addon-components.yaml" = "a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
    "kubehenge/cluster-api-bootstrap-provider-talos/releases/download/v0.7.0/metadata.yaml" = "d5774698bf60bd884217a24870366979b5c2ba9dc90fb1bf94e5edc13a7fe152"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
    "kubehenge/cluster-api-bootstrap-provider-talos/releases/download/v0.7.0/bootstrap-components.yaml" = "0a46d57902fa63a9ad47d986f26596bd93c8a82b0ca6af8bc89753f9a65eabd8"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
    "kubehenge/cluster-api-control-plane-provider-talos/releases/download/v0.6.0/metadata.yaml" = "12f69f607250e060f7dc0b52cb58aa606c33cab227aa876182baa45475ddf0e1"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
    "kubehenge/cluster-api-control-plane-provider-talos/releases/download/v0.6.0/control-plane-components.yaml" = "806f88d687dd87862004aee1a557d8a7974df7bdb6bfb28f1f44e7ec7f1a2092"
  }

  # Each provider's controller image by digest at its tested version, in place of the tag its
  # manifests name. capmox and the Add-on Provider for Helm run Kubehenge's builds below while
  # those apply. A version held back brings its own in provider_images.
  tested_provider_images = {
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/cluster-api-controller
    "CoreProvider/cluster-api" = "registry.k8s.io/cluster-api/cluster-api-controller:v1.14.4@sha256:ee7b274a4c237d7831840c1a05a950c383f11a1755d3cca6292103cabb00bd26"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/kubeadm-bootstrap-controller
    "BootstrapProvider/kubeadm" = "registry.k8s.io/cluster-api/kubeadm-bootstrap-controller:v1.14.4@sha256:674dd5e666d209c7bdb72525dd3bb40a670c068bd12d610b596b532a84463e62"
    # renovate: datasource=docker depName=ghcr.io/rancher/cluster-api-provider-rke2-bootstrap
    "BootstrapProvider/rke2" = "ghcr.io/rancher/cluster-api-provider-rke2-bootstrap:v0.27.2@sha256:314b5993c8534608a92e479e886892f4091b7f956373d8c74bff67c549768702"
    # renovate: datasource=docker depName=ghcr.io/k3s-io/cluster-api-k3s/bootstrap-controller
    "BootstrapProvider/k3s" = "ghcr.io/k3s-io/cluster-api-k3s/bootstrap-controller:v0.5.0@sha256:96fb0622640e0eba2f11b5fff919c4bae6829e2225c31c8e34d5a2b81760d94c"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/kubeadm-control-plane-controller
    "ControlPlaneProvider/kubeadm" = "registry.k8s.io/cluster-api/kubeadm-control-plane-controller:v1.14.4@sha256:49f5cc1a0f9b3f909434e1cccf87d5d868e85e5c9ce849e8ae65ff409458befc"
    # renovate: datasource=docker depName=ghcr.io/rancher/cluster-api-provider-rke2-controlplane
    "ControlPlaneProvider/rke2" = "ghcr.io/rancher/cluster-api-provider-rke2-controlplane:v0.27.2@sha256:9745a1aa154d86b78d8b5eb7ea685f1091b5b361c00f65fe02a983cac3898ec2"
    # renovate: datasource=docker depName=ghcr.io/k3s-io/cluster-api-k3s/controlplane-controller
    "ControlPlaneProvider/k3s" = "ghcr.io/k3s-io/cluster-api-k3s/controlplane-controller:v0.5.0@sha256:35200f224612caca2153e6f6bafce25f83440917d0e3b51eb2ad701cdfe13ab6"
    # renovate: datasource=docker depName=ghcr.io/ionos-cloud/cluster-api-provider-proxmox
    "InfrastructureProvider/proxmox" = "ghcr.io/ionos-cloud/cluster-api-provider-proxmox:v0.9.2@sha256:682df819bb9a3b42eb12b784e47a29044555cc4d98178c71e220d97a84183252"
    # renovate: datasource=docker depName=registry.k8s.io/capi-ipam-ic/cluster-api-ipam-in-cluster-controller
    "IPAMProvider/in-cluster" = "registry.k8s.io/capi-ipam-ic/cluster-api-ipam-in-cluster-controller:v1.1.1@sha256:49a4cc0c5d7cd8deef20105e71ce168407ddcaab612f5819947163bd4b66ff0b"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api-helm/cluster-api-helm-controller
    "AddonProvider/helm" = "registry.k8s.io/cluster-api-helm/cluster-api-helm-controller:v0.6.4@sha256:97d2a86f8e35d23bb4656b26f4564b35f109e47a0dc4860b71089e52eb6e6a24"
    # renovate: datasource=docker depName=ghcr.io/kubehenge/cluster-api-bootstrap-provider-talos
    "BootstrapProvider/talos" = "ghcr.io/kubehenge/cluster-api-bootstrap-provider-talos:v0.7.0@sha256:bda9e12fd1050afa0cffac31b53ed37c58b3a3fe23438ff9ab6cdedde41c170d"
    # renovate: datasource=docker depName=ghcr.io/kubehenge/cluster-api-control-plane-provider-talos
    "ControlPlaneProvider/talos" = "ghcr.io/kubehenge/cluster-api-control-plane-provider-talos:v0.6.0@sha256:fdd30ae41424e6f36b30c5aa4ab13f46bf159050b365ed2d0194c053523c7bd2"
  }

  # capmox v0.9.2 lacks five fixes and the VM firewall, which scripts/capmox-patches adds to the
  # release. A failed clone forgets its VM ID. Kept while the ID stays free, the machine keeps
  # looking for a VM that was never made, and never clones again. A clone's VM ID is stored before the clone starts. The
  # spec and the status are written apart, so a spec write that failed after the clone left its task
  # recorded without its VM ID, and the next reconcile cloned again. A stored ID that no VM holds,
  # after a restart or a refused clone, is dropped for a new one. A deleted machine's VM is destroyed
  # only once the VM under its ID is confirmed as the machine's, by SMBIOS UUID once that is
  # recorded as the provider ID and by name before: Proxmox VE hands a freed ID to the next clone,
  # and the delete's next pass would otherwise destroy that clone. A renamed VM is still destroyed.
  # The destroy purges the VM from HA, backup and replication configuration, which Proxmox VE
  # otherwise refuses, and a VM with Proxmox VE's protection flag is left alone, with the machine
  # saying how to clear it. A ProxmoxCluster marked for deletion before its Cluster, as a namespace
  # delete does, is requeued every 10 seconds until the Cluster is deleted. Upstream returns without
  # a requeue, and the Cluster watch never passes a Cluster that gains its deletion timestamp, so
  # the deletion waited for the 10-hour resync. A machine annotated with Proxmox VE security groups
  # gets its VM firewall before the first start: an IPSet of the cluster's own addresses and the
  # ProxmoxCluster's further sources, a rule accepting it, the groups, the firewall flag on every
  # interface and the VM's firewall on. A change of the ProxmoxCluster rewrites the IPSet of every
  # running VM. Upstream has no setting for the interface's firewall flag. The build runs on the
  # release's own manifests.
  # scripts/provider-image.sh builds it reproducibly, and a release publishes it only when it comes out with this digest. It applies
  # only while capmox is at `release`.
  capmox_build = {
    upstream = "ionos-cloud/cluster-api-provider-proxmox"
    package  = "./cmd"
    release  = "v0.9.2"
    commit   = "0691ffd179cef609cad6f26fc3beed82927f30a3"
    # capmox's own image is built with Go 1.26 on distroless static.
    go         = "go1.26.10"
    base_image = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
    image      = "ghcr.io/kubehenge/cluster-api-provider-proxmox@sha256:43fb02325db12d0b8bc8689027eadae89b13f1859ffb26ea7405ec2213715273"
  }

  # The Add-on Provider for Helm creates a cluster's HelmReleaseProxy under a generated name and
  # looks for it in its cache on the next reconcile, which its own status patch starts at once; when
  # the cache lags, that reconcile creates a second one, and from then on every reconcile of the
  # HelmChartProxy fails and its changes never reach the release. scripts/helm-addon-patches waits
  # after the create until the cache has it, and pins each chart by the digest a HelmChartProxy's
  # version carries after an @: an OCI chart is pulled by it, and an HTTP repository's archive is
  # checked against it. Built as capmox's is; it applies only while the provider is at `release`.
  helm_addon_build = {
    upstream = "kubernetes-sigs/cluster-api-addon-provider-helm"
    package  = "."
    release  = "v0.6.4"
    commit   = "825662962a26dc339f3871184c91ed4bd2f83a4f"
    # The provider's own image is built with Go 1.25 on distroless static.
    go         = "go1.25.14"
    base_image = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
    image      = "ghcr.io/kubehenge/cluster-api-addon-provider-helm@sha256:6c78cee77e4be012c3960954fb823688724992858b9d5d876ac3bb374267baef"
  }
}
