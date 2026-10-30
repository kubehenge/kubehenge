#!/bin/sh
# Unit test of the copy a node of kubeadm, RKE2 or k3s keeps of its boot's journals for the guest
# agent to hand out (pkg/templates/node/boot-log.sh): stand-ins for journalctl and systemctl, and a
# node with kubelet and RKE2's server, then one with k3s alone.
#
#   test/unit/boot-log.sh
set -eu

here=$(dirname "$0")
script=${here}/../../pkg/templates/node/boot-log.sh
root=$(mktemp -d)
bin=${root}/bin
dir=${root}/log/kubehenge
mkdir "${bin}"
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
# has <what> <text>: whether the copy holds the text.
has() {
    if grep -qF -- "$2" "${dir}/boot.log"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted the copy to say: %s\n  it said:\n' "$1" "$2"
        cat "${dir}/boot.log"
        failures=$((failures + 1))
    fi
}
# lacks <what> <text>: whether the copy leaves the text out.
lacks() {
    if grep -qF -- "$2" "${dir}/boot.log"; then
        printf 'FAIL: %s\n  wanted the copy not to say: %s\n' "$1" "$2"
        failures=$((failures + 1))
    else
        passes=$((passes + 1))
    fi
}
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

# journalctl says what it was asked, then a line far longer than any the copy keeps.
cat > "${bin}/journalctl" << 'EOF'
#!/bin/sh
echo "journalctl $*"
printf 'long:%04000d\n' 0
EOF
# systemctl knows the units in UNITS.
cat > "${bin}/systemctl" << 'EOF'
#!/bin/sh
[ "$1" = cat ] || exit 2
case " ${UNITS} " in
    *" ${2%.service} "*) exit 0 ;;
esac
exit 1
EOF
chmod +x "${bin}/journalctl" "${bin}/systemctl"

copy() {
    UNITS=$1 PATH="${bin}:${PATH}" sh "${script}" "${dir}"
}

code=0
UNITS="kubelet rke2-server" PATH="${bin}:${PATH}" sh "${script}" "${dir}" || code=$?
truth "the copy is written" [ "${code}" = 0 ]
first=$(head -n 1 "${dir}/boot.log")
truth "cloud-init's journal comes first" [ "${first}" = "--- kubehenge boot log: journal of cloud-init" ]
has "of every stage of cloud-init" "journalctl --boot --unit cloud-init* --unit cloud-config --unit cloud-final --lines 200 --no-pager --output short-iso"
has "kubelet's journal is copied" "--- kubehenge boot log: journal of kubelet"
has "from this boot" "journalctl --boot --unit kubelet --lines 200"
has "RKE2's server's journal is copied" "journal of rke2-server"
lacks "a unit the node lacks is left out" "journal of k3s"
lacks "containerd, which RKE2 runs inside itself, is left out" "journal of containerd"
longer=$(LC_ALL=C awk 'length > 1024' "${dir}/boot.log")
truth "no line is longer than a KiB" [ -z "${longer}" ]
truth "the long lines are kept, cut" grep -q '^long:0' "${dir}/boot.log"
truth "nothing half-written is left beside it" [ ! -e "${dir}/.boot.log.next" ]

# Only where the file system keeps modes, which Windows' does not.
chmod 600 "${root}/bin/systemctl"
kept=$(stat -c %a "${root}/bin/systemctl")
if [ "${kept}" = 600 ]; then
    file_mode=$(stat -c %a "${dir}/boot.log")
    dir_mode=$(stat -c %a "${dir}")
    truth "root alone reads the copy" [ "${file_mode}" = 600 ]
    truth "and its directory" [ "${dir_mode}" = 700 ]
fi
chmod 700 "${root}/bin/systemctl"

copy "k3s"
has "the next copy replaces the last" "journal of k3s"
lacks "rather than growing it" "journal of rke2-server"

echo "boot-log: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
