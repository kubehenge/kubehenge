package provisioner

import (
	"context"
	"encoding/base64"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	hookValue  = "https://hooks.example.org/services/T000/B000/XXXXSECRETXXXX"
	mailValue  = "mail-password-XXXX"
	writeValue = "write-secret-XXXX"
)

func storedCredentials(purposes ...string) monitoringState {
	s := monitoringState{credentials: map[string]map[string][]byte{}}
	for _, p := range purposes {
		switch p {
		case purposeAlertsEmail:
			s.credentials[p] = map[string][]byte{passwordKey: []byte(mailValue)}
		case purposeAlertsWebhook:
			s.credentials[p] = map[string][]byte{urlKey: []byte(hookValue), tokenKey: []byte("hook-token-XXXX")}
		case purposeAlertsSlack, purposeAlertsTeams:
			s.credentials[p] = map[string][]byte{urlKey: []byte(hookValue)}
		default:
			s.credentials[p] = map[string][]byte{secretKey: []byte(writeValue)}
		}
	}
	return s
}

// monitoringOf are the values and proxies of a cluster whose monitoring is m, with the credentials
// stored.
func monitoringOf(t *testing.T, m *Monitoring, stored ...string) (map[string]any, string) {
	t.Helper()
	enabled := *m
	enabled.Enabled = true
	values, raw := proxiesOf(t, templates.Kubeadm, &Addons{Monitoring: enabled, monitoringFound: storedCredentials(stored...)})
	return values["web-monitoring"], raw["web-monitoring"]
}

func TestMonitoringSelectsEveryMonitorAndRule(t *testing.T) {
	t.Parallel()
	values, _ := monitoringOf(t, &Monitoring{})
	for _, key := range []string{
		"serviceMonitorSelectorNilUsesHelmValues", "podMonitorSelectorNilUsesHelmValues", "probeSelectorNilUsesHelmValues",
		"scrapeConfigSelectorNilUsesHelmValues", "ruleSelectorNilUsesHelmValues",
	} {
		if got := lookup[bool](t, values, prometheus, "prometheusSpec", key); got {
			t.Errorf("%s is true: Prometheus reads only what is labelled for the release", key)
		}
	}
	// What Kubehenge brings is read once: the chart's monitors carry the release label the
	// selectors no longer need, and nothing else is added to scrape the same targets.
	for name, m := range manifests(t, values) {
		kind, _, _ := strings.Cut(name, "/")
		if slices.Contains([]string{"ServiceMonitor", "PodMonitor", "Probe", "ScrapeConfig", "PrometheusRule"}, kind) {
			t.Errorf("Kubehenge's own extra manifest %s (%v) would be scraped or loaded beside the chart's", name, m)
		}
	}
	for _, key := range []string{"additionalServiceMonitors", "additionalPodMonitors", "additionalScrapeConfigs", "additionalPrometheusRulesMap"} {
		if _, there := values[key]; there {
			t.Errorf("values set %s", key)
		}
		if _, there := lookup[map[string]any](t, values, prometheus, "prometheusSpec")[key]; there {
			t.Errorf("prometheusSpec sets %s", key)
		}
	}
}

