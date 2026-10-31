//go:build integration

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/test/integration"
)

const (
	// startWait bounds the wait for the controller to come up and act.
	startWait = time.Minute
	poll      = 100 * time.Millisecond
	// providerLabel marks a Cluster API provider's Deployment, as the operator labels it.
	providerLabel = "cluster.x-k8s.io/provider"
)

// k3sProviders are integration-values.yaml's providerNamespaces of the k3s class.
var k3sProviders = []string{"k3s-bootstrap-system", "k3s-control-plane-system"}

func TestMain(m *testing.M) {
	if err := integration.OwnChildren(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	m.Run()
}

// startManagementCluster starts an API server with the providers' chart and Kubehenge's installed,
// Kubehenge's webhook configurations pointing at the local server the test serves, and returns it
// with the paths those configurations call.
func startManagementCluster(t *testing.T) (*integration.API, []string) {
	t.Helper()
	objects, webhooks := integration.Charts(t, "../..")
	// Before Start, which points the configurations at the local server by URL.
	paths := webhooks.Paths(t)
	namespaces := append([]string{integration.ClassNamespace, "team-a", "kubehenge-autoscaler"}, k3sProviders...)
	api := integration.Start(t, &integration.Options{Objects: objects, Webhooks: webhooks, Namespaces: namespaces})
	return api, paths
}

// useKubeconfig points KUBECONFIG at the administrator's kubeconfig of cfg, its CA in a file of its
// own as a kubeconfig written by hand has it.
func useKubeconfig(t *testing.T, cfg *rest.Config) {
	t.Helper()
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, cfg.CAData, 0o600); err != nil {
		t.Fatal(err)
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthority: caFile}
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "admin"}
	kc.CurrentContext = "envtest"
	path := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// fakeProxmox is a Proxmox VE API with nothing in it, over HTTPS on loopback: every read finds
// nothing. It records the paths asked for.
type fakeProxmox struct {
	mu    sync.Mutex
	asked []string
}

func (p *fakeProxmox) start(t *testing.T) *pve.Config {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.asked = append(p.asked, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "proxmox-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &pve.Config{URL: srv.URL, CAFile: caFile, Token: testToken}
}

func (p *fakeProxmox) wasAsked(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.asked, path)
}

// freeAddress is a loopback address with a port nothing listens on.
func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// controllerRun is the controller running in the background: done is closed once it has stopped,
// err then being what it stopped with.
type controllerRun struct {
	done chan struct{}
	err  error
}

