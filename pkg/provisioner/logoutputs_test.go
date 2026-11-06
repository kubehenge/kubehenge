package provisioner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func testCABundle(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "siem-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

var allOutputs = []LogOutput{
	{Name: "splunk", Kind: logOutputOTLP, Endpoint: "https://otlp.example.com:4318", Auth: logAuthBearer},
	{Name: "siem", Kind: logOutputSyslog, Endpoint: "tls://syslog.example.com:6514"},
	{Name: "cloud", Kind: logOutputLoki, Endpoint: "https://loki.example.com/loki/api/v1/push", Auth: logAuthBasic},
}

// blockOf is the Alloy block that starts at header, up to the next block at the margin.
func blockOf(t *testing.T, config, header string) string {
	t.Helper()
	_, rest, ok := strings.Cut(config, header)
	if !ok {
		t.Fatalf("no %s in\n%s", header, config)
	}
	block, _, _ := strings.Cut(rest, "\n}\n")
	return block
}

func TestLogOutputsGetEveryStreamBesideLoki(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: allOutputs}))
	node := lookup[string](t, values["web-alloy-node"], "alloy", "configMap", "content")
	events := lookup[string](t, values["web-alloy"], "alloy", "configMap", "content")
	for _, want := range []string{
		`otelcol.exporter.otlphttp "out_splunk"`, `otelcol.exporter.syslog "out_siem"`, `loki.write "out_cloud"`,
		`otelcol.receiver.loki "out_splunk"`, `otelcol.processor.transform "out_siem"`,
	} {
		if !strings.Contains(node, want) {
			t.Errorf("the node Alloy lacks %s:\n%s", want, node)
		}
	}
	if strings.Contains(node, "@") {
		t.Errorf("a stream is left unrouted:\n%s", node)
	}
	for _, header := range []string{`loki.source.journal "journal"`, `loki.process "pods"`, `loki.process "audit"`} {
		block := blockOf(t, node, header)
		for _, receiver := range []string{"loki.write.loki.receiver", "otelcol.receiver.loki.out_splunk.receiver", "otelcol.receiver.loki.out_siem.receiver", "loki.write.out_cloud.receiver"} {
			if !strings.Contains(block, receiver) {
				t.Errorf("%s does not forward to %s:\n%s", header, receiver, block)
			}
		}
	}
	if !strings.Contains(blockOf(t, events, `loki.source.kubernetes_events "events"`), "loki.write.out_cloud.receiver") {
		t.Errorf("events do not reach the output:\n%s", events)
	}
	if strings.Contains(node, "kubernetes_events") {
		t.Error("the node Alloy reads the events too")
	}
}

func TestLogOutputsLimitedToStreams(t *testing.T) {
	t.Parallel()
	outputs := []LogOutput{
		{Name: "audit-only", Kind: logOutputLoki, Endpoint: "https://a.example.com/push", Streams: []string{"audit"}},
		{Name: "no-audit", Kind: logOutputLoki, Endpoint: "https://b.example.com/push", Streams: []string{"containers", "nodes"}},
	}
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: outputs}))
	node := lookup[string](t, values["web-alloy-node"], "alloy", "configMap", "content")
	for header, want := range map[string]map[string]bool{
		`loki.process "audit"`:          {"out_audit_only": true, "out_no_audit": false},
		`loki.process "pods"`:           {"out_audit_only": false, "out_no_audit": true},
		`loki.source.journal "journal"`: {"out_audit_only": false, "out_no_audit": true},
	} {
		block := blockOf(t, node, header)
		for id, wanted := range want {
			if got := strings.Contains(block, "loki.write."+id+".receiver"); got != wanted {
				t.Errorf("%s forwards to %s: %t, want %t", header, id, got, wanted)
			}
		}
		if !strings.Contains(block, "loki.write.loki.receiver") {
			t.Errorf("%s no longer reaches Loki", header)
		}
	}
	api := lookup[string](t, values["web-alloy"], "alloy", "configMap", "content")
	if strings.Contains(api, "out_") {
		t.Errorf("events reach an output that takes none:\n%s", api)
	}
}

