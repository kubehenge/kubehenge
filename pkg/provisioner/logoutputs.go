package provisioner

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// LogOutput is a destination every log also goes to, beside the cluster's Loki.
type LogOutput struct {
	// Name labels the output and its credential, so that an output can move in the list without
	// its credential going to another destination.
	Name string `json:"name"`
	// Kind is syslog, otlp or loki.
	Kind string `json:"kind"`
	// Endpoint is tcp://, tls:// or udp:// host:port for syslog, the collector's base URL for otlp
	// and Loki's whole push URL for loki.
	Endpoint string `json:"endpoint"`
	// CABundle is PEM text that signs the endpoint's certificate, for a private CA.
	CABundle string `json:"caBundle,omitempty"`
	// Streams limits the output to some of the logs. None means all of them.
	Streams []string `json:"streams,omitempty"`
	// Auth is none (default), basic or bearer, whose values are the output's stored credential.
	Auth string `json:"auth,omitempty"`
}

const (
	logOutputSyslog = "syslog"
	logOutputOTLP   = "otlp"
	logOutputLoki   = "loki"

	logAuthNone   = "none"
	logAuthBasic  = "basic"
	logAuthBearer = "bearer"

	// The streams an output can be limited to. The Alloy configurations name them as @<stream>.
	streamContainers = "containers"
	streamEvents     = "events"
	streamNodes      = "nodes"
	streamAudit      = "audit"

	// maxLogOutputs and maxLogOutputName keep the Alloy configuration small and the credential's
	// purpose, log-<name>, far inside a label's 63 characters.
	maxLogOutputs    = 4
	maxLogOutputName = 20
	maxCABundleBytes = 16 << 10

	// LogOutputPurposePrefix starts the purpose of an output's stored credential.
	LogOutputPurposePrefix = "log-"
	// logOutputsSecret holds the credentials of every output, in the namespaces of both Alloys, as
	// <output>.username, <output>.password or <output>.token.
	logOutputsSecret = "kubehenge-log-outputs" //nolint:gosec // a Secret's name, not a credential
	logOutputsPath   = "/etc/kubehenge/log-outputs"
	logOutputsVolume = "log-outputs"

	credentialUsername = "username"
	credentialPassword = "password"
	credentialToken    = "token"

	syslogDefaultPort = 514

	// logOutputsAnnotation records on the Cluster what was last written into its workload cluster
	// as logOutputsSecret, so that the controller reaches it only when that changes.
	logOutputsAnnotation = "kubehenge.dev/log-outputs-secret" //nolint:gosec // an annotation's name, not a credential

	schemeTCP      = "tcp"
	schemeTLS      = "tls"
	schemeUDP      = "udp"
	pemCertificate = "CERTIFICATE"
)

var (
	logOutputKinds = []string{logOutputSyslog, logOutputOTLP, logOutputLoki}
	logStreams     = []string{streamContainers, streamEvents, streamNodes, streamAudit}
	logAuths       = []string{logAuthNone, logAuthBasic, logAuthBearer}
)

// CredentialPurpose is the purpose of the output's stored credential.
func (o *LogOutput) CredentialPurpose() string {
	return LogOutputPurposePrefix + o.Name
}

func (o *LogOutput) auth() string {
	if o.Auth == "" {
		return logAuthNone
	}
	return o.Auth
}

// takes tells whether the output gets a stream.
func (o *LogOutput) takes(stream string) bool {
	return len(o.Streams) == 0 || slices.Contains(o.Streams, stream)
}

// id names the output's Alloy components, whose labels allow no hyphen.
func (o *LogOutput) id() string {
	return "out_" + strings.ReplaceAll(o.Name, "-", "_")
}

// logOutputPurpose finds the credential purpose of a log output by its name, which no static list
// holds as outputs are named by the cluster.
func logOutputPurpose(name string) (CredentialPurpose, bool) {
	rest, ok := strings.CutPrefix(name, LogOutputPurposePrefix)
	if !ok || len(rest) > maxLogOutputName || len(validation.IsDNS1123Label(rest)) > 0 {
		return CredentialPurpose{}, false
	}
	return CredentialPurpose{Name: name, Optional: []string{credentialUsername, credentialPassword, credentialToken}}, true
}

