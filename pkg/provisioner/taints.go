package provisioner

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// controlPlaneRole labels a control-plane node in every distribution.
const controlPlaneRole = "node-role.kubernetes.io/control-plane"

// controlPlaneTaint keeps workloads off control-plane nodes. kubeadm registers them with it, the
// k3s and RKE2 classes ask for it, and everything that has to run there tolerates it: the
// distributions' own components, k3s's etcd proxy, and the cloud controller manager and CSI
// controller Kubehenge installs. CriticalAddonsOnly, which k3s and RKE2 suggest for dedicated
// servers, would evict the k3s control-plane provider's etcd proxy.
var controlPlaneTaint = corev1.Taint{Key: controlPlaneRole, Effect: corev1.TaintEffectNoSchedule}

// workerCheckInterval is how often a cluster's nodes are looked at again: a worker joining is seen
// only in the workload cluster, which is not watched.
const workerCheckInterval = time.Minute

// TaintReconciler keeps a cluster's control-plane nodes tainted while it has workers or an
// autoscaled pool, so etcd and the API server do not share their nodes with workloads, and takes
// the taint off otherwise, so a cluster of control planes alone still runs them.
type TaintReconciler struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes
}

// SetupWithManager registers the reconciler for Clusters. A new generation is what scales the
// workers, so the taint comes off before Cluster API drains the last of them.
func (r *TaintReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("control-plane-taint").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the control-plane taint reconciler: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (r *TaintReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	if err := taintControlPlanes(ctx, workload.client, wantsWorkers(cluster), autoscaled(cluster)); err != nil {
		r.Remotes.forget(cluster)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workerCheckInterval}, nil
}

// wantsWorkers tells whether any of the cluster's machine deployments asks for workers: unset
// replicas are Cluster API's default of one, and an autoscaled pool asks for its most.
func wantsWorkers(cluster *clusterv1.Cluster) bool {
	return slices.ContainsFunc(cluster.Spec.Topology.Workers.MachineDeployments, func(md clusterv1.MachineDeploymentTopology) bool {
		return workerRange(&md).most > 0
	})
}

// taintControlPlanes taints the control-plane nodes once a worker has joined a cluster that wants
// workers, and takes the taint off otherwise. A worker that is not ready keeps them tainted:
// moving workloads onto the control planes whenever a worker fails would crowd etcd just then. A
// cluster with an autoscaled pool keeps them tainted before any worker joins too: a workload that
// finds no room is what has the autoscaler add the first worker.
func taintControlPlanes(ctx context.Context, workload client.Client, wantsWorkers, autoscaled bool) error {
	nodes := &corev1.NodeList{}
	if err := workload.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	isControlPlane := func(n corev1.Node) bool { _, ok := n.Labels[controlPlaneRole]; return ok }
	isTaint := func(t corev1.Taint) bool { return controlPlaneTaint.MatchTaint(&t) }
	tainted := autoscaled || wantsWorkers && slices.ContainsFunc(nodes.Items, func(n corev1.Node) bool { return !isControlPlane(n) })
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !isControlPlane(*n) || slices.ContainsFunc(n.Spec.Taints, isTaint) == tainted {
			continue
		}
		before := n.DeepCopy()
		if tainted {
			n.Spec.Taints = append(n.Spec.Taints, controlPlaneTaint)
		} else {
			n.Spec.Taints = slices.DeleteFunc(n.Spec.Taints, isTaint)
		}
		if err := workload.Patch(ctx, n, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("changing the taints of node %s: %w", n.Name, err)
		}
	}
	return nil
}
