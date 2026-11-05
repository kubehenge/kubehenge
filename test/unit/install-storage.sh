#!/bin/sh
# Unit test of how appliance/install.sh chooses the storages for the appliance's disks and the
# container template, and how it checks what it downloads: a stand-in for pvesm and files on disk.
# Needs no Proxmox VE.
#
#   test/unit/install-storage.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
bin=${root}/bin
mkdir "${bin}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# expect <what> <wanted> <command>...: whether the command prints what is wanted.
expect() {
    what=$1 wanted=$2
    shift 2
    got=$("$@" || true)
    if [ "${got}" = "${wanted}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "${what}" "${wanted}" "${got}"
        failures=$((failures + 1))
    fi
}
# expect_in <what> <text wanted> <command>...: whether the command's output holds the text.
expect_in() {
    what=$1 wanted=$2
    shift 2
    got=$("$@" 2>&1 || true)
    case "${got}" in
        *"${wanted}"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted it to say: %s\n  it said: %s\n' "${what}" "${wanted}" "${got}"
            failures=$((failures + 1))
            ;;
    esac
}

# pvesm answers from $root/status, as `pvesm status --content <content> --enabled 1` prints it for
# that content, and from $root/list-<storage> for `pvesm list`. A disabled storage is left out, as
# Proxmox VE leaves it out.
cat > "${bin}/pvesm" << EOF
#!/bin/sh
case "\$1" in
    status)
        printf 'Name Type Status Total Used Available %%\n'
        awk -v content="\$3" '\$1 == content { \$1 = ""; print substr(\$0, 2) }' "${root}/status"
        ;;
    list) cat "${root}/list-\$2" 2> /dev/null ;;
    *) exit 2 ;;
esac
EOF
chmod +x "${bin}/pvesm"
PATH=${bin}:${PATH}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"
tty=${root}/no-terminal
said() { cat "${root}/said"; }
# choose <content> [<what>]: the storage choose_storage settles on, what it said kept in $root/said.
disks="the appliance's disks"
choose() { choose_storage "$1" "${2:-${disks}}" --storage 2> "${root}/said"; }

# One storage takes container disks, another is inactive.
cat > "${root}/status" << 'EOF'
rootdir local-lvm lvmthin active 100000000 20000000 80000000 20.00%
rootdir nfs-down nfs inactive 0 0 0 0.00%
vztmpl nas nfs active 900000000 100000000 800000000 11.11%
EOF
expect "the only active storage that takes it" local-lvm choose rootdir
expect_in "it says why" "storage local-lvm for the appliance's disks, the only one here that takes it (--storage names another)" said
# shellcheck disable=SC2310 # takes is a predicate; its status is the answer.
takes_answer() { if takes "$1" "$2"; then echo yes; else echo no; fi; }
expect "an active storage that takes it" yes takes_answer local-lvm rootdir
expect "an inactive one does not" no takes_answer nfs-down rootdir
expect "one that takes other content does not" no takes_answer nas rootdir
expect "a storage Proxmox VE lists as disabled, so not at all, does not" no takes_answer local rootdir

# Several.
cat > "${root}/status" << 'EOF'
rootdir local-lvm lvmthin active 100000000 80000000 20000000 80.00%
rootdir ceph rbd active 900000000 100000000 800000000 11.11%
rootdir zfs zfspool active 500000000 100000000 400000000 20.00%
EOF
expect "without a terminal, the one with the most free space" ceph choose rootdir
expect_in "it says which and why" "storage ceph for the appliance's disks, the one with the most free space" said
tty=${root}/answers
printf '3\n' > "${tty}"
expect "picked on the terminal, listed by free space" local-lvm choose rootdir
expect_in "the terminal lists them with their free space" "1) ceph (rbd, 762.9 GiB free)" said
expect_in "and asks for what" "Which one for the appliance's disks? [1]" said
printf '\n' > "${tty}"
expect "the first, by default" ceph choose rootdir
printf '0\nzfs\n4\n2\n' > "${tty}"
expect "asked again until the answer is one of them" zfs choose rootdir
tty=${root}/no-terminal

: > "${root}/status"
expect "none takes it: nothing" "" choose rootdir

# The template, where a storage holds it already.
cat > "${root}/status" << EOF
vztmpl local dir active 100000000 20000000 80000000 20.00%
vztmpl nas nfs active 900000000 100000000 800000000 11.11%
EOF
printf 'Volid Format Type Size VMID\nlocal:vztmpl/debian-12-standard_12.12-1_amd64.tar.zst tzst vztmpl 1 \n' > "${root}/list-nas"
expect "no storage holds this template" "" template_held
printf 'Volid Format Type Size VMID\nlocal:vztmpl/%s tzst vztmpl 1 \n' "${TEMPLATE}" > "${root}/list-local"
expect "the storage that holds it, though another has more room" local template_held

# Downloads are checked against their SHA-256 or SHA-512.
printf 'release\n' > "${root}/source"
sha256=$(sha256sum "${root}/source" | cut -d ' ' -f 1)
sha512=$(sha512sum "${root}/source" | cut -d ' ' -f 1)
wrong=$(printf 'other\n' | sha512sum | cut -d ' ' -f 1)
# A curl built for Windows reads a file URL by its Windows path.
source_path=$(cygpath -m "${root}/source" 2> /dev/null || printf '%s' "${root}/source")
# fetched <checksum>: what fetch says fetching the file with it, stopping where it fails.
fetched() { (fetch "file:///${source_path#/}" "${root}/got" "$1" "the file" '=file') 2>&1; }
expect_in "a SHA-256 that matches" "the file: checksum verified" fetched "${sha256}"
expect_in "a SHA-512 that matches" "the file: checksum verified" fetched "${sha512}"
expect_in "a SHA-512 that does not: stops" "does not match its checksum" fetched "${wrong}"

if [ "${failures}" -ne 0 ]; then
    echo "install-storage: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-storage: all ${passes} checks passed"
