#!/bin/sh
# Unit test of how appliance/install.sh --certificate keeps this node's Kubernetes entry in
# Proxmox VE's interface leading to the appliance. A stand-in for kubehenge-pve-entry records its
# arguments. Needs no Proxmox VE.
#
#   test/unit/install-entry.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT

passes=0 failures=0
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
# expect_empty <what> <command>...: whether the command prints nothing.
expect_empty() {
    what=$1
    shift
    got=$("$@" 2>&1 || true)
    if [ -z "${got}" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted nothing, got: %s\n' "${what}" "${got}"
        failures=$((failures + 1))
    fi
}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"

pve_entry_conf=${root}/kubehenge-pve-entry.conf
pve_entry_self=${root}/kubehenge-pve-entry
cat > "${pve_entry_self}" << EOF
#!/bin/sh
printf '%s\n' "\$*" > "${root}/entry-args"
[ ! -e "${root}/entry-fails" ] || exit 1
EOF
chmod +x "${pve_entry_self}"
args() { cat "${root}/entry-args"; }

expect_empty "no entry on this node: nothing done" update_pve_entry https://192.0.2.41
printf 'url=https://192.0.2.40\n' > "${pve_entry_conf}"
without_script() {
    pve_entry_self=${root}/missing
    update_pve_entry "$@"
    pve_entry_self=${root}/kubehenge-pve-entry
}
expect_empty "an entry without its script: nothing done" without_script https://192.0.2.41

expect_in "an entry leading to the old address is pointed at the new one" "kubehenge-pve-entry --url https://192.0.2.41" update_pve_entry https://192.0.2.41
expect_in "through the entry's own script" "--url https://192.0.2.41" args
printf 'url=https://192.0.2.41\n' > "${pve_entry_conf}"
rm -f "${root}/entry-args"
expect_empty "an entry leading to the appliance already: left as it is" update_pve_entry https://192.0.2.41
expect_empty "and its script not run" sh -c "cat '${root}/entry-args' 2> /dev/null || true"
touch "${root}/entry-fails"
expect_in "a failed update is said, without stopping" "the entry still leads to https://192.0.2.41" update_pve_entry https://kubehenge.example.org

if [ "${failures}" -ne 0 ]; then
    echo "install-entry: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "install-entry: all ${passes} checks passed"