// validateLogOutputs refuses outputs the Alloy configuration cannot honour.
func validateLogOutputs(outputs []LogOutput) error {
	if len(outputs) > maxLogOutputs {
		return fmt.Errorf("logs take at most %d outputs", maxLogOutputs)
	}
	seen := map[string]bool{}
	for i := range outputs {
		o := &outputs[i]
		if len(o.Name) > maxLogOutputName || len(validation.IsDNS1123Label(o.Name)) > 0 {
			return fmt.Errorf("log output name %q must be a lower-case label of at most %d characters", o.Name, maxLogOutputName)
		}
		if seen[o.Name] {
			return fmt.Errorf("two log outputs are named %s", o.Name)
		}
		seen[o.Name] = true
		if err := o.validate(); err != nil {
			return fmt.Errorf("log output %s: %w", o.Name, err)
		}
	}
	return nil
}

func (o *LogOutput) validate() error {
	if !slices.Contains(logOutputKinds, o.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(logOutputKinds, ", "))
	}
	if !slices.Contains(logAuths, o.auth()) {
		return fmt.Errorf("auth must be one of %s", strings.Join(logAuths, ", "))
	}
	if o.Kind == logOutputSyslog && o.auth() != logAuthNone {
		return errors.New("syslog takes no authentication")
	}
	for i, s := range o.Streams {
		if !slices.Contains(logStreams, s) {
			return fmt.Errorf("stream %q must be one of %s", s, strings.Join(logStreams, ", "))
		}
		if slices.Contains(o.Streams[:i], s) {
			return fmt.Errorf("stream %s is listed twice", s)
		}
	}
	u, err := o.endpointURL()
	if err != nil {
		return err
	}
	encrypted := u.Scheme == schemeHTTPS || u.Scheme == schemeTLS
	if o.CABundle != "" {
		if !encrypted {
			return errors.New("a CA bundle needs an https or tls endpoint")
		}
		if _, err = normalisedBundle(o.CABundle); err != nil {
			return err
		}
	}
	if o.auth() != logAuthNone && !encrypted {
		return errors.New("credentials go only to an https endpoint")
	}
	return nil
}

// endpointURL parses the endpoint and holds it to its kind's schemes.
func (o *LogOutput) endpointURL() (*url.URL, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("endpoint must be a URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("endpoint holds no user, query or fragment: credentials are stored, not typed into the URL")
	}
	switch o.Kind {
	case logOutputSyslog:
		if !slices.Contains([]string{schemeTCP, schemeTLS, schemeUDP}, u.Scheme) {
			return nil, errors.New("a syslog endpoint starts with tcp://, tls:// or udp://")
		}
		if u.Port() == "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("a syslog endpoint is scheme://host:port")
		}
	case logOutputOTLP, logOutputLoki:
		if u.Scheme != schemeHTTPS && u.Scheme != "http" {
			return nil, errors.New("endpoint starts with https:// or http://")
		}
	}
	return u, nil
}

// normalisedBundle parses a CA bundle and gives it back as certificates alone, so that nothing
// else of what was typed reaches the Alloy configuration.
func normalisedBundle(bundle string) (string, error) {
	if len(bundle) > maxCABundleBytes {
		return "", errors.New("the CA bundle is too large")
	}
	var out []byte
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemCertificate {
			return "", errors.New("the CA bundle holds certificates alone")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return "", fmt.Errorf("the CA bundle holds a certificate that does not parse: %w", err)
		}
		out = append(out, pem.EncodeToMemory(block)...)
	}
	if len(out) == 0 {
		return "", errors.New("the CA bundle holds no PEM certificate")
	}
	return string(out), nil
}

// needsCredentials lists the outputs that read a stored credential.
func needsCredentials(outputs []LogOutput) []LogOutput {
	var out []LogOutput
	for _, o := range outputs {
		if o.auth() != logAuthNone {
			out = append(out, o)
		}
	}
	return out
}

