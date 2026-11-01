package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// sectionAlerts is the health page's list of the alerts firing in the cluster's Alertmanager.
const sectionAlerts = "alerts"

// alertsShown bounds the alerts listed, the worst and oldest first.
const alertsShown = 100

// alertView is one alert firing in the cluster, as Alertmanager has it.
type alertView struct {
	Name      string    `json:"name"`
	Severity  string    `json:"severity"`
	Namespace string    `json:"namespace,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Since     time.Time `json:"since"`
}

// alertmanagerAlert is what Alertmanager's API v2 answers of an alert.
type alertmanagerAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
}

// alertSeverities are the alert severities the page lists, as its own. kube-prometheus-stack's
// Watchdog and InfoInhibitor, and its info alerts, are not.
var alertSeverities = map[string]string{"critical": severityError, "warning": severityWarning}

// monitored tells whether the cluster runs the monitoring add-on.
func monitored(c *clusterv1.Cluster) bool {
	var m provisioner.Monitoring
	raw := variableValue(c, "monitoring")
	return raw != nil && json.Unmarshal(raw, &m) == nil && m.Enabled
}

// workloadAlerts reads what fires in the cluster's Alertmanager, as the alerts identity.
func (s *Server) workloadAlerts(ctx context.Context, kube client.Client, c *clusterv1.Cluster) ([]alertmanagerAlert, error) {
	w, why, err := s.healthWorkload(ctx, kube, c, provisioner.IdentityAlerts)
	switch {
	case err != nil:
		return nil, err
	case why != "":
		return nil, errors.New(why)
	case w.Alerts == nil:
		return nil, errors.New("no way to Alertmanager")
	}
	body, err := w.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	var alerts []alertmanagerAlert
	if err := json.Unmarshal(body, &alerts); err != nil {
		return nil, fmt.Errorf("reading Alertmanager's answer: %w", err)
	}
	return alerts, nil
}

// alertsSection lists the alerts that fire, and adds each to what needs a look.
func alertsSection(wl *workloadRead, a *attention) section[alertView] {
	s := section[alertView]{Items: []alertView{}}
	if wl.alertsErr != nil {
		s.Error = problem(wl.alertsErr)
		return s
	}
	for i := range wl.alerts {
		al := &wl.alerts[i]
		severity, ok := alertSeverities[al.Labels["severity"]]
		if !ok {
			continue
		}
		summary := cmp.Or(al.Annotations["summary"], al.Annotations["description"], al.Annotations["message"])
		s.Items = append(s.Items, alertView{
			Name: al.Labels["alertname"], Severity: severity, Namespace: al.Labels["namespace"], Summary: summary, Since: al.StartsAt,
		})
	}
	slices.SortStableFunc(s.Items, func(x, y alertView) int {
		return cmp.Or(cmp.Compare(severityRank(x.Severity), severityRank(y.Severity)), x.Since.Compare(y.Since),
			strings.Compare(x.Name, y.Name), strings.Compare(x.Namespace, y.Namespace))
	})
	if len(s.Items) > alertsShown {
		s.Items, s.Truncated = s.Items[:alertsShown], true
	}
	for _, v := range s.Items {
		what := "Alert " + v.Name
		if v.Namespace != "" {
			what += " in " + v.Namespace
		}
		a.add(v.Severity, sectionAlerts, what, v.Summary, v.Since)
	}
	return s
}
