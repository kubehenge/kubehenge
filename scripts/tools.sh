#!/bin/sh
# Installs the tools `make test`, `make dist`, `make image` and the end-to-end tests need besides Go,
# Node and the system's own (curl, jq, openssl, python3), at pinned versions checked against their
# published checksums, into a directory of the caller's choosing:
#
#   scripts/tools.sh <directory>
#
# CI puts the directory on PATH. yamllint, zizmor and markdownlint run through uvx and npx at the
# versions the Makefile names.
set -eu

dir=${1:?usage: $0 <directory>}
mkdir -p "${dir}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# tool <name> <url> <sha256> [member of the archive]
tool() {
    curl -fsSL --proto '=https' --tlsv1.2 -o "${work}/download" "$2"
    echo "$3  ${work}/download" | sha256sum -c --quiet -
    case "${4:-}" in
        '') install -m 0755 "${work}/download" "${dir}/$1" ;;
        *.zip) unzip -q -o "${work}/download" -d "${work}/x" && install -m 0755 "${work}/x/${4%.zip}" "${dir}/$1" ;;
        *) tar -xzf "${work}/download" -C "${work}" "$4" && install -m 0755 "${work}/$4" "${dir}/$1" ;;
    esac
    rm -rf "${work}/download" "${work}/x"
}

# release <name> <owner/repository> <tag> <asset> <sha256> [member of the archive]
release() {
    tool "$1" "https://github.com/$2/releases/download/$3/$4" "$5" "${6:-}"
}

# Each tag sits on the line above its SHA-256, so Renovate moves the two together. Helm and kubectl
# publish their downloads outside GitHub, so Renovate moves only their tag, and their SHA-256 is
# taken from the checksum file beside the download.

# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU=v1.13.1
TOFU_SHA256=378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69
release tofu opentofu/opentofu "${TOFU}" "tofu_${TOFU#v}_linux_amd64.tar.gz" "${TOFU_SHA256}" tofu

# renovate: datasource=github-releases depName=helm/helm
HELM=v4.3.1
HELM_SHA256=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
tool helm "https://get.helm.sh/helm-${HELM}-linux-amd64.tar.gz" "${HELM_SHA256}" linux-amd64/helm

# renovate: datasource=github-release-attachments depName=terraform-linters/tflint
TFLINT=v0.64.0
TFLINT_SHA256=cca9d13e2e1d7a2c627af60ff899a3c9b74212899416aeb96ec764d2ef954537
release tflint terraform-linters/tflint "${TFLINT}" tflint_linux_amd64.zip "${TFLINT_SHA256}" \
    tflint.zip

# renovate: datasource=github-release-attachments depName=golangci/golangci-lint
GOLANGCI_LINT=v2.15.0
GOLANGCI_LINT_SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea
release golangci-lint golangci/golangci-lint "${GOLANGCI_LINT}" \
    "golangci-lint-${GOLANGCI_LINT#v}-linux-amd64.tar.gz" "${GOLANGCI_LINT_SHA256}" \
    "golangci-lint-${GOLANGCI_LINT#v}-linux-amd64/golangci-lint"

# renovate: datasource=github-release-attachments depName=ko-build/ko
KO=v0.19.1
KO_SHA256=635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5
release ko ko-build/ko "${KO}" "ko_${KO#v}_Linux_x86_64.tar.gz" "${KO_SHA256}" ko

# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBECTL=v1.37.1
KUBECTL_SHA256=65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
tool kubectl "https://dl.k8s.io/release/${KUBECTL}/bin/linux/amd64/kubectl" "${KUBECTL_SHA256}"

# renovate: datasource=github-release-attachments depName=rhysd/actionlint
ACTIONLINT=v1.7.12
ACTIONLINT_SHA256=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
release actionlint rhysd/actionlint "${ACTIONLINT}" \
    "actionlint_${ACTIONLINT#v}_linux_amd64.tar.gz" "${ACTIONLINT_SHA256}" actionlint

# The distribution's ShellCheck is older, and every release adds optional checks.
# renovate: datasource=github-release-attachments depName=koalaman/shellcheck
SHELLCHECK=v0.11.0
SHELLCHECK_SHA256=b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6
release shellcheck koalaman/shellcheck "${SHELLCHECK}" \
    "shellcheck-${SHELLCHECK}.linux.x86_64.tar.gz" "${SHELLCHECK_SHA256}" "shellcheck-${SHELLCHECK}/shellcheck"

# renovate: datasource=github-release-attachments depName=mvdan/sh
SHFMT=v3.14.1
SHFMT_SHA256=76e77641faa025814b77f153b29796b8e6fa2fca03e0c76a691608b86c7ea7bf
release shfmt mvdan/sh "${SHFMT}" "shfmt_${SHFMT}_linux_amd64" "${SHFMT_SHA256}"

# renovate: datasource=github-release-attachments depName=hadolint/hadolint
HADOLINT=v2.15.1
HADOLINT_SHA256=c7187db94eeeeca956519a6af171adc31453941a1e777961f6e680f697c8c507
release hadolint hadolint/hadolint "${HADOLINT}" hadolint-linux-x86_64 "${HADOLINT_SHA256}"
