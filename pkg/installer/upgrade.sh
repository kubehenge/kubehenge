#!/bin/sh
# An upgrade as it runs in the container, under the interface's job lock, once the installer pushed
# the new release beside the old: it sets the installed software aside, puts the new in place and
# restarts the interface on it. Should the interface not serve and keep running, the check the
# appliance's own updater makes, it puts the old software back and restarts that.
#   sh -c "$(cat upgrade.sh)" sh <root> <container ID> <true|false|""> <units> [<API URL> [<trusted proxies>]]
# <root> is "" but in the tests; the third argument stops (true) or starts (false) the checks of
# the API's certificate, or leaves them. An API URL replaces the one the appliance reaches Proxmox
# VE at. The trusted proxies, a comma-separated list, replace the reverse proxies the interface
# trusts. Either leaves its setting when empty. Exits 76 rolled back, with the reason on standard
# output, and 77 when putting the old software back failed too. An upgrade of its own that was cut
# off in the middle it undoes first. One the appliance's updater left it refuses with 78, as the
# interface's next start undoes that.
set -eu
R=$1 ctid=$2 insecure=$3 units=$4 url=${5:-} proxies=${6:-}
aside=$R/usr/local/share/.kubehenge-rollback bin=$R/usr/local/bin share=$R/usr/local/share
env=$R/etc/kubehenge/appliance.env ca=$R/etc/kubehenge/proxmox-ca.pem trusted=$R/usr/local/share/ca-certificates/proxmox-ca.crt

# roll_back puts the software set aside back and restarts the interface on it. Each step is
# checked by hand: set -e does not hold inside a function called as a condition.
roll_back() {
    cp -p "$aside/kubehenge" "$bin/kubehenge.old" && mv -f "$bin/kubehenge.old" "$bin/kubehenge" || return 1
    if [ -e "$aside/tofu" ]; then
        cp -p "$aside/tofu" "$bin/tofu.old" && mv -f "$bin/tofu.old" "$bin/tofu" || return 1
    fi
    # Without it, the old share directory was never moved.
    if [ -e "$aside/share" ]; then
        rm -rf "$share/kubehenge" && mv "$aside/share" "$share/kubehenge" || return 1
    fi
    for unit in $units; do
        if [ -e "$aside/units/$unit" ]; then
            cp -p "$aside/units/$unit" "$R/etc/systemd/system/$unit" || return 1
        else
            rm -f "$R/etc/systemd/system/$unit" || return 1
        fi
    done
    cp -p "$aside/appliance.env" "$env" || return 1
    if [ -n "$ca_changed" ]; then
        cp -p "$aside/proxmox-ca.pem" "$ca" || return 1
        if [ -e "$aside/proxmox-ca.crt" ]; then
            cp -p "$aside/proxmox-ca.crt" "$trusted" || return 1
        else
            rm -f "$trusted" || return 1
        fi
        update-ca-certificates > /dev/null || return 1
    fi
    systemctl daemon-reload && systemctl restart kubehenge.service || return 1
    systemctl stop kubehenge-headlamp.service || return 1
    rm -rf "$aside"
}

# What an upgrade cut off in the middle set aside is the only copy of the old software.
if [ -e "$aside/pending.json" ]; then
    echo "an upgrade by the updater in the appliance was cut off in the middle"
    exit 78
fi
if [ -e "$aside/shell-pending" ]; then
    ca_changed=$(cat "$aside/shell-pending")
    roll_back || exit 77
fi

rm -rf "$aside" "$share/kubehenge.new"
mkdir -p "$aside/units" "$share/kubehenge.new"
cp -p "$bin/kubehenge" "$env" "$aside/"
[ ! -e "$bin/tofu.new" ] || cp -p "$bin/tofu" "$aside/"
for unit in $units; do
    [ ! -e "$R/etc/systemd/system/$unit" ] || cp -p "$R/etc/systemd/system/$unit" "$aside/units/"