// eventually waits until check passes, failing the test with its last error once startWait is up
// or the controller has stopped.
func eventually(t *testing.T, run *controllerRun, what string, check func() error) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(startWait); ; time.Sleep(poll) {
		if err = check(); err == nil {
			return
		}
		select {
		case <-run.done:
			t.Fatalf("%s: the controller stopped: %v", what, run.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
	}
}

// TestIntegrationControllerServesWhatTheChartNames runs the controller command as the chart runs
// it, against an API server holding the charts: its probes answer, every webhook the chart's
// configurations call reaches it through the API server, Cluster API finds its four hooks, it
// announces them on the chart's ExtensionConfig, and a controller it set up acts.
func TestIntegrationControllerServesWhatTheChartNames(t *testing.T) {
	api, paths := startManagementCluster(t)
	useKubeconfig(t, api.Config)
	proxmox := &fakeProxmox{}
	conn := proxmox.start(t)
	t.Setenv("POD_NAME", "kubehenge-controller-7d9f6")
	t.Setenv("KUBEHENGE_PROXMOX_HA_TOKEN", "kubehenge@pve!ha=00000000-0000-0000-0000-000000000000")
	t.Setenv("KUBEHENGE_PROXMOX_RESTORE_TOKEN", "kubehenge@pve!restore=00000000-0000-0000-0000-000000000000")
	ctx := context.Background()
	c := api.Client

	// A provider Deployment left running though no cluster uses its class.
	labels := map[string]string{"app": "manager"}
	provider := &appsv1.Deployment{
		Namespace: k3sProviders[0], Name: "controller-manager", Labels: map[string]string{providerLabel: k3sProviders[0]},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: "registry.example/manager:v1"}}},
			},
		},
	}
	if err := c.Create(ctx, provider); err != nil {
		t.Fatal(err)
	}

	o, _ := validControllerOptions(conn.URL, conn.CAFile)
	o.r.ClassNamespace = integration.ClassNamespace
	o.leaderElect = false
	// Unset, controller-runtime serves the metrics on :8080 of every interface.
	o.healthAddr, o.metricsAddr = freeAddress(t), freeAddress(t)
	o.extensionHost, o.port, o.certDir = api.Webhooks.LocalServingHost, api.Webhooks.LocalServingPort, api.Webhooks.LocalServingCertDir
	o.extensionConfig = "kubehenge"
	o.classProviders = []string{"kubehenge-k3s=" + strings.Join(k3sProviders, ",")}
	o.templateRefresh = true
	o.offerHARules = true
	o.upgradeInterval = time.Hour
	o.autoscaler.Management.Server = "https://192.0.2.10:6443"
	runCtx, cancel := context.WithCancel(ctx)
	running := &controllerRun{done: make(chan struct{})}
	go func() {
		running.err = o.run(runCtx, conn)
		close(running.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-running.done
		if running.err != nil {
			t.Errorf("the controller stopped with %v", running.err)
		}
	})

	probe := func(path string) func() error {
		return func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+o.healthAddr+path, http.NoBody)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s answers %s", path, resp.Status)
			}
			return nil
		}
	}
	eventually(t, running, "the liveness probe", probe("/healthz"))
	eventually(t, running, "the readiness probe", probe("/readyz"))

	refused := func() error { return refusedByKubehenge(ctx, c, rancherOnTwoCores()) }
	admitted := map[string]func() error{
		// The API server calls the validating webhooks only once the mutating ones have answered,
		// and fails closed while one does not.
		provisioner.DefaultClusterPath:  refused,
		provisioner.ValidateClusterPath: refused,
	}
	for _, path := range paths {
		check, ok := admitted[path]
		if !ok {
			t.Errorf("the chart's webhooks call %s, which this test does not send a request to", path)
			continue
		}
		eventually(t, running, "the webhook at "+path, check)
	}

	hooks := discover(t, api)
	if want := []string{"before-cluster-create", "before-cluster-delete", "before-cluster-upgrade", "generate-patches"}; !slices.Equal(hooks, want) {
		t.Errorf("Cluster API discovers the hooks %v, want %v", hooks, want)
	}

	eventually(t, running, "the announcement on the ExtensionConfig", func() error {
		ec := &unstructured.Unstructured{}
		ec.SetAPIVersion("runtime.cluster.x-k8s.io/v1beta2")
		ec.SetKind("ExtensionConfig")
		if err := c.Get(ctx, client.ObjectKey{Name: "kubehenge"}, ec); err != nil {
			return err
		}
		if by := ec.GetAnnotations()["kubehenge.dev/served-by"]; by != "kubehenge-controller-7d9f6" {
			return fmt.Errorf("served by %q", by)
		}
		return nil
	})
	eventually(t, running, "the unused k3s providers scaled to none", func() error {
		dep := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(provider), dep); err != nil {
			return err
		}
		if *dep.Spec.Replicas != 0 {
			return fmt.Errorf("%d replicas", *dep.Spec.Replicas)
		}
		return nil
	})
	eventually(t, running, "the HA rules offered to cluster creators", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: integration.ClassNamespace, Name: provisioner.HARulesName}, cm); err != nil {
			return err
		}
		if !proxmox.wasAsked("GET /api2/json/cluster/ha/rules") {
			return fmt.Errorf("%s was written without reading Proxmox VE's rules", provisioner.HARulesName)
		}
		return nil
	})
}

// rancherOnTwoCores is a cluster of the RKE2 class that installs Rancher on its control planes' two
// vCPUs, which Kubehenge's cluster check refuses.
func rancherOnTwoCores() *clusterv1.Cluster {
	vars := map[string]string{
		"rancher":              `{"install":true,"hostname":"rancher.example.org"}`,
		"storage":              `"local-lvm"`,
		"controlPlaneEndpoint": `"10.20.30.5"`,
		"network":              `{"bridge":"vmbr0","addresses":["10.20.30.10-10.20.30.30"],"prefix":24,"gateway":"10.20.30.1","dnsServers":["10.20.30.1"]}`,
	}
	variables := make([]clusterv1.ClusterVariable, 0, len(vars))
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		variables = append(variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(vars[name])}})
	}
	return &clusterv1.Cluster{
		Namespace: "team-a", Name: "rancher",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef:  clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: integration.ClassNamespace},
			Version:   "v1.36.5+rke2r1",
			Variables: variables,
		}},
	}
}

// refusedByKubehenge creates a cluster as a dry run, and passes once Kubehenge's own check refused
// it: while nothing serves the webhook the API server refuses every cluster for that alone.
func refusedByKubehenge(ctx context.Context, c client.Client, cluster *clusterv1.Cluster) error {
	err := c.Create(ctx, cluster, client.DryRunAll)
	if err == nil {
		return fmt.Errorf("cluster %s was admitted", cluster.Name)
	}
	if !strings.Contains(err.Error(), `admission webhook "clusters.kubehenge.dev" denied the request`) || !strings.Contains(err.Error(), "at least 4 vCPUs") {
		return err
	}
	return nil
}