func TestMonitoringUpgradesTheChartsCRDs(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../charts/kubehenge/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart map[string]any
	if err = yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	// As the controller renders them: the chart's values beneath Kubehenge's.
	base := lookup[map[string]any](t, chart, "addons", "monitoring", "values")
	rendered := overlay(base, monitoringValues(templates.Kubeadm, testCharts.KubeRBACProxy, &Addons{Monitoring: Monitoring{Enabled: true}}))
	job := lookup[map[string]any](t, rendered, "crds", "upgradeJob")
	if !lookup[bool](t, job, "enabled") {
		t.Error("the chart's CRD upgrade job is off: a newer operator meets the CRDs of the release before")
	}
	// The chart reads the digest from its tag: its own digest keys are not looked at.
	pinned := regexp.MustCompile(`^[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	for _, name := range []string{"busybox", "kubectl"} {
		tag := lookup[string](t, job, "image", name, "tag")
		if !pinned.MatchString(tag) {
			t.Errorf("the CRD upgrade job's %s image is not pinned by digest: %q", name, tag)
		}
	}
	if _, there := lookup[map[string]any](t, job, "resources")["requests"]; !there {
		t.Errorf("the CRD upgrade job requests nothing: %v", job)
	}
}

func TestMonitoringSendsNothingUnlessAsked(t *testing.T) {
	t.Parallel()
	values, _ := monitoringOf(t, &Monitoring{})
	alertmanager := lookup[map[string]any](t, values, "alertmanager")
	for _, key := range []string{"config"} {
		if _, there := alertmanager[key]; there {
			t.Errorf("alertmanager sets %s without a receiver", key)
		}
	}
	if _, there := lookup[map[string]any](t, alertmanager, "alertmanagerSpec")["secrets"]; there {
		t.Error("alertmanager mounts a Secret without a receiver")
	}
	spec := lookup[map[string]any](t, values, prometheus, "prometheusSpec")
	for _, key := range []string{"remoteWrite", "additionalAlertManagerConfigs", "secrets"} {
		if _, there := spec[key]; there {
			t.Errorf("prometheusSpec sets %s", key)
		}
	}
}

func TestMonitoringRoutesAlertsToEveryReceiver(t *testing.T) {
	t.Parallel()
	notTLS := false
	m := Monitoring{AlertReceivers: []AlertReceiver{
		{Type: alertEmail, Smarthost: "mail.example.org:587", From: "alerts@example.org", To: []string{"ops@example.org", "oncall@example.org"}, Username: "alerts", RequireTLS: &notTLS},
		{Type: alertWebhook},
		{Type: alertSlack, Channel: "#ops"},
		{Type: alertTeams},
	}}
	values, raw := monitoringOf(t, &m, purposeAlertsEmail, purposeAlertsWebhook, purposeAlertsSlack, purposeAlertsTeams)
	config := lookup[map[string]any](t, values, "alertmanager", "config")
	if lookup[string](t, config, "route", "receiver") != kubehengeReceiver ||
		lookup[string](t, config, "route", "routes", 0, "receiver") != nullReceiver ||
		lookup[string](t, config, "route", "routes", 0, "matchers", 0) != `alertname = "Watchdog"` {
		t.Errorf("route %v", config["route"])
	}
	if lookup[string](t, config, "receivers", 0, "name") != nullReceiver || lookup[string](t, config, "receivers", 1, "name") != kubehengeReceiver {
		t.Errorf("receivers %v", config["receivers"])
	}
	receiver := lookup[map[string]any](t, config, "receivers", 1)
	dir := "/etc/alertmanager/secrets/kubehenge-alertmanager/"
	for _, tc := range []struct {
		path []any
		want any
	}{
		{[]any{"email_configs", 0, "to"}, "ops@example.org, oncall@example.org"},
		{[]any{"email_configs", 0, "smarthost"}, "mail.example.org:587"},
		{[]any{"email_configs", 0, "from"}, "alerts@example.org"},
		{[]any{"email_configs", 0, "auth_username"}, "alerts"},
		{[]any{"email_configs", 0, "auth_password_file"}, dir + "email-password"},
		{[]any{"email_configs", 0, "require_tls"}, false},
		{[]any{"webhook_configs", 0, "url_file"}, dir + "webhook-url"},
		{[]any{"webhook_configs", 0, "http_config", "authorization", "credentials_file"}, dir + "webhook-token"},
		{[]any{"slack_configs", 0, "api_url_file"}, dir + "slack-url"},
		{[]any{"slack_configs", 0, "channel"}, "#ops"},
		{[]any{"msteamsv2_configs", 0, "webhook_url_file"}, dir + "teams-url"},
	} {
		if got := lookup[any](t, receiver, tc.path...); got != tc.want {
			t.Errorf("%v = %v, want %v", tc.path, got, tc.want)
		}
	}
	if secrets := lookup[[]any](t, values, "alertmanager", "alertmanagerSpec", "secrets"); len(secrets) != 1 || secrets[0] != alertmanagerSecret {
		t.Errorf("secrets %v", secrets)
	}
	// Values are plain text in the management cluster. A secret is a file in the workload cluster.
	for _, secret := range []string{hookValue, "XXXXSECRETXXXX", mailValue, "hook-token-XXXX"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the values hold the secret %q", secret)
		}
	}
}

func TestMonitoringReceiverWaitsForItsCredential(t *testing.T) {
	t.Parallel()
	m := Monitoring{AlertReceivers: []AlertReceiver{
		{Type: alertEmail, Smarthost: "mail.example.org:25", From: "alerts@example.org", To: []string{"ops@example.org"}},
		{Type: alertSlack},
	}}
	// An email receiver that signs in to nothing needs no credential. The others wait.
	state := storedCredentials()
	got := state.active(&m)
	if len(got) != 1 || got[0].Type != alertEmail {
		t.Errorf("active receivers %+v", got)
	}
	if missing := state.missing(&m); !slices.Equal(missing, []string{purposeAlertsSlack}) {
		t.Errorf("missing %v", missing)
	}
	signedIn := Monitoring{AlertReceivers: []AlertReceiver{{Type: alertEmail, Username: "alerts"}}}
	if missing := state.missing(&signedIn); !slices.Equal(missing, []string{purposeAlertsEmail}) {
		t.Errorf("a mail server that wants a password: missing %v", missing)
	}
	values, _ := monitoringOf(t, &Monitoring{AlertReceivers: []AlertReceiver{{Type: alertSlack}}})
	if _, there := lookup[map[string]any](t, values, "alertmanager")["config"]; there {
		t.Error("a Slack receiver without its URL is configured")
	}
	// No file to read means nothing to mount, or the pod waits on a Secret that never comes.
	values, _ = monitoringOf(t, &Monitoring{AlertReceivers: []AlertReceiver{
		{Type: alertEmail, Smarthost: "mail.example.org:25", From: "alerts@example.org", To: []string{"ops@example.org"}},
	}})
	if _, there := lookup[map[string]any](t, values, "alertmanager", "alertmanagerSpec")["secrets"]; there {
		t.Error("alertmanager mounts a Secret for a mail server that needs no password")
	}
}

func TestMonitoringRemoteWriteAndExternalAlertmanagers(t *testing.T) {
	t.Parallel()
	m := Monitoring{
		RemoteWrite:           &RemoteWrite{URL: "https://mimir.example.org/api/v1/push", Auth: authBasic, Username: "tenant"},
		ExternalAlertmanagers: &ExternalAlertmanagers{URLs: []string{"https://am.example.org:9093/prefix/", "http://am2.example.org"}, Auth: authBearer},
	}
	values, raw := monitoringOf(t, &m, purposeRemoteWrite, purposeExternalAlertmanager)
	spec := lookup[map[string]any](t, values, prometheus, "prometheusSpec")
	if lookup[string](t, spec, "remoteWrite", 0, "url") != "https://mimir.example.org/api/v1/push" ||
		lookup[string](t, spec, "remoteWrite", 0, "basicAuth", "username", "key") != remoteWriteUserKey ||
		lookup[string](t, spec, "remoteWrite", 0, "basicAuth", "password", "name") != prometheusSecret ||
		lookup[string](t, spec, "remoteWrite", 0, "basicAuth", "password", "key") != remoteWriteKey {
		t.Errorf("remote write %v", spec["remoteWrite"])
	}
	file := "/etc/prometheus/secrets/kubehenge-prometheus/external-alertmanager-secret"
	if lookup[string](t, spec, "additionalAlertManagerConfigs", 0, "scheme") != "https" ||
		lookup[string](t, spec, "additionalAlertManagerConfigs", 0, "static_configs", 0, "targets", 0) != "am.example.org:9093" ||
		lookup[string](t, spec, "additionalAlertManagerConfigs", 0, "path_prefix") != "/prefix" ||
		lookup[string](t, spec, "additionalAlertManagerConfigs", 0, "authorization", "credentials_file") != file ||
		lookup[string](t, spec, "additionalAlertManagerConfigs", 1, "scheme") != "http" {
		t.Errorf("external Alertmanagers %v", spec["additionalAlertManagerConfigs"])
	}
	if secrets := lookup[[]any](t, spec, "secrets"); len(secrets) != 1 || secrets[0] != prometheusSecret {
		t.Errorf("secrets %v", secrets)
	}
	if strings.Contains(raw, writeValue) {
		t.Error("the values hold a secret")
	}

	bearer := Monitoring{RemoteWrite: &RemoteWrite{URL: "https://mimir.example.org/push", Auth: authBearer}}
	values, _ = monitoringOf(t, &bearer, purposeRemoteWrite)
	spec = lookup[map[string]any](t, values, prometheus, "prometheusSpec")
	if lookup[string](t, spec, "remoteWrite", 0, "authorization", "type") != "Bearer" || lookup[string](t, spec, "remoteWrite", 0, "authorization", "credentials", "key") != remoteWriteKey {
		t.Errorf("bearer remote write %v", spec["remoteWrite"])
	}

	open := Monitoring{RemoteWrite: &RemoteWrite{URL: "https://mimir.example.org/push"}}
	values, _ = monitoringOf(t, &open)
	spec = lookup[map[string]any](t, values, prometheus, "prometheusSpec")
	if lookup[string](t, spec, "remoteWrite", 0, "url") != "https://mimir.example.org/push" {
		t.Errorf("open remote write %v", spec["remoteWrite"])
	}
	if _, there := lookup[map[string]any](t, spec, "remoteWrite", 0)["basicAuth"]; there {
		t.Error("an open remote write signs in")
	}

	// A target that signs in waits for its credential rather than sending without it.
	values, _ = monitoringOf(t, &m)
	spec = lookup[map[string]any](t, values, prometheus, "prometheusSpec")
	for _, key := range []string{"remoteWrite", "additionalAlertManagerConfigs", "secrets"} {
		if _, there := spec[key]; there {
			t.Errorf("%s set without its credential", key)
		}
	}
}

func TestValidateMonitoring(t *testing.T) {
	t.Parallel()
	email := `{"type":"email","smarthost":"mail.example.org:587","from":"Alerts <alerts@example.org>","to":["ops@example.org"]}`
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"nothing", `{"enabled":true}`, ""},
		{"every receiver", `{"enabled":true,"alertReceivers":[` + email + `,{"type":"webhook"},{"type":"slack","channel":"#ops"},{"type":"teams"}]}`, ""},
		{"a receiver twice", `{"enabled":true,"alertReceivers":[{"type":"webhook"},{"type":"webhook"}]}`, "Duplicate value"},
		{"an email address that is none", `{"enabled":true,"alertReceivers":[{"type":"email","smarthost":"mail.example.org:587","from":"nobody","to":["ops@example.org"]}]}`, "must be an email address"},
		{"a recipient that is none", `{"enabled":true,"alertReceivers":[{"type":"email","smarthost":"mail.example.org:587","from":"a@example.org","to":["x"]}]}`, "alertReceivers[0].to[0]"},
		{"a mail server without a port", `{"enabled":true,"alertReceivers":[{"type":"email","smarthost":"mail.example.org","from":"a@example.org","to":["ops@example.org"]}]}`, "must be host:port"},
		{"mail settings on a webhook", `{"enabled":true,"alertReceivers":[{"type":"webhook","from":"a@example.org"}]}`, "only an email receiver takes from"},
		{"a channel on mail", `{"enabled":true,"alertReceivers":[` + strings.TrimSuffix(email, "}") + `,"channel":"#x"}]}`, "only a Slack receiver takes a channel"},
		{"a password in the remote write address", `{"enabled":true,"remoteWrite":{"url":"https://user:pass@mimir.example.org/push"}}`, "must hold no user name or password"},
		{"a remote write that is no URL", `{"enabled":true,"remoteWrite":{"url":"mimir"}}`, "must be an http or https address"},
		{"basic authentication without a user", `{"enabled":true,"remoteWrite":{"url":"https://m.example.org","auth":"basic"}}`, "basic authentication needs a user name"},
		{"a user without basic authentication", `{"enabled":true,"remoteWrite":{"url":"https://m.example.org","auth":"bearer","username":"x"}}`, "only basic authentication takes a user name"},
		{"an external Alertmanager with a secret in its address", `{"enabled":true,"externalAlertmanagers":{"urls":["https://ok.example.org","https://:token@am.example.org"]}}`, "externalAlertmanagers.urls[1]"},
	} {
		c := withAddons(storageCluster(), map[string]string{monitoringAddon: tc.raw})
		errs := validateMonitoring(c)
		got := errs.ToAggregate()
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: refused: %v", tc.name, got)
		case tc.want != "" && (got == nil || !strings.Contains(got.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, got, tc.want)
		}
	}
	// Nothing in the error repeats what the URL held.
	c := withAddons(storageCluster(), map[string]string{monitoringAddon: `{"enabled":true,"remoteWrite":{"url":"https://user:` + "hunter2" + `@mimir.example.org/push"}}`})
	if err := validateMonitoring(c).ToAggregate(); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %v", err)
	}
	if errs := validateMonitoring(storageCluster()); len(errs) != 0 {
		t.Errorf("a cluster without monitoring is refused: %v", errs)
	}
}

// monitoringReconcile is a cluster with monitoring on and its credentials stored in the management
// cluster, with a stand-in for its workload cluster.
type monitoringRig struct {
	r        *Reconciler
	kube     client.Client
	c        *clusterv1.Cluster
	workload *certWorkload
}

func monitoringReconcile(t *testing.T, variable string, secrets ...*corev1.Secret) monitoringRig {
	t.Helper()
	c := withAddons(storageCluster(), map[string]string{monitoringAddon: variable})
	objs := make([]client.Object, 0, 1+len(secrets))
	objs = append(objs, c)
	for _, s := range secrets {
		objs = append(objs, s)
	}
	r, kube := newReconciler(t, &fakeCloud{}, objs...)
	workload := newCertWorkload(t)
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	return monitoringRig{r, kube, c, workload}
}

func monitoringCredential(purpose string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		Namespace: "team-a", Name: CredentialName("web", purpose), Type: CredentialType,
		Labels: map[string]string{CredentialLabel: purpose}, Data: data,
	}
}

func appliedSecret(t *testing.T, w *certWorkload, name string) map[string]string {
	t.Helper()
	s := w.applied["Secret/"+MonitoringNamespace+"/"+name]
	if s == nil {
		return nil
	}
	out := map[string]string{}
	for k := range lookup[map[string]any](t, s, "data") {
		raw, err := base64.StdEncoding.DecodeString(lookup[string](t, s, "data", k))
		if err != nil {
			t.Fatal(err)
		}
		out[k] = string(raw)
	}
	return out
}

func TestMonitoringKeepsItsCredentialsInTheWorkloadCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	variable := `{"enabled":true,"alertReceivers":[{"type":"webhook"},{"type":"slack"}],"remoteWrite":{"url":"https://m.example.org/push","auth":"basic","username":"tenant"},` +
		`"externalAlertmanagers":{"urls":["https://am.example.org"],"auth":"bearer"}}`
	rig := monitoringReconcile(t, variable,
		monitoringCredential(purposeAlertsWebhook, map[string][]byte{urlKey: []byte("https://hook"), tokenKey: []byte("tok")}),
		monitoringCredential(purposeAlertsSlack, map[string][]byte{urlKey: []byte("https://slack")}),
		monitoringCredential(purposeRemoteWrite, map[string][]byte{secretKey: []byte("w")}),
		// A Secret of this name that is no credential is never taken for one.
		&corev1.Secret{Namespace: "team-a", Name: CredentialName("web", purposeExternalAlertmanager), Data: map[string][]byte{secretKey: []byte("planted")}},
	)
	r, kube, c, workload := rig.r, rig.kube, rig.c, rig.workload
	addons, _, err := r.addons(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := addons.monitoringFound.missing(&addons.Monitoring); !slices.Equal(got, []string{purposeExternalAlertmanager}) {
		t.Fatalf("missing %v", got)
	}

	// The chart creates the namespace: until it is there, the controller waits.
	waiting, err := r.reconcileMonitoringSecrets(ctx, c, &addons)
	if err != nil || !waiting {
		t.Fatalf("before the namespace: %v, %v", waiting, err)
	}
	workload.installed = true
	if waiting, err = r.reconcileMonitoringSecrets(ctx, c, &addons); err != nil || waiting {
		t.Fatalf("reconcile: %v, %v", waiting, err)
	}
	alertmanager := appliedSecret(t, workload, alertmanagerSecret)
	if alertmanager[webhookURLKey] != "https://hook" || alertmanager[webhookTokenKey] != "tok" || alertmanager[slackURLKey] != "https://slack" || len(alertmanager) != 3 {
		t.Errorf("Alertmanager's Secret %v", alertmanager)
	}
	prom := appliedSecret(t, workload, prometheusSecret)
	if prom[remoteWriteKey] != "w" || prom[remoteWriteUserKey] != "tenant" || len(prom) != 2 {
		t.Errorf("Prometheus's Secret %v", prom)
	}
	current := &clusterv1.Cluster{}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	if _, marked := current.Annotations[monitoringSecretsAnnotation]; !marked {
		t.Error("the cluster is not marked as holding monitoring's Secrets")
	}

	// A receiver dropped from the variable takes its value out of the Secret.
	c = current
	addons.Monitoring.AlertReceivers = addons.Monitoring.AlertReceivers[:1]
	if _, err = r.reconcileMonitoringSecrets(ctx, c, &addons); err != nil {
		t.Fatal(err)
	}
	if alertmanager = appliedSecret(t, workload, alertmanagerSecret); alertmanager[slackURLKey] != "" || alertmanager[webhookURLKey] == "" {
		t.Errorf("Alertmanager's Secret after dropping Slack: %v", alertmanager)
	}

	// Monitoring switched off takes both Secrets, and the mark, away.
	addons.Monitoring.Enabled = false
	if _, err = r.reconcileMonitoringSecrets(ctx, c, &addons); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{alertmanagerSecret, prometheusSecret} {
		if !slices.ContainsFunc(workload.deleted, func(k string) bool { return strings.HasSuffix(k, "/"+MonitoringNamespace+"/"+name) }) {
			t.Errorf("%s not deleted: %v", name, workload.deleted)
		}
	}
	if err = kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	if _, marked := current.Annotations[monitoringSecretsAnnotation]; marked {
		t.Error("the cluster is still marked as holding monitoring's Secrets")
	}
	// Nothing to do once they are gone.
	workload.deleted = nil
	if _, err = r.reconcileMonitoringSecrets(ctx, current, &addons); err != nil || len(workload.deleted) != 0 {
		t.Errorf("reached again: %v, %v", workload.deleted, err)
	}
}

func TestMonitoringCredentialPurposesAreStoredPerCluster(t *testing.T) {
	t.Parallel()
	for _, name := range []string{purposeAlertsEmail, purposeAlertsWebhook, purposeAlertsSlack, purposeAlertsTeams, purposeRemoteWrite, purposeExternalAlertmanager} {
		p, ok := CredentialPurposeNamed(name)
		if !ok || p.Appliance || p.ApplianceOnly || len(p.Keys) == 0 {
			t.Errorf("purpose %s: %+v, %v", name, p, ok)
		}
	}
}
