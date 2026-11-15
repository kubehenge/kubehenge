package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// siteCARollOf plans the rollout of the site CA ca over the clusters, settling being the clusters
// still taking up an earlier one. An upgrade window that names no zone opens in zone, the site's,
// as the controller reads it.
func siteCARollOf(ca, zone, classNamespace string, clusters []clusterv1.Cluster, settling map[client.ObjectKey]bool, now time.Time) siteCARoll {
	rolls := provisioner.SiteRollClusters(clusters, classNamespace, settling)
	for i := range rolls {
		if p := rolls[i].Policy; p != nil && p.Window.TimeZone == "" {
			p.Window.TimeZone = zone
		}
	}
	plan := provisioner.PlanSiteRoll(strings.TrimSpace(ca), rolls, provisioner.SiteRollLimit, now)
	out := siteCARoll{Inherits: plan.Inherits, Now: len(plan.Now), Windows: len(plan.Windows), Waiting: len(plan.Waiting)}
	for _, w := range plan.Windows {
		if out.Opens == nil || w.Opens.Before(*out.Opens) {
			opens := w.Opens.UTC()
			out.Opens = &opens
		}
	}
	return out
}

// siteZone is the time zone the controller runs schedules naming none in: the saved site time zone
// when it is one, else UTC.
func (s *Server) siteZone() string {
	if s.Appliance != nil {
		if saved := s.Appliance.SavedSettings(); saved != nil && provisioner.CheckTimeZone(saved.TimeZone) == nil {
			return saved.TimeZone
		}
	}
	return provisioner.DefaultTimeZone
}

// timeZoneCheck answers whether the server's time zone database knows a time zone name, which is
// the one the controller and the appliance read it with, so that Setup agrees with what saving does.
func (s *Server) timeZoneCheck(w http.ResponseWriter, r *http.Request, u user) {
	if s.Appliance == nil {
		writeError(w, http.StatusNotFound, "this interface does not manage its management cluster")
		return
	}
	var req struct {
		TimeZone string `json:"timeZone"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	if provisioner.CheckTimeZone(req.TimeZone) != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Unknown time zone %q. Use an IANA name, such as Europe/Zurich.", req.TimeZone))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
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
	now := time.Now()
	var settling map[client.ObjectKey]bool
	if kube := s.currentKube(); kube != nil {
		if err := kube.List(r.Context(), &list); err != nil {
			s.kubeError(w, r, err)
			return
		}
		settling = provisioner.SiteRollSettling(r.Context(), kube, list.Items, now)
	}
	writeJSON(w, http.StatusOK, siteCARollOf(check.SiteCA, s.siteZone(), s.ClassNamespace, list.Items, settling, now))
}
