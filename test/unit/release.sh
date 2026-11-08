#!/bin/sh
# Unit test of the download of an RKE2 or k3s release on a node (pkg/provisioner/release.sh): a
# stand-in curl serves files from a directory by their address, and stand-ins for install, mv,
# mkdir and mktemp record what the script would install. It covers GitHub, a release mirror, a
# mirror with its own authority, a mirror missing a file and one serving a file that is not pinned.
#
#   test/unit/release.sh
set -eu

here=$(dirname "$0")
script=${here}/../../pkg/provisioner/release.sh
root=$(mktemp -d)
bin=${root}/bin
served=${root}/served
real_mkdir=$(command -v mkdir)
"${real_mkdir}" "${bin}" "${served}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# truth <what> <command>...: whether the command succeeds.
truth() {
    what=$1
    shift
    if "$@"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n' "${what}"
        failures=$((failures + 1))
    fi
}
# has <what> <file> <text>: whether the file holds the text.
has() {
    if grep -qF -- "$3" "$2"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted %s to hold: %s\n  it holds:\n' "$1" "$2" "$3"
        cat "$2"
        failures=$((failures + 1))
    fi
}
# lacks <what> <file> <text>: whether the file leaves the text out.
lacks() {
    if grep -qF -- "$3" "$2"; then
        printf 'FAIL: %s\n  wanted %s not to hold: %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    else
        passes=$((passes + 1))
    fi
}

# curl serves ${SERVED}/<host>/<path> for http(s)://<host>/<path>, records each address with the
# authority it was given, and answers 404 for a file it lacks, as curl --fail does.
cat > "${bin}/curl" << 'EOF'
#!/bin/sh
output= write= ca= url=
while [ $# -gt 0 ]; do
    case "$1" in
        --output | -o) output=$2; shift 2 ;;
        --write-out | -w) write=$2; shift 2 ;;
        --cacert) ca=$2; shift 2 ;;
        --proto | --retry | --header | -H) shift 2 ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