// logOutputSecretData is the Secret that holds the outputs' credentials in the workload cluster.
// The message names an output whose stored credential lacks what its auth needs.
func logOutputSecretData(outputs []LogOutput, stored map[string]map[string][]byte) (map[string][]byte, string) {
	data := map[string][]byte{}
	for _, o := range needsCredentials(outputs) {
		values := stored[o.Name]
		keys := []string{credentialToken}
		if o.auth() == logAuthBasic {
			keys = []string{credentialUsername, credentialPassword}
		}
		for _, k := range keys {
			if len(values[k]) == 0 {
				return nil, fmt.Sprintf("log output %s has no %s in its stored credential %s", o.Name, k, o.CredentialPurpose())
			}
			data[o.Name+"."+k] = values[k]
		}
	}
	return data, ""
}

// routed gives a configuration its streams' receivers, and appends the outputs that any of its
// streams reach. Loki's own is always first.
func routed(config string, outputs []LogOutput) string {
	var parts []string
	for _, stream := range logStreams {
		token := "@" + stream
		if !strings.Contains(config, token) {
			continue
		}
		receivers := []string{"loki.write.loki.receiver"}
		for _, o := range outputs {
			if o.takes(stream) {
				receivers = append(receivers, o.receiver())
			}
		}
		config = strings.ReplaceAll(config, token, strings.Join(receivers, ", "))
	}
	for _, o := range outputs {
		if o.reached(config) {
			parts = append(parts, o.alloy())
		}
	}
	return config + strings.Join(parts, "")
}

// reached tells whether the (already routed) configuration forwards to the output.
func (o *LogOutput) reached(config string) bool {
	return strings.Contains(config, o.receiver())
}

func (o *LogOutput) receiver() string {
	if o.Kind == logOutputLoki {
		return "loki.write." + o.id() + ".receiver"
	}
	return "otelcol.receiver.loki." + o.id() + ".receiver"
}

// alloy is the output's components. The endpoint and bundle passed validation, and strconv.Quote
// leaves nothing in a string that ends it.
func (o *LogOutput) alloy() string {
	var b strings.Builder
	b.WriteString("\n")
	u, _ := o.endpointURL()
	bundle, _ := normalisedBundle(o.CABundle)
	b.WriteString(o.secretFiles())
	switch o.Kind {
	case logOutputLoki:
		o.writeLoki(&b, bundle)
	case logOutputOTLP:
		o.writeOTLP(&b, bundle)
	case logOutputSyslog:
		o.writeSyslog(&b, u, bundle)
	}
	return b.String()
}

func (o *LogOutput) file(key string) string {
	return "local.file." + o.id() + "_" + key + ".content"
}

// secretFiles reads the output's credential from the mounted Secret, so that a changed credential
// reaches Alloy without a restart and nothing secret is in the configuration.
func (o *LogOutput) secretFiles() string {
	var keys []string
	switch o.auth() {
	case logAuthBasic:
		keys = []string{credentialUsername, credentialPassword}
	case logAuthBearer:
		keys = []string{credentialToken}
	}
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "local.file %q {\n  filename  = %q\n  is_secret = %t\n}\n\n", o.id()+"_"+k, logOutputsPath+"/"+o.Name+"."+k, k != credentialUsername)
	}
	return b.String()
}

func (o *LogOutput) writeLoki(b *strings.Builder, bundle string) {
	fmt.Fprintf(b, "loki.write %q {\n  endpoint {\n    url = %s\n", o.id(), strconv.Quote(o.Endpoint))
	switch o.auth() {
	case logAuthBasic:
		fmt.Fprintf(b, "    basic_auth {\n      username = %s\n      password = %s\n    }\n", o.file(credentialUsername), o.file(credentialPassword))
	case logAuthBearer:
		// spellchecker:disable-next-line
		fmt.Fprintf(b, "    authorization {\n      type        = \"Bearer\"\n      credentials = %s\n    }\n", o.file(credentialToken))
	}
	if bundle != "" {
		fmt.Fprintf(b, "    tls_config {\n      ca_pem = `%s`\n    }\n", bundle)
	}
	b.WriteString("  }\n}\n")
}

