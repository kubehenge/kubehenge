#!/bin/sh
# Installs the tools `make test`, `make dist`, `make image`, the release and the end-to-end tests
# need besides the system's own (curl, jq, openssl, python3), Go, Node and uv among them, at pinned
# versions checked against their published checksums, into a directory of the caller's choosing;
# every one, or only those named:
#
#   scripts/tools.sh <directory> [tool...]
#
# CI puts the directory on PATH. yamllint and zizmor run from scripts/lint's lock file through uv,
# markdownlint from the frontend's through npm.
set -eu

dir=${1:?usage: $0 <directory> [tool...]}
shift
only=" $* "
mkdir -p "${dir}"
here=$(dirname "$0")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
KUBEHENGE_FETCH_STATE=${work}/releases-down
export KUBEHENGE_FETCH_STATE

# wanted <name>: whether the caller named this tool, or named none.
wanted() {
    case "${only}" in
        '  ' | *" $1 "*) true ;;
        *) false ;;
    esac
}

# fetch <url>: downloads to ${work}/download through fetch.sh, which retries what a host resets
# mid-transfer (the caller's checksum makes that safe) and sends every GitHub release asset of the
# run through the API once the release address has failed. KUBEHENGE_FETCH_STATE ties that switch
# to this run.
fetch() {
    "${here}/fetch.sh" "$1" "${work}/download"
}

