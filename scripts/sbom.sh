#!/bin/sh
# Writes the SPDX SBOM of a release archive or image with syft (scripts/syft.yaml), then checks it
# lists what ships, since an SBOM that lists nothing looks exactly like a passing step:
#
#   scripts/sbom.sh archive <archive> <version> <sbom>
#   scripts/sbom.sh image <repository@digest> <main module> <sbom>
#
# An archive's SBOM has to list every file in it with its SHA-256, every module each of its Go
# binaries records, the interface's packages at the versions its lock file holds, and the OpenTofu
# providers at the versions the module's lock file holds. An image's has to list the Go module its
# program is built from and the base image's Debian packages.
set -eu

usage() {
    echo "usage: $0 archive <archive> <version> <sbom>" >&2
    echo "       $0 image <repository@digest> <main module> <sbom>" >&2
    exit 2
}
[ $# -eq 4 ] || usage
scripts=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0

fail() {
    echo "sbom: $*" >&2
    failed=1
}

# need <name> [version]: fails the check unless the SBOM lists the package, at that version if one
# is given.
need() {
    jq -e --arg n "$1" --arg v "${2-}" \
        'any(.packages[]; .name == $n and ($v == "" or .versionInfo == $v))' "${sbom}" > /dev/null ||
        fail "${sbom} does not list $1${2:+ $2}"
}

case $1 in
    archive)
        archive=$2 version=$3 sbom=$4
        mkdir "${work}/in"
        tar -xzf "${archive}" -C "${work}/in"
        # The frontend's packages are bundled into the binary, which names none of them; the lock
        # file they were installed from does, and it is the one file syft reads that is not shipped.
        cp "${scripts}/../web/package-lock.json" "${work}/in/package-lock.json"
        syft scan "dir:${work}/in" --config "${scripts}/syft.yaml" --quiet \
            --source-name kubehenge --source-version "${version}" --output "spdx-json=${sbom}"

        need kubehenge "${version}"

        tar -tzf "${archive}" | sed -e 's|^\./||' -e '/\/$/d' -e '/^$/d' | LC_ALL=C sort > "${work}/members"
        jq -r '(.files // [])[] | select(any((.checksums // [])[]; .algorithm == "SHA256")) | .fileName' "${sbom}" |
            sed '/^package-lock\.json$/d' | LC_ALL=C sort > "${work}/listed"
        diff -u "${work}/members" "${work}/listed" || fail "the files ${sbom} lists with a SHA-256 are not the archive's"

        # What each Go binary records: its main module, and every module it was built from, as the
        # replacement where go.mod replaces one. The main module's version is the build's own
        # account of its commit, which syft may spell otherwise, so only its name has to match.
        go version -m "${work}/in" | awk -F '\t' '
            function flush() { if (name != "") print kind, name, version; name = "" }
            $2 == "mod" || $2 == "dep" { flush(); kind = $2; name = $3; version = $4 }
            $2 == "=>" { name = $3; version = $4 }
            END { flush() }' > "${work}/modules"
        grep -q '^mod github.com/kubehenge/kubehenge ' "${work}/modules" ||
            fail "go version -m finds no kubehenge binary in ${archive}"
        while read -r kind name version; do
            case ${kind} in
                mod) need "${name}" ;;
                *) need "${name}" "${version}" ;;
            esac
        done < "${work}/modules"

        jq -r --slurpfile lock "${scripts}/../web/package-lock.json" \
            '.dependencies | keys[] | "\(.) \($lock[0].packages["node_modules/" + .].version)"' \
            "${scripts}/../web/package.json" > "${work}/packages"
        [ -s "${work}/packages" ] || fail "web/package.json names no dependencies"
        while read -r name version; do
            need "${name}" "${version}"
        done < "${work}/packages"

        awk '/^provider "/ { gsub(/"/, "", $2); name = $2 }
            /^ *version *=/ && name != "" { gsub(/"/, "", $3); print name, $3; name = "" }' \
            "${work}/in/share/kubehenge/management-cluster/.terraform.lock.hcl" > "${work}/providers"
        [ -s "${work}/providers" ] || fail "the management-cluster module's lock file names no providers"
        while read -r name version; do
            need "${name}" "${version}"
        done < "${work}/providers"
        ;;
    image)
        ref=$2 main=$3 sbom=$4
        case ${ref} in
            *@sha256:*) ;;
            *) usage ;;
        esac
        syft scan "registry:${ref}" --config "${scripts}/syft.yaml" --quiet \
            --source-name "${ref%@*}" --source-version "${ref#*@}" --output "spdx-json=${sbom}"
        need "${main}"
        jq -e 'any(.packages[]; any((.externalRefs // [])[];
                .referenceType == "purl" and (.referenceLocator | startswith("pkg:deb/"))))' \
            "${sbom}" > /dev/null || fail "${sbom} lists none of the base image's packages"
        ;;
    *) usage ;;
esac

exit "${failed}"
