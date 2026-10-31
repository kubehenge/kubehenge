package ui

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// managementHealthTimeout keeps the Administration page from waiting on a management cluster
// that does not answer; its card then says so.
const managementHealthTimeout = 10 * time.Second

// managementHealthView is how the management cluster itself is doing: its nodes and the
// controllers that run on it, Cluster API's providers and Kubehenge's own among them.
type managementHealthView struct {
	Answers     bool                   `json:"answers"`
	Error       string                 `json:"error,omitempty"`
	Nodes       []managementNode       `json:"nodes,omitempty"`
	Controllers []managementController `json:"controllers,omitempty"`
}

type managementNode struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
	Kubelet string `json:"kubelet,omitempty"`
}

type managementController struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ready     int32  `json:"ready"`
	Wanted    int32  `json:"wanted"`
}

// managementHealth answers those who may set Kubehenge up, also while the management cluster is
// down: that is when the Administration page is needed most.
func (s *Server) managementHealth(w http.ResponseWriter, r *http.Request, u user) {
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	kube := s.currentKube()
	if kube == nil {
		writeError(w, http.StatusServiceUnavailable, "the management cluster is not set up yet")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), managementHealthTimeout)
	defer cancel()
	v, err := readManagementHealth(ctx, kube)
	if err != nil {
		s.Log.Warn("reading the management cluster's health", "error", err)
		v = managementHealthView{Error: err.Error()}
	}
	writeJSON(w, http.StatusOK, v)
}

func readManagementHealth(ctx context.Context, kube client.Client) (managementHealthView, error) {
	var nodes corev1.NodeList
	if err := kube.List(ctx, &nodes); err != nil {
		return managementHealthView{}, fmt.Errorf("listing the nodes: %w", err)
	}
	var deployments appsv1.DeploymentList
	if err := kube.List(ctx, &deployments); err != nil {
		return managementHealthView{}, fmt.Errorf("listing the deployments: %w", err)
	}
	v := managementHealthView{Answers: true}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		mn := managementNode{Name: n.Name, Kubelet: n.Status.NodeInfo.KubeletVersion}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				mn.Ready = c.Status == corev1.ConditionTrue
				if !mn.Ready {
					mn.Message = cmp.Or(c.Message, c.Reason)
				}
			}
		}
		v.Nodes = append(v.Nodes, mn)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		v.Controllers = append(v.Controllers, managementController{
			Namespace: d.Namespace,
			Name:      d.Name,
			Ready:     d.Status.ReadyReplicas,
			Wanted:    deref(d.Spec.Replicas),
		})
	}
	slices.SortFunc(v.Nodes, func(a, b managementNode) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(v.Controllers, func(a, b managementController) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return v, nil
}
