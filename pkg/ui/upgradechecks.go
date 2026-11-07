package ui

import (
	"context"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

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
	return provisioner.CheckUpgrade(tctx, wl.Kube, wl.Metrics, version)
}
