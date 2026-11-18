#!/bin/bash
# dracut module kubehenge-fips-boot, which node.sh installs into a FIPS node's image: ostree mounts
# /boot only once the node has switched to its root (ostree-system-generator), while dracut's fips
# module checks the kernel against its checksum in /boot before that. The module binds the physical
# root's /boot for that check, read-only, and takes it away again before the switch.

check() {
    return 255
}

depends() {
    echo fips ostree
}

install() {
    # 00-fips-boot.sh, then this, then 01-fips-noboot.sh, which runs the check.
    inst_hook pre-pivot 00 "${moddir}/kubehenge-fips-boot.sh"
    inst_hook pre-pivot 02 "${moddir}/kubehenge-fips-unboot.sh"
}
