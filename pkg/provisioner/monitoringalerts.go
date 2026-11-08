package provisioner

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The receivers of Alertmanager's own that a cluster can name. Each takes the same alerts, and each
// is one receiver type of Alertmanager, so that a Slack-compatible or Teams-compatible service
// needs no gateway.
const (
	alertEmail   = "email"
	alertWebhook = "webhook"
	alertSlack   = "slack"
	alertTeams   = "teams"
)

// How a remote write target or an external Alertmanager is signed in to.
const (
	authNone   = "none"
	authBasic  = "basic"
	authBearer = "bearer"
)

// The user credentials of the monitoring add-on, by purpose (credentials.go). Each holds what the
// cluster's variable must not: a password, a token, or a URL that carries one.
const (
	purposeAlertsEmail          = "alerts-email"
	purposeAlertsWebhook        = "alerts-webhook"
	purposeAlertsSlack          = "alerts-slack"
	purposeAlertsTeams          = "alerts-teams"
	purposeRemoteWrite          = "remote-write"
	purposeExternalAlertmanager = "external-alertmanager"

	// secretKey is the one value of a remote write or external Alertmanager credential: the password
	// of basic authentication, or the bearer token.
	secretKey = "secret"
	// tokenKey is the optional bearer token of a webhook's credential.
	tokenKey = "token"

	credentialsAuthKey = "authorization"
	schemeKey          = "scheme"
)

const (
	// alertmanagerSecret and prometheusSecret are the Secrets the controller keeps in the monitoring
	// namespace of the workload cluster, from the cluster's stored credentials. They never pass
	// through the chart's values, which are plain text in the management cluster.
	alertmanagerSecret = "kubehenge-alertmanager" //nolint:gosec // a Secret's name, not a credential
	prometheusSecret   = "kubehenge-prometheus"   //nolint:gosec // a Secret's name, not a credential
	// monitoringSecretsAnnotation marks a cluster whose workload cluster may hold those Secrets, until
	// the controller has taken them out again.
	monitoringSecretsAnnotation = "kubehenge.dev/monitoring-secrets" //nolint:gosec // an annotation's name, not a credential

	// The operator mounts a Secret named in a spec's secrets under these directories.
	alertmanagerSecretDir = "/etc/alertmanager/secrets/" + alertmanagerSecret
	prometheusSecretDir   = "/etc/prometheus/secrets/" + prometheusSecret

	// The keys of the two Secrets.
	emailPasswordKey   = "email-password"
	webhookURLKey      = "webhook-url"
	webhookTokenKey    = "webhook-token"
	slackURLKey        = "slack-url"
	teamsURLKey        = "teams-url"
	remoteWriteUserKey = "remote-write-username"
	remoteWriteKey     = "remote-write-secret"
	externalAlertKey   = "external-alertmanager-secret"

	// kubehengeReceiver is the one receiver all the cluster's integrations belong to.
	kubehengeReceiver = "kubehenge"
	nullReceiver      = "null"
)

// AlertReceiver is where Alertmanager sends what fires. Email takes smarthost, from and to. A
// webhook, a Slack-compatible service and a Teams-compatible service take their URL from the
// credential of their type, a webhook its bearer token too.
type AlertReceiver struct {
	Type string `json:"type"`
	// Smarthost, From, To, Username and RequireTLS are the email receiver's. Its password is the
	// alerts-email credential.
	Smarthost  string   `json:"smarthost,omitempty"`
	From       string   `json:"from,omitempty"`
	To         []string `json:"to,omitempty"`
	Username   string   `json:"username,omitempty"`
	RequireTLS *bool    `json:"requireTLS,omitempty"`
	// Channel is the Slack-compatible receiver's channel, where the URL's own is not wanted.
	Channel string `json:"channel,omitempty"`
}

// RemoteWrite is a store that takes Prometheus's samples. Its secret is the remote-write credential.
type RemoteWrite struct {
	URL      string `json:"url"`
	Auth     string `json:"auth,omitempty"`
	Username string `json:"username,omitempty"`
}

// ExternalAlertmanagers are Alertmanagers besides the cluster's own. Their secret is the
// external-alertmanager credential.
type ExternalAlertmanagers struct {
	URLs     []string `json:"urls"`
	Auth     string   `json:"auth,omitempty"`
	Username string   `json:"username,omitempty"`
}

func (r *RemoteWrite) auth() string {
	if r.Auth == "" {
		return authNone
	}
	return r.Auth
}

func (e *ExternalAlertmanagers) auth() string {
	if e.Auth == "" {
		return authNone
	}
	return e.Auth
}

