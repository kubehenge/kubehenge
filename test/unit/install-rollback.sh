#!/bin/sh
# Unit test of how appliance/install.sh --upgrade puts a release in place in the container and,
# when its interface does not serve, the old release back: the script it runs there, on a root of
# its own, with stand-ins for systemd and for the two releases' binaries. Needs no Proxmox VE.
#
#   test/unit/install-rollback.sh
set -eu

here=$(dirname "$0")
installer=${here}/../../appliance/install.sh
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT

passes=0 failures=0
# check <what> <command>...: whether the command succeeds.
check() {
    what=$1
    shift
    if "$@"; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n' "${what}"
        failures=$((failures + 1))
    fi
}

KUBEHENGE_INSTALL_FUNCTIONS=1
# shellcheck source=../../appliance/install.sh
. "${installer}"

# Stand-ins for what the container has: systemd, which records what it is asked; install, which
# owns nothing outside a container; the container's address; the trust store's update; and a
# sleep that does not.
bin=${work}/bin
mkdir -p "${bin}"
cat > "${bin}/systemctl" << EOF
#!/bin/sh
echo "\$*" >> "${work}/systemctl.log"
EOF
cat > "${bin}/install" << 'EOF'
#!/bin/sh
for last; do :; done
mkdir -p "${last}"
EOF
printf '#!/bin/sh\necho "2: eth0    inet 192.0.2.40/24 brd 192.0.2.255 scope global eth0"\n' > "${bin}/ip"
printf '#!/bin/sh\n' > "${bin}/update-ca-certificates"
printf '#!/bin/sh\n' > "${bin}/sleep"
chmod +x "${bin}"/*
PATH=${bin}:${PATH}

# release <root> <version> <health>: an installed release of that version in <root>, and the next
# one staged beside it as stage_software leaves it, whose binary's health check exits <health>.
# The old release has a unit the new one drops, and the new one a unit the old one lacks.
release() {
    r=$1
    rm -rf "${r}" "${work}/systemctl.log"
    mkdir -p "${r}/usr/local/bin" "${r}/usr/local/share/kubehenge/appliance" "${r}/etc/systemd/system" "${r}/etc/kubehenge" "${r}/tmp"
    printf '#!/bin/sh\necho old\n' > "${r}/usr/local/bin/kubehenge"
    # shellcheck disable=SC2016 # expanded by the stand-in when it runs
    printf '#!/bin/sh\n[ "$1 $2" = "appliance health" ] || exit 0\necho "kubehenge: the interface did not answer within 2m0s" >&2\nexit %s\n' "$3" \
        > "${r}/usr/local/bin/kubehenge.new"
    chmod +x "${r}/usr/local/bin/kubehenge" "${r}/usr/local/bin/kubehenge.new"
    echo "$2" > "${r}/usr/local/share/kubehenge/VERSION"
    echo KUBEHENGE_PROXMOX_URL=https://pve.example:8006 > "${r}/etc/kubehenge/appliance.env"
    mkdir -p "${work}/new"
    rm -rf "${work}/new"/*
    echo next > "${work}/new/VERSION"
    (cd "${work}/new" && tar -cf "${r}/tmp/share.tar" .)
    for unit in ${units}; do
        echo "old ${unit}" > "${r}/etc/systemd/system/${unit}"
        echo "new ${unit}" > "${r}/etc/systemd/system/${unit}.new"
    done
}

# upgrade <root>: the upgrade as install.sh runs it in the container; its output in $said and its
# exit status in $status.
upgrade() {
    status=0
    said=$(sh -c "${upgrade_script}" sh "$1" 900 '' "${units}") || status=$?
}

root=${work}/root

# The new release serves: it stays, and nothing is kept aside.
release "${root}" 0.1.0 0
upgrade "${root}"
check "a release that serves is installed (status ${status}: ${said})" [ "${status}" -eq 0 ]
check "its share directory is in place" grep -qx next "${root}/usr/local/share/kubehenge/VERSION"
check "its binary is in place" grep -q 'appliance health' "${root}/usr/local/bin/kubehenge"
check "its units are in place" grep -qx 'new kubehenge.service' "${root}/etc/systemd/system/kubehenge.service"
check "nothing is left aside" [ ! -e "${root}/usr/local/share/.kubehenge-rollback" ]
check "the appliance's settings gained its ID" grep -qx KUBEHENGE_APPLIANCE_VMID=900 "${root}/etc/kubehenge/appliance.env"
check "the interface was restarted" grep -qx 'restart kubehenge.service' "${work}/systemctl.log"

# The new release does not serve: the old one is put back, the reason told.
release "${root}" 0.1.0 1
upgrade "${root}"
check "a release that does not serve is rolled back (status ${status})" [ "${status}" -eq 76 ]
check "the reason is told (${said})" [ "${said}" = "the interface did not answer within 2m0s" ]
check "the old share directory is back" grep -qx 0.1.0 "${root}/usr/local/share/kubehenge/VERSION"
check "the old binary is back" grep -qx 'echo old' "${root}/usr/local/bin/kubehenge"
check "the old units are back" grep -qx 'old kubehenge.service' "${root}/etc/systemd/system/kubehenge.service"
settings=$(cat "${root}/etc/kubehenge/appliance.env")
check "the old settings are back" [ "${settings}" = KUBEHENGE_PROXMOX_URL=https://pve.example:8006 ]
check "nothing is left aside" [ ! -e "${root}/usr/local/share/.kubehenge-rollback" ]
restarts=$(grep -c 'restart kubehenge.service' "${work}/systemctl.log")
check "the old interface was restarted" [ "${restarts}" -eq 2 ]

# A unit only the new release has goes with it.
release "${root}" 0.1.0 1
rm "${root}/etc/systemd/system/kubehenge-headlamp-proxy.service"
upgrade "${root}"
check "a unit the old release lacks is removed again" [ ! -e "${root}/etc/systemd/system/kubehenge-headlamp-proxy.service" ]

# Putting the old release back fails: told apart.
release "${root}" 0.1.0 1
cat > "${bin}/systemctl" << EOF
#!/bin/sh
echo "\$*" >> "${work}/systemctl.log"
[ "\$1" != is-active ]
EOF
upgrade "${root}"
check "a rollback that fails is told apart (status ${status})" [ "${status}" -eq 77 ]

echo "install-rollback: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ]
