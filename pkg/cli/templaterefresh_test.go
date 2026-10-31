package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const refreshNamespace = "kubehenge-system"

// leaseAPI is the part of a Kubernetes API server that leader election uses: one namespace's
// Leases, with their resource versions checked on update as the API server does.
type leaseAPI struct {
	t     *testing.T
	mu    sync.Mutex
	lease *coordinationv1.Lease
	// holders is every holder the Lease was written with, in turn.
	holders []string
	// renewAfterRead is how many reads have the Lease's version move on right after them.
	renewAfterRead int
	// read is closed once the Lease has been read while another holds it.
	read     chan struct{}
	readOnce sync.Once
}

func (a *leaseAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	const leases = "/apis/coordination.k8s.io/v1/namespaces/" + refreshNamespace + "/leases"
	name := provisioner.TemplateRefreshName
	switch {
	case r.Method == http.MethodGet && r.URL.Path == leases+"/"+name:
		if a.lease == nil {
			a.status(w, http.StatusNotFound, metav1.StatusReasonNotFound)
			return
		}
		if holder := a.holder(); holder != "" && holder != a.self() {
			a.readOnce.Do(func() { close(a.read) })
		}
		a.write(w, http.StatusOK)
		if a.renewAfterRead > 0 {
			// A renewal in flight lands after the read, as one cut off by the run's end can.
			a.renewAfterRead--
			n, _ := strconv.Atoi(a.lease.ResourceVersion)
			a.lease.ResourceVersion = strconv.Itoa(n + 1)
		}
	case r.Method == http.MethodPost && r.URL.Path == leases:
		if a.lease != nil {
			a.status(w, http.StatusConflict, metav1.StatusReasonAlreadyExists)
			return
		}
		a.store(w, r, "", http.StatusCreated)
	case r.Method == http.MethodPut && r.URL.Path == leases+"/"+name:
		if a.lease == nil {
			a.status(w, http.StatusNotFound, metav1.StatusReasonNotFound)
			return
		}
		a.store(w, r, a.lease.ResourceVersion, http.StatusOK)
	default:
		a.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (a *leaseAPI) self() string {
	name, err := os.Hostname()
	if err != nil {
		a.t.Error(err)
	}
	return name
}

func (a *leaseAPI) holder() string {
	if a.lease == nil || a.lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *a.lease.Spec.HolderIdentity
}

// store keeps the Lease a request sends and answers with it, or refuses one written over a version
// other than the API server's with a conflict, as the API server does.
func (a *leaseAPI) store(w http.ResponseWriter, r *http.Request, version string, code int) {
	lease := &coordinationv1.Lease{}
	if err := json.NewDecoder(r.Body).Decode(lease); err != nil {
		a.t.Error(err)
		return
	}
	if lease.ResourceVersion != version {
		a.status(w, http.StatusConflict, metav1.StatusReasonConflict)
		return
	}
	n, _ := strconv.Atoi(version)
	lease.ResourceVersion = strconv.Itoa(n + 1)
	a.lease = lease
	a.holders = append(a.holders, a.holder())
	a.write(w, code)
}

func (a *leaseAPI) write(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	lease := a.lease.DeepCopy()
	lease.APIVersion, lease.Kind = "coordination.k8s.io/v1", "Lease"
	if err := json.NewEncoder(w).Encode(lease); err != nil {
		a.t.Error(err)
	}
}

func (a *leaseAPI) status(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	status := metav1.Status{APIVersion: "v1", Kind: "Status", Status: metav1.StatusFailure, Reason: reason, Code: int32(code)} //nolint:gosec // an HTTP status
	if err := json.NewEncoder(w).Encode(status); err != nil {
		a.t.Error(err)
	}
}

func (a *leaseAPI) start(t *testing.T) *rest.Config {
	t.Helper()
	a.read = make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	// The fake reads JSON alone; an API server takes it as it does protobuf.
	return &rest.Config{Host: srv.URL, ContentType: "application/json"}
}

func TestARefreshRunsHoldingTheLeaseAndReleasesIt(t *testing.T) {
	t.Parallel()
	api := &leaseAPI{t: t}
	cfg := api.start(t)
	built := errors.New("built nothing")
	var heldBy string
	// Far inside the wait for another run's Lease, which a Lease this run cannot take would end in.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := withRefreshLease(ctx, cfg, refreshNamespace, func(context.Context) error {
		api.mu.Lock()
		defer api.mu.Unlock()
		heldBy = api.holder()
		return built
	})
	if !errors.Is(err, built) {
		t.Errorf("withRefreshLease gave %v; want the run's own error", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if heldBy != api.self() {
		t.Errorf("the run went while %q held the Lease", heldBy)
	}
	if len(api.holders) == 0 || api.holder() != "" || api.lease.Spec.LeaseDurationSeconds == nil || *api.lease.Spec.LeaseDurationSeconds != 1 {
		t.Errorf("the Lease went through holders %q and is left for %v s after the run; want it released", api.holders, api.lease.Spec.LeaseDurationSeconds)
	}
}

// A renewal the run's end cut off may still have moved the Lease on; the release reads it again
// rather than leave it held until it runs out, and never takes another run's.
func TestARefreshReleasesTheLeaseAVersionLater(t *testing.T) {
	t.Parallel()
	held := func(holder string) *leaseAPI {
		now := metav1.NewMicroTime(time.Now())
		return &leaseAPI{t: t, renewAfterRead: 1, lease: &coordinationv1.Lease{
			Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName, ResourceVersion: "3",
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: new(int32(60)), AcquireTime: &now, RenewTime: &now},
		}}
	}
	leases := func(api *leaseAPI) coordinationv1client.LeaseInterface {
		clients, err := kubernetes.NewForConfig(api.start(t))
		if err != nil {
			t.Fatal(err)
		}
		return clients.CoordinationV1().Leases(refreshNamespace)
	}
	own := held((&leaseAPI{t: t}).self())
	if err := releaseRefreshLease(t.Context(), leases(own), own.self()); err != nil {
		t.Fatal(err)
	}
	own.mu.Lock()
	if own.holder() != "" || *own.lease.Spec.LeaseDurationSeconds != 1 || own.lease.ResourceVersion != "5" {
		t.Errorf("the Lease is held by %q for %d s at version %s; want it released over the renewal", own.holder(), *own.lease.Spec.LeaseDurationSeconds, own.lease.ResourceVersion)
	}
	own.mu.Unlock()

	other := held("kubehenge-template-refresh-29384756-abcde")
	if err := releaseRefreshLease(t.Context(), leases(other), other.self()); err != nil {
		t.Fatal(err)
	}
	other.mu.Lock()
	defer other.mu.Unlock()
	if other.holder() != "kubehenge-template-refresh-29384756-abcde" || len(other.holders) != 0 {
		t.Errorf("another run's Lease was written: held by %q after writes %v", other.holder(), other.holders)
	}
}

func TestARefreshWaitsForTheOneUnderWay(t *testing.T) {
	t.Parallel()
	other := "kubehenge-template-refresh-29384756-abcde"
	now := metav1.NewMicroTime(time.Now())
	api := &leaseAPI{t: t, lease: &coordinationv1.Lease{
		Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName, ResourceVersion: "7",
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &other, LeaseDurationSeconds: new(int32(60)), AcquireTime: &now, RenewTime: &now,
		},
	}}
	cfg := api.start(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ran := false
	result := make(chan error, 1)
	go func() {
		result <- withRefreshLease(ctx, cfg, refreshNamespace, func(context.Context) error {
			ran = true
			return nil
		})
	}()
	// The other run holds the Lease for a minute yet: once this one has seen that, it is stopped.
	<-api.read
	cancel()
	err := <-result
	if err == nil || !strings.Contains(err.Error(), "another template refresh holds Lease "+refreshNamespace+"/"+provisioner.TemplateRefreshName) || ran {
		t.Errorf("withRefreshLease gave %v, ran %v; want the run refused without building", err, ran)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.holder() != other || len(api.holders) != 0 {
		t.Errorf("the other run's Lease was written: held by %q after writes %v", api.holder(), api.holders)
	}
}

func TestWithRefreshLeaseNeedsAnAPI(t *testing.T) {
	t.Parallel()
	err := withRefreshLease(t.Context(), &rest.Config{Host: "https://[::1"}, refreshNamespace, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "the management cluster's API") {
		t.Errorf("withRefreshLease gave %v", err)
	}
}

func refreshStatusIn(t *testing.T, objects ...client.Object) (*refreshStatus, client.Client, *bytes.Buffer) {
	t.Helper()
	kube := fake.NewClientBuilder().WithObjects(objects...).Build()
	var out bytes.Buffer
	return &refreshStatus{kube: kube, namespace: refreshNamespace, out: &out}, kube, &out
}

func statusData(t *testing.T, kube client.Client) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName}, cm); err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

