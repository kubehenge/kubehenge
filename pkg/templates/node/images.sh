#!/bin/sh
# Pulls the control-plane images of a Kubernetes version into a kubeadm template as it is baked,
# keeping each only once Kubernetes' release signature for the digest pulled checks out, and has
# containerd run kubeadm's pause image rather than pulling its own by tag on a node's first pod:
#
#   kubehenge-images <Kubernetes version>
set -eu

version=$1
# renovate: datasource=github-release-attachments depName=sigstore/cosign
COSIGN=v3.1.4
COSIGN_SHA256=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa

work=$(mktemp -d -p /var/tmp)
trap 'rm -rf "${work}"' EXIT
curl --fail --silent --show-error --location --proto '=https' --retry 5 --retry-all-errors \
    --output "${work}/cosign" "https://github.com/sigstore/cosign/releases/download/${COSIGN}/cosign-linux-amd64"
echo "${COSIGN_SHA256}  ${work}/cosign" | sha256sum --check --strict --quiet
chmod 0755 "${work}/cosign"

images=$(kubeadm config images list --kubernetes-version "${version}")
for image in ${images}; do
    crictl pull "${image}" > /dev/null
    pulled=$(crictl inspecti --output go-template --template '{{range .status.repoDigests}}{{println .}}{{end}}' "${image}" | grep -m 1 -F "${image%:*}@sha256:")
    HOME="${work}" "${work}/cosign" verify --certificate-identity krel-trust@k8s-releng-prod.iam.gserviceaccount.com \
        --certificate-oidc-issuer https://accounts.google.com "${pulled}" > /dev/null || {
        crictl rmi "${image}" > /dev/null
        echo "kubehenge-images: ${pulled} has no signature of Kubernetes' release" >&2
        exit 1
    }
done

pause=$(printf '%s\n' "${images}" | grep -m 1 /pause:)
sed -i "s#registry.k8s.io/pause:[^\"']*#${pause}#" /etc/containerd/config.toml
systemctl restart containerd
