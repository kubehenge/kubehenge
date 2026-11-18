#!/bin/sh
# kubehenge-fips-boot's second hook (el/fips-module-setup.sh): the bind of the first goes once the
# check has run, so the booted node mounts /boot as ostree does.
if [ -f /tmp/kubehenge-fips-boot ]; then
    umount "${NEWROOT}/boot"
    rm -f /tmp/kubehenge-fips-boot
fi