func (a *AlertReceiver) requireTLS() bool { return a.RequireTLS == nil || *a.RequireTLS }

// monitoringState is what the controller found of the monitoring add-on's stored credentials: each
// purpose's values, for those that hold what they need.
type monitoringState struct {
	credentials map[string]map[string][]byte
}

// receiverPurpose is the credential an alert receiver type takes.
var receiverPurpose = map[string]string{
	alertEmail: purposeAlertsEmail, alertWebhook: purposeAlertsWebhook, alertSlack: purposeAlertsSlack, alertTeams: purposeAlertsTeams,
}

func (s *monitoringState) value(purpose, key string) []byte {
	return s.credentials[purpose][key]
}

// needsCredential reports whether an alert receiver cannot work without its credential: all but an
// email receiver that signs in to nothing.
func (a *AlertReceiver) needsCredential() bool {
	return a.Type != alertEmail || a.Username != ""
}

// active lists the receivers whose credential is there when they need one.
func (s *monitoringState) active(m *Monitoring) []AlertReceiver {
	var active []AlertReceiver
	for i := range m.AlertReceivers {
		a := &m.AlertReceivers[i]
		if !a.needsCredential() || s.credentials[receiverPurpose[a.Type]] != nil {
			active = append(active, *a)
		}
	}
	return active
}

// remoteWriteOn reports whether Prometheus has a remote write target to send to.
func (s *monitoringState) remoteWriteOn(m *Monitoring) bool {
	return m.RemoteWrite != nil && (m.RemoteWrite.auth() == authNone || s.credentials[purposeRemoteWrite] != nil)
}

// externalOn reports whether Prometheus has external Alertmanagers to send to.
func (s *monitoringState) externalOn(m *Monitoring) bool {
	e := m.ExternalAlertmanagers
	return e != nil && len(e.URLs) > 0 && (e.auth() == authNone || s.credentials[purposeExternalAlertmanager] != nil)
}

// missing names the stored credentials the cluster's settings need that are not there.
func (s *monitoringState) missing(m *Monitoring) []string {
	var missing []string
	for i := range m.AlertReceivers {
		a := &m.AlertReceivers[i]
		if a.needsCredential() && s.credentials[receiverPurpose[a.Type]] == nil {
			missing = append(missing, receiverPurpose[a.Type])
		}
	}
	if m.RemoteWrite != nil && !s.remoteWriteOn(m) {
		missing = append(missing, purposeRemoteWrite)
	}
	if m.ExternalAlertmanagers != nil && !s.externalOn(m) {
		missing = append(missing, purposeExternalAlertmanager)
	}
	return missing
}

// alertmanagerData is what Alertmanager's Secret holds: the values of the receivers that are active.
func (s *monitoringState) alertmanagerData(m *Monitoring) map[string][]byte {
	data := map[string][]byte{}
	for _, a := range s.active(m) {
		switch a.Type {
		case alertEmail:
			if a.Username != "" {
				data[emailPasswordKey] = s.value(purposeAlertsEmail, passwordKey)
			}
		case alertWebhook:
			data[webhookURLKey] = s.value(purposeAlertsWebhook, urlKey)
			if token := s.value(purposeAlertsWebhook, tokenKey); len(token) > 0 {
				data[webhookTokenKey] = token
			}
		case alertSlack:
			data[slackURLKey] = s.value(purposeAlertsSlack, urlKey)
		case alertTeams:
			data[teamsURLKey] = s.value(purposeAlertsTeams, urlKey)
		}
	}
	return data
}

// prometheusData is what Prometheus's Secret holds: the remote write target's and the external
// Alertmanagers' secrets. The user name is no secret, but a remote write target's basic
// authentication takes it from a Secret as well.
func (s *monitoringState) prometheusData(m *Monitoring) map[string][]byte {
	data := map[string][]byte{}
	if s.remoteWriteOn(m) && m.RemoteWrite.auth() != authNone {
		data[remoteWriteKey] = s.value(purposeRemoteWrite, secretKey)
		if m.RemoteWrite.auth() == authBasic {
			data[remoteWriteUserKey] = []byte(m.RemoteWrite.Username)
		}
	}
	if s.externalOn(m) && m.ExternalAlertmanagers.auth() != authNone {
		data[externalAlertKey] = s.value(purposeExternalAlertmanager, secretKey)
	}
	return data
}