echo "${url} ca=${ca}" >> "${CALLS}"
file=${SERVED}/${url#*://}
if [ ! -f "${file}" ]; then
    [ -z "${write}" ] || printf 404
    echo "curl: (22) The requested URL returned error: 404" >&2
    exit 22
fi
cp "${file}" "${output}"
[ -z "${write}" ] || printf 200
EOF
# shellcheck disable=SC2016 # the stand-ins expand their own variables.
for tool in install mv mkdir; do
    printf '#!/bin/sh\necho "%s $*" >> "${INSTALLS}"\n' "${tool}" > "${bin}/${tool}"
done
# shellcheck disable=SC2016 # the stand-in expands its own variables.
printf '#!/bin/sh\nd=${ROOT}/work.$$\n"%s" "${d}"\necho "${d}"\n' "${real_mkdir}" > "${bin}/mktemp"
chmod +x "${bin}"/*

version=v1.37.1+k3s1
# publish <host and path beneath which GitHub's paths start> <raw host>: the release's files there.
publish() {
    assets=${served}/$1/k3s-io/k3s/releases/download/${version}
    "${real_mkdir}" -p "${assets}" "${served}/$2"
    echo "k3s binary" > "${assets}/k3s"
    echo "k3s images" > "${assets}/k3s-airgap-images-amd64.tar.zst"
    # As the release lists them, in text mode, which sha256sum on Windows does not default to.
    for asset in k3s k3s-airgap-images-amd64.tar.zst; do
        sum=$(sha256sum < "${assets}/${asset}")
        printf '%s  %s\n' "${sum%% *}" "${asset}"
    done > "${assets}/sha256sum-amd64.txt"
    echo '#!/bin/sh' > "${served}/$2/install.sh"
}
publish github.com "raw.githubusercontent.com/k3s-io/k3s/${version}"
publish mirror.example.org/github "mirror.example.org/github/k3s-io/k3s/raw/${version}"
sums=$(sha256sum < "${served}/github.com/k3s-io/k3s/releases/download/${version}/sha256sum-amd64.txt" | cut -d' ' -f1)
script_sum=$(sha256sum < "${served}/raw.githubusercontent.com/k3s-io/k3s/${version}/install.sh" | cut -d' ' -f1)

# run <name> [option...]: runs the script with the options, its record under ${root}/<name>.
run() {
    name=$1
    shift
    : > "${root}/${name}.calls"
    : > "${root}/${name}.installs"
    status=0
    PATH="${bin}:${PATH}" CALLS="${root}/${name}.calls" INSTALLS="${root}/${name}.installs" SERVED="${served}" ROOT="${root}" \
        sh "${script}" "$@" k3s "${version}" "${sums}" "${script_sum}" k3s k3s-airgap-images-amd64.tar.zst \
        > "${root}/${name}.out" 2> "${root}/${name}.err" || status=$?
}

run github
truth "without a mirror the release installs" [ "${status}" = 0 ]
has "its files come from GitHub's release address" "${root}/github.calls" "https://github.com/k3s-io/k3s/releases/download/${version}/k3s ca="
has "its install script from GitHub's raw files" "${root}/github.calls" "https://raw.githubusercontent.com/k3s-io/k3s/${version}/install.sh"
has "k3s is installed" "${root}/github.installs" "/usr/local/bin/k3s"

run mirror --mirror https://mirror.example.org/github
truth "with a mirror the release installs" [ "${status}" = 0 ]
has "its files come from the mirror" "${root}/mirror.calls" "https://mirror.example.org/github/k3s-io/k3s/releases/download/${version}/k3s-airgap-images-amd64.tar.zst ca="
has "its install script from the mirror's raw path" "${root}/mirror.calls" "https://mirror.example.org/github/k3s-io/k3s/raw/${version}/install.sh"
lacks "nothing comes from GitHub" "${root}/mirror.calls" "github.com/k3s-io"
has "k3s is installed" "${root}/mirror.installs" "/usr/local/bin/k3s"

run mirror-ca --mirror https://mirror.example.org/github --mirror-ca /etc/kubehenge/release-mirror-ca.pem
truth "a mirror with its own authority installs" [ "${status}" = 0 ]
unchecked=$(grep -vc 'ca=/etc/kubehenge/release-mirror-ca.pem$' "${root}/mirror-ca.calls" || true)
truth "every download trusts the mirror's authority" [ "${unchecked}" = 0 ]

rm "${served}/mirror.example.org/github/k3s-io/k3s/releases/download/${version}/k3s-airgap-images-amd64.tar.zst"
run missing --mirror https://mirror.example.org/github
truth "a mirror missing a file fails" [ "${status}" != 0 ]
has "naming the file and the mirror" "${root}/missing.err" "the release mirror https://mirror.example.org/github has no k3s-airgap-images-amd64.tar.zst of k3s-io/k3s ${version}"
lacks "and installs nothing" "${root}/missing.installs" "install"

echo "other images" > "${served}/mirror.example.org/github/k3s-io/k3s/releases/download/${version}/k3s-airgap-images-amd64.tar.zst"
run mismatch --mirror https://mirror.example.org/github
truth "a file that does not match its checksum is refused" [ "${status}" != 0 ]
has "naming the mirror" "${root}/mismatch.err" "from the release mirror https://mirror.example.org/github do not match the pinned checksums"
lacks "and installs nothing" "${root}/mismatch.installs" "install"

echo "other listing" > "${served}/mirror.example.org/github/k3s-io/k3s/releases/download/${version}/sha256sum-amd64.txt"
run listing --mirror https://mirror.example.org/github
truth "a checksum file other than the pinned one is refused" [ "${status}" != 0 ]
lacks "and installs nothing" "${root}/listing.installs" "install"

echo "release: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