// discover asks the controller's Runtime Extension server for its hooks, as Cluster API does, and
// returns their names.
func discover(t *testing.T, api *integration.API) []string {
	t.Helper()
	catalog := runtimecatalog.New()
	if err := runtimehooksv1.AddToCatalog(catalog); err != nil {
		t.Fatal(err)
	}
	gvh, err := catalog.GroupVersionHook(runtimehooksv1.Discovery)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(api.Webhooks.LocalServingCAData) {
		t.Fatal("no CA of the webhook server")
	}
	https := &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	address := net.JoinHostPort(api.Webhooks.LocalServingHost, strconv.Itoa(api.Webhooks.LocalServingPort))
	body := []byte(`{"apiVersion":"hooks.runtime.cluster.x-k8s.io/v1alpha1","kind":"DiscoveryRequest"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+address+runtimecatalog.GVHToPath(gvh, ""), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := https.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var discovery runtimehooksv1.DiscoveryResponse
	if err = json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.Status != runtimehooksv1.ResponseStatusSuccess {
		t.Fatalf("discovery answered %s: %s", discovery.Status, discovery.Message)
	}
	names := make([]string, 0, len(discovery.Handlers))
	for _, h := range discovery.Handlers {
		names = append(names, h.Name)
	}
	slices.Sort(names)
	return names
}

// TestIntegrationTemplateRefreshRunsUnderItsLease runs the template refresh's Job as the chart runs
// it: under its Lease, released once it ends, with how it went in the status the interface shows;
// a run without its API token says so there instead. It reads which clusters run which builds from
// the same management cluster, and the Helm hooks' wait gets a client of it.
func TestIntegrationTemplateRefreshRunsUnderItsLease(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	api, _ := startManagementCluster(t)
	useKubeconfig(t, api.Config)
	proxmox := &fakeProxmox{}
	conn := proxmox.start(t)
	ctx := context.Background()
	c := api.Client
	ns := integration.ClassNamespace
	status := func() map[string]string {
		t.Helper()
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: provisioner.TemplateRefreshName}, cm); err != nil {
			t.Fatal(err)
		}
		return cm.Data
	}
	refresh := func(conn *pve.Config) error {
		o := &templateRefreshOptions{namespace: ns, classNamespace: ns, keep: 2, missingOnly: true, trigger: "Build now", tokenSecret: "kubehenge-proxmox"} //nolint:gosec // a Secret's name, not a credential
		o.flags.builder.Node = "pve1"
		var log bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetErr(&log)
		return o.run(cmd, conn) //nolint:contextcheck // run takes its context from cmd, as cobra hands it over
	}

	if err := refresh(conn); err != nil {
		t.Fatal(err)
	}
	s := status()
	if s[provisioner.RefreshState] != "succeeded" || s[provisioner.RefreshTrigger] != "Build now" || s[provisioner.RefreshError] != "" ||
		s[provisioner.RefreshStarted] == "" || s[provisioner.RefreshFinished] == "" {
		t.Errorf("the status of a run with nothing to build is %v", s)
	}
	if !proxmox.wasAsked("GET /api2/json/cluster/resources") {
		t.Errorf("the run did not look at the template pool: %v", proxmox.asked)
	}
	lease := &unstructured.Unstructured{}
	lease.SetGroupVersionKind(coordinationv1.SchemeGroupVersion.WithKind("Lease"))
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: provisioner.TemplateRefreshName}, lease); err != nil {
		t.Fatalf("the run held no Lease: %v", err)
	}
	if holder, _, _ := unstructured.NestedString(lease.Object, "spec", "holderIdentity"); holder != "" {
		t.Errorf("the Lease is still held by %q after the run", holder)
	}

	noToken := *conn
	noToken.Token = ""
	err := refresh(&noToken)
	if err == nil || !strings.Contains(err.Error(), "Secret kubehenge-proxmox with key token is missing in namespace "+ns) {
		t.Errorf("a run without its token gave %v", err)
	}
	if s = status(); s[provisioner.RefreshState] != "failed" || !strings.Contains(s[provisioner.RefreshError], "Secret kubehenge-proxmox") {
		t.Errorf("the status of a run without its token is %v", s)
	}

	users, err := buildUsers(ctx, ns, nil)
	if err != nil || len(users) != 0 {
		t.Errorf("buildUsers gave %v, %v; want no builds in use", users, err)
	}

	err = inCluster(ctx, time.Minute, func(ctx context.Context, c client.Client) error {
		var list appsv1.DeploymentList
		return c.List(ctx, &list, client.InNamespace(ns))
	})
	if err != nil {
		t.Errorf("the hook's client lists Deployments: %v", err)
	}
}