// alertmanagerValues are Alertmanager's: its volume and requests, and when the cluster names
// receivers, a route to them besides the chart's own for the Watchdog alert, which fires always and
// goes nowhere. Every secret is a file in the Secret the controller keeps, never a value.
func alertmanagerValues(m *Monitoring, state *monitoringState) map[string]any {
	spec := map[string]any{
		"storage":   claim(alertmanagerStorage),
		"resources": reserved(cpu10m, "32Mi"),
		"portName":  alertmanagerPort,
	}
	values := map[string]any{"alertmanagerSpec": spec}
	integrations := state.integrations(m)
	if len(integrations) == 0 {
		return values
	}
	if len(state.alertmanagerData(m)) > 0 {
		spec["secrets"] = []any{alertmanagerSecret}
	}
	receiver := map[string]any{"name": kubehengeReceiver}
	maps.Copy(receiver, integrations)
	values["config"] = map[string]any{
		"route": map[string]any{
			"receiver": kubehengeReceiver,
			"routes":   []any{map[string]any{"receiver": nullReceiver, "matchers": []any{`alertname = "Watchdog"`}}},
		},
		"receivers": []any{map[string]any{"name": nullReceiver}, receiver},
	}
	return values
}

// integrations are the active receivers as Alertmanager's receiver configurations, by their key.
func (s *monitoringState) integrations(m *Monitoring) map[string]any {
	out := map[string]any{}
	for _, a := range s.active(m) {
		switch a.Type {
		case alertEmail:
			out["email_configs"] = []any{emailConfig(&a)}
		case alertWebhook:
			out["webhook_configs"] = []any{webhookConfig(s.value(purposeAlertsWebhook, tokenKey))}
		case alertSlack:
			slack := map[string]any{"api_url_file": alertmanagerSecretDir + "/" + slackURLKey, sendResolved: true}
			if a.Channel != "" {
				slack["channel"] = a.Channel
			}
			out["slack_configs"] = []any{slack}
		case alertTeams:
			out["msteamsv2_configs"] = []any{map[string]any{"webhook_url_file": alertmanagerSecretDir + "/" + teamsURLKey, sendResolved: true}}
		}
	}
	return out
}

const sendResolved = "send_resolved"

func emailConfig(a *AlertReceiver) map[string]any {
	config := map[string]any{
		"to": strings.Join(a.To, ", "), "from": a.From, "smarthost": a.Smarthost,
		"require_tls": a.requireTLS(), sendResolved: true,
	}
	if a.Username != "" {
		config["auth_username"] = a.Username
		config["auth_password_file"] = alertmanagerSecretDir + "/" + emailPasswordKey
	}
	return config
}

func webhookConfig(token []byte) map[string]any {
	config := map[string]any{"url_file": alertmanagerSecretDir + "/" + webhookURLKey, sendResolved: true}
	if len(token) > 0 {
		config["http_config"] = map[string]any{credentialsAuthKey: map[string]any{"credentials_file": alertmanagerSecretDir + "/" + webhookTokenKey}}
	}
	return config
}

// prometheusSpec is Prometheus's: its volume, retention and requests, and what the cluster names to
// send samples and alerts to.
func prometheusSpec(m *Monitoring, state *monitoringState) map[string]any {
	spec := map[string]any{
		"storageSpec":   claim(prometheusStorage),
		"retentionSize": prometheusRetention,
		"resources":     reserved("100m", "256Mi"),
		// Prometheus selects every monitor and rule of the cluster, not only those labelled for the
		// release: the chart's defaults leave a workload's own ServiceMonitors, PodMonitors, Probes,
		// ScrapeConfigs and PrometheusRules unread. The chart's own carry the label too, so each is
		// read once.
		"serviceMonitorSelectorNilUsesHelmValues": false,
		"podMonitorSelectorNilUsesHelmValues":     false,
		"probeSelectorNilUsesHelmValues":          false,
		"scrapeConfigSelectorNilUsesHelmValues":   false,
		"ruleSelectorNilUsesHelmValues":           false,
	}
	if state.remoteWriteOn(m) {
		spec["remoteWrite"] = []any{remoteWriteConfig(m.RemoteWrite)}
	}
	if state.externalOn(m) {
		spec["additionalAlertManagerConfigs"] = externalConfigs(m.ExternalAlertmanagers)
		if e := m.ExternalAlertmanagers; e.auth() != authNone {
			spec["secrets"] = []any{prometheusSecret}
		}
	}
	return spec
}

func remoteWriteConfig(r *RemoteWrite) map[string]any {
	selector := func(key string) map[string]any { return map[string]any{"name": prometheusSecret, "key": key} }
	config := map[string]any{"url": r.URL}
	switch r.auth() {
	case authBasic:
		config["basicAuth"] = map[string]any{usernameKey: selector(remoteWriteUserKey), "password": selector(remoteWriteKey)}
	case authBearer:
		config[credentialsAuthKey] = map[string]any{"type": "Bearer", "credentials": selector(remoteWriteKey)}
	}
	return config
}