func TestTheRefreshKeepsTheEndOfItsLog(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName, Data: map[string]string{provisioner.RefreshState: "running"}}
	status, kube, out := refreshStatusIn(t, cm)
	const lines = refreshLogLines + 5
	for i := range lines {
		status.logf("step %d of %s", i, "the run")
	}
	written := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(written) != lines || !strings.HasSuffix(written[0], " step 0 of the run") {
		t.Errorf("the Job's log has %d lines, the first %q; want every line", len(written), written[0])
	}
	data := statusData(t, kube)
	kept := strings.Split(data[provisioner.RefreshLog], "\n")
	if len(kept) != refreshLogLines || !strings.HasSuffix(kept[0], " step 5 of the run") || !strings.HasSuffix(kept[len(kept)-1], fmt.Sprintf(" step %d of the run", lines-1)) {
		t.Errorf("the status keeps %d lines from %q to %q; want the last %d", len(kept), kept[0], kept[len(kept)-1], refreshLogLines)
	}
	if data[provisioner.RefreshState] != "running" {
		t.Errorf("writing the log changed the state to %q", data[provisioner.RefreshState])
	}
}

func TestARefusedRefreshSaysWhy(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName, Data: map[string]string{
		provisioner.RefreshState: "succeeded", provisioner.RefreshLog: "an earlier run's log", provisioner.RefreshLines: "rke2-almalinux-10",
	}}
	status, kube, _ := refreshStatusIn(t, cm)
	err := status.refused(t.Context(), "Build now", errors.New("no API token"))
	if err == nil || err.Error() != "the template refresh cannot start: no API token" {
		t.Errorf("refused gave %v", err)
	}
	data := statusData(t, kube)
	if data[provisioner.RefreshState] != "failed" || data[provisioner.RefreshError] != "no API token" || data[provisioner.RefreshTrigger] != "Build now" ||
		data[provisioner.RefreshLog] != "" || data[provisioner.RefreshLines] != "" || data[provisioner.RefreshStarted] == "" || data[provisioner.RefreshStarted] != data[provisioner.RefreshFinished] {
		t.Errorf("the status is %v", data)
	}
}

