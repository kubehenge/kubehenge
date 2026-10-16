#!/bin/sh
# Installs the tools `make test`, `make dist`, `make image` and the end-to-end tests need besides Go,
# Node and the system's own (curl, jq, openssl, python3), at pinned versions checked against their
# published checksums, into a directory of the caller's choosing:
#
#   hack/tools.sh <directory>
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

tool tofu https://github.com/opentofu/opentofu/releases/download/v1.13.1/tofu_1.13.1_linux_amd64.tar.gz \
    378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69 tofu
tool helm https://get.helm.sh/helm-v4.3.1-linux-amd64.tar.gz \
    0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b linux-amd64/helm
tool tflint https://github.com/terraform-linters/tflint/releases/download/v0.64.0/tflint_linux_amd64.zip \
    cca9d13e2e1d7a2c627af60ff899a3c9b74212899416aeb96ec764d2ef954537 tflint.zip
tool golangci-lint https://github.com/golangci/golangci-lint/releases/download/v2.15.0/golangci-lint-2.15.0-linux-amd64.tar.gz \
    9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea golangci-lint-2.15.0-linux-amd64/golangci-lint
tool ko https://github.com/ko-build/ko/releases/download/v0.19.1/ko_0.19.1_Linux_x86_64.tar.gz \
    635ac6ea3fd376c935fee597fbb29ab2c2449f49ef1655085fe3aa9c25fed7a5 ko
tool kubectl https://dl.k8s.io/release/v1.37.1/bin/linux/amd64/kubectl \
    65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8
tool actionlint https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz \
    8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 actionlint
# The distribution's ShellCheck is older, and every release adds optional checks.
tool shellcheck https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.gz \
    b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6 shellcheck-v0.11.0/shellcheck
tool shfmt https://github.com/mvdan/sh/releases/download/v3.14.1/shfmt_v3.14.1_linux_amd64 \
    76e77641faa025814b77f153b29796b8e6fa2fca03e0c76a691608b86c7ea7bf
tool hadolint https://github.com/hadolint/hadolint/releases/download/v2.15.1/hadolint-linux-x86_64 \
    c7187db94eeeeca956519a6af171adc31453941a1e777961f6e680f697c8c507
