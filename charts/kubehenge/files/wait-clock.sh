#!/bin/sh
# wait-clock.sh: holds the bootstrap until chrony has set the clock, for at most three minutes.
#
# A VM's clock starts from its host's. A host that is hours off would have the node sign its
# certificates with a start date in the future, which every component refuses once chrony corrects
# the clock. The caller ends the bootstrap when this fails (`|| exit 1` in its command list).
set -u
# Boot loaded the modules and started chronyd before cloud-init wrote the drop-ins with the cluster's
# time servers and the hypervisor's clock (ptp_kvm, the source when no server answers).
systemctl restart systemd-modules-load.service
systemctl restart chronyd
# 18 tries, ten seconds apart. A correction over 0.1 seconds means the clock is not set yet.
if timeout 190 chronyc waitsync 18 0.1 0 10 > /dev/null; then
    exit 0
fi
message="clock not synced: chrony trusted neither a time server nor the host clock (/dev/ptp0) within three minutes. Check that the nodes reach their time servers on UDP 123, that the VM has /dev/ptp0 and that the Proxmox VE host's clock is right."
echo "${message}" >&2
logger -p user.err -t kubehenge "${message}"
echo "kubehenge: ${message}" > /dev/console 2> /dev/null || true
exit 1
