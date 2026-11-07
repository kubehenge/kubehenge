package provisioner

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
	timeres "github.com/siderolabs/talos/pkg/machinery/resources/time"
	talosnamespace "github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TalosClockProbe asks the Talos node at an address, with a cluster's talosconfig, whether its
// clock is synced. Tests stand in for nodes with their own.
type TalosClockProbe func(ctx context.Context, clientConfig []byte, address string) (bool, error)

// ProbeTalosClock is the TalosClockProbe of the Talos API itself. It reads the node's TimeStatus
// resource, which Talos sets once a time source has answered. A node whose time sync is switched
// off counts as synced, since that is a choice made by hand.
func ProbeTalosClock(ctx context.Context, raw []byte, address string) (bool, error) {
	n, err := connectTalos(ctx, raw, address)
	if err != nil {
		return false, err
	}
	r, err := n.c.COSI.Get(ctx, resource.NewMetadata(talosnamespace.NamespaceName, timeres.StatusType, timeres.StatusID, resource.VersionUndefined))
	closeErr := n.Close()
	if err != nil {
		return false, fmt.Errorf("reading the time status of %s: %w", address, err)
	}
	status, ok := r.(*timeres.Status)
	if !ok {
		return false, fmt.Errorf("the time status of %s is a %T", address, r)
	}
	return status.TypedSpec().Synced || status.TypedSpec().SyncDisabled, closeErr
}

// IsTalosCluster tells whether a cluster is built from the Talos ClusterClass.
func IsTalosCluster(c *clusterv1.Cluster) bool { return isTalos(c) }

// ReadTalosconfig reads a Talos cluster's talosconfig, which signs in to its nodes' Talos API.
func ReadTalosconfig(ctx context.Context, r client.Reader, namespace, cluster string) ([]byte, error) {
	return readTalosconfig(ctx, r, namespace, cluster)
}
