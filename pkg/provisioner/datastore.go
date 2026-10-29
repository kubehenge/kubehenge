package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// csiVariable configures a cluster's Proxmox VE CSI plugin.
const csiVariable = "csi"

// CSI is a cluster's csi variable.
type CSI struct {
	// DatastoreAllocate runs the plugin's newest release, which reads a storage's configuration and
	// deletes volumes through the storage, and gives the cluster's token Datastore.Allocate on the
	// cluster's storage, which both need.
	DatastoreAllocate bool `json:"datastoreAllocate,omitempty"`
	// VolumeSnapshots has the plugin take volume snapshots, restore them and clone volumes, signed
	// in as root@pam, the one user Proxmox VE lets copy a volume.
	VolumeSnapshots bool `json:"volumeSnapshots,omitempty"`
}

// csiOf reads a cluster's csi variable, the zero value if it has none.
func csiOf(cluster *clusterv1.Cluster) (CSI, error) {
	c, _, err := optionalVariable[CSI](cluster, csiVariable)
	return c, err
}

// variableChanged reports whether a change from old to c sets the variable name to another value.
func variableChanged(old, c *clusterv1.Cluster, name string) bool {
	was, _, err1 := optionalVariable[json.RawMessage](old, name)
	is, _, err2 := optionalVariable[json.RawMessage](c, name)
	return err1 != nil || err2 != nil || !bytes.Equal(was, is)
}

// TemplateStoragesAnnotation lists on every ClusterClass, comma-separated, the storages the
// templates in the template pool have disks on. Datastore.Allocate on one of them would let a
// cluster delete the templates every cluster's machines are cloned from.
const TemplateStoragesAnnotation = "kubehenge.dev/template-storages"

// DatastoreAllocate is what lets a cluster hold Datastore.Allocate on its storage, for the CSI
// plugin's newest release. That privilege reaches every volume on the storage and its definition,
// so the storage has to be the cluster's alone.
type DatastoreAllocate struct {
	// Role is what the controller grants a cluster's user on its storage; empty when Kubehenge's
	// administrator lets no cluster hold Datastore.Allocate.
	Role string
	// Management are the storages the management cluster's disks, its disk image and every
	// cluster's cloud-init drives are on.
	Management []string
}

// Refusal says why the cluster may not run as its variables ask: it holds Datastore.Allocate on a
// storage that the management cluster, the templates (class's TemplateStoragesAnnotation) or
// another of clusters uses, or uses a storage another cluster holds it on. It returns "" when
// nothing stands in the way. clusters are every Cluster, the cluster itself among them or not.
func (d DatastoreAllocate) Refusal(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass, clusters []clusterv1.Cluster, classNamespace string) (string, error) {
	storage, _, err := optionalVariable[string](cluster, "storage")
	if err != nil || storage == "" {
		return "", err
	}
	csi, err := csiOf(cluster)
	if err != nil {
		return "", err
	}
	if csi.DatastoreAllocate {
		switch {
		case d.Role == "":
			return "clusters may not hold Datastore.Allocate. Ask an administrator to allow it", nil
		case slices.Contains(d.Management, storage):
			return fmt.Sprintf("Datastore.Allocate is not allowed on storage %s, which the management cluster uses", storage), nil
		case class != nil && slices.Contains(splitList(class.Annotations[TemplateStoragesAnnotation]), storage):
			return fmt.Sprintf("Datastore.Allocate is not allowed on storage %s, which holds VM templates", storage), nil
		}
	}
	for i := range clusters {
		other := &clusters[i]
		if other.Namespace == cluster.Namespace && other.Name == cluster.Name || !Owns(other, classNamespace) {
			continue
		}
		if s, _, err := optionalVariable[string](other, "storage"); err != nil || s != storage {
			continue
		}
		if csi.DatastoreAllocate {
			return fmt.Sprintf("Datastore.Allocate is not allowed on storage %s, which cluster %s/%s uses too", storage, other.Namespace, other.Name), nil
		}
		if theirs, err := csiOf(other); err == nil && theirs.DatastoreAllocate {
			return fmt.Sprintf("cluster %s/%s holds Datastore.Allocate on storage %s. Choose another storage", other.Namespace, other.Name, storage), nil
		}
	}
	return "", nil
}

// checkDatastoreAllocate refuses Datastore.Allocate on a storage that is not the cluster's alone,
// and a storage another cluster holds it on.
func (h *Hooks) checkDatastoreAllocate(ctx context.Context, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	clusters := &clusterv1.ClusterList{}
	if err := h.Reader.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing clusters: %w", err)
	}
	refused, err := h.DatastoreAllocate.Refusal(cluster, class, clusters.Items, h.ClassNamespace)
	if err != nil {
		return err
	}
	if refused != "" {
		return errors.New(refused)
	}
	return nil
}
