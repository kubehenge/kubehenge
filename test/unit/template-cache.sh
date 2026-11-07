#!/bin/sh
# Unit test of the config rewrites test/e2e/ci/templates.sh wraps a cached template's backup in:
# vzdump leaves out a disk set to backup=0, so the backup clears the option and the restore sets it
# again on the same drives, giving back the template's config line for line.
#
#   test/unit/template-cache.sh
set -eu

here=$(dirname "$0")
# shellcheck source=../e2e/ci/templates.sh
. "${here}/../e2e/ci/templates.sh"

failures=0
fail() {
    printf '%s\n' "$*" >&2
    failures=$((failures + 1))
}

config='boot: order=scsi0
cores: 2
efidisk0: local:100/base-100-disk-1.raw,efitype=4m,size=528K
name: kubehenge-k3s-almalinux-10-build-202611071744
scsi0: local:100/base-100-disk-0.raw,backup=0,discard=on,size=8G,ssd=1
scsi1: local:100/base-100-disk-2.raw,backup=0
tags: build-202611071744;k3s;kubehenge;os-almalinux-10
template: 1

[pending]
scsi2: local:100/base-100-disk-3.raw,backup=0'

drives=$(printf '%s\n' "${config}" | no_backup_drives | tr '\n' ' ')
[ "${drives}" = 'scsi0 scsi1 ' ] || fail "no_backup_drives gave '${drives}', not 'scsi0 scsi1 '"

backed=$(printf '%s\n' "${config}" | backed_up)
case ${backed} in
    *backup=0*) fail "backed_up kept a drive out of backups: ${backed}" ;;
    *) ;;
esac

# shellcheck disable=SC2086 # one drive per word
restored=$(printf '%s\n' "${backed}" | backup_off ${drives})
expected=$(printf '%s\n' "${config}" | sed 's/^\(scsi2: .*\),backup=0$/\1/')
[ "${restored}" = "${expected}" ] || fail "backup_off gave
${restored}
not
${expected}"

unchanged=$(printf '%s\n' "${config}" | backup_off)
[ "${unchanged}" = "${config}" ] || fail "backup_off with no drives changed the config"

if [ "${failures}" -gt 0 ]; then
    printf '%s failed\n' "${failures}"
    exit 1
fi
echo 'template-cache: ok'
