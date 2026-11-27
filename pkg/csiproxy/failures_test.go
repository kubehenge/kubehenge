package csiproxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// pluginCalls are the calls the CSI plugin's controller makes to attach, detach and delete a
// volume, and the zone check before each, whose empty answer reads as a volume gone.
var pluginCalls = []struct{ name, method, path string }{
	{"zone check", http.MethodGet, "/cluster/resources"},
	{"attach", http.MethodPost, "/nodes/pve1/qemu/101/config"},
	{"detach", http.MethodPut, "/nodes/pve1/qemu/101/unlink"},
	{"delete", http.MethodDelete, "/nodes/pve1/storage/local-zfs/content/vm-4242-pvc-1"},
}

// failsForBoth reports whether both of the plugin's releases read an answer as an error. The held
// release's client (luthermonson/go-proxmox, Client.Req and handleResponse) errs only on 400, 401,
// 403, 500 and 501, and decodes any other status as an empty success. The newest release's client
// (go-proxmox-rest) errs on every status from 400 on, and reads a 500 whose message says something
// does not exist as that thing gone.
func failsForBoth(status int, errs map[string]string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusNotImplemented:
	default:
		return false
	}
	for _, message := range errs {
		lower := strings.ToLower(message)
		for _, gone := range []string{"does not exist", "no such ", "binary not installed:", "not ha managed"} {
			if status == http.StatusInternalServerError && strings.Contains(lower, gone) {
				return false
			}
		}
	}
	return len(errs) > 0
}

// proxyHarness serves p in front of upstream with the workload client kube.
func proxyHarness(t *testing.T, upstream string, kube client.Client, direct bool) *harness {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	p := &Proxy{
		Upstream: u, Transport: http.DefaultTransport, Client: kube, Log: slog.New(slog.NewTextHandler(log, nil)),
		Owner: owner, Pool: pool, Holders: holders, Copies: !direct, Direct: direct, Poll: 5 * time.Millisecond,
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return &harness{srv: srv, kube: kube, log: log, p: p}
}

// unreachable is the address of a Proxmox VE that no longer answers.
func unreachable(t *testing.T) string {
	t.Helper()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	return gone.URL
}

// What the proxy answers itself when it cannot do a call, the plugin's releases both read as a
// failure: attaching, detaching or deleting through it never reads as done when it was not.
func TestProxyFailuresAreFailuresToBothReleases(t *testing.T) {
	t.Parallel()
	unreadable := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("dial tcp 192.0.2.10:6443: connect: connection refused")
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("dial tcp 192.0.2.10:6443: connect: connection refused")
		},
	}).Build()
	pve := httptest.NewServer(newFakePVE())
	t.Cleanup(pve.Close)
	for name, h := range map[string]*harness{
		"records unreadable":           proxyHarness(t, pve.URL, unreadable, false),
		"Proxmox VE gone, with copies": proxyHarness(t, unreachable(t), fake.NewClientBuilder().Build(), false),
		"Proxmox VE gone, direct":      proxyHarness(t, unreachable(t), fake.NewClientBuilder().Build(), true),
	} {
		for _, c := range pluginCalls {
			status, a := h.call(t, c.method, c.path, url.Values{"idlist": {"scsi1"}})
			if !failsForBoth(status, a.Errors) {
				t.Errorf("%s: %s answered %d %v, which a release of the plugin reads as done", name, c.name, status, a.Errors)
			}
		}
	}
}

// A cluster that makes no copies and has none left gets a proxy that never reads the copies'
// records: every call but the storage's configuration reaches Proxmox VE as it came.
func TestDirectProxyReadsNoRecords(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	refuse := func() error {
		asked.Add(1)
		return errors.New("the workload cluster was asked")
	}
	kube := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return refuse()
		},
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return refuse()
		},
	}).Build()
	pve := newFakePVE()
	upstream := httptest.NewServer(pve)
	t.Cleanup(upstream.Close)
	h := proxyHarness(t, upstream.URL, kube, true)
	for _, c := range pluginCalls {
		status, a := h.call(t, c.method, c.path, nil)
		if len(pve.seen(c.method+" "+c.path)) == 0 {
			t.Errorf("%s did not reach Proxmox VE: answered %d %v", c.name, status, a.Errors)
		}
	}
	status, a := h.call(t, http.MethodGet, "/storage/shared-lvm", nil)
	if status != http.StatusOK || !strings.Contains(string(a.Data), `"shared":1`) {
		t.Errorf("the shared storage's configuration answered %d %s %v", status, a.Data, a.Errors)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the workload cluster was asked %d times", n)
	}
}
