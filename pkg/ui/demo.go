package ui

import (
	"context"
	_ "embed" // the demo's ClusterClasses
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// The demo's appliance folder is its process's alone.
const (
	demoDirMode  = 0o700
	demoFileMode = 0o600
)

// demoClassesJSON are the chart's ClusterClasses, cut to what the interface reads.
// TestDemoClassesMatchTheChart keeps them current.
//
//go:embed demo-classes.json
var demoClassesJSON []byte

// demoHeader marks every answer of the demo, so the interface can say so before anyone signs in.
const demoHeader = "X-Kubehenge-Demo"

// demoUser is who the demo signs in whoever opens it.
const demoUser = "admin@pve"

// demoName is the demo's release, and the password and ticket of its sign-in.
const demoName = "demo"

// demoSessionMaxAge is how long a demo session lasts unless the server says otherwise, as long as
// serve's default.
const demoSessionMaxAge = 8 * time.Hour

// errDemo is what the demo answers in place of a change in Proxmox VE. The interface never gets
// that far, as the demo refuses changes before any handler runs.
var errDemo = errors.New("the demo changes nothing")

// SetUpDemo makes s the demo: the interface over a sample fleet held in memory. Every client the
// handlers use is a fake, so nothing reaches a Proxmox VE host, a management cluster or a workload
// cluster, and changes are refused. clock is the demo's time, which its sample data is dated
// against. The appliance it sets up keeps its settings in a folder that cleanup removes.
func SetUpDemo(s *Server, clock func() time.Time) (func(), error) {
	if s.Appliance != nil || s.Headlamp != nil || s.Upgrades != nil {
		return nil, errors.New("the demo runs without the appliance, Headlamp or upgrades")
	}
	dir, err := os.MkdirTemp("", "kubehenge-demo-")
	if err != nil {
		return nil, fmt.Errorf("the demo's appliance: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := fillDemo(s, clock, dir); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func fillDemo(s *Server, clock func() time.Time, dir string) error {
	fleet, err := newDemoFleet(clock().UTC())
	if err != nil {
		return err
	}
	scheme, mapper, err := demoScheme()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(dir, "management"), demoDirMode); err != nil {
		return fmt.Errorf("the demo's appliance: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "management", "settings.json"), fleet.data.Management, demoFileMode); err != nil {
		return fmt.Errorf("the demo's appliance: %w", err)
	}
	s.Appliance = &appliance.Appliance{Dir: dir, Release: demoName, Now: clock}
	s.Kube = fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(fleet.objects...).Build()
	s.Accounts = &demoAccounts{fleet: fleet}
	s.ConnectWorkload = fleet.connect
	s.Probe = func(context.Context, []netip.Addr) map[netip.Addr]bool { return map[netip.Addr]bool{} }
	s.OpenStore = func(*provisioner.BackupTarget, map[string][]byte) (provisioner.ObjectStore, error) {
		return nil, errors.New("the demo has no backup target")
	}
	s.NewKube = func([]byte) (client.Client, error) { return nil, errDemo }
	s.NewClientset = nil
	s.MakeTemplate = func(context.Context, *pve.Ticket, templates.Builder, *templates.Template) (int, error) {
		return 0, errDemo
	}
	s.PinChecks = &provisioner.ReleaseChecksums{
		HTTP: &http.Client{Transport: demoChannels{}}, GitHub: "https://github.com", Raw: "https://raw.githubusercontent.com",
	}
	s.Releases = &provisioner.ChannelServer{
		HTTP: &http.Client{Transport: demoChannels{}}, URLs: provisioner.DefaultChannelURLs, Pins: provisioner.DefaultReleasePins(),
	}
	s.TalosReleases = func(context.Context) ([]string, error) {
		return []string{templates.DefaultTalosVersion}, nil
	}
	s.ProxmoxURL, s.WorkloadPool, s.ClassNamespace, s.Namespace = "", demoPool, demoClassNamespace, demoNamespace
	if s.SessionMaxAge == 0 {
		s.SessionMaxAge = demoSessionMaxAge
	}
	s.demo = &demoState{}
	s.clock = clock
	s.init()
	for i := range fleet.activity {
		if err = s.activityLog.add(&fleet.activity[i]); err != nil {
			return fmt.Errorf("the demo's activity: %w", err)
		}
	}
	return nil
}

// demoScheme knows the kinds the interface reads: Cluster API's typed, the add-ons and the
// control planes of each distribution as unstructured objects, as a management cluster serves them.
func demoScheme() (*runtime.Scheme, *meta.DefaultRESTMapper, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, nil, fmt.Errorf("the demo's scheme: %w", err)
		}
	}
	kinds := []schema.GroupVersionKind{demoHelmReleaseProxy}
	for _, cp := range demoControlPlanes {
		kinds = append(kinds, cp)
	}
	// A lookup by group and kind alone, as the control plane's reference makes, tries only the
	// mapper's default versions.
	versions := make([]schema.GroupVersion, 0, len(kinds))
	for _, gvk := range kinds {
		versions = append(versions, gvk.GroupVersion())
	}
	mapper := meta.NewDefaultRESTMapper(versions)
	for _, gvk := range kinds {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return scheme, mapper, nil
}

// demoState is the session the demo signs everyone into. One for all, so the reads a page starts
// with at once, each without a cookie yet, share one session and its CSRF token.
type demoState struct {
	mu      sync.Mutex
	session *session
}

// demoGuard marks every answer as the demo's, signs in whoever opens it, and refuses every change.
// Reads that are sent as POSTs pass: free addresses, a change's plan and a dry run of a creation.
func (s *Server) demoGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(demoHeader, "1")
		signingIn := r.URL.Path == "/api/session" && r.Method != http.MethodGet
		if strings.HasPrefix(r.URL.Path, "/api/") && !signingIn && s.sessions.lookup(r) == nil {
			var err error
			if r, err = s.demoSignIn(w, r); err != nil {
				s.proxmoxError(w, r, err)
				return
			}
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead || demoRead(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "the demo changes nothing. Install Kubehenge to manage clusters")
	})
}