func TestLogOutputCredentialsAreFileReferences(t *testing.T) {
	t.Parallel()
	values, raw := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: allOutputs}))
	node := lookup[string](t, values["web-alloy-node"], "alloy", "configMap", "content")
	for _, want := range []string{
		`filename  = "/etc/kubehenge/log-outputs/splunk.token"`, `token = local.file.out_splunk_token.content`,
		`password = local.file.out_cloud_password.content`, `username = local.file.out_cloud_username.content`,
		`is_secret = true`,
	} {
		if !strings.Contains(node, want) {
			t.Errorf("the configuration lacks %s:\n%s", want, node)
		}
	}
	if strings.Contains(node, "log-outputs/siem") {
		t.Error("the syslog output reads a credential")
	}
	found := false
	for i := range lookup[[]any](t, values["web-alloy-node"], "controller", "volumes", "extra") {
		volume := lookup[map[string]any](t, values["web-alloy-node"], "controller", "volumes", "extra", i)
		if volume["name"] != logOutputsVolume {
			continue
		}
		found = true
		if lookup[string](t, volume, "secret", "secretName") != logOutputsSecret || !lookup[bool](t, volume, "secret", "optional") {
			t.Errorf("volume %v", volume)
		}
	}
	if !found {
		t.Error("no volume holds the credentials")
	}
	if !strings.Contains(raw["web-alloy-node"], logOutputsPath) {
		t.Error("the credentials are not mounted")
	}
	plain, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: []LogOutput{allOutputs[1]}}))
	if strings.Contains(lookup[string](t, plain["web-alloy-node"], "alloy", "configMap", "content"), "local.file \"out") {
		t.Error("an output without credentials reads files")
	}
}

func TestLogOutputsTLSAndStability(t *testing.T) {
	t.Parallel()
	bundle := testCABundle(t)
	outputs := []LogOutput{
		{Name: "a", Kind: logOutputLoki, Endpoint: "https://a.example.com/push", CABundle: bundle},
		{Name: "b", Kind: logOutputOTLP, Endpoint: "https://b.example.com", CABundle: bundle},
		{Name: "c", Kind: logOutputSyslog, Endpoint: "tls://c.example.com:6514", CABundle: bundle},
		{Name: "d", Kind: logOutputSyslog, Endpoint: "tcp://d.example.com:514"},
		{Name: "e", Kind: logOutputSyslog, Endpoint: "udp://e.example.com:514"},
	}
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: outputs}))
	node := lookup[string](t, values["web-alloy-node"], "alloy", "configMap", "content")
	if got := strings.Count(node, "ca_pem = `"+bundle+"`"); got != 3 {
		t.Errorf("the bundle is set %d times, want 3:\n%s", got, node)
	}
	if !strings.Contains(blockOf(t, node, `otelcol.exporter.syslog "out_d"`), "insecure = true") ||
		!strings.Contains(blockOf(t, node, `otelcol.exporter.syslog "out_e"`), `network  = "udp"`) {
		t.Error("plain tcp and udp syslog are not set up as such")
	}
	if lookup[string](t, values["web-alloy-node"], "alloy", "stabilityLevel") != "public-preview" ||
		lookup[string](t, values["web-alloy"], "alloy", "stabilityLevel") != "public-preview" {
		t.Error("otelcol.exporter.syslog needs the public-preview level")
	}
	loki, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node", Outputs: outputs[:2]}))
	if _, set := lookup[map[string]any](t, loki["web-alloy-node"], "alloy")["stabilityLevel"]; set {
		t.Error("a level is raised without syslog")
	}
}

func TestLogOutputsStayOffByDefault(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node"}))
	node := lookup[string](t, values["web-alloy-node"], "alloy", "configMap", "content")
	if strings.Contains(node, "out_") || strings.Contains(node, "@") || !strings.Contains(node, "loki.write.loki.receiver") {
		t.Errorf("without outputs the configuration changes:\n%s", node)
	}
}