done
ca_changed=
if [ -e "$ca.new" ]; then
    ca_changed=1
    cp -p "$ca" "$aside/proxmox-ca.pem"
    [ ! -e "$trusted" ] || cp -p "$trusted" "$aside/proxmox-ca.crt"
fi
# From here until the new release serves, the old one is all in $aside.
echo "$ca_changed" > "$aside/shell-pending"

tar -xf "$R/tmp/share.tar" -C "$share/kubehenge.new" && rm "$R/tmp/share.tar"
mv "$share/kubehenge" "$aside/share"
mv "$share/kubehenge.new" "$share/kubehenge"
mv "$bin/kubehenge.new" "$bin/kubehenge"
[ ! -e "$bin/tofu.new" ] || mv "$bin/tofu.new" "$bin/tofu"
for unit in $units; do mv "$R/etc/systemd/system/$unit.new" "$R/etc/systemd/system/$unit"; done
if [ -n "$ca_changed" ]; then
    mv "$ca.new" "$ca"
    mv "$trusted.new" "$trusted"
    update-ca-certificates > /dev/null
fi
install -d -o root -g root -m 0755 "$R/var/lib/kubehenge-update"
install -d -o root -g kubehenge -m 0770 "$R/var/lib/kubehenge-update/requests"
install -d -o kubehenge -g kubehenge -m 0700 "$R/var/lib/kubehenge-keys"
grep -q "^KUBEHENGE_APPLIANCE_VMID=" "$env" || echo "KUBEHENGE_APPLIANCE_VMID=$ctid" >> "$env"
ip=$(ip -4 -o addr show dev eth0 | sed -n "s/.* inet \([0-9.]*\).*/\1/p" | head -n 1)
if [ -z "$ip" ]; then
    ip6=$(ip -6 -o addr show dev eth0 scope global | grep -v -e temporary -e deprecated -e tentative | sed -n "s/.* inet6 \([0-9A-Fa-f:]*\)\/.*/\1/p" | head -n 1)
    [ -z "$ip6" ] || ip="[$ip6]"
fi
grep -q "^KUBEHENGE_PUBLIC_URL=" "$env" || echo "KUBEHENGE_PUBLIC_URL=https://$ip" >> "$env"
if [ -n "$insecure" ]; then
    { grep -v "^KUBEHENGE_PROXMOX_INSECURE=" "$env" || true; [ "$insecure" = false ] || echo "KUBEHENGE_PROXMOX_INSECURE=true"; } > "$env.new"
    chmod 0644 "$env.new" && mv "$env.new" "$env"
fi
if [ -n "$url" ]; then
    { grep -v "^KUBEHENGE_PROXMOX_URL=" "$env" || true; echo "KUBEHENGE_PROXMOX_URL=$url"; } > "$env.new"
    chmod 0644 "$env.new" && mv "$env.new" "$env"
fi
if [ -n "$proxies" ]; then
    { grep -v "^KUBEHENGE_TRUSTED_PROXIES=" "$env" || true; [ "$proxies" = none ] || echo "KUBEHENGE_TRUSTED_PROXIES=$proxies"; } > "$env.new"
    chmod 0644 "$env.new" && mv "$env.new" "$env"
fi
[ -f "$R/etc/kubehenge/headlamp.env" ] ||
    echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$ip:8443/oidc-callback" > "$R/etc/kubehenge/headlamp.env"
systemctl daemon-reload
systemctl enable --now kubehenge-update.path > /dev/null 2>&1
systemctl restart kubehenge.service
# The next visit starts the new Headlamp.
systemctl stop kubehenge-headlamp.service

if reason=$("$bin/kubehenge" appliance health --cert "$R/etc/kubehenge/tls.crt" 2>&1); then
    rm -rf "$aside"
    exit 0
fi
printf "%s\n" "${reason#kubehenge: }"

if roll_back && sleep 5 && systemctl is-active --quiet kubehenge.service; then
    exit 76
fi
exit 77
