#!/bin/sh
# The memory of the runner a lane's VMs share, in one place. Sourced by the workload test, which
# works out the floors a guest's balloon may take it down to, and by test/unit/lane-sizing.sh,
# which holds each lane's VMs at those floors to the room left below.

# MiB of memory the runner has.
runner_mib=15983
# MiB of it the runner's own processes, the Proxmox VE container and the kernel's reserve keep.
runner_fixed_mib=3379
# MiB a QEMU process holds beyond its guest's memory.
qemu_overhead_mib=55
# MiB a lane must still have available with every balloon at its floor. Proxmox VE inflates them
# only once the runner is over 80 % memory use, so in practice the room is larger.
runner_headroom_mib=512
# MiB the guest's kernel keeps for itself: a node given 4096 MiB sees 3908. A balloon floor of a
# node's need plus this leaves the node the need as its own memory.
guest_reserve_mib=192

# The tests whose workload nodes get balloon floors: those whose peak leaves the runner no room
# with every node at its size (docs/testing.md).
balloon_tests='rke2-etcd rke2-machines'

# balloon_floor <need> <memory>: the least MiB a node of <memory> MiB may be ballooned down to, in
# steps of 8: its need and the guest's reserve, or <memory> where that is not less.
balloon_floor() {
    floor=$((($1 + guest_reserve_mib + 7) / 8 * 8))
    if [ "${floor}" -ge "$2" ]; then
        floor=$2
    fi
    echo "${floor}"
}
