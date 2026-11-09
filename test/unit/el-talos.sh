#!/bin/sh
# Unit test of the Talos disk write (pkg/templates/el/talos.sh): stand-ins for podman, blockdev, chown
# and install go first on PATH, so that it runs as any user with no container runtime, no EL host and
# no /etc/pki. The podman stand-in records its arguments and writes the raw images a case asks for
# into the directory mounted as /out, and the target disk is a file. Needs nothing but POSIX tools.
#
#   test/unit/el-talos.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=${here}/../../pkg/templates/el/talos.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

mib=1048576
bin=${root}/bin
mkdir "${bin}"

cat > "${bin}/podman" << 'EOF'
#!/bin/sh
echo "$*" >> "${CASE}/podman.log"
[ "$1" = run ] || exit 0
for arg in "$@"; do
    case ${arg} in
        *:/out:Z) out=${arg%:/out:Z} ;;
    esac
done
n=0
for size in ${RAW_MIB}; do
    dd if=/dev/urandom of="${out}/nocloud-${n}.raw" bs=1048576 count="${size}" 2> /dev/null
    n=$((n + 1))
done
for raw in "${out}"/*.raw; do
    [ ! -e "${raw}" ] || cp "${raw}" "${CASE}/raw.copy"
done
EOF
cat > "${bin}/blockdev" << 'EOF'
#!/bin/sh
wc -c < "$2" | tr -d ' '
EOF
cat > "${bin}/chown" << 'EOF'
#!/bin/sh
echo "$*" >> "${CASE}/chown.log"
EOF
cat > "${bin}/install" << 'EOF'
#!/bin/sh
echo "$*" >> "${CASE}/install.log"
for destination; do :; done
mkdir -p "$(dirname "${destination}")"
echo "test bundle" > "${destination}"
EOF
chmod +x "${bin}/podman" "${bin}/blockdev" "${bin}/chown" "${bin}/install"

passes=0 failures=0
pass() { passes=$((passes + 1)); }
fail() {
    printf 'FAIL: %s\n' "$1"
    failures=$((failures + 1))
}
# has <what> <file> <text>: whether the file holds the text.
has() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        pass
    else
        fail "$1 (wanted $2 to say: $3)"
    fi
}
# lacks <what> <file> <text>: whether the file, if there is one, leaves the text out.
lacks() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        fail "$1 (wanted $2 not to say: $3)"
    else
        pass
    fi
}
# expect <what> <wanted> <got>: whether the two are the same.
expect() {
    if [ "$2" = "$3" ]; then
        pass
    else
        fail "$1 (wanted: $2, got: $3)"
    fi
}
# exists <what> <file> and absent <what> <file>: whether the file is there.
exists() {
    if [ -e "$2" ]; then pass; else fail "$1 (no $2)"; fi
}
absent() {
    if [ -e "$2" ]; then fail "$1 ($2 exists)"; else pass; fi
}
# same <what> <file> <file>: whether the two files hold the same bytes.
same() {
    if cmp -s "$2" "$3"; then
        pass
    else
        fail "$1 ($2 and $3 differ)"
    fi
}

# prepare <name> <target MiB> [privileged]: a bake's directory with the files the build writes, and a
# target disk of random bytes. It sets case_dir.
prepare() {
    case_dir=${root}/$1
    mkdir -p "${case_dir}/el"
    case_dir=$(cd "${case_dir}" && pwd)
    cp "${script}" "${case_dir}/el/talos.sh"
    echo "ghcr.io/siderolabs/imager@sha256:aaaa" > "${case_dir}/el/talos-imager"
    echo "arch: amd64" > "${case_dir}/el/talos-profile.yaml"
    [ "${3:-}" != privileged ] || : > "${case_dir}/el/talos-privileged"
    dd if=/dev/urandom of="${case_dir}/target" bs="${mib}" count="$2" 2> /dev/null
    cp "${case_dir}/target" "${case_dir}/target.orig"
}

# run_talos <raw sizes in MiB>: runs the script on the prepared case and sets status.
run_talos() {
    status=0
    CASE=${case_dir} RAW_MIB=$1 PATH="${bin}:${PATH}" sh "${case_dir}/el/talos.sh" "${case_dir}/target" \
        > "${case_dir}/stdout" 2> "${case_dir}/stderr" || status=$?
}

# Unprivileged: the imager runs as the user Sidero's own run uses, with no privilege and no /dev.
prepare unprivileged 2
run_talos 2
log=${case_dir}/podman.log
expect "an unprivileged run succeeds" 0 "${status}"
same "the target holds the image" "${case_dir}/raw.copy" "${case_dir}/target"
exists "the build leaves its marker" "${case_dir}/built"
has "the imager is pulled by its pinned name" "${log}" "pull ghcr.io/siderolabs/imager@sha256:aaaa"
has "the profile goes in on standard input" "${log}" " ghcr.io/siderolabs/imager@sha256:aaaa -"
has "the container takes input" "${log}" "run --rm -i --network=host"
has "the imager runs as nobody" "${log}" "--user 65534:65534"
has "the build date is fixed" "${log}" "-e SOURCE_DATE_EPOCH=1559424892"
has "the seed is fixed" "${log}" "-e DETERMINISTIC_SEED=1"
has "the proxy's address passes through" "${log}" "-e HTTPS_PROXY"
has "the output is relabelled for the container" "${log}" "--volume ${case_dir}/talos-out:/out:Z"
lacks "no privilege" "${log}" "--privileged"
lacks "no /dev" "${log}" "/dev:/dev"
owner=$(cat "${case_dir}/chown.log" 2> /dev/null || true)
expect "the output belongs to the imager's user" "65534:65534 ${case_dir}/talos-out" "${owner}"

# The CA bundle: a copy of the builder's own, readable to the container, on every bake.
has "the builder's bundle is copied" "${case_dir}/install.log" \
    "-D -m 0644 /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem ${case_dir}/talos-ca/ca.pem"
has "the copy holds the bundle" "${case_dir}/talos-ca/ca.pem" "test bundle"
has "the copy is mounted read-only and relabelled" "${log}" "--volume ${case_dir}/talos-ca:/etc/imager-trust:ro,Z"
has "the file list is the copy" "${log}" "-e SSL_CERT_FILE=/etc/imager-trust/ca.pem"
has "the folder list is the copy" "${log}" "-e SSL_CERT_DIR=/etc/imager-trust"
lacks "the system bundle is not mounted" "${log}" "/etc/pki"
lacks "no shared relabel" "${log}" ":z"
lacks "no shared relabel after a comma" "${log}" ",z"

# Privileged: an imager before v1.13 attaches a loop device.
prepare privileged 2 privileged
run_talos 2
log=${case_dir}/podman.log
expect "a privileged run succeeds" 0 "${status}"
has "the marker gives it privilege" "${log}" "--privileged"
has "the marker gives it /dev" "${log}" "--volume /dev:/dev"
lacks "it runs as the image's root" "${log}" "--user"
absent "the privileged run changes no owner" "${case_dir}/chown.log"
has "the privileged run gets the bundle too" "${log}" "--volume ${case_dir}/talos-ca:/etc/imager-trust:ro,Z"
has "the privileged run sets SSL_CERT_DIR" "${log}" "-e SSL_CERT_DIR=/etc/imager-trust"
has "the privileged run copies the bundle" "${case_dir}/install.log" "-D -m 0644 /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"
lacks "the privileged run mounts no system bundle" "${log}" "/etc/pki"

# An image larger than the disk is refused before anything is written.
prepare large 2
run_talos 3
expect "an image too large fails" 1 "${status}"
has "both sizes are named" "${case_dir}/stderr" "the image of $((3 * mib)) bytes does not fit the disk of $((2 * mib)) bytes"
same "the target is untouched" "${case_dir}/target.orig" "${case_dir}/target"
absent "a refused image leaves no marker" "${case_dir}/built"

# An image smaller than the disk leaves the rest of it as it was.
prepare small 3
head -c $((3 * mib)) /dev/zero | tr '\0' '\377' > "${case_dir}/target"
head -c $((mib)) /dev/zero | tr '\0' '\377' > "${case_dir}/ff"
run_talos 2
expect "a smaller image succeeds" 0 "${status}"
length=$(wc -c < "${case_dir}/target")
expect "the target keeps its length" "$((3 * mib))" "$((length))"
if cmp -s -n $((2 * mib)) "${case_dir}/raw.copy" "${case_dir}/target"; then pass; else fail "the target's start is not the image"; fi
tail -c "${mib}" "${case_dir}/target" > "${case_dir}/tail"
same "the rest of the disk is as it was" "${case_dir}/ff" "${case_dir}/tail"
exists "the build leaves its marker" "${case_dir}/built"

# Two outputs: which one to write is not for the script to guess.
prepare two 4
run_talos "2 2"
expect "two images fail" 1 "${status}"
has "the count is named" "${case_dir}/stderr" "the imager wrote 2 raw images, not one"
same "the target is untouched by two images" "${case_dir}/target.orig" "${case_dir}/target"
absent "two images leave no marker" "${case_dir}/built"

if [ "${failures}" -ne 0 ]; then
    echo "el-talos: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "el-talos: all ${passes} checks passed"
