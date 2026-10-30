#!/bin/sh
# Copies the end of the journals of the units a node boots and joins its cluster with into one plain
# file, which the QEMU guest agent hands out to Kubehenge with no more than VM.GuestAgent.FileRead:
# the journal itself is binary, and reading it otherwise takes running commands in the VM.
# kubehenge-boot-log.timer runs it from boot on. Each journal follows a line of its own, the mark
# and the journal's name, as Kubehenge's controller splits them.
#
#   boot-log.sh [<directory>]
set -eu

dir=${1:-/var/log/kubehenge}
mark='--- kubehenge boot log: '
lines=200
# Lines are cut to this many bytes, so the file stays within one read through the agent, however
# long a unit's lines are, and the file is replaced whole rather than grown, so it needs no rotation.
width=1024

umask 077
mkdir -p "${dir}"
next=${dir}/.boot.log.next
# journal <name> <journalctl's unit options>...
journal() {
    name=$1
    shift
    printf '%s%s\n' "${mark}" "journal of ${name}"
    journalctl --boot "$@" --lines "${lines}" --no-pager --output short-iso 2>&1 | cut -b "1-${width}"
}
{
    journal cloud-init --unit 'cloud-init*' --unit cloud-config --unit cloud-final
    for unit in containerd kubelet rke2-server rke2-agent k3s k3s-agent; do
        if systemctl cat "${unit}.service" > /dev/null 2>&1; then
            journal "${unit}" --unit "${unit}"
        fi
    done
} > "${next}"
mv -f "${next}" "${dir}/boot.log"
