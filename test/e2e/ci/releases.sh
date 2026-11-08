#!/bin/sh
# The RKE2 and k3s releases the guests of an end-to-end lane install, downloaded once on the runner
# and served to them as their release mirror (pkg/provisioner/release.sh), so that no guest
# downloads from GitHub. A lane with a github-releases test is left without one, so that the
# nightly run proves the download from GitHub as well. The Actions cache keeps them under a key of
# their pins:
#
#   test/e2e/ci/releases.sh key <lane>           # key=<cache key>, nothing for a lane that needs none
#   test/e2e/ci/releases.sh fetch <dir> <lane>   # what <dir> lacks, from GitHub, then checks it all
#
# <lane> is a test or tests as test/e2e/ci/run.sh takes them. A lane takes the files of every
# distribution its tests run: a release of each pinned version they start on, upgrade to or block,
# for RKE2 with the images of each CNI the lane's tests choose. <dir> holds them on GitHub's paths,
# as a mirror serves them.
set -eu

usage="usage: $0 key <lane> | fetch <dir> <lane>"
mode=${1:?${usage}}
here=$(dirname "$0")
repo=$(readlink -f "${here}/../../..")
pins=${repo}/pkg/provisioner/releasepins.txt
# shellcheck source=variant.sh
. "${here}/variant.sh"
# shellcheck source=templates.sh
. "${here}/templates.sh"
kubernetes_versions "${repo}"

# pinned <rke2|k3s> <version>: the newest release of the version's minor that the controller pins.
pinned() {
    awk -v minor="${2%.*}." -v build="+$1" 'index($1, minor) == 1 && index($1, build) { print $1 }' "${pins}" |
        sort -V | tail -n 1 | grep . || {
        echo "${pins} pins no $1 release of $2's minor version" >&2
        return 1
    }
}

# groups <lane>: the distributions the lane's tests install, one a line, RKE2's with its CNI. None
# when one test takes github-releases, since every test in a lane shares its management cluster's mirror.
groups() {
    for lane_test in $(printf '%s' "$1" | tr , ' '); do
        case $(variant "${lane_test}") in
            *KUBEHENGE_E2E_RELEASES=github*) return 0 ;;
            *) ;;
        esac
    done
    for lane_test in $(printf '%s' "$1" | tr , ' '); do
        exports=$(variant "${lane_test}")
        cni=$(
            eval "${exports}"
            printf '%s' "${KUBEHENGE_E2E_CNI:-}"
        )
        case "${lane_test%%+*}" in
            rke2 | rke2-*) echo "rke2-${cni:-canal}" ;;
            k3s | k3s-* | rancher | cluster-mesh | node-image-refresh | autoscaler | control-plane-ha | two-nodes) echo k3s ;;
            *) ;;
        esac
    done | sort -u
}

# needs <lane>: "<repository> <version> <file>..." for each release the lane installs.
needs() {
    for group in $(groups "$1"); do
        case "${group}" in
            rke2-*)
                first=$(pinned rke2 "${from}")
                last=$(pinned rke2 "${to}")
                for version in $(printf '%s\n' "${first}" "${last}" | sort -u); do
                    echo "rancher/rke2 ${version} rke2.linux-amd64.tar.gz rke2-images-core.linux-amd64.tar.zst rke2-images-${group#rke2-}.linux-amd64.tar.zst"
                done
                ;;
            k3s)
                # control-plane-ha and two-nodes start on the from release itself, the others on
                # the newest of its minor.
                first=$(pinned k3s "${from}")
                last=$(pinned k3s "${to}")
                for version in $(printf '%s\n' "${first}" "${last}" "${from}+k3s1" | sort -u); do
                    echo "k3s-io/k3s ${version} k3s k3s-airgap-images-amd64.tar.zst"
                done
                ;;
            *) ;;
        esac
    done
}

# pin_of <version>: the line of releasepins.txt that pins it.
pin_of() {
    awk -v v="$1" '$1 == v { print; found = 1 } END { exit !found }' "${pins}" || {
        echo "${pins} pins no ${1}" >&2
        return 1
    }
}

case "${mode}" in
    key)
        lane=${2:?${usage}}
        need=$(needs "${lane}")
        [ -n "${need}" ] || exit 0
        name=$(groups "${lane}" | paste -sd- -)
        sum=$(printf '%s\n' "${need}" | while read -r repository version files; do
            pin=$(pin_of "${version}")
            echo "${repository} ${files} ${pin}"
        done | sha256sum | cut -c1-16)
        echo "key=e2e-releases-${name}-${sum}"
        ;;
    fetch)
        dir=${2:?${usage}}
        lane=${3:?${usage}}
        need=$(needs "${lane}")
        printf '%s\n' "${need}" | while read -r repository version files; do
            [ -n "${repository}" ] || continue
            assets=${dir}/${repository}/releases/download/${version}
            raw=${dir}/${repository}/raw/${version}
            mkdir -p "${assets}" "${raw}"
            for file in sha256sum-amd64.txt ${files}; do
                [ -s "${assets}/${file}" ] ||
                    "${repo}/scripts/fetch.sh" "https://github.com/${repository}/releases/download/${version}/${file}" "${assets}/${file}"
            done
            [ -s "${raw}/install.sh" ] ||
                "${repo}/scripts/fetch.sh" "https://raw.githubusercontent.com/${repository}/${version}/install.sh" "${raw}/install.sh"
            # The guests check the same, but a file a cache kept wrong is better found here.
            # A pin is "<version> <checksum file's SHA-256> <install script's SHA-256>".
            pin=$(pin_of "${version}")
            sums_sum=${pin#* }
            sums_sum=${sums_sum%% *}
            printf '%s  %s\n%s  %s\n' "${sums_sum}" "${assets}/sha256sum-amd64.txt" "${pin##* }" "${raw}/install.sh" |
                sha256sum --check --strict --quiet || {
                rm -rf "${assets}" "${raw}"
                echo "the checksum file or install script of ${repository} ${version} is not the pinned one" >&2
                exit 1
            }
            for file in ${files}; do
                awk -v file="${file}" '$2 == file { print; found = 1 } END { exit !found }' "${assets}/sha256sum-amd64.txt"
            done | (cd "${assets}" && sha256sum --check --strict --quiet) || {
                rm -rf "${assets}"
                echo "a file of ${repository} ${version} does not match its checksum" >&2
                exit 1
            }
            echo "release ${repository} ${version}: ${files}"
        done
        # What the workflow keeps in the cache, and only once all of it is here and checked.
        : > "${dir}/.complete"
        ;;
    *)
        echo "${usage}" >&2
        exit 2
        ;;
esac
