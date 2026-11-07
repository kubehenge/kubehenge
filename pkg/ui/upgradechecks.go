package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// upgradeChecksDeadline bounds the reads of one check: the API server's metrics are a few MiB.
const upgradeChecksDeadline = 30 * time.Second

// upgradeChecksView is what an upgrade to a version would run into inside the cluster, for the
// upgrade dialog and the CLI: the findings, and each as a sentence.
type upgradeChecksView struct {
	provisioner.UpgradeFindings

	Lines []string `json:"lines"`
}

// upgradeChecks reads the cluster as its health identity, like the Health page, for the removed
// APIs in use and the fail-closed webhooks an upgrade to ?version= would meet. What cannot be read
// is named in the answer, never taken for clean.
func (s *Server) upgradeChecks(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	findings := s.checkUpgrade(r.Context(), u, c, r.URL.Query().Get("version"))
	writeJSON(w, http.StatusOK, upgradeChecksView{UpgradeFindings: findings, Lines: append([]string{}, findings.Lines()...)})
}

func (s *Server) checkUpgrade(ctx context.Context, u user, c *clusterv1.Cluster, version string) provisioner.UpgradeFindings {
	unread := func(why string) provisioner.UpgradeFindings {
		return provisioner.UpgradeFindings{RemovedAPIs: []provisioner.RemovedAPI{}, Webhooks: []provisioner.RiskyWebhook{}, Unchecked: []string{why}}
	}
	if !u.namespaces.has(c.Namespace) {
		return unread("the interface reads inside clusters in " + namespacesPhrase(u.namespaces) + " only")
	}
	if !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return unread("the cluster's API server is not up yet")
	}
	tctx, cancel := context.WithTimeout(ctx, upgradeChecksDeadline)
	defer cancel()
	wl, why, err := s.healthWorkload(tctx, u.kube, c, provisioner.IdentityHealth)
	switch {
	case err != nil:
		return unread("the cluster could not be reached: " + problem(err))
	case why != "":
		return unread(why)
	}
	found := provisioner.CheckUpgrade(tctx, wl.Kube, wl.Metrics, version)
	found.ApplyHistory(provisioner.HistoryOf(c), s.now())
	return found
}

// acknowledgeUpgrade accepts what an upgrade to ?version= would run into for the automatic upgrade
// to it: the cluster keeps the findings, by name, in an annotation, and the automatic upgrade goes
// ahead unless something else turns up. Findings that could not be read cannot be accepted.
func (s *Server) acknowledgeUpgrade(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	version := r.URL.Query().Get("version")
	found := s.checkUpgrade(r.Context(), u, c, version)
	if len(found.Unchecked) > 0 {
		writeError(w, http.StatusConflict, "the upgrade checks could not be made: "+strings.Join(found.Unchecked, ", "))
		return
	}
	value, err := provisioner.Acknowledgement(c, version, &found)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	patch := client.MergeFrom(c.DeepCopy())
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[provisioner.AcknowledgedAnnotation] = value
	if err := u.kube.Patch(r.Context(), c, patch); err != nil {
		s.kubeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "accepted what an upgrade runs into", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "version", version)
	writeJSON(w, http.StatusOK, upgradeChecksView{UpgradeFindings: found, Lines: append([]string{}, found.Lines()...)})
}