# tool <name> <url> <sha256> [member of the archive]
tool() {
    # shellcheck disable=SC2310 # wanted only matches a word, nothing in it can fail.
    wanted "$1" || return 0
    # shellcheck disable=SC2310 # its failure is the message below.
    fetch "$2" || {
        echo "tools: could not download $2" >&2
        exit 1
    }
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

# tree <name> <url> <sha256> <directory of its programs in the archive> <program>...: a tool that
# comes as a directory tree, unpacked into <directory>/lib/<name>, with its programs linked beside
# the others.
tree() {
    # shellcheck disable=SC2310
    wanted "$1" || return 0
    # shellcheck disable=SC2310 # its failure is the message below.
    fetch "$2" || {
        echo "tools: could not download $2" >&2
        exit 1
    }
    echo "$3  ${work}/download" | sha256sum -c --quiet -
    rm -rf "${dir:?}/lib/$1" && mkdir -p "${dir}/lib/$1"
    tar -xzf "${work}/download" -C "${dir}/lib/$1" --strip-components=1
    rm -f "${work}/download"
    name=$1 bin=$4
    shift 4
    for program in "$@"; do
        ln -sfn "lib/${name}/${bin}/${program}" "${dir}/${program}"
    done
}

# Each tag sits on the line above its SHA-256, so Renovate moves the two together. Go, Node, Helm
# and kubectl publish their downloads outside GitHub, so Renovate moves only their version, and
# their SHA-256 is taken from the checksum list beside the download.

# Go finds its own tree through the link, and the workflows hold it to this release
# (GOTOOLCHAIN=local) rather than download the one go.mod names.
# renovate: datasource=golang-version depName=go
GO=1.27.4
GO_SHA256=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
tree go "https://go.dev/dl/go${GO}.linux-amd64.tar.gz" "${GO_SHA256}" bin go gofmt

# renovate: datasource=node-version depName=node
NODE=26.13.0
NODE_SHA256=5848553868b402d3fabb5b09270649977780a390f832155721025a08b5104455
tree node "https://nodejs.org/dist/v${NODE}/node-v${NODE}-linux-x64.tar.gz" "${NODE_SHA256}" bin node npm npx

# renovate: datasource=github-release-attachments depName=astral-sh/uv
UV=0.13.8
UV_SHA256=5acfd9177cfecf0bf87578f059728215ccc884388988f55f7a65041b57387d91
tree uv "https://github.com/astral-sh/uv/releases/download/${UV}/uv-x86_64-unknown-linux-gnu.tar.gz" "${UV_SHA256}" . uv

# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU=v1.13.9
TOFU_SHA256=b34b327cc1ef07178207b6f2cd1f5276537d3e5b7be316500f6dac50d4c0a857
release tofu opentofu/opentofu "${TOFU}" "tofu_${TOFU#v}_linux_amd64.tar.gz" "${TOFU_SHA256}" tofu

# renovate: datasource=github-releases depName=helm/helm
HELM=v4.3.1
HELM_SHA256=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
tool helm "https://get.helm.sh/helm-${HELM}-linux-amd64.tar.gz" "${HELM_SHA256}" linux-amd64/helm

# renovate: datasource=github-release-attachments depName=terraform-linters/tflint
TFLINT=v0.66.0
TFLINT_SHA256=22be11cadc25cd02401e94c0ccc91aa63467debb8485c2a755d98f4c0a9d1e2e
release tflint terraform-linters/tflint "${TFLINT}" tflint_linux_amd64.zip "${TFLINT_SHA256}" \
    tflint.zip

# renovate: datasource=github-release-attachments depName=golangci/golangci-lint
GOLANGCI_LINT=v2.15.0
GOLANGCI_LINT_SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea
release golangci-lint golangci/golangci-lint "${GOLANGCI_LINT}" \
    "golangci-lint-${GOLANGCI_LINT#v}-linux-amd64.tar.gz" "${GOLANGCI_LINT_SHA256}" \
    "golangci-lint-${GOLANGCI_LINT#v}-linux-amd64/golangci-lint"

# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBECTL=v1.37.2
KUBECTL_SHA256=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
tool kubectl "https://dl.k8s.io/release/${KUBECTL}/bin/linux/amd64/kubectl" "${KUBECTL_SHA256}"

# The end-to-end tests reach Talos nodes through their API with it.
# renovate: datasource=github-release-attachments depName=siderolabs/talos
TALOSCTL=v1.14.6
TALOSCTL_SHA256=11acde7f7578ffe9cbe6fb8349b34595065f597de0842615982e350b365de987
release talosctl siderolabs/talos "${TALOSCTL}" talosctl-linux-amd64 "${TALOSCTL_SHA256}"

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

# renovate: datasource=github-release-attachments depName=crate-ci/typos
TYPOS=v1.52.2
TYPOS_SHA256=3b13d1291281d39d01822e54035636487a7a0f228e70097899b1e33aeb0e7e93
release typos crate-ci/typos "${TYPOS}" "typos-${TYPOS}-x86_64-unknown-linux-musl.tar.gz" "${TYPOS_SHA256}" ./typos

# `alloy validate` proves that the Alloy configurations the controller generates parse, and that
# their components and arguments exist. It is the release the charts run: TestAlloyToolIsTheChartsRelease holds the two together.
# renovate: datasource=github-release-attachments depName=grafana/alloy
ALLOY=v1.22.0
ALLOY_SHA256=62d177feb18c9b6ffd636c8273aa82a6afc75a507d096ff951749df23c3176f9
release alloy grafana/alloy "${ALLOY}" alloy-linux-amd64.zip "${ALLOY_SHA256}" alloy-linux-amd64.zip

# The nightly link check runs it (make links).
# renovate: datasource=github-release-attachments depName=lycheeverse/lychee
LYCHEE=lychee-v0.24.2
LYCHEE_SHA256=1f4e0ef7f6554a6ed33dd7ac144fb2e1bbed98598e7af973042fc5cd43951c9a
release lychee lycheeverse/lychee "${LYCHEE}" lychee-x86_64-unknown-linux-gnu.tar.gz "${LYCHEE_SHA256}" \
    lychee-x86_64-unknown-linux-gnu/lychee

# The release signs its images with it.
# renovate: datasource=github-release-attachments depName=sigstore/cosign
COSIGN=v3.1.4
COSIGN_SHA256=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
release cosign sigstore/cosign "${COSIGN}" cosign-linux-amd64 "${COSIGN_SHA256}"

# The release writes the SBOMs of its archive and images with it (scripts/sbom.sh).
# renovate: datasource=github-release-attachments depName=anchore/syft
SYFT=v1.57.1
SYFT_SHA256=bf52519eb57f675d47c07d43213cb6dff6449f3d6d1147ff6701ed91f44eef0c
release syft anchore/syft "${SYFT}" "syft_${SYFT#v}_linux_amd64.tar.gz" "${SYFT_SHA256}" syft
