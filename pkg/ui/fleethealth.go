package ui

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fleetHealthItems is how many of a cluster's items needing attention the fleet's summary carries:
// the worst, for the rail to say why, and a few after it.
const fleetHealthItems = 3

// fleetHealthView is how one cluster stands, as its health page sums it up.
type fleetHealthView struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Read      time.Time       `json:"read"`
	Verdict   string          `json:"verdict"`
	Workload  string          `json:"workload,omitempty"`
	Attention []attentionView `json:"attention"`
	// More counts the items needing attention past those carried.
	More int `json:"more,omitempty"`
}

// fleetHealth answers with the verdict and the worst items needing attention of every cluster the
// user may see, from the reads the health pages share.
func (s *Server) fleetHealth(w http.ResponseWriter, r *http.Request, u user) {
	shown, ok := s.shownClusters(w, r, u)
	if !ok {
		return
	}
	ctx := r.Context()
	out := make([]*fleetHealthView, len(shown))
	inParallel(len(shown), func(i int) {
		c := shown[i]
		key := c.Namespace + "/" + c.Name + "/" + string(c.UID)
		v, err := s.health.get(ctx, key, fleetHealthFresh, s.now, func(ctx context.Context) healthView {
			return s.readHealth(ctx, u.kube, c, u.namespaces)
		})
		if err == nil {
			out[i] = summed(c, &v)
		}
	})

	// A cluster whose read the user did not wait for is left out, and shows again once read.
	result := []*fleetHealthView{}
	for _, v := range out {
		if v != nil {
			result = append(result, v)
		}
	}
	slices.SortFunc(result, func(a, b *fleetHealthView) int {
		if n := strings.Compare(a.Namespace, b.Namespace); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})
	writeJSON(w, http.StatusOK, result)
}

// summed is a cluster's health page cut down to what the fleet shows of it.
func summed(c *clusterv1.Cluster, v *healthView) *fleetHealthView {
	items := v.Attention
	more := 0
	if len(items) > fleetHealthItems {
		more = len(items) - fleetHealthItems
		items = items[:fleetHealthItems]
	}
	return &fleetHealthView{
		Namespace: c.Namespace,
		Name:      c.Name,
		Read:      v.Read,
		Verdict:   v.Verdict,
		Workload:  v.Workload,
		Attention: slices.Clone(items),
		More:      more,
	}
}

// shownClusters are the clusters the user may see, or nil with the request answered when they
// could not be read.
func (s *Server) shownClusters(w http.ResponseWriter, r *http.Request, u user) ([]*clusterv1.Cluster, bool) {
	var list clusterv1.ClusterList
	if err := u.kube.List(r.Context(), &list); err != nil {
		s.kubeError(w, err)
		return nil, false
	}
	all, err := s.Accounts.AllPrivileges(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return nil, false
	}
	views, err := s.visibleClusters(r.Context(), u, list.Items, all)
	if err != nil {
		s.proxmoxError(w, err)
		return nil, false
	}
	seen := visibleKeys(views)
	shown := []*clusterv1.Cluster{}
	for i := range list.Items {
		if c := &list.Items[i]; seen[clusterKey(c.Namespace, c.Name)] {
			shown = append(shown, c)
		}
	}
	return shown, true
}