// demoRead tells whether a request that is not a GET only reads.
func demoRead(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/session", p == "/api/session/second-factor":
		return true
	case r.Method != http.MethodPost:
		return false
	case p == "/api/release-pins/check":
		return true
	case p == "/api/clusters":
		return r.URL.Query().Get("dryRun") == "All"
	case strings.HasPrefix(p, "/api/proxmox/nodes/") && strings.HasSuffix(p, "/free"):
		return true
	case strings.HasPrefix(p, "/api/clusters/") && strings.HasSuffix(p, "/plan"):
		return true
	}
	return false
}

// demoSignIn puts a request without a session into the demo's, starting it when there is none
// yet, so the demo opens on the fleet. The request it returns carries the session's cookie.
func (s *Server) demoSignIn(w http.ResponseWriter, r *http.Request) (*http.Request, error) {
	d := s.demo
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx := r.Context()
	if d.session != nil && s.sessions.lookup(withSession(ctx, r, d.session.id)) != nil {
		setSessionCookie(w, d.session.id)
		return withSession(ctx, r, d.session.id), nil
	}
	ticket, _, err := s.Accounts.SignIn(ctx, demoUser, demoName)
	if err != nil {
		return r, fmt.Errorf("signing in the demo's administrator: %w", err)
	}
	d.session = s.sessions.start(w, ticket, nil)
	return withSession(ctx, r, d.session.id), nil
}

// withSession is r with the cookie of session id in place of its own.
func withSession(ctx context.Context, r *http.Request, id string) *http.Request {
	in := r.Clone(ctx)
	in.Header.Del("Cookie")
	in.AddCookie(&http.Cookie{Name: sessionCookie, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return in
}

// demoChannels answers the release channels RKE2, k3s and Kubernetes publish, from memory. The
// release pins then pick the versions offered, as they do for the real channels.
type demoChannels struct{}

func (demoChannels) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := "", false
	switch {
	case r.URL.Host == "update.rke2.io":
		body, ok = demoChannelList("rke2r1"), true
	case r.URL.Host == "update.k3s.io":
		body, ok = demoChannelList("k3s1"), true
	case r.URL.Host == "github.com" && strings.HasSuffix(r.URL.Path, "/sha256sum-amd64.txt"):
		body, ok = demoChecksumFile(r.URL.Path), true
	case r.URL.Host == "raw.githubusercontent.com" && strings.HasSuffix(r.URL.Path, "/install.sh"):
		body, ok = "#!/bin/sh\n# install.sh of "+r.URL.Path+"\n", true
	case r.URL.Host == "dl.k8s.io" && strings.HasPrefix(r.URL.Path, "/release/stable-1."):
		minor := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/release/stable-1."), ".txt")
		body, ok = "v1."+minor+".2\n", true
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}, nil
}

// demoReleaseFiles are the files an RKE2 or k3s release's checksum file lists that its nodes install.
var demoReleaseFiles = []string{
	"rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst", "rke2-images-canal.linux-amd64.tar.zst",
	"rke2-images-calico.linux-amd64.tar.zst", "rke2-images-cilium.linux-amd64.tar.zst",
	string(templates.K3s), "k3s-airgap-images-amd64.tar.zst",
}

// demoChecksumFile is the checksum file at path, which lists every file with a SHA-256 of its own.
func demoChecksumFile(path string) string {
	var b strings.Builder
	for _, f := range demoReleaseFiles {
		b.WriteString(demoDigest(path, f) + "  " + f + "\n")
	}
	return b.String()
}

func demoChannelList(suffix string) string {
	var list struct {
		Data []map[string]string `json:"data"`
	}
	for minor := 33; minor <= 37; minor++ {
		list.Data = append(list.Data, map[string]string{"id": fmt.Sprintf("v1.%d", minor), "latest": fmt.Sprintf("v1.%d.99+%s", minor, suffix)})
	}
	list.Data = append(list.Data, map[string]string{"id": "stable", "latest": "v1.36.99+" + suffix})
	raw, _ := json.Marshal(list) //nolint:errchkjson // maps of strings always encode
	return string(raw)
}
