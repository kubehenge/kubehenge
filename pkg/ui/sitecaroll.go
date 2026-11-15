package ui

import (
	"net/http"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// siteCARoll is what saving a site CA does to the clusters: how many take it, how many replace
// their nodes at once, how many wait for their upgrade window and how many for a free place.
type siteCARoll struct {
	Inherits int `json:"inherits"`
	Now      int `json:"now"`
	Windows  int `json:"windows"`
	Waiting  int `json:"waiting"`
	// Opens is when the first waiting window opens.
	Opens *time.Time `json:"opens,omitempty"`
}

// siteCARollOf plans the rollout of the site CA ca over the clusters.
func siteCARollOf(ca, classNamespace string, clusters []clusterv1.Cluster, now time.Time) (siteCARoll, error) {
	plan, err := provisioner.PlanSiteRoll(strings.TrimSpace(ca), provisioner.SiteRollClusters(clusters, classNamespace), provisioner.SiteRollLimit, now)
	if err != nil {
		return siteCARoll{}, err //nolint:wrapcheck // the plan names the cluster
	}
	out := siteCARoll{Inherits: plan.Inherits, Now: len(plan.Now), Windows: len(plan.Windows), Waiting: len(plan.Waiting)}
	for _, w := range plan.Windows {
		if out.Opens == nil || w.Opens.Before(*out.Opens) {
			opens := w.Opens.UTC()
			out.Opens = &opens
		}
	}
	return out, nil
}

// siteCARollPreview answers what saving a site CA would do to the clusters, so that the interface
// says so before it saves. It validates the authority as saving does.
func (s *Server) siteCARollPreview(w http.ResponseWriter, r *http.Request, u user) {
	if s.Appliance == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		SiteCA string `json:"siteCA"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	check := appliance.Settings{SiteCA: req.SiteCA}
	if err := check.ValidateSiteDefaults(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Before the first setup there is no management cluster, so no cluster rolls.
	var list clusterv1.ClusterList
	if kube := s.currentKube(); kube != nil {
		if err := kube.List(r.Context(), &list); err != nil {
			s.kubeError(w, r, err)
			return
		}
	}
	roll, err := siteCARollOf(check.SiteCA, s.ClassNamespace, list.Items, time.Now())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, roll)
}
