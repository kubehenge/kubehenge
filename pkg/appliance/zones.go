package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// zonesSecret holds the Proxmox VE token that reads where the management cluster's VMs run. The
// storage chart makes it on every storage.
const (
	zonesSecret = backupstore.Service + "-zones"
	zonesKey    = "token"
)

// volumeZone annotates the store's Deployment, from the storage chart, with the Proxmox VE node
// whose own storage holds the backup volume. Shared storage leaves it out.
const volumeZone = "kubehenge.dev/volume-zone"

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

// followZones points each management node's zone labels at the Proxmox VE node its VM runs on now.
// The appliance is their only owner: the machine config sets only the region, so no kubelet start
// puts back a zone that HA or an administrator moved the VM away from. On a node's own storage the
// CSI plugin attaches the backup volume to a node with the disk's zone, and a stale label lets the
// store's pod land on a VM elsewhere, where the attach fails.
//
// A node Proxmox VE lists no VM for keeps its labels and is returned: its VM is gone, so its node
// is not ready and takes no pod. With zone, the disk's node, set, the call fails only where no
// management VM runs there, since the store then has nowhere to start. Of the labels, only those
// of nodes in or claiming that zone must be set for the store to land right.
//
// A node's VM is the one with its SMBIOS UUID (vmsByUUID), whatever the VM's name.
//
// The label is all that goes stale. The plugin finds a node's VM by its ID or SMBIOS UUID at each
// attach, and tells the kubelet the zone it reads from this label whenever it registers, so its
// node pod needs no restart. Only the static backup volume uses the plugin's storage class, so no
// volume is ever placed by the zones the node registered with.
func (a *Appliance) followZones(ctx context.Context, kube kubernetes.Interface, zone string) ([]string, error) {
	secret, err := kube.CoreV1().Secrets(backupstore.Namespace).Get(ctx, zonesSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the token that reads where the management VMs run: %w", err)
	}
	hosts, err := a.readVMHosts(ctx, string(secret.Data[zonesKey]))
	if err != nil {
		return nil, fmt.Errorf("reading where the management VMs run: %w", err)
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("the management cluster's nodes: %w", err)
	}
	var unknown []string
	inZone := false
	for i := range nodes.Items {
		n := &nodes.Items[i]
		host := hosts[strings.ToLower(n.Status.NodeInfo.SystemUUID)]
		if host == "" {
			unknown = append(unknown, n.Name)
			continue
		}
		inZone = inZone || host == zone
		if err = a.setZone(ctx, kube, n, host); err == nil {
			continue
		}
		if zone == "" || host == zone || n.Labels[corev1.LabelTopologyZone] == zone {
			return unknown, err
		}
		if a.Log != nil {
			a.Log.Warn("a management node's zone label was not set", "node", n.Name, "error", err)
		}
	}
	if zone != "" && !inZone {
		// Proxmox VE answered, so the message keeps off the start of a failed read's.
		return unknown, fmt.Errorf("no management VM runs on Proxmox VE node %s, which holds the backup volume", zone)
	}
	return unknown, nil
}

// setZone labels node n with the Proxmox VE node host, the beta zone label too where it has one.
func (a *Appliance) setZone(ctx context.Context, kube kubernetes.Interface, n *corev1.Node, host string) error {
	if n.Labels[corev1.LabelTopologyZone] == host {
		return nil
	}
	labels := map[string]string{corev1.LabelTopologyZone: host}
	if _, beta := n.Labels[corev1.LabelFailureDomainBetaZone]; beta {
		labels[corev1.LabelFailureDomainBetaZone] = host
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("labelling node %s: %w", n.Name, err)
	}
	if _, err = kube.CoreV1().Nodes().Patch(ctx, n.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("labelling node %s with zone %s: %w", n.Name, host, err)
	}
	if a.Log != nil {
		a.Log.Info("a management node's zone label follows its VM", "node", n.Name, "from", n.Labels[corev1.LabelTopologyZone], "to", host)
	}
	return nil
}

func (a *Appliance) readVMHosts(ctx context.Context, token string) (map[string]string, error) {
	if a.vmHosts != nil {
		return a.vmHosts(ctx, token)
	}
	c, err := pve.Connect(pve.Config{URL: a.ProxmoxURL, Token: token, CAFile: a.ProxmoxCAFile, Insecure: a.ProxmoxInsecure})
	if err != nil {
		return nil, err //nolint:wrapcheck // followZones says what it was for
	}
	vms, err := vmsByUUID(ctx, c)
	if err != nil {
		return nil, err
	}
	hosts := make(map[string]string, len(vms))
	for uuid, vm := range vms {
		hosts[uuid] = vm.Node
	}
	return hosts, nil
}
