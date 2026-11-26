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
# GitHub's release download address can answer 504 for hours while its API serves the same file.
# The API's limit without a token is 60 requests an hour per address, which is why it comes second.
curl --fail --silent --show-error --location --proto '=https' --retry 2 --retry-all-errors \
    --output "${work}/cosign" "https://github.com/sigstore/cosign/releases/download/${COSIGN}/cosign-linux-amd64" || {
    api=https://api.github.com/repos/sigstore/cosign/releases
    curl --fail --silent --show-error --location --proto '=https' --retry 8 --retry-all-errors \
        --output "${work}/release.json" "${api}/tags/${COSIGN}"
    # The pretty-printed answer lists each asset's "url" before its "name".
    id=$(awk '
        /releases\/assets\/[0-9]+"/ { id = $0; sub(/.*releases\/assets\//, "", id); sub(/".*/, "", id) }
        $1 == "\"name\":" && $2 == "\"cosign-linux-amd64\"," { print id; exit }' "${work}/release.json")
    [ -n "${id}" ] || {
        echo "kubehenge-images: cosign ${COSIGN} has no asset cosign-linux-amd64" >&2
        exit 1
    }
    curl --fail --silent --show-error --location --proto '=https' --retry 8 --retry-all-errors \
        --header 'Accept: application/octet-stream' --output "${work}/cosign" "${api}/assets/${id}"
}
echo "${COSIGN_SHA256}  ${work}/cosign" | sha256sum --check --strict --quiet
chmod 0755 "${work}/cosign"

# Sigstore's trusted root is fetched once, before any pull, and every verify reads it from disk. A
# dropped connection to Sigstore is then never read as a missing signature and never costs a pulled
# image. Only a failed connection is retried, since an answer TUF refuses is refused every time.
attempt=1
until HOME="${work}" TUF_ROOT="${work}/tuf" "${work}/cosign" initialize > "${work}/tuf.log" 2>&1; do
    cat "${work}/tuf.log" >&2
    grep -q -E 'dial tcp|i/o timeout|connection refused|connection reset|no such host|network is unreachable|TLS handshake timeout|Client\.Timeout|unexpected EOF|proxyconnect|http status code: (429|5[0-9][0-9])' "${work}/tuf.log" || {
        echo "kubehenge-images: cosign refused Sigstore's trusted root" >&2
        exit 1
    }
    [ "${attempt}" -lt 5 ] || {
        echo "kubehenge-images: Sigstore is unreachable: no trusted root after 5 attempts" >&2
        exit 1
    }
    attempt=$((attempt + 1))
    rm -rf "${work}/tuf"
    sleep 5
done
trusted_root=$(find "${work}/tuf" -name trusted_root.json | head -n 1)
[ -n "${trusted_root}" ] || {
    echo "kubehenge-images: cosign initialize wrote no trusted_root.json" >&2
    exit 1
}

images=$(kubeadm config images list --kubernetes-version "${version}")
for image in ${images}; do
    crictl pull "${image}" > /dev/null
    pulled=$(crictl inspecti --output go-template --template '{{range .status.repoDigests}}{{println .}}{{end}}' "${image}" | grep -m 1 -F "${image%:*}@sha256:")
    HOME="${work}" "${work}/cosign" verify --trusted-root "${trusted_root}" --certificate-identity krel-trust@k8s-releng-prod.iam.gserviceaccount.com \
        --certificate-oidc-issuer https://accounts.google.com "${pulled}" > /dev/null || {
        crictl rmi "${image}" > /dev/null
        echo "kubehenge-images: ${pulled} has no signature of Kubernetes' release" >&2
        exit 1
    }
done

pause=$(printf '%s\n' "${images}" | grep -m 1 /pause:)
sed -i "s#registry.k8s.io/pause:[^\"']*#${pause}#" /etc/containerd/config.toml
systemctl restart containerd
