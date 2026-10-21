package provisioner

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// rancherConfigMap is where Rancher reads the values of the charts it installs itself, one
	// key per chart.
	rancherConfigMap = "rancher-config"
	turtlesChart     = "rancher-turtles"
)

// turtlesValues name Turtles' image and the Cluster API controller it runs by digest. Turtles'
// chart names its image by digest alone when its tag is one; the Cluster API operator inside it
// runs the core provider's imageUrl as given.
func turtlesValues(turtles, clusterAPI Image) (string, error) {
	values, err := yaml.Marshal(map[string]any{
		"image": map[string]any{"repository": turtles.Registry + "/" + turtles.Repository, "tag": "sha256:" + turtles.Digest},
		"cluster-api-operator": map[string]any{"cluster-api": map[string]any{"core": map[string]any{
			"imageUrl": clusterAPI.String(),
		}}},
	})
	if err != nil {
		return "", fmt.Errorf("the values of Turtles: %w", err)
	}
	return string(values), nil
}

// rancherConfigWaits gives Rancher the values it installs Turtles with before Rancher's chart
// installs, and reports whether Rancher's chart has to wait, as it does while the cluster cannot
// be reached. Rancher reads them only from its rancher-config ConfigMap, which its chart makes
// without them; installed first, it would pull Turtles by tag. The ConfigMap carries the marks
// of Rancher's release, so Helm takes it over as it installs, and keeps the key it does not
// manage there.
func (r *Reconciler) rancherConfigWaits(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	values, err := turtlesValues(r.Charts.Turtles, r.Charts.TurtlesClusterAPI)
	if err != nil {
		return false, err
	}
	if err := writeRancherConfig(ctx, workload.client, values); err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	return false, nil
}

func writeRancherConfig(ctx context.Context, workload client.Client, values string) error {
	ns := &corev1.Namespace{Name: rancherNamespace}
	if err := client.IgnoreAlreadyExists(workload.Create(ctx, ns)); err != nil {
		return fmt.Errorf("creating namespace %s: %w", rancherNamespace, err)
	}
	cm := &corev1.ConfigMap{}
	err := workload.Get(ctx, client.ObjectKey{Namespace: rancherNamespace, Name: rancherConfigMap}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			Namespace:   rancherNamespace,
			Name:        rancherConfigMap,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			Annotations: map[string]string{"meta.helm.sh/release-name": rancherAddon, "meta.helm.sh/release-namespace": rancherNamespace},
			Data:        map[string]string{turtlesChart: values},
		}
		err = workload.Create(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("writing Rancher's %s: %w", rancherConfigMap, err)
	}
	if cm.Data[turtlesChart] == values {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{turtlesChart: values}})
	if err != nil {
		return fmt.Errorf("writing Rancher's %s: %w", rancherConfigMap, err)
	}
	if err := workload.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("writing Rancher's %s: %w", rancherConfigMap, err)
	}
	return nil
}
