package templates

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// What a node of kubeadm, RKE2 or k3s keeps for Kubehenge to read through its guest agent with
// VM.GuestAgent.FileRead alone: cloud-init's output, and the copy node/boot-log.sh makes of the
// end of each journal of the units it boots and joins with, every journal after a line of
// BootLogMark and its name.
const (
	BootLogFile     = "/var/log/kubehenge/boot.log"
	BootLogMark     = "--- kubehenge boot log: "
	CloudInitOutput = "/var/log/cloud-init-output.log"
	// BootLogRead is more than node/boot-log.sh ever writes: a few hundred lines of each journal,
	// each cut to a KiB.
	BootLogRead = 2 << 20
)

// bootLogUnit is the timer that has a node write BootLogFile from boot on.
const bootLogUnit = "kubehenge-boot-log.timer"

// checkBootLog proves, through Proxmox VE as Kubehenge's controller reads them, that the guest
// agent hands out the node's boot log and cloud-init's output: the agent's allowed commands and
// its SELinux policy both decide that inside the image, where no check of the node's own sees it.
func (b *Builder) checkBootLog(ctx context.Context, vm *proxmox.VirtualMachine) error {
	const largest = 999999999 // Proxmox VE's own upper bound
	if vm.VMID > largest {
		return fmt.Errorf("VM ID %d is outside Proxmox VE's range", vm.VMID)
	}
	id := int64(vm.VMID)
	log, _, err := pve.GuestFileRead(ctx, b.Client, vm.Node, id, BootLogFile, BootLogRead)
	if err != nil {
		return fmt.Errorf("the node's boot log: %w", err)
	}
	if !strings.Contains(log, BootLogMark+"journal of cloud-init\n") {
		return errors.New("the node's boot log holds no journal of cloud-init")
	}
	if _, _, err := pve.GuestFileRead(ctx, b.Client, vm.Node, id, CloudInitOutput, 1); err != nil {
		return fmt.Errorf("cloud-init's output: %w", err)
	}
	b.logf("VM %d hands out its boot log and cloud-init's output through the guest agent", vm.VMID)
	return nil
}
