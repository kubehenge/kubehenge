package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ApplianceAddressesName is the ConfigMap in the controller's namespace that holds the
	// appliance's own addresses. The chart makes it empty, and the appliance is its one writer.
	ApplianceAddressesName = "kubehenge-appliance-addresses"
	// ApplianceAddressesKey holds the appliance's addresses, comma-separated.
	ApplianceAddressesKey = "addresses"
	// FirewallSourcesAnnotation names, on the ProxmoxCluster of a cluster with security groups, the
	// addresses beside the cluster's own that Kubehenge's capmox build keeps in each node's IPSet:
	// the management cluster's nodes and the appliance. FirewallSources is its one writer.
	FirewallSourcesAnnotation = "kubehenge.dev/firewall-sources"
)

// FirewallSources keeps FirewallSourcesAnnotation on the ProxmoxCluster of every cluster with
// security groups at the management cluster's nodes and the appliance's addresses, and takes it off
// a cluster without groups. capmox writes a change into the nodes' IPSets, so a new address of the
// appliance or a management node replaces no node.
type FirewallSources struct {
	Client client.Client
	// Reader reads the appliance's ConfigMap past the cache, which holds only the classes'.
	Reader    client.Reader
	Namespace string
	// Management are the management cluster's node addresses, which its module sets.
	Management []string
	Interval   time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*FirewallSources) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (f *FirewallSources) Start(ctx context.Context) error {
	tick := time.NewTicker(f.Interval)
	defer tick.Stop()
	for {
		if err := f.runOnce(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("firewall-sources").Error(err, "keeping the firewall sources of the clusters")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Addresses are the sources as a list. The admission check of a cluster's security groups asks for
// them too, so that it holds the same addresses the firewall keeps.
func (f *FirewallSources) Addresses(ctx context.Context) ([]string, error) {
	sources := slices.Clone(f.Management)
	cm := &corev1.ConfigMap{}
	err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: ApplianceAddressesName}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the appliance's addresses: %w", err)
	}
	for address := range strings.SplitSeq(cm.Data[ApplianceAddressesKey], ",") {
		if address = strings.TrimSpace(address); address != "" && !slices.Contains(sources, address) {
			sources = append(sources, address)
		}
	}
	return sources, nil
}

func (f *FirewallSources) runOnce(ctx context.Context) error {
	sources, err := f.sources(ctx)
	if err != nil {
		return err
	}
	clusters := &clusterv1.ClusterList{}
	if err := f.Client.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	proxmoxClusters := &unstructured.UnstructuredList{}
	proxmoxClusters.SetGroupVersionKind(proxmoxClusterListGVK)
	if err := f.Client.List(ctx, proxmoxClusters); err != nil {
		return fmt.Errorf("listing the ProxmoxClusters: %w", err)
	}
	byCluster := make(map[types.NamespacedName]*unstructured.Unstructured, len(proxmoxClusters.Items))
	for i := range proxmoxClusters.Items {
		pc := &proxmoxClusters.Items[i]
		if name := pc.GetLabels()[clusterv1.ClusterNameLabel]; name != "" {
			byCluster[types.NamespacedName{Namespace: pc.GetNamespace(), Name: name}] = pc
		}
	}
	var errs []error
	for i := range clusters.Items {
		c := &clusters.Items[i]
		pc := byCluster[types.NamespacedName{Namespace: c.Namespace, Name: c.Name}]
		if pc == nil || !Owns(c, f.Namespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		want := ""
		if firewalled(c) {
			want = sources
		}
		if err := f.annotate(ctx, pc, want); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// firewalled tells whether a cluster names security groups.
func firewalled(c *clusterv1.Cluster) bool {
	groups, set, err := optionalVariable[[]string](c, SecurityGroupsVariable)
	return err == nil && set && len(groups) > 0
}

// sources are the management cluster's nodes, then the appliance's addresses, each once. A
// ConfigMap the appliance has not written adds nothing, as on a management cluster set up without
// the appliance.
func (f *FirewallSources) sources(ctx context.Context) (string, error) {
	sources, err := f.Addresses(ctx)
	return strings.Join(sources, ","), err
}

// annotate sets the ProxmoxCluster's sources to want where they differ, and takes the annotation
// off when want is empty. capmox reads an empty annotation and none alike.
func (f *FirewallSources) annotate(ctx context.Context, pc *unstructured.Unstructured, want string) error {
	held, has := pc.GetAnnotations()[FirewallSourcesAnnotation]
	if (want == "" && !has) || (has && held == want && want != "") {
		return nil
	}
	var value any
	if want != "" {
		value = want
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: map[string]any{FirewallSourcesAnnotation: value}}})
	if err != nil {
		return fmt.Errorf("the firewall sources: %w", err)
	}
	if err := f.Client.Patch(ctx, pc, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("annotating ProxmoxCluster %s with the firewall sources: %w", pc.GetName(), err)
	}
	return nil
}
