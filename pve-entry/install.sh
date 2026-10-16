#!/bin/sh
# Adds a Kubernetes entry to this Proxmox VE node's web interface, leading to Kubehenge's appliance.
# Proxmox VE has no supported way to extend its interface, so this adds one line to the page
# template pve-manager ships, loading kubehenge-entry.js; an update of pve-manager replaces the
# template, and an apt hook puts the line back. Run as root on every node, since each node serves
# its own interface:
#
#   sh install.sh --url https://<appliance>    add or update the entry
#   sh install.sh --reapply                     put the line back if an update removed it (the apt hook)
#   sh install.sh --remove                      take everything out again
set -eu

template=/usr/share/pve-manager/index.html.tpl
js=/usr/share/pve-manager/js/kubehenge-entry.js
conf=/etc/kubehenge-pve-entry.conf
hook=/etc/apt/apt.conf.d/80kubehenge-pve-entry
self=/usr/local/sbin/kubehenge-pve-entry
marker='<!-- kubehenge-pve-entry -->'
anchor='pvemanagerlib.js'
# The appliance's address goes into a script, so it is held to the characters an https:// URL
# of a host name or address needs: no quote, backslash or angle bracket gets in.
url_pattern='^https://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$'

fail() {
    echo "kubehenge-pve-entry: $*" >&2
    exit 1
}
script=$(readlink -f "$0")
here=$(dirname "${script}")

uid=$(id -u)
[ "${uid}" -eq 0 ] || fail "run this as root on a Proxmox VE node"
[ -f "${template}" ] || fail "${template} not found: this is not a Proxmox VE node"

write_js() {
    tmp=$(mktemp "${js}.XXXXXX")
    sed "s|@APPLIANCE_URL@|$1|" "${self}.js" > "${tmp}"
    chmod 0644 "${tmp}"
    mv "${tmp}" "${js}"
}

# add_line puts the script line after pvemanagerlib.js's; the caller checks that it is not there yet.
add_line() {
    grep -qF "${anchor}" "${template}" || fail "${template} no longer loads ${anchor}; this Proxmox VE release is not supported"
    tmp=$(mktemp "${template}.XXXXXX")
    awk -v m="${marker}" -v a="${anchor}" '
        { print }
        index($0, a) && !done { print "    <script type=\"text/javascript\" src=\"/pve2/js/kubehenge-entry.js?v=[% version %]\"></script> " m; done = 1 }
    ' "${template}" > "${tmp}"
    chmod --reference="${template}" "${tmp}"
    mv "${tmp}" "${template}"
}

case "${1:-}" in
    --url)
        url=${2:-}
        printf '%s' "${url}" | grep -Eq "${url_pattern}" || fail "--url takes the appliance's https:// address, such as https://192.0.2.40"
        # Run from the release or from the installed copy, which keeps the script beside itself.
        if [ "${script}" != "${self}" ]; then
            install -m 0755 "${script}" "${self}"
            install -m 0644 "${here}/kubehenge-entry.js" "${self}.js"
        fi
        printf 'url=%s\n' "${url}" > "${conf}"
        write_js "${url}"
        grep -qF "${marker}" "${template}" || add_line
        printf 'DPkg::Post-Invoke { "if [ -x %s ]; then %s --reapply || true; fi"; };\n' "${self}" "${self}" > "${hook}"
        systemctl reload pveproxy
        echo "Kubernetes entry added; reload Proxmox VE's interface in the browser."
        ;;
    --reapply)
        [ -f "${conf}" ] || exit 0
        url=$(sed -n 's/^url=//p' "${conf}")
        printf '%s' "${url}" | grep -Eq "${url_pattern}" || fail "${conf} holds no valid url"
        [ -f "${js}" ] || write_js "${url}"
        if ! grep -qF "${marker}" "${template}"; then
            add_line
            logger -t kubehenge-pve-entry "an update replaced ${template}; the Kubernetes entry was added again"
            systemctl reload pveproxy
        fi
        ;;
    --remove)
        if grep -qF "${marker}" "${template}"; then
            tmp=$(mktemp "${template}.XXXXXX")
            grep -vF "${marker}" "${template}" > "${tmp}"
            chmod --reference="${template}" "${tmp}"
            mv "${tmp}" "${template}"
        fi
        rm -f "${js}" "${conf}" "${hook}" "${self}" "${self}.js"
        systemctl reload pveproxy
        echo "Kubernetes entry removed."
        ;;
    *)
        sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2
        exit 2
        ;;
esac
