package backupstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// ZonesSecret holds the Proxmox VE token that reads where the management cluster's VMs run, and
// the management nodes' names, comma-separated. The storage chart makes it on every storage.
const (
	ZonesSecret = Service + "-zones"
	zonesKey    = "token"
	zonesNodes  = "nodes"
)

// VolumeZone annotates the store's Deployment, from the storage chart, with the Proxmox VE node
// whose own storage holds the backup volume. Shared storage leaves it out.
const VolumeZone = "kubehenge.dev/volume-zone"

// VMReader reads with token the VMs it may see, with the Proxmox VE node each runs on.
type VMReader func(ctx context.Context, token string) (*VMIndex, error)

// ProxmoxVMs reads the VMs through the Proxmox VE API, with a client connect makes for the token.
func ProxmoxVMs(connect func(token string) (*proxmox.Client, error)) VMReader {
	return func(ctx context.Context, token string) (*VMIndex, error) {
		c, err := connect(token)
		if err != nil {
			return nil, err
		}
		return ReadVMIndex(ctx, c)
	}
}

// FollowZones points each management node's zone labels at the Proxmox VE node its VM runs on now.
// Kubehenge is their only owner: the machine config sets only the region, so no kubelet start
// puts back a zone that HA or an administrator moved the VM away from. On a node's own storage the
// CSI plugin attaches the backup volume to a node with the disk's zone, and a stale label lets the
// store's pod land on a VM elsewhere, where the attach fails.
//
// The management nodes are those ZonesSecret names that joined. A node Proxmox VE lists no VM for
// keeps its labels and is returned: its VM is gone, so its node is not ready and takes no pod. With zone, the disk's node, set, the call fails only where no
// management VM runs there, since the store then has nowhere to start. Of the labels, only those
// of nodes in or claiming that zone must be set for the store to land right.
//
// A node's VM is the one with its SMBIOS UUID (VMIndex), whatever the VM's name. A node whose VM
// cannot be read, as on a Proxmox VE node that is down, is returned too. Two VMs with the node's
// UUID fail the call.
//
// The plugin finds a node's VM by its ID or SMBIOS UUID at each attach. Its node pod exits while
// the node has no zone label, and reads the label only when it starts. Only the static backup volume uses the plugin's storage class, so no
// volume is ever placed by the zones the node registered with.
func FollowZones(ctx context.Context, kube kubernetes.Interface, read VMReader, zone string, log *slog.Logger) ([]string, error) {
	secret, err := kube.CoreV1().Secrets(Namespace).Get(ctx, ZonesSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the token that reads where the management VMs run: %w", err)
	}
	vms, err := read(ctx, string(secret.Data[zonesKey]))
	if err != nil {
		return nil, fmt.Errorf("reading where the management VMs run: %w", err)
	}
	nodes, err := joined(ctx, kube, string(secret.Data[zonesNodes]))
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 && zone != "" {
		return nil, fmt.Errorf("no node named under %q in Secret %s/%s is in the management cluster. Apply again to set them from backupVolume.nodes", zonesNodes, Namespace, ZonesSecret)
	}
	var unknown []string
	inZone := false
	for _, n := range nodes {
		var host string
		if host, err = hostOf(vms, n, log); err != nil {
			return unknown, err
		}
		if host == "" {
			unknown = append(unknown, n.Name)
			continue
		}
		inZone = inZone || host == zone
		if err = setZone(ctx, kube, n, host, log); err == nil {
			continue
		}
		if zone == "" || host == zone || n.Labels[corev1.LabelTopologyZone] == zone {
			return unknown, err
		}
		if log != nil {
			log.Warn("a management node's zone label was not set", "node", n.Name, "error", err)
		}
	}
	if zone != "" && !inZone {
		// Proxmox VE answered, so the message keeps off the start of a failed read's.
		return unknown, fmt.Errorf("no management VM runs on Proxmox VE node %s, which holds the backup volume", zone)
	}
	return unknown, nil
}

// joined reads the nodes of names, comma-separated, that are in the cluster.
func joined(ctx context.Context, kube kubernetes.Interface, names string) ([]*corev1.Node, error) {
	var nodes []*corev1.Node
	for name := range strings.SplitSeq(names, ",") {
		if name == "" {
			continue
		}
		n, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("management node %s: %w", name, err)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// hostOf is the Proxmox VE node that n's VM runs on, empty when no readable VM has n's UUID.
func hostOf(vms *VMIndex, n *corev1.Node, log *slog.Logger) (string, error) {
	vm, ok, err := vms.Find(n.Status.NodeInfo.SystemUUID)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", n.Name, err)
	}
	if !ok && log != nil && len(vms.Unreadable) > 0 {
		log.Warn("a management node's zone label stays as it is", "node", n.Name, "error", vms.Missing(n.Status.NodeInfo.SystemUUID))
	}
	return vm.Node, nil
}

// setZone labels node n with the Proxmox VE node host, the beta zone label too where it has one.
func setZone(ctx context.Context, kube kubernetes.Interface, n *corev1.Node, host string, log *slog.Logger) error {
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
	if log != nil {
		log.Info("a management node's zone label follows its VM", "node", n.Name, "from", n.Labels[corev1.LabelTopologyZone], "to", host)
	}
	return nil
}