func (o *LogOutput) writeOTLP(b *strings.Builder, bundle string) {
	fmt.Fprintf(b, "otelcol.receiver.loki %q {\n  output {\n    logs = [otelcol.exporter.otlphttp.%s.input]\n  }\n}\n\n", o.id(), o.id())
	fmt.Fprintf(b, "otelcol.exporter.otlphttp %q {\n  client {\n    endpoint = %s\n", o.id(), strconv.Quote(o.Endpoint))
	switch o.auth() {
	case logAuthBasic:
		fmt.Fprintf(b, "    auth     = otelcol.auth.basic.%s.handler\n", o.id())
	case logAuthBearer:
		fmt.Fprintf(b, "    auth     = otelcol.auth.bearer.%s.handler\n", o.id())
	}
	if bundle != "" {
		fmt.Fprintf(b, "    tls {\n      ca_pem = `%s`\n    }\n", bundle)
	}
	b.WriteString("  }\n}\n")
	switch o.auth() {
	case logAuthBasic:
		fmt.Fprintf(b, "\notelcol.auth.basic %q {\n  client_auth {\n    username = %s\n    password = %s\n  }\n}\n", o.id(), o.file(credentialUsername), o.file(credentialPassword))
	case logAuthBearer:
		fmt.Fprintf(b, "\notelcol.auth.bearer %q {\n  token = %s\n}\n", o.id(), o.file(credentialToken))
	}
}

// writeSyslog sends each line as an RFC 5424 message. The exporter reads a message's fields from
// the log record's attributes, so the line becomes the message and the node and job labels the
// host and application.
func (o *LogOutput) writeSyslog(b *strings.Builder, u *url.URL, bundle string) {
	fmt.Fprintf(b, "otelcol.receiver.loki %q {\n  output {\n    logs = [otelcol.processor.transform.%s.input]\n  }\n}\n\n", o.id(), o.id())
	fmt.Fprintf(b, "otelcol.processor.transform %q {\n  error_mode = \"ignore\"\n  log_statements {\n    context    = \"log\"\n    statements = [\n", o.id())
	b.WriteString("      `set(attributes[\"message\"], body)`,\n")
	b.WriteString("      `set(attributes[\"hostname\"], attributes[\"node\"]) where attributes[\"node\"] != nil`,\n")
	b.WriteString("      `set(attributes[\"appname\"], attributes[\"job\"]) where attributes[\"job\"] != nil`,\n")
	fmt.Fprintf(b, "    ]\n  }\n  output {\n    logs = [otelcol.exporter.syslog.%s.input]\n  }\n}\n\n", o.id())
	port := syslogDefaultPort
	if p, err := strconv.Atoi(u.Port()); err == nil {
		port = p
	}
	network := schemeTCP
	if u.Scheme == schemeUDP {
		network = schemeUDP
	}
	fmt.Fprintf(b, "otelcol.exporter.syslog %q {\n  endpoint = %s\n  port     = %d\n  network  = %q\n", o.id(), strconv.Quote(u.Hostname()), port, network)
	switch {
	case u.Scheme == schemeTCP:
		b.WriteString("  tls {\n    insecure = true\n  }\n")
	case bundle != "":
		fmt.Fprintf(b, "  tls {\n    ca_pem = `%s`\n  }\n", bundle)
	}
	b.WriteString("}\n")
}

// usesSyslog tells whether an output needs otelcol.exporter.syslog, which Alloy keeps at its
// public-preview stability level.
func usesSyslog(outputs []LogOutput) bool {
	return slices.ContainsFunc(outputs, func(o LogOutput) bool { return o.Kind == logOutputSyslog })
}

// outputVolume mounts the outputs' credentials. The Secret is optional so that Alloy starts before
// the controller has put it there.
func outputVolume(outputs []LogOutput) ([]any, []any) {
	if len(needsCredentials(outputs)) == 0 {
		return nil, nil
	}
	optional := true
	return []any{corev1.Volume{Name: logOutputsVolume, Secret: &corev1.SecretVolumeSource{SecretName: logOutputsSecret, Optional: &optional}}},
		[]any{corev1.VolumeMount{Name: logOutputsVolume, MountPath: logOutputsPath, ReadOnly: true}}
}