func TestLogOutputsAreRefused(t *testing.T) {
	t.Parallel()
	bundle := testCABundle(t)
	good := LogOutput{Name: "siem", Kind: logOutputLoki, Endpoint: "https://l.example.com/push"}
	with := func(f func(o *LogOutput)) []LogOutput {
		o := good
		f(&o)
		return []LogOutput{o}
	}
	many := make([]LogOutput, maxLogOutputs+1)
	for i := range many {
		many[i] = good
		many[i].Name = "o" + string(rune('a'+i))
	}
	for name, tc := range map[string]struct {
		outputs []LogOutput
		want    string
	}{
		"too many":                {many, "at most"},
		"upper-case name":         {with(func(o *LogOutput) { o.Name = "Siem" }), "lower-case label"},
		"long name":               {with(func(o *LogOutput) { o.Name = strings.Repeat("a", maxLogOutputName+1) }), "lower-case label"},
		"twice":                   {[]LogOutput{good, good}, "two log outputs"},
		"unknown kind":            {with(func(o *LogOutput) { o.Kind = "http" }), "kind must be"},
		"syslog over https":       {with(func(o *LogOutput) { o.Kind = logOutputSyslog }), "tcp://, tls:// or udp://"},
		"syslog without a port":   {with(func(o *LogOutput) { o.Kind, o.Endpoint = logOutputSyslog, "tls://h.example.com" }), "scheme://host:port"},
		"syslog authentication":   {with(func(o *LogOutput) { o.Kind, o.Endpoint, o.Auth = logOutputSyslog, "tls://h:6514", logAuthBasic }), "no authentication"},
		"loki over tcp":           {with(func(o *LogOutput) { o.Endpoint = "tcp://h:1" }), "https:// or http://"},
		"user in the URL":         {with(func(o *LogOutput) { o.Endpoint = "https://u:p@h.example.com/x" }), "no user"},
		"query in the URL":        {with(func(o *LogOutput) { o.Endpoint = "https://h.example.com/x?token=1" }), "no user, query"},
		"no host":                 {with(func(o *LogOutput) { o.Endpoint = "https://" }), "with a host"},
		"unknown stream":          {with(func(o *LogOutput) { o.Streams = []string{"kernel"} }), "stream"},
		"stream twice":            {with(func(o *LogOutput) { o.Streams = []string{"audit", "audit"} }), "twice"},
		"unknown auth":            {with(func(o *LogOutput) { o.Auth = "digest" }), "auth must be"},
		"credentials in the open": {with(func(o *LogOutput) { o.Endpoint, o.Auth = "http://h.example.com/x", logAuthBearer }), "only to an https"},
		"bundle without TLS":      {with(func(o *LogOutput) { o.Endpoint, o.CABundle = "http://h.example.com/x", bundle }), "needs an https"},
		"bundle that is no PEM":   {with(func(o *LogOutput) { o.CABundle = "not a certificate" }), "no PEM certificate"},
		"bundle with a key":       {with(func(o *LogOutput) { o.CABundle = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n" }), "certificates alone"},
	} {
		raw, err := json.Marshal(map[string]any{"enabled": true, "outputs": tc.outputs})
		if err != nil {
			t.Fatal(err)
		}
		err = checkLogs(cluster(variableOf("logs", string(raw)), variableOf("monitoring", `{"enabled":true}`)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want it to say %q", name, err, tc.want)
		}
	}
	for name, o := range map[string]LogOutput{
		"https bearer": {Name: "a", Kind: logOutputOTLP, Endpoint: "https://o.example.com", Auth: logAuthBearer, CABundle: bundle},
		"udp syslog":   {Name: "b", Kind: logOutputSyslog, Endpoint: "udp://s.example.com:514", Streams: []string{"audit", "nodes"}},
	} {
		if err := validateLogOutputs([]LogOutput{o}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLogOutputCredentialPurposes(t *testing.T) {
	t.Parallel()
	if p, ok := CredentialPurposeNamed("log-splunk"); !ok || p.Name != "log-splunk" || p.Appliance {
		t.Errorf("log-splunk: %v %t", p, ok)
	}
	for _, name := range []string{"log-", "log-Upper", "log-" + strings.Repeat("a", maxLogOutputName+1), "logs-x", "log--x"} {
		if _, ok := CredentialPurposeNamed(name); ok {
			t.Errorf("%s is a purpose", name)
		}
	}
}

func TestLogOutputsSecretFollowsTheCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	credential := &corev1.Secret{
		Namespace: "team-a", Name: "web.log-splunk", Type: CredentialType,
		Labels: map[string]string{CredentialLabel: "log-splunk"},
		Data:   map[string][]byte{"token": []byte("first")},
	}
	c := cluster()
	r, kube := newReconciler(t, &fakeCloud{}, c, credential)
	workload := fake.NewClientBuilder().WithScheme(newRemoteScheme()).Build()
	reach := func() {
		r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	}
	// An unreachable cluster fails any call that needs it, which proves a call that does not.
	unreachable := func() { r.Remotes = &Remotes{} }
	outputs := Logs{Collector: collectorNode, Outputs: []LogOutput{{Name: "splunk", Kind: logOutputOTLP, Endpoint: "https://o.example.com", Auth: logAuthBearer}}}
	held := func(namespace string) (string, bool) {
		s := &corev1.Secret{}
		err := workload.Get(ctx, client.ObjectKey{Namespace: namespace, Name: logOutputsSecret}, s)
		return string(s.Data["splunk.token"]), err == nil
	}
	wait := func(logs Logs) {
		t.Helper()
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), c); err != nil {
			t.Fatal(err)
		}
		if waits, err := r.logOutputsWait(ctx, c, logs); err != nil || waits {
			t.Fatalf("waits %v: %v", waits, err)
		}
	}

	reach()
	wait(outputs)
	for _, namespace := range []string{logsNamespace, logsNodeNamespace} {
		if token, ok := held(namespace); !ok || token != "first" {
			t.Errorf("%s holds %q, %t", namespace, token, ok)
		}
	}
	if c.Annotations[logOutputsAnnotation] == "" {
		t.Error("the Cluster does not record the Secret")
	}

	unreachable()
	wait(outputs)

	reach()
	credential.Data["token"] = []byte("second")
	if err := kube.Update(ctx, credential); err != nil {
		t.Fatal(err)
	}
	wait(outputs)
	if token, _ := held(logsNamespace); token != "second" {
		t.Errorf("a changed credential is not written: %q", token)
	}

	wait(Logs{Collector: collectorAPI, Outputs: outputs.Outputs})
	if _, ok := held(logsNodeNamespace); ok {
		t.Error("the node Alloy's namespace keeps the Secret under the API collector")
	}
	if _, ok := held(logsNamespace); !ok {
		t.Error("the Secret went from the API Alloy's namespace")
	}

	wait(Logs{Collector: collectorAPI, Outputs: []LogOutput{allOutputs[1]}})
	for _, namespace := range []string{logsNamespace, logsNodeNamespace} {
		if _, ok := held(namespace); ok {
			t.Errorf("%s keeps the Secret with no credential in use", namespace)
		}
	}
	if _, set := c.Annotations[logOutputsAnnotation]; set {
		t.Error("the Cluster still records the Secret")
	}
	unreachable()
	wait(Logs{})
}

func TestLogOutputsMissingCredentialWritesNothing(t *testing.T) {
	t.Parallel()
	c := cluster()
	r, _ := newReconciler(t, &fakeCloud{}, c)
	r.Remotes = &Remotes{}
	waits, err := r.logOutputsWait(context.Background(), c, Logs{Outputs: allOutputs[:1]})
	if err != nil || waits {
		t.Fatalf("waits %v: %v", waits, err)
	}
	if c.Annotations[logOutputsAnnotation] != "" {
		t.Error("the Cluster records a Secret that was never written")
	}
}

func TestLogOutputSecretHoldsWhatTheAuthNeeds(t *testing.T) {
	t.Parallel()
	stored := map[string]map[string][]byte{
		"splunk": {"token": []byte("t"), "username": []byte("ignored")},
		"cloud":  {"username": []byte("u"), "password": []byte("p")},
	}
	data, missing := logOutputSecretData(allOutputs, stored)
	if missing != "" || len(data) != 3 || string(data["splunk.token"]) != "t" || string(data["cloud.password"]) != "p" {
		t.Errorf("data %v, missing %q", data, missing)
	}
	delete(stored["cloud"], "password")
	if _, missing = logOutputSecretData(allOutputs, stored); !strings.Contains(missing, "cloud has no password") {
		t.Errorf("missing %q", missing)
	}
	if _, missing = logOutputSecretData(allOutputs, nil); !strings.Contains(missing, "splunk") {
		t.Errorf("missing %q", missing)
	}
}