// A run that could not start says why in a Warning event too, for kubectl.
func TestARefusedRefreshRecordsAnEvent(t *testing.T) {
	t.Parallel()
	status, kube, _ := refreshStatusIn(t, &corev1.ConfigMap{Namespace: refreshNamespace, Name: provisioner.TemplateRefreshName})
	if err := status.refused(t.Context(), "schedule", errors.New("no API token")); err == nil {
		t.Fatal("no error")
	}
	var events corev1.EventList
	if err := kube.List(t.Context(), &events, client.InNamespace(refreshNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("events %+v", events.Items)
	}
	e := events.Items[0]
	if e.Type != corev1.EventTypeWarning || e.Reason != "TemplateBuildFailed" || e.InvolvedObject.Name != provisioner.TemplateRefreshName ||
		!strings.Contains(e.Message, "template build (schedule) failed: no API token") {
		t.Errorf("event %+v", e)
	}
}

// The status is for showing: a run whose status cannot be written goes on, saying so in its log.
func TestAStatusThatCannotBeWrittenIsLogged(t *testing.T) {
	t.Parallel()
	status, _, out := refreshStatusIn(t)
	status.set(t.Context(), map[string]string{provisioner.RefreshState: "running"})
	if !strings.Contains(out.String(), "writing the refresh's status:") {
		t.Errorf("the Job's log is %q", out.String())
	}
}

// writeKubeconfig writes a kubeconfig for an API server at server, and points KUBECONFIG at it.
func writeKubeconfig(t *testing.T, server string) {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["test"] = &clientcmdapi.Cluster{Server: server}
	cfg.AuthInfos["test"] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["test"] = &clientcmdapi.Context{Cluster: "test", AuthInfo: "test"}
	cfg.CurrentContext = "test"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

func TestTheRefreshChecksItsFlagsFirst(t *testing.T) {
	// No configuration at all, so that nothing reaches a cluster this machine's own kubeconfig names.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	for _, c := range []struct {
		name   string
		change func(*templateRefreshOptions)
		want   string
	}{
		{"no namespace", func(o *templateRefreshOptions) { o.namespace = "" }, "--namespace and --class-namespace are required"},
		{"no class namespace", func(o *templateRefreshOptions) { o.classNamespace = "" }, "--namespace and --class-namespace are required"},
		{"keeping none", func(o *templateRefreshOptions) { o.keep = 0 }, "--keep must be at least 1"},
		{"a missing line that is none", func(o *templateRefreshOptions) { o.missing = []string{"windows-11"} }, "--missing:"},
		{"no management cluster", func(*templateRefreshOptions) {}, "the management cluster's API"},
	} {
		o := &templateRefreshOptions{namespace: refreshNamespace, classNamespace: refreshNamespace, keep: 2, missing: []string{"rke2-almalinux-10"}}
		c.change(o)
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		if err := o.run(cmd, &pve.Config{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: run gave %v; want %q", c.name, err, c.want)
		}
	}
}