// logOutputsWait puts the outputs' stored credentials into the namespaces of the cluster's Alloys
// before the Alloy chart, which mounts them as an optional Secret. It reports whether the chart has
// to wait for the cluster's API. A missing credential holds nothing back: Alloy runs, and the
// output reports the unreadable file.
//
// The Cluster's annotation holds what was last written (the credentials' resource versions and the
// collector), so the workload cluster is reached only when that changes, and when the last output
// with a credential goes the Secret is taken out of both namespaces once.
func (r *Reconciler) logOutputsWait(ctx context.Context, cluster *clusterv1.Cluster, logs Logs) (bool, error) {
	stored, versions, err := r.logOutputCredentials(ctx, cluster, logs.Outputs)
	if err != nil {
		return false, err
	}
	data, missing := logOutputSecretData(logs.Outputs, stored)
	if missing != "" {
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "LogOutputCredentialMissing", "ShipLogs", "%s", missing)
		}
		return false, nil
	}
	want := ""
	if len(data) > 0 {
		want = logs.collector() + ";" + strings.Join(versions, ";")
	}
	if cluster.Annotations[logOutputsAnnotation] == want {
		return false, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	for _, namespace := range []string{logsNamespace, logsNodeNamespace} {
		if len(data) > 0 && (namespace == logsNamespace || logs.collector() == collectorNode) {
			err = putLogOutputsSecret(ctx, workload.client, namespace, data)
		} else {
			err = deleteLogOutputsSecret(ctx, workload.client, namespace)
		}
		if err != nil {
			r.Remotes.forget(cluster)
			return false, err
		}
	}
	return false, r.markLogOutputs(ctx, cluster, want)
}

// logOutputCredentials reads the stored credentials of the outputs that need one, with the
// resource version of each Secret for the annotation.
func (r *Reconciler) logOutputCredentials(ctx context.Context, cluster *clusterv1.Cluster, outputs []LogOutput) (map[string]map[string][]byte, []string, error) {
	stored := map[string]map[string][]byte{}
	var versions []string
	for _, o := range needsCredentials(outputs) {
		s := &corev1.Secret{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: CredentialName(cluster.Name, o.CredentialPurpose())}, s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading the credential of log output %s: %w", o.Name, err)
		}
		if s.Type == CredentialType && s.Labels[CredentialLabel] == o.CredentialPurpose() {
			stored[o.Name] = s.Data
			versions = append(versions, o.Name+"="+s.ResourceVersion)
		}
	}
	return stored, versions, nil
}

// markLogOutputs records what was written into the workload cluster, or with nothing, that it
// holds no credential Secret.
func (r *Reconciler) markLogOutputs(ctx context.Context, cluster *clusterv1.Cluster, written string) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	if written == "" {
		delete(cluster.Annotations, logOutputsAnnotation)
	} else {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[logOutputsAnnotation] = written
	}
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("marking the log outputs' credentials as written: %w", err)
	}
	return nil
}

func deleteLogOutputsSecret(ctx context.Context, workload client.Client, namespace string) error {
	s := &corev1.Secret{Namespace: namespace, Name: logOutputsSecret}
	if err := workload.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting Secret %s/%s: %w", namespace, logOutputsSecret, err)
	}
	return nil
}

func putLogOutputsSecret(ctx context.Context, workload client.Client, namespace string, data map[string][]byte) error {
	ns := &corev1.Namespace{Name: namespace}
	if err := workload.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the %s namespace: %w", namespace, err)
	}
	s := &corev1.Secret{Namespace: namespace, Name: logOutputsSecret}
	_, err := controllerutil.CreateOrUpdate(ctx, workload, s, func() error {
		s.Type = corev1.SecretTypeOpaque
		s.Data = data
		return nil
	})
	if err != nil {
		return fmt.Errorf("applying Secret %s/%s: %w", namespace, logOutputsSecret, err)
	}
	return nil
}
