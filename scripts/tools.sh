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
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# wanted <name>: whether the caller named this tool, or named none.
wanted() {
    case "${only}" in
        '  ' | *" $1 "*) true ;;
        *) false ;;
    esac
}

# tool <name> <url> <sha256> [member of the archive]
tool() {
    # shellcheck disable=SC2310 # wanted only matches a word, nothing in it can fail.
    wanted "$1" || return 0
    # --retry alone leaves out a connection the host resets mid-transfer (curl's exit 92); the
    # checksum makes trying again safe.
    curl -fsSL --proto '=https' --retry 5 --retry-all-errors --tlsv1.2 -o "${work}/download" "$2"
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
    curl -fsSL --proto '=https' --retry 5 --retry-all-errors --tlsv1.2 -o "${work}/download" "$2"
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
GO=1.27.3
GO_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
tree go "https://go.dev/dl/go${GO}.linux-amd64.tar.gz" "${GO_SHA256}" bin go gofmt

# renovate: datasource=node-version depName=node
NODE=26.12.0
NODE_SHA256=d9f88af72ad34f7ddf8e67289829f1698f54b6254f7d4f54b7453117ff1a3d2c
tree node "https://nodejs.org/dist/v${NODE}/node-v${NODE}-linux-x64.tar.gz" "${NODE_SHA256}" bin node npm npx

# renovate: datasource=github-release-attachments depName=astral-sh/uv
UV=0.13.6
UV_SHA256=f29c3e94ffabd7e6f3ba521dba169a9b7ec12100bf3257087f0f24f6123f3645
tree uv "https://github.com/astral-sh/uv/releases/download/${UV}/uv-x86_64-unknown-linux-gnu.tar.gz" "${UV_SHA256}" . uv

# renovate: datasource=github-release-attachments depName=opentofu/opentofu
TOFU=v1.13.2
TOFU_SHA256=ac05d9952662c7db28dd57e93f0a68018837bfdf9342384ec0987977c16a9645
release tofu opentofu/opentofu "${TOFU}" "tofu_${TOFU#v}_linux_amd64.tar.gz" "${TOFU_SHA256}" tofu

# renovate: datasource=github-releases depName=helm/helm
HELM=v4.3.1
HELM_SHA256=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
tool helm "https://get.helm.sh/helm-${HELM}-linux-amd64.tar.gz" "${HELM_SHA256}" linux-amd64/helm

# renovate: datasource=github-release-attachments depName=terraform-linters/tflint
TFLINT=v0.65.0
TFLINT_SHA256=d4c545ed2e29d6338604b586f34f544f380f70f243aa72f7b484713835eaec61
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

# The end-to-end tests reach Talos nodes through their API with it.
# renovate: datasource=github-release-attachments depName=siderolabs/talos
TALOSCTL=v1.14.5
TALOSCTL_SHA256=ff432b68986709e6b69c563a08a2ced4232d5196b0dccd6d2e783e9d714d7d7d
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

# The release signs its images with it.
# renovate: datasource=github-release-attachments depName=sigstore/cosign
COSIGN=v3.1.4
COSIGN_SHA256=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
release cosign sigstore/cosign "${COSIGN}" cosign-linux-amd64 "${COSIGN_SHA256}"

# The release writes the SBOMs of its archive and images with it (scripts/sbom.sh).
# renovate: datasource=github-release-attachments depName=anchore/syft
SYFT=v1.55.0
SYFT_SHA256=bb65249542db5fedeeb4d3f9b018b75e7264dbfbbb10486f044581a92f90490d
release syft anchore/syft "${SYFT}" "syft_${SYFT#v}_linux_amd64.tar.gz" "${SYFT_SHA256}" syft
