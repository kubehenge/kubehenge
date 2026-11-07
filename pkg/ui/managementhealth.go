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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// managementHealthTimeout keeps the Administration page from waiting on a management cluster
// that does not answer. Its card then says so.
const managementHealthTimeout = 10 * time.Second

// managementHealthView is how the management cluster itself is doing: its nodes and the
// controllers that run on it, Cluster API's providers and Kubehenge's own among them.
type managementHealthView struct {
	Answers     bool                   `json:"answers"`
	Error       string                 `json:"error,omitempty"`
	Nodes       []managementNode       `json:"nodes,omitempty"`
	Controllers []managementController `json:"controllers,omitempty"`
	// Proxmox tells whether the controller reaches the Proxmox VE API from inside the cluster.
	Proxmox *proxmoxReachView `json:"proxmox,omitempty"`
	// Etcd is how many of the control-plane nodes' etcd members are healthy.
	Etcd *etcdMembersView `json:"etcd,omitempty"`
}

type etcdMembersView struct {
	Healthy int `json:"healthy"`
	Members int `json:"members"`
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
	if v.Answers {
		v.Proxmox = s.proxmoxReach(ctx, kube)
		v.Etcd = s.managementMembers(ctx, kube)
	}
	writeJSON(w, http.StatusOK, v)
}

// managementMembers counts the management cluster's healthy etcd members. It is nil where they
// cannot be read, so the card shows nothing rather than a wrong figure.
func (s *Server) managementMembers(ctx context.Context, kube client.Client) *etcdMembersView {
	set, err := provisioner.ManagementEtcd(ctx, kube, s.managementHosts())
	if err != nil {
		s.Log.WarnContext(ctx, "reading the management cluster's etcd members", "error", err)
		return nil
	}
	if len(set.Members) == 0 {
		return nil
	}
	return &etcdMembersView{Healthy: set.Healthy(), Members: len(set.Members)}
}

// proxmoxReachView is whether the controller reaches the Proxmox VE API from inside the management
// cluster, as of its last ask.
type proxmoxReachView struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Blind is the privilege the controller's token lacks to see hosts in maintenance.
	Blind string `json:"blind,omitempty"`
	// Tokens are the tokens that fail their test, each with what is wrong. Working ones are not listed.
	Tokens  []tokenProblemView `json:"tokens,omitempty"`
	Checked time.Time          `json:"checked"`
}

// tokenProblemView is a Proxmox VE token that is refused, cannot verify the API or lacks privileges.
type tokenProblemView struct {
	Name string `json:"name"`
	// Problem is Refused, Certificate, Unreachable or Privileges.
	Problem string `json:"problem"`
	// Message says what is wrong. Each missing privilege is named with its path.
	Message string `json:"message"`
}

// tokenProblems lists the tokens that failed their last test.
func tokenProblems(states []provisioner.TokenState) []tokenProblemView {
	var out []tokenProblemView
	for i := range states {
		if s := &states[i]; !s.OK {
			out = append(out, tokenProblemView{Name: s.Name, Problem: s.Problem, Message: s.Message()})
		}
	}
	return out
}

// proxmoxReach reads the controller's record of its last ask. It is nil where there is none yet, as
// right after an apply, or where it cannot be read.
func (s *Server) proxmoxReach(ctx context.Context, kube client.Client) *proxmoxReachView {
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.ProxmoxReachName}, cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			s.Log.WarnContext(ctx, "reading whether the controller reaches Proxmox VE", "error", err)
		}
		return nil
	}
	r, err := provisioner.ParseReachability(cm)
	if err != nil {
		s.Log.WarnContext(ctx, "reading whether the controller reaches Proxmox VE", "error", err)
		return nil
	}
	return &proxmoxReachView{OK: r.OK, Error: r.Error, Blind: r.Blind, Tokens: tokenProblems(r.Tokens), Checked: r.Checked}
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
