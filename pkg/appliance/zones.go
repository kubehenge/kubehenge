package appliance

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// labelZones sets the management nodes' zones after an apply, through the admin kubeconfig, which
// may add a label a new node lacks. It returns the nodes Proxmox VE lists no VM for, whose labels
// stay as they are.
func (a *Appliance) labelZones(ctx context.Context, admin []byte) ([]string, error) {
	kube, err := a.managementClient(admin)
	if err != nil {
		return nil, err
	}
	return a.followZones(ctx, kube, "")
}

// followZones points the management nodes' zone labels at where their VMs run, as
// backupstore.FollowZones says.
func (a *Appliance) followZones(ctx context.Context, kube kubernetes.Interface, zone string) ([]string, error) {
	return backupstore.FollowZones(ctx, kube, a.readVMHosts, zone, a.Log) //nolint:wrapcheck // FollowZones says what failed
}

func (a *Appliance) readVMHosts(ctx context.Context, token string) (*backupstore.VMIndex, error) {
	if a.vmHosts != nil {
		return a.vmHosts(ctx, token)
	}
	c, err := pve.Connect(pve.Config{URL: a.ProxmoxURL, Token: token, CAFile: a.ProxmoxCAFile, Insecure: a.ProxmoxInsecure})
	if err != nil {
		return nil, err //nolint:wrapcheck // FollowZones says what it was for
	}
	return backupstore.ReadVMIndex(ctx, c) //nolint:wrapcheck // ReadVMIndex says what failed
}
