package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// cutShort has the harness's Cluster creates and Secret deletes fail with err while *on holds, as a
// client whose request was cancelled or ran past its deadline sees them.
func cutShort(t *testing.T, h *harness, err error, on *bool) client.Client {
	t.Helper()
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		t.Fatal("the harness's client cannot be wrapped")
	}
	h.server.setKube(interceptor.NewClient(kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, cluster := obj.(*clusterv1.Cluster); cluster && *on {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, secret := obj.(*corev1.Secret); secret && *on {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}))
	return kube
}

// keyLeftBehind runs a create with a key that is cut short after storing it, and returns the key
// it left: marked with its request, and owned by nobody.
func keyLeftBehind(t *testing.T, h *harness, b *browser, cause error) *corev1.Secret {
	t.Helper()
	on := true
	cutShort(t, h, cause, &on)
	defer func() { on = false }()
	if rec := b.do("POST", "/api/clusters", withKey(createBody, `{"accessKeyID":"OLD","secretAccessKey":"S"}`)); rec.Code == http.StatusCreated {
		t.Fatalf("a create cut short answered %d", rec.Code)
	}
	s, err := readSecret(h, "web.backup")
	if err != nil || s.Labels[provisioner.CreateRequestKey] == "" || len(s.OwnerReferences) > 0 || string(s.Data["accessKeyID"]) != "OLD" {
		t.Fatalf("left behind: %+v, %v", s, err)
	}
	return s
}

// A create cut short, by its deadline or by the user, leaves its key behind. A retry while that
// create may still be under way is refused and leaves the key alone. Once that create has ended,
// the retry replaces the leftover with its own key, which its cluster then owns.
func TestCreateRetryReplacesTheKeyACutShortCreateLeft(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		h := newHarness(t)
		h.accounts.grants["dev@pve"]["/pool/kubehenge/default.web"] = []string{"Sys.Modify"}
		b := h.browser()
		b.signIn("dev@pve")
		old := keyLeftBehind(t, h, b, cause)
		fresh := withKey(createBody, `{"accessKeyID":"NEW","secretAccessKey":"S2"}`)

		rec := b.do("POST", "/api/clusters", fresh)
		expect(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "under way") {
			t.Errorf("%v: a retry at once was refused with %s", cause, rec.Body)
		}
		if s, err := readSecret(h, "web.backup"); err != nil || s.UID != old.UID || string(s.Data["accessKeyID"]) != "OLD" {
			t.Errorf("%v: the key of a create maybe under way: %+v, %v", cause, s, err)
		}

		h.clock = h.clock.Add(createTimeout)
		expect(t, b.do("POST", "/api/clusters", fresh), http.StatusCreated)
		s, err := readSecret(h, "web.backup")
		if err != nil || string(s.Data["accessKeyID"]) != "NEW" || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "web" {
			t.Fatalf("%v: the key after the retry: %+v, %v", cause, s, err)
		}
		c := &clusterv1.Cluster{}
		if err = h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil ||
			s.Labels[provisioner.CreateRequestKey] == old.Labels[provisioner.CreateRequestKey] ||
			c.Annotations[provisioner.CreateRequestKey] != s.Labels[provisioner.CreateRequestKey] {
			t.Errorf("%v: the retry's mark: key %v, cluster %v, %v", cause, s.Labels, c.Annotations, err)
		}
	}
}

// The other way out of a cut-short create: the cluster is made without a key, and the key is stored
// through the cluster's credential endpoint. That key is the cluster's, and the controller's sweep
// past the grace keeps it.
func TestCreateWithoutAKeyThenStoringOneKeepsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.accounts.grants["dev@pve"]["/pool/kubehenge/default.web"] = []string{"Sys.Modify"}
	b := h.browser()
	b.signIn("dev@pve")
	keyLeftBehind(t, h, b, context.DeadlineExceeded)

	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusCreated)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	expect(t, b.do("PUT", "/api/clusters/default/web/credentials/backup", `{"values":{"accessKeyID":"NEW","secretAccessKey":"S2"}}`), http.StatusOK)
	s, err := readSecret(h, "web.backup")
	if err != nil || string(s.Data["accessKeyID"]) != "NEW" || s.Labels[provisioner.CreateRequestKey] != "" ||
		len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != c.UID {
		t.Fatalf("the key stored through the endpoint: %+v, %v", s, err)
	}

	sweep := &provisioner.CredentialOrphans{Client: h.kube, Grace: 10 * time.Minute, Now: func() time.Time { return time.Now().Add(11 * time.Minute) }}
	if err = sweep.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s, err = readSecret(h, "web.backup"); err != nil || string(s.Data["accessKeyID"]) != "NEW" {
		t.Errorf("after the controller's sweep: %+v, %v", s, err)
	}
}

// A create lists the nodes once for all its checks, so that a node that does not answer costs its
// wait once against the request's deadline.
func TestCreateListsTheNodesOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	class.Spec.Variables = append(class.Spec.Variables, clusterv1.ClusterClassVariable{
		Name: "allowedNodes", Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{Type: "array"}},
	})
	for _, name := range []string{"controlPlaneMachine", "workerMachine"} {
		class.Spec.Variables = append(class.Spec.Variables, clusterv1.ClusterClassVariable{
			Name: name, Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{
				Type: "object", Default: &apiextensionsv1.JSON{Raw: []byte(`{"cores":2,"memoryMiB":4096,"diskGiB":40}`)},
			}},
		})
	}
	if err := h.kube.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	h.accounts.mu.Lock()
	h.accounts.nodeReads = 0
	h.accounts.mu.Unlock()
	expect(t, b.do("POST", "/api/clusters", strings.Replace(createBody, `"storage":`, `"allowedNodes":["pve1"],"storage":`, 1)), http.StatusCreated)
	h.accounts.mu.Lock()
	defer h.accounts.mu.Unlock()
	if h.accounts.nodeReads != 1 {
		t.Errorf("the create listed the nodes %d times, want once", h.accounts.nodeReads)
	}
}