// externalConfigs are Prometheus's alertmanager configurations, one for each address, since the
// scheme and path prefix belong to a configuration. A secret is a file in the Secret the
// controller keeps.
func externalConfigs(e *ExternalAlertmanagers) []any {
	file := prometheusSecretDir + "/" + externalAlertKey
	configs := make([]any, 0, len(e.URLs))
	for _, raw := range e.URLs {
		u, err := url.Parse(raw)
		if err != nil {
			// Refused at admission.
			continue
		}
		config := map[string]any{schemeKey: u.Scheme, "static_configs": []any{map[string]any{"targets": []any{u.Host}}}}
		if prefix := strings.TrimSuffix(u.Path, "/"); prefix != "" {
			config["path_prefix"] = prefix
		}
		switch e.auth() {
		case authBasic:
			config["basic_auth"] = map[string]any{usernameKey: e.Username, "password_file": file}
		case authBearer:
			config[credentialsAuthKey] = map[string]any{"credentials_file": file}
		}
		configs = append(configs, config)
	}
	return configs
}

// validateMonitoring holds the monitoring add-on's alert, remote write and external Alertmanager
// settings to what the class's schema cannot say: addresses that parse, hold no secret, and
// receivers that are given once.
func validateMonitoring(cluster *clusterv1.Cluster) field.ErrorList {
	variable := field.NewPath("spec", "topology", "variables").Key(monitoringAddon)
	m, _, err := optionalVariable[Monitoring](cluster, monitoringAddon)
	if err != nil {
		return field.ErrorList{field.Invalid(variable, monitoringAddon, err.Error())}
	}
	var errs field.ErrorList
	seen := map[string]bool{}
	for i := range m.AlertReceivers {
		a := &m.AlertReceivers[i]
		path := variable.Child("alertReceivers").Index(i)
		if seen[a.Type] {
			errs = append(errs, field.Duplicate(path.Child("type"), a.Type))
		}
		seen[a.Type] = true
		errs = append(errs, a.validate(path)...)
	}
	if r := m.RemoteWrite; r != nil {
		path := variable.Child("remoteWrite")
		errs = append(errs, validateTarget(path, []*field.Path{path.Child("url")}, []string{r.URL}, r.auth(), r.Username)...)
	}
	if e := m.ExternalAlertmanagers; e != nil {
		path := variable.Child("externalAlertmanagers")
		paths := make([]*field.Path, len(e.URLs))
		for i := range e.URLs {
			paths[i] = path.Child("urls").Index(i)
		}
		errs = append(errs, validateTarget(path, paths, e.URLs, e.auth(), e.Username)...)
	}
	return errs
}

func (a *AlertReceiver) validate(path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if a.Type != alertEmail {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"smarthost", a.Smarthost != ""}, {"from", a.From != ""}, {"to", len(a.To) > 0}, {usernameKey, a.Username != ""}, {"requireTLS", a.RequireTLS != nil},
		} {
			if f.set {
				errs = append(errs, field.Forbidden(path.Child(f.name), "only an email receiver takes "+f.name))
			}
		}
	}
	if a.Type != alertSlack && a.Channel != "" {
		errs = append(errs, field.Forbidden(path.Child("channel"), "only a Slack receiver takes a channel"))
	}
	if a.Type != alertEmail {
		return errs
	}
	if _, _, err := net.SplitHostPort(a.Smarthost); err != nil {
		errs = append(errs, field.Invalid(path.Child("smarthost"), a.Smarthost, "must be host:port"))
	}
	if _, err := mail.ParseAddress(a.From); err != nil {
		errs = append(errs, field.Invalid(path.Child("from"), a.From, "must be an email address"))
	}
	if len(a.To) == 0 {
		errs = append(errs, field.Required(path.Child("to"), "an email receiver needs at least one address"))
	}
	for i, to := range a.To {
		if _, err := mail.ParseAddress(to); err != nil {
			errs = append(errs, field.Invalid(path.Child("to").Index(i), to, "must be an email address"))
		}
	}
	return errs
}

// validateTarget holds addresses to http or https without a user name or password in them, which
// would sit in the variable in plain text, and basic authentication to a user name.
func validateTarget(path *field.Path, at []*field.Path, urls []string, auth, username string) field.ErrorList {
	var errs field.ErrorList
	for i, raw := range urls {
		u, err := url.Parse(raw)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			errs = append(errs, field.Invalid(at[i], raw, "must be an http or https address"))
		case u.User != nil:
			errs = append(errs, field.Invalid(at[i], "(hidden)", "must hold no user name or password. Use auth and the stored credential"))
		}
	}
	if auth == authBasic && username == "" {
		errs = append(errs, field.Required(path.Child(usernameKey), "basic authentication needs a user name"))
	}
	if auth != authBasic && username != "" {
		errs = append(errs, field.Forbidden(path.Child(usernameKey), "only basic authentication takes a user name"))
	}
	return errs
}

