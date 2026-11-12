#!/bin/sh
# Builds ko into <directory> at the pinned release with the pinned Go, unless the ko there already
# is that build. Every image build here runs this ko, never one from PATH.
#
#   scripts/ko.sh <directory>
set -eu

dir=${1:?usage: $0 <directory>}

# ko gzips the layers with the compress/flate of the Go that compiled it, so that Go decides every
# image digest. Moving either pin means building the digests management-cluster/versions.tf names
# again.
# renovate: datasource=github-releases depName=ko-build/ko
KO=v0.19.1
# renovate: datasource=golang-version depName=go
KO_GO=1.27.4

ko=${dir}/ko$(go env GOEXE)
if [ -x "${ko}" ] && go version -m "${ko}" 2> /dev/null | grep -q ": go${KO_GO}$" &&
    go version -m "${ko}" | grep -Eq "^[[:space:]]+mod[[:space:]]+github.com/google/ko[[:space:]]+${KO}[[:space:]]"; then
    exit 0
fi
CGO_ENABLED=0 GOTOOLCHAIN=go${KO_GO} GOFLAGS='' GOBIN=${dir} go install "github.com/google/ko@${KO}" || {
    echo "ko: could not build ko ${KO} with Go ${KO_GO}" >&2
    exit 1
}
