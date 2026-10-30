package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// poolAutoscaling is a worker pool's autoscaling: the least and the most workers the cluster
// autoscaler keeps it between.
type poolAutoscaling struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}

const autoscalingCounts = "autoscaling: minimum 0 or more, maximum 1 to 100 and not below the minimum"

func (a *poolAutoscaling) valid() bool {
	return a.Min >= 0 && a.Max >= 1 && a.Min <= a.Max && a.Max <= provisioner.MaxAutoscaledWorkers
}

// autoscalingOf reads a pool's autoscaling from Cluster API's annotations on its topology or its
// MachineDeployment, nil for a pool of fixed workers.
func autoscalingOf(annotations map[string]string) *poolAutoscaling {
	lo, hasLo := annotations[provisioner.AutoscalerMinAnnotation]
	hi, hasHi := annotations[provisioner.AutoscalerMaxAnnotation]
	if !hasLo || !hasHi {
		return nil
	}
	least, err1 := strconv.ParseInt(lo, 10, 32)
	most, err2 := strconv.ParseInt(hi, 10, 32)
	if err1 != nil || err2 != nil {
		return nil
	}
	return &poolAutoscaling{Min: int32(least), Max: int32(most)}
}

// setAutoscaling has the autoscaler keep a pool between a's bounds, with its replicas unset so that
// Cluster API leaves the number to the autoscaler, or fixes it at replicas when a is nil.
func setAutoscaling(md *clusterv1.MachineDeploymentTopology, a *poolAutoscaling, replicas int32) {
	if a == nil {
		delete(md.Metadata.Annotations, provisioner.AutoscalerMinAnnotation)
		delete(md.Metadata.Annotations, provisioner.AutoscalerMaxAnnotation)
		md.Replicas = &replicas
		return
	}
	if md.Metadata.Annotations == nil {
		md.Metadata.Annotations = map[string]string{}
	}
	md.Metadata.Annotations[provisioner.AutoscalerMinAnnotation] = strconv.Itoa(int(a.Min))
	md.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation] = strconv.Itoa(int(a.Max))
	md.Replicas = nil
}

// applyAutoscaling sets the autoscaling a change asks for by pool: bounds switch a pool's
// autoscaling on or move them; null switches it off and fixes the pool at the workers it runs now,
// as its MachineDeployment counts them, so that nothing is added or removed by switching. It says
// what is wrong with the change, or returns "".
func applyAutoscaling(ctx context.Context, kube client.Reader, c *clusterv1.Cluster, change map[string]*poolAutoscaling) (string, error) {
	for name, a := range change {
		if a != nil && !a.valid() {
			return autoscalingCounts + " in each pool", nil
		}
		var pool *clusterv1.MachineDeploymentTopology
		for i := range c.Spec.Topology.Workers.MachineDeployments {
			if md := &c.Spec.Topology.Workers.MachineDeployments[i]; md.Name == name {
				pool = md
			}
		}
		if pool == nil {
			return "the cluster has no worker pool " + name, nil
		}
		was := autoscalingOf(pool.Metadata.Annotations)
		if a == nil && was == nil {
			continue
		}
		replicas := int32(0)
		if a == nil {
			n, err := runningWorkers(ctx, kube, c, name)
			if err != nil {
				return "", err
			}
			replicas = min(max(n, was.Min), was.Max)
		}
		setAutoscaling(pool, a, replicas)
	}
	return "", nil
}

// isAutoscaled tells whether the cluster autoscaler sizes the cluster's pool name.
func isAutoscaled(c *clusterv1.Cluster, name string) bool {
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		if md := &c.Spec.Topology.Workers.MachineDeployments[i]; md.Name == name {
			return autoscalingOf(md.Metadata.Annotations) != nil
		}
	}
	return false
}

// runningWorkers is the number of workers a pool's MachineDeployment asks for now.
func runningWorkers(ctx context.Context, kube client.Reader, c *clusterv1.Cluster, pool string) (int32, error) {
	var list clusterv1.MachineDeploymentList
	err := kube.List(ctx, &list, client.InNamespace(c.Namespace), client.MatchingLabels{
		clusterv1.ClusterNameLabel: c.Name, clusterv1.ClusterTopologyMachineDeploymentNameLabel: pool,
	})
	if err != nil {
		return 0, fmt.Errorf("reading the workers of pool %s: %w", pool, err)
	}
	n := int32(0)
	for i := range list.Items {
		if r := list.Items[i].Spec.Replicas; r != nil {
			n += *r
		}
	}
	return n, nil
}

// countAutoscaled gives each autoscaled pool of the view the workers its MachineDeployment asks for
// now, which the Cluster leaves to the autoscaler.
func countAutoscaled(ctx context.Context, kube client.Reader, c *clusterv1.Cluster, v *clusterView) error {
	for i := range v.WorkerPools {
		if p := &v.WorkerPools[i]; p.Autoscaling != nil {
			n, err := runningWorkers(ctx, kube, c, p.Name)
			if err != nil {
				return err
			}
			p.Replicas = n
		}
	}
	return nil
}

// autoscalerVariable is the cluster variable with where the autoscaler runs and how it scales.
const autoscalerVariable = provisioner.AutoscalerVariable

// placesAutoscalerInside tells whether an autoscaler variable runs the autoscaler inside its cluster.
func placesAutoscalerInside(raw json.RawMessage) bool {
	var v struct {
		Placement string `json:"placement"`
	}
	return raw != nil && json.Unmarshal(raw, &v) == nil && v.Placement == provisioner.PlacementWorkload
}

// mayPlaceAutoscaler checks the right to run a cluster's autoscaler inside it. Its management
// cluster token there reads the machines of every cluster in the namespace, so putting it there
// needs what seeing all of their VMs would: VM.Audit on the workload pool.
func (s *Server) mayPlaceAutoscaler(w http.ResponseWriter, r *http.Request, u user, value, old json.RawMessage) bool {
	if !placesAutoscalerInside(value) || placesAutoscalerInside(old) {
		return true
	}
	return s.require(w, r, u, s.workloadPoolPath(), privSee)
}
