terraform {
  # Ephemeral resources and write-only arguments.
  required_version = ">= 1.11"

  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.117"
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
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api
    cluster_api = "v1.14.3"
    # renovate: datasource=github-releases depName=ionos-cloud/cluster-api-provider-proxmox
    capmox = "v0.9.1"
    # renovate: datasource=github-releases depName=rancher/cluster-api-provider-rke2
    rke2 = "v0.27.0"
    # renovate: datasource=github-releases depName=k3s-io/cluster-api-k3s
    k3s = "v0.4.0"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    ipam_in_cluster = "v1.1.1"
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-addon-provider-helm
    helm_addon = "v0.6.4"
    # Talos's providers come from Kubehenge's forks of them: upstream's releases are on Cluster
    # API's v1beta1 contract, which Cluster API's core removes, and its control-plane provider has
    # no TalosControlPlaneTemplate, which a ClusterClass's control plane needs. The forks publish
    # pre-releases.
    # renovate: datasource=github-releases depName=kubehenge/cluster-api-bootstrap-provider-talos
    talos_bootstrap = "v0.7.0-alpha.5"
    # renovate: datasource=github-releases depName=kubehenge/cluster-api-control-plane-provider-talos
    talos_control_plane = "v0.6.0-alpha.2"
  }

  # The charts the management cluster runs beneath its providers, with their images, by digest.
  # They are part of the tested set but cannot be held back: nothing outside the management cluster
  # depends on their versions.
  cert_manager = {
    # renovate: datasource=docker depName=quay.io/jetstack/charts/cert-manager
    chart = "v1.21.2@sha256:634dce9c13b56677a2c05e2ab76c312d0be2664022d5dd05815da67e1fd5f610"
    images = {
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-controller
      controller = "v1.21.2@sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-webhook
      webhook = "v1.21.2@sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-cainjector
      cainjector = "v1.21.2@sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-startupapicheck
      startupapicheck = "v1.21.2@sha256:46e75b6866359ffb5d82624f41e3ed1c70b2994982702ced547ce5edb418a8f5"
      # renovate: datasource=docker depName=quay.io/jetstack/cert-manager-acmesolver
      acmesolver = "v1.21.2@sha256:699b40d622211ab7accad8a21b04c5fbaa1841ef7a12621e8de492dbe27b2503"
    }
  }
  capi_operator = {
    # renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api-operator extractVersion=^v(?<version>.+)$
    chart = "0.30.0"
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
    "kubernetes-sigs/cluster-api/releases/download/v1.14.3/metadata.yaml" = "c470906f551ac3e3e9aedc9a3e733b5a5994428693df43e95f65ea67c035f9fe"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.3/core-components.yaml" = "f70be959af4aba061b91cb264b3b1961ae38ca49e2eb542ccf2640146f5221a7"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.3/bootstrap-components.yaml" = "537d6db3b70aad723710825f76073659865dc1a115715ef5d5c470da3b9bcc39"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api
    "kubernetes-sigs/cluster-api/releases/download/v1.14.3/control-plane-components.yaml" = "a211e2e54b6b6fb93e59191f5db8ee76ad6ff92a698f1c62f9d6ce4d5218cf87"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.0/metadata.yaml" = "dc75670ce2d929e00c1b6cf1f2f91e0ad7c0e9b4637df101b2bb884ef8c74e0d"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.0/bootstrap-components.yaml" = "6fe35b58363242e10b73ab144fbda4623a81367a2b1a099c2c53516221f9dddd"
    # renovate: datasource=github-release-attachments depName=rancher/cluster-api-provider-rke2
    "rancher/cluster-api-provider-rke2/releases/download/v0.27.0/control-plane-components.yaml" = "8dd0be498f09bfd4c9fd0a1070874891097b3a162233d3da1e1f5486fe036513"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.4.0/metadata.yaml" = "bb1c0c8356cf3d78a83812769f657c327209153732c189291c4602e5d1f9326d"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.4.0/bootstrap-components.yaml" = "26301df3f6a1b9a14317061bd586a73711d54c956ee8f679982fb3301c6ab71d"
    # renovate: datasource=github-release-attachments depName=k3s-io/cluster-api-k3s
    "k3s-io/cluster-api-k3s/releases/download/v0.4.0/control-plane-components.yaml" = "2e3754bb42be197c31b7567496767f22091f4da74453eb366d29c8bd06e28270"
    # renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
    "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.1/metadata.yaml" = "df2916a79503ac053f6a9866a8de025e780e24d6d472f00f59f84898b5a1c572"
    # renovate: datasource=github-release-attachments depName=ionos-cloud/cluster-api-provider-proxmox
    "ionos-cloud/cluster-api-provider-proxmox/releases/download/v0.9.1/infrastructure-components.yaml" = "424fb38189659147df452490bb2b11f9f4ae3a75c7179b92a85041658bfd74d5"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    "kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/metadata.yaml" = "eb280ab315f478324973a7b65739a655d57e3c22a1356efd9f5127534d3502b7"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-ipam-provider-in-cluster
    "kubernetes-sigs/cluster-api-ipam-provider-in-cluster/releases/download/v1.1.1/ipam-components.yaml" = "8a0656a39d9e85ee07219702254669b7e543d09957e86315f3d20b4bf6aebfa5"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
    "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/metadata.yaml" = "d50a44ee0db2f0368699d15ff011b9b5a61d9969483de1c4f6e4e6046390530f"
    # renovate: datasource=github-release-attachments depName=kubernetes-sigs/cluster-api-addon-provider-helm
    "kubernetes-sigs/cluster-api-addon-provider-helm/releases/download/v0.6.4/addon-components.yaml" = "a70f4eb77eac626231daca1e2a046b4b069bb84320efa327cc8c56a9c4ca03e6"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
    "kubehenge/cluster-api-bootstrap-provider-talos/releases/download/v0.7.0-alpha.5/metadata.yaml" = "d5774698bf60bd884217a24870366979b5c2ba9dc90fb1bf94e5edc13a7fe152"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-bootstrap-provider-talos
    "kubehenge/cluster-api-bootstrap-provider-talos/releases/download/v0.7.0-alpha.5/bootstrap-components.yaml" = "f2d0cecc9416664125b96cfbc1077bc1310e7abfe977ec59824031cb56205206"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
    "kubehenge/cluster-api-control-plane-provider-talos/releases/download/v0.6.0-alpha.2/metadata.yaml" = "12f69f607250e060f7dc0b52cb58aa606c33cab227aa876182baa45475ddf0e1"
    # renovate: datasource=github-release-attachments depName=kubehenge/cluster-api-control-plane-provider-talos
    "kubehenge/cluster-api-control-plane-provider-talos/releases/download/v0.6.0-alpha.2/control-plane-components.yaml" = "de1a213310bc1f23db9740e9035cea56337b86fad5bf411146221a23ff0cc230"
  }

  # Each provider's controller image by digest at its tested version, in place of the tag its
  # manifests name. capmox and the Add-on Provider for Helm run Kubehenge's builds below while
  # those apply. A version held back brings its own in provider_images.
  tested_provider_images = {
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/cluster-api-controller
    "CoreProvider/cluster-api" = "registry.k8s.io/cluster-api/cluster-api-controller:v1.14.3@sha256:1110ddb0f4a9cfc1b557f305199e2d56911fe442edef6361e12e5cc4b11e23fd"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/kubeadm-bootstrap-controller
    "BootstrapProvider/kubeadm" = "registry.k8s.io/cluster-api/kubeadm-bootstrap-controller:v1.14.3@sha256:bc056ff51f8df7c86481745cb86cbc07b7e8cfd4ee3684017f9a98362ab45b8f"
    # renovate: datasource=docker depName=ghcr.io/rancher/cluster-api-provider-rke2-bootstrap
    "BootstrapProvider/rke2" = "ghcr.io/rancher/cluster-api-provider-rke2-bootstrap:v0.27.0@sha256:3d66bf62e3d9b77b94278dcc7f604b459b2f0e6f079e2ad253b7e0d2c7e571a4"
    # renovate: datasource=docker depName=ghcr.io/k3s-io/cluster-api-k3s/bootstrap-controller
    "BootstrapProvider/k3s" = "ghcr.io/k3s-io/cluster-api-k3s/bootstrap-controller:v0.4.0@sha256:aac5968f5664e9c4927f07c9d6b7541d1b20f6be0a69665a63942ba3d73417f1"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api/kubeadm-control-plane-controller
    "ControlPlaneProvider/kubeadm" = "registry.k8s.io/cluster-api/kubeadm-control-plane-controller:v1.14.3@sha256:fbf0b07fd4e4575160d82977f0daeba4792c992a2f56efac111fb76cfe29d0e1"
    # renovate: datasource=docker depName=ghcr.io/rancher/cluster-api-provider-rke2-controlplane
    "ControlPlaneProvider/rke2" = "ghcr.io/rancher/cluster-api-provider-rke2-controlplane:v0.27.0@sha256:0c3ee14cd4502b1e27d736d8093aae79186635f5273cf1f1dba087390eb565d0"
    # renovate: datasource=docker depName=ghcr.io/k3s-io/cluster-api-k3s/controlplane-controller
    "ControlPlaneProvider/k3s" = "ghcr.io/k3s-io/cluster-api-k3s/controlplane-controller:v0.4.0@sha256:4f7305f8871954afa2c2e37a0b19eac3b352e2028bc56617dc78822da5975c20"
    # renovate: datasource=docker depName=ghcr.io/ionos-cloud/cluster-api-provider-proxmox
    "InfrastructureProvider/proxmox" = "ghcr.io/ionos-cloud/cluster-api-provider-proxmox:v0.9.1@sha256:17b01f3e160d930eb5bd2a75853e0aaac70821130f49eef0228483482a36d0f3"
    # renovate: datasource=docker depName=registry.k8s.io/capi-ipam-ic/cluster-api-ipam-in-cluster-controller
    "IPAMProvider/in-cluster" = "registry.k8s.io/capi-ipam-ic/cluster-api-ipam-in-cluster-controller:v1.1.1@sha256:49a4cc0c5d7cd8deef20105e71ce168407ddcaab612f5819947163bd4b66ff0b"
    # renovate: datasource=docker depName=registry.k8s.io/cluster-api-helm/cluster-api-helm-controller
    "AddonProvider/helm" = "registry.k8s.io/cluster-api-helm/cluster-api-helm-controller:v0.6.4@sha256:97d2a86f8e35d23bb4656b26f4564b35f109e47a0dc4860b71089e52eb6e6a24"
    # renovate: datasource=docker depName=ghcr.io/kubehenge/cluster-api-bootstrap-provider-talos
    "BootstrapProvider/talos" = "ghcr.io/kubehenge/cluster-api-bootstrap-provider-talos:v0.7.0-alpha.5@sha256:5563ee245820933a33162280ee46a36d7bb765ef3519b7b6c5da7d6e0aef1cb2"
    # renovate: datasource=docker depName=ghcr.io/kubehenge/cluster-api-control-plane-provider-talos
    "ControlPlaneProvider/talos" = "ghcr.io/kubehenge/cluster-api-control-plane-provider-talos:v0.6.0-alpha.2@sha256:836b3dbc56068c862f7d718545bf818021c802a55c553d3c3399307be790e882"
  }

  # capmox up to v0.9.1 refuses the changed machine template of a kubeadm cluster's upgrade (#735);
  # the fix (#832) is on its main branch but in no release yet. Until one carries it, capmox v0.9.1
  # runs as built from that commit, on v0.9.1's own manifests: main has changed no CRD, RBAC or
  # webhook since. The build also carries scripts/capmox-patches: capmox starts a VM's disk resize
  # without waiting for it, and a reconcile that read the machine before the resize's task reference
  # was written clears that reference, so the bootstrap ISO's attach can time out on the VM's lock,
  # which capmox takes as a VMProvisionFailed it never retries. One patch waits for the resize, the
  # other for any task still running on the VM. A third reads the machine from the API server before
  # cloning: the cached copy can predate the previous reconcile's clone, and cloning from it leaves a
  # second VM behind. scripts/provider-image.sh builds it reproducibly,
  # and a release publishes it only when it comes out with this digest. It applies only while capmox
  # is at `release`.
  capmox_build = {
    upstream = "ionos-cloud/cluster-api-provider-proxmox"
    package  = "./cmd"
    release  = "v0.9.1"
    commit   = "edd7461c2a2f335cd4fc1839171671681f4216c6"
    # capmox's own image is built with Go 1.26 on distroless static.
    go         = "go1.26.9"
    base_image = "gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3"
    image      = "ghcr.io/kubehenge/cluster-api-provider-proxmox@sha256:c055349990da20734783e8da117a50224b2225071fd95528fd1fce4a97e7d863"
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
    image      = "ghcr.io/kubehenge/cluster-api-addon-provider-helm@sha256:14ba4b0c8594352948d0a08ef9746ec4196739cfc821c3d38fbbe0768f8b427e"
  }
}
