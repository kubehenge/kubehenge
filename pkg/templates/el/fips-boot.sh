#!/bin/sh
# kubehenge-fips-boot's first hook (el/fips-module-setup.sh): the physical root's /boot, where
# ostree keeps the kernel and its checksum, bound read-only at the new root's /boot, which dracut's
# fips module reads.
command -v ismounted > /dev/null || . /lib/dracut-lib.sh

if [ "$(cat /proc/sys/crypto/fips_enabled 2> /dev/null)" = 1 ] && [ -d "${NEWROOT}/sysroot/boot/ostree" ] &&
    ! ismounted "${NEWROOT}/boot"; then
    mount --bind -o ro "${NEWROOT}/sysroot/boot" "${NEWROOT}/boot" && : > /tmp/kubehenge-fips-boot
fi
