package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const removeBody = `{"passphrase":"correct horse battery staple","confirm":"remove"}`

// unansweringHarness is a harness whose management cluster refuses every list, as one does after
// a removal destroyed its VMs and stopped before the rest.
func unansweringHarness(t *testing.T, members ...pve.Member) *harness {
	t.Helper()
	h := newHarness(t)
	h.server.Appliance = &appliance.Appliance{Dir: t.TempDir()}
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		t.Fatal("the harness's client does not watch")
	}
	h.server.setKube(interceptor.NewClient(kube, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("dial tcp 10.0.0.10:6443: connect: connection refused")
		},
	}))
	h.accounts.mu.Lock()
	h.accounts.members = members
	h.accounts.mu.Unlock()
	return h
}

// With no management VM left the removal goes on. The test appliance holds no state, so a removal
// that got past the check answers that there is nothing to remove, which a refusal never says.
func TestRemovalGoesOnWhenTheUnansweringManagementClusterHasNoVMs(t *testing.T) {
	t.Parallel()
	h := unansweringHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("POST", "/api/setup/remove", removeBody)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "no management cluster to remove") {
		t.Fatalf("removal answered %s, want it to go past the check", rec.Body)
	}
}

func TestRemovalNamesTheVMsAnUnansweringManagementClusterStillHas(t *testing.T) {
	t.Parallel()
	h := unansweringHarness(t, pve.Member{VMID: 211, Type: "qemu", Name: "kubehenge-cp-0"})
	a := h.browser()
	a.signIn("admin@pve")
	rec := a.do("POST", "/api/setup/remove", removeBody)
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "VMs 211") || !strings.Contains(rec.Body.String(), "remove them in Proxmox VE") {
		t.Fatalf("removal refused with %s", rec.Body)
	}
}
