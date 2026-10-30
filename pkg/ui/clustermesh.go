package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// meshVariable is Cilium Cluster Mesh, with the clusters a cluster lists to link with.
const meshVariable = "clusterMesh"

// hiddenPeer is why a peer the user cannot see is not linked: whether a cluster of that name exists,
// and how it is set up, is not theirs to know. That it linked back is: its owner agreed to.
const hiddenPeer = "not linked, or not visible to you"

func meshOn(raw json.RawMessage) bool {
	var m struct {
		Enabled bool `json:"enabled"`
	}
	return raw != nil && json.Unmarshal(raw, &m) == nil && m.Enabled
}

func clusterKey(namespace, name string) string { return namespace + "/" + name }

// addMesh tells, on each view of a cluster with Cluster Mesh on, how each peer it lists stands.
// visible are the clusters the user may see, by clusterKey.
func (s *Server) addMesh(ctx context.Context, u user, clusters []clusterv1.Cluster, views []clusterView, visible map[string]bool) error {
	var classes *clusterv1.ClusterClassList
	for i := range views {
		v := &views[i]
		if !meshOn(v.Variables[meshVariable]) {
			continue
		}
		if classes == nil {
			classes = &clusterv1.ClusterClassList{}
			if err := u.kube.List(ctx, classes, client.InNamespace(s.ClassNamespace)); err != nil {
				return fmt.Errorf("listing the cluster classes: %w", err)
			}
		}
		j := slices.IndexFunc(clusters, func(c clusterv1.Cluster) bool { return c.Namespace == v.Namespace && c.Name == v.Name })
		if j < 0 {
			continue
		}
		v.Mesh = provisioner.MeshPeers(&clusters[j], clusters, classes.Items)
		for k := range v.Mesh {
			if p := &v.Mesh[k]; p.Refused != "" && !visible[clusterKey(v.Namespace, p.Name)] {
				p.Refused = hiddenPeer
			}
		}
	}
	return nil
}

// addClusterMesh is addMesh for one cluster's view, with its namespace's clusters and the user's
// privileges on them looked up.
func (s *Server) addClusterMesh(ctx context.Context, u user, v *clusterView) error {
	if !meshOn(v.Variables[meshVariable]) {
		return nil
	}
	var list clusterv1.ClusterList
	if err := u.kube.List(ctx, &list, client.InNamespace(v.Namespace)); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	all, err := s.Accounts.AllPrivileges(ctx, u.ticket)
	if err != nil {
		return fmt.Errorf("reading the privileges: %w", err)
	}
	seen, err := s.visibleClusters(ctx, u, list.Items, all)
	if err != nil {
		return err
	}
	views := []clusterView{*v}
	if err := s.addMesh(ctx, u, list.Items, views, visibleKeys(seen)); err != nil {
		return err
	}
	v.Mesh = views[0].Mesh
	return nil
}

// runsCilium tells whether a create request's cluster runs Cilium, by its cni variable or else
// the class's default.
func runsCilium(req *createRequest, class *clusterv1.ClusterClass) bool {
	raw := req.Variables["cni"]
	if raw == nil {
		if def := classDefault(class, "cni"); def != nil {
			raw = def.Raw
		}
	}
	var cni string
	return raw != nil && json.Unmarshal(raw, &cni) == nil && cni == "cilium"
}

// newRanges are a new cluster's pod and service ranges, or answers the request. A cluster that runs
// Cilium may join a Cluster Mesh later, so its name has to be one Cilium takes, and its ranges keep
// clear of every other cluster's.
func (s *Server) newRanges(w http.ResponseWriter, r *http.Request, u user, req *createRequest, class *clusterv1.ClusterClass) ([]string, []string, bool) {
	nodes, err := nodesPrefix(req.Variables["network"])
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, nil, false
	}
	var others *otherRanges
	if runsCilium(req, class) {
		if len(req.Name) > provisioner.MaxCiliumClusterName {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("name: at most %d characters with Cilium", provisioner.MaxCiliumClusterName))
			return nil, nil, false
		}
		if others, err = s.otherClusterRanges(r.Context(), u); err != nil {
			s.kubeError(w, r, err)
			return nil, nil, false
		}
	}
	pods, services, err := clusterRanges(class, nodes, req.Pods, req.Services, others)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, nil, false
	}
	return pods, services, true
}

// otherClusterRanges are the pod and service ranges of every Kubehenge cluster.
func (s *Server) otherClusterRanges(ctx context.Context, u user) (*otherRanges, error) {
	var list clusterv1.ClusterList
	if err := u.kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	var classes clusterv1.ClusterClassList
	if err := u.kube.List(ctx, &classes, client.InNamespace(s.ClassNamespace)); err != nil {
		return nil, fmt.Errorf("listing the cluster classes: %w", err)
	}
	others := &otherRanges{}
	for i := range list.Items {
		if c := &list.Items[i]; provisioner.Owns(c, s.ClassNamespace) {
			pods, services := provisioner.ClusterRanges(c, classes.Items)
			others.pods = append(others.pods, parseRanges(pods)...)
			others.services = append(others.services, parseRanges(services)...)
		}
	}
	return others, nil
}

func visibleKeys(views []clusterView) map[string]bool {
	keys := make(map[string]bool, len(views))
	for i := range views {
		keys[clusterKey(views[i].Namespace, views[i].Name)] = true
	}
	return keys
}
