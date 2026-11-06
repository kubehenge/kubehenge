#!/bin/sh
# Unit test of how appliance/install.sh finds the container template once Proxmox's mirror drops
# the pinned one: whether it notices, and which template the signed index gives it. Stand-ins
# serve the mirror and check the signature. Needs no Proxmox VE.
#
#   test/unit/install-template.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# is <what> <wanted> <got>
is() {
    if [ "$3" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"

# curl serves <root>/mirror for the mirror's URLs: a HEAD answers 404 for a missing file, and a
# download fails for one. sqv accepts the index unless <root>/forged exists.
bin=${root}/bin
mkdir -p "${bin}" "${root}/mirror/system"
cat > "${bin}/curl" << EOF
#!/bin/sh
out='' head=''
while [ \$# -gt 1 ]; do
    case "\$1" in
        -o) out=\$2; shift ;;
        -sI) head=1 ;;
        -w | --proxy | --proto | --retry) shift ;;
    esac
    shift
done
file=${root}/mirror/\${1#"${TEMPLATE_MIRROR}"/}
if [ -n "\${head}" ]; then
    if [ -f "\${file}" ]; then printf 200; else printf 404; fi
    exit 0
fi
[ -f "\${file}" ] || exit 22
cp "\${file}" "\${out}"
EOF
printf '#!/bin/sh\n[ ! -e "%s/forged" ]\n' "${root}" > "${bin}/sqv"
chmod +x "${bin}"/*
PATH=${bin}:${PATH}
download_proxy=''

# sum <n>: a SHA-512 made of the number n, padded with zeros.
sum() { printf "%0128d" "$1"; }
# index <entries>...: the mirror's index, of entries "<package> <version> <architecture> <file>".
# Each entry's SHA-512 is the length of its file name.
index() {
    for e in "$@"; do
        # shellcheck disable=SC2086 # each entry is four words on purpose.
        set -- ${e}
        printf 'Package: %s\nVersion: %s\nType: lxc\nArchitecture: %s\nLocation: system/%s\nsha512sum: %0128d\nDescription: a template\n a long description\n\n' \
            "$1" "$2" "$3" "$4" "${#4}"
    done | gzip > "${root}/mirror/aplinfo-pve-9.dat.gz"
    : > "${root}/mirror/aplinfo-pve-9.dat.asc"
}
# gone: what template_gone answers.
gone() {
    rc=0
    # shellcheck disable=SC2310 # template_gone is a predicate; its status is the answer.
    template_gone || rc=$?
    if [ "${rc}" -eq 0 ]; then echo gone; else echo there; fi
}
# from_index: what template_from_index prints, its refusal included.
from_index() {
    work=$(mktemp -d "${root}/work.XXXXXX")
    # shellcheck disable=SC2310 # a refusal is an answer here, printed like the rest.
    (template_from_index "${work}") 2>&1 || true
}

# Whether the pinned template is still there.
: > "${root}/mirror/system/${TEMPLATE}"
got=$(gone)
is "a pinned template the mirror has" there "${got}"
rm "${root}/mirror/system/${TEMPLATE}"
got=$(gone)
is "a pinned template the mirror dropped" gone "${got}"

# The newest amd64 Debian 13 standard template, by version order, not the order listed.
index "debian-13-standard 13.9-1 amd64 debian-13-standard_13.9-1_amd64.tar.zst" \
    "debian-13-standard 13.10-1 amd64 debian-13-standard_13.10-1_amd64.tar.zst" \
    "debian-13-standard 13.11-1 arm64 debian-13-standard_13.11-1_arm64.tar.zst" \
    "debian-12-standard 12.12-1 amd64 debian-12-standard_12.12-1_amd64.tar.zst" \
    "ubuntu-26.04-standard 26.04-1 amd64 debian-13-standard_13.12-1_amd64.tar.zst"
want=$(sum 40)
got=$(from_index)
is "the newest listed" "debian-13-standard_13.10-1_amd64.tar.zst ${want} system/debian-13-standard_13.10-1_amd64.tar.zst" "${got}"

# Refusals.
touch "${root}/forged"
got=$(from_index)
is "an index whose signature fails" "kubehenge: Proxmox's template index ${TEMPLATE_MIRROR}/aplinfo-pve-9.dat fails its signature check against ${template_keyring}" "${got}"
rm "${root}/forged"
index "debian-13-standard 13.9-1 amd64 ../../etc/passwd" "debian-13-standard 13.9-1 arm64 debian-13-standard_13.9-1_arm64.tar.zst"
got=$(from_index)
is "an index without a usable amd64 template" "kubehenge: Proxmox's template index lists no amd64 Debian 13 standard template" "${got}"
rm "${root}/mirror/aplinfo-pve-9.dat.asc"
got=$(from_index)
is "an index without its signature" "kubehenge: could not download Proxmox's template index ${TEMPLATE_MIRROR}/aplinfo-pve-9.dat.asc" "${got}"

if [ "${failures}" -ne 0 ]; then
    echo "install-template: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-template: all ${passes} checks passed"
