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

# upgrade <root> [<API URL> [<trusted proxies>]]: the upgrade as install.sh runs it in the
# container; its output in $said and its exit status in $status.
upgrade() {
    status=0
    said=$(sh -c "${upgrade_script}" sh "$1" 900 '' "${units}" "${2:-}" "${3:-}") || status=$?
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

# --trusted-proxy replaces the list the appliance has, and without it the list stays.
release "${root}" 0.1.0 0
echo KUBEHENGE_TRUSTED_PROXIES=203.0.113.1 >> "${root}/etc/kubehenge/appliance.env"
upgrade "${root}"
check "no --trusted-proxy keeps the list" grep -qx KUBEHENGE_TRUSTED_PROXIES=203.0.113.1 "${root}/etc/kubehenge/appliance.env"
release "${root}" 0.1.0 0
echo KUBEHENGE_TRUSTED_PROXIES=203.0.113.1 >> "${root}/etc/kubehenge/appliance.env"
upgrade "${root}" '' 10.0.0.0/8,2001:db8::/32
check "--trusted-proxy replaces the list" grep -qx KUBEHENGE_TRUSTED_PROXIES=10.0.0.0/8,2001:db8::/32 "${root}/etc/kubehenge/appliance.env"
lists=$(grep -c '^KUBEHENGE_TRUSTED_PROXIES=' "${root}/etc/kubehenge/appliance.env")
check "and leaves one line" [ "${lists}" -eq 1 ]
release "${root}" 0.1.0 0
upgrade "${root}" '' none
lists=$(grep -c '^KUBEHENGE_TRUSTED_PROXIES=' "${root}/etc/kubehenge/appliance.env" || true)
check "--no-trusted-proxy removes the list" [ "${lists}" -eq 0 ]
check "the other settings stay" grep -qx KUBEHENGE_PROXMOX_URL=https://pve.example:8006 "${root}/etc/kubehenge/appliance.env"

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

# --proxmox-url replaces the API's address, and a rollback puts the old one back.
release "${root}" 0.1.0 0
upgrade "${root}" https://pve2.example:8006
check "a new API address is installed (status ${status}: ${said})" [ "${status}" -eq 0 ]
check "the settings name the new API address" grep -qx KUBEHENGE_PROXMOX_URL=https://pve2.example:8006 "${root}/etc/kubehenge/appliance.env"
moved=$(grep -c '^KUBEHENGE_PROXMOX_URL=' "${root}/etc/kubehenge/appliance.env")
check "the settings name one API address" [ "${moved}" -eq 1 ]
release "${root}" 0.1.0 1
upgrade "${root}" https://pve2.example:8006
check "a rolled-back release keeps the old API address" grep -qx KUBEHENGE_PROXMOX_URL=https://pve.example:8006 "${root}/etc/kubehenge/appliance.env"

# A unit only the new release has goes with it.
release "${root}" 0.1.0 1
rm "${root}/etc/systemd/system/kubehenge-headlamp-proxy.service"
upgrade "${root}"
check "a unit the old release lacks is removed again" [ ! -e "${root}/etc/systemd/system/kubehenge-headlamp-proxy.service" ]

# An upgrade of its own cut off in the middle, with the old release all in the aside directory and
# half of the new one in place: it is put back before anything is set aside again. The new release
# then fails, so the rollback shows what was set aside the second time.
release "${root}" 0.1.0 1
aside=${root}/usr/local/share/.kubehenge-rollback
mkdir -p "${aside}/units"
cp -p "${root}/usr/local/bin/kubehenge" "${root}/etc/kubehenge/appliance.env" "${aside}/"
cp -p "${root}/etc/systemd/system/kubehenge.service" "${aside}/units/"
mv "${root}/usr/local/share/kubehenge" "${aside}/share"
mkdir -p "${root}/usr/local/share/kubehenge"
echo half > "${root}/usr/local/share/kubehenge/VERSION"
printf '#!/bin/sh\necho half\n' > "${root}/usr/local/bin/kubehenge"
: > "${aside}/shell-pending"
upgrade "${root}"
check "the release that fails afterwards is rolled back (status ${status})" [ "${status}" -eq 76 ]
check "to the old binary, not the half-upgraded one" grep -qx 'echo old' "${root}/usr/local/bin/kubehenge"
check "and the old share directory" grep -qx 0.1.0 "${root}/usr/local/share/kubehenge/VERSION"

# One the appliance's own updater left is refused, changing nothing.
release "${root}" 0.1.0 0
mkdir -p "${aside}"
echo '{"from":"0.1.0","to":"0.2.0"}' > "${aside}/pending.json"
upgrade "${root}"
check "an upgrade the updater left is refused (status ${status})" [ "${status}" -eq 78 ]
check "saying why (${said})" [ "${said}" = "an upgrade by the updater in the appliance was cut off in the middle" ]
check "what it set aside is kept" [ -e "${aside}/pending.json" ]
check "and the installed binary too" grep -qx 'echo old' "${root}/usr/local/bin/kubehenge"
rm -rf "${aside}"

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