// monitoringCredentials reads the stored credentials the monitoring add-on's settings name, and
// tells the cluster which of them are not there, so that the receiver waits instead of failing.
func (r *Reconciler) monitoringCredentials(ctx context.Context, cluster *clusterv1.Cluster, m *Monitoring) (monitoringState, error) {
	state := monitoringState{credentials: map[string]map[string][]byte{}}
	if !m.Enabled {
		return state, nil
	}
	wanted := map[string]bool{}
	for i := range m.AlertReceivers {
		wanted[receiverPurpose[m.AlertReceivers[i].Type]] = true
	}
	if m.RemoteWrite != nil {
		wanted[purposeRemoteWrite] = true
	}
	if m.ExternalAlertmanagers != nil {
		wanted[purposeExternalAlertmanager] = true
	}
	for purpose := range wanted {
		values, ok, err := r.credential(ctx, cluster, purpose)
		if err != nil {
			return state, err
		}
		if ok {
			state.credentials[purpose] = values
		}
	}
	if missing := state.missing(m); len(missing) > 0 {
		slices.Sort(missing)
		ctrl.LoggerFrom(ctx).Info("monitoring waits for stored credentials", "credentials", missing)
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "MonitoringWaiting", "SendAlerts",
				"monitoring leaves out what needs a credential not stored for the cluster yet: %s", strings.Join(missing, ", "))
		}
	}
	return state, nil
}

// reconcileMonitoringSecrets keeps the two Secrets of the monitoring add-on's credentials in the
// workload cluster's monitoring namespace, and takes them out again when nothing needs them. It
// reports whether it waits for the namespace, which the chart creates.
func (r *Reconciler) reconcileMonitoringSecrets(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) (bool, error) {
	_, marked := cluster.Annotations[monitoringSecretsAnnotation]
	m, state := &addons.Monitoring, &addons.monitoringFound
	wanted := map[string]map[string][]byte{}
	if m.Enabled {
		wanted[alertmanagerSecret], wanted[prometheusSecret] = state.alertmanagerData(m), state.prometheusData(m)
	}
	needed := len(wanted[alertmanagerSecret]) > 0 || len(wanted[prometheusSecret]) > 0
	if !needed && !marked {
		return false, nil
	}
	if needed && !marked {
		// Marked first, so that whatever is applied from here on is taken out again.
		if err := r.markMonitoringSecrets(ctx, cluster); err != nil {
			return false, err
		}
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return needed, err
	}
	waiting, err := writeMonitoringSecrets(ctx, workload.client, wanted)
	if err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	if !needed && !waiting {
		return false, r.unmarkMonitoringSecrets(ctx, cluster)
	}
	return waiting, nil
}

// writeMonitoringSecrets applies the Secrets that hold a value and deletes the others.
func writeMonitoringSecrets(ctx context.Context, c client.Client, wanted map[string]map[string][]byte) (bool, error) {
	waiting := false
	for _, name := range []string{alertmanagerSecret, prometheusSecret} {
		data := wanted[name]
		if len(data) == 0 {
			if err := deleteSecrets(ctx, c, MonitoringNamespace, name); err != nil {
				return false, err
			}
			continue
		}
		o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&corev1.Secret{
			APIVersion: "v1", Kind: secretKind,
			Namespace: MonitoringNamespace, Name: name,
			Type: corev1.SecretTypeOpaque,
			Data: data,
		})
		if err != nil {
			return false, fmt.Errorf("writing Secret %s: %w", name, err)
		}
		w, err := applyInWorkload(ctx, c, &unstructured.Unstructured{Object: o})
		if err != nil {
			return false, err
		}
		waiting = waiting || w
	}
	return waiting, nil
}

func (r *Reconciler) markMonitoringSecrets(ctx context.Context, cluster *clusterv1.Cluster) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[monitoringSecretsAnnotation] = strconv.FormatBool(true)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("marking the cluster as holding monitoring's Secrets: %w", err)
	}
	return nil
}

func (r *Reconciler) unmarkMonitoringSecrets(ctx context.Context, cluster *clusterv1.Cluster) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	delete(cluster.Annotations, monitoringSecretsAnnotation)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("marking the cluster as holding no monitoring Secrets: %w", err)
	}
	return nil
}
