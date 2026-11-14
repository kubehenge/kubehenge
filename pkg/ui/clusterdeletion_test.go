package ui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// admitClusters has the harness's API server answer cluster writes as the real one does with the
// chart installed: a patch goes through the clusters.kubehenge.dev webhook, and a delete of a
// Kubehenge cluster without kubehenge.dev/allow-deletion meets the deletion policy.
func (h *harness) admitClusters() {
	h.t.Helper()
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		h.t.Fatal("the harness's client does not watch")
	}
	validator := &provisioner.ClusterValidator{Hooks: &provisioner.Hooks{Reader: kube, ClassNamespace: "kubehenge-system"}}
	clusters := clusterv1.GroupVersion.WithResource("clusters").GroupResource()
	h.server.setKube(interceptor.NewClient(kube, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isCluster := obj.(*clusterv1.Cluster); !isCluster {
				return c.Patch(ctx, obj, patch, opts...)
			}
			old := &clusterv1.Cluster{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), old); err != nil {
				return err
			}
			changed := old.DeepCopy()
			if err := c.Patch(ctx, changed, patch, append(opts, client.DryRunAll)...); err != nil {
				return err
			}
			if _, err := validator.ValidateUpdate(ctx, old, changed); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isCluster := obj.(*clusterv1.Cluster); isCluster {
				stored := &clusterv1.Cluster{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
					return err
				}
				_, allowed := stored.Annotations[provisioner.AllowDeletionAnnotation]
				if provisioner.Owns(stored, "kubehenge-system") && stored.DeletionTimestamp.IsZero() && !allowed {
					return apierrors.NewForbidden(clusters, stored.Name, errors.New("cluster "+stored.Name+
						" is protected from deletion: annotate it with kubehenge.dev/allow-deletion=true, then delete it"))
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	}))
}

// expiredCertificate is a self-signed authority that expired an hour ago.
func expiredCertificate(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "old"}, NotBefore: now.Add(-48 * time.Hour), NotAfter: now.Add(-time.Hour),
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A cluster whose trusted authority expired since it was made still goes when its user confirms
// the deletion, the CLI's included, since only the annotation is written on the way.
func TestAClusterWhoseAuthorityExpiredCanBeDeleted(t *testing.T) {
	t.Parallel()
	cas, err := json.Marshal([]string{expiredCertificate(t)})
	if err != nil {
		t.Fatal(err)
	}
	one := cluster("default", "one")
	one.Finalizers = []string{clusterv1.ClusterFinalizer}
	one.Spec.Topology.Variables = append(one.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "trustedCAs", Value: apiextensionsv1.JSON{Raw: cas}})
	h := newHarness(t, one)
	h.admitClusters()
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("DELETE", "/api/clusters/default/one", `{"confirm":"one"}`), http.StatusAccepted)
	got := storedCluster(t, h, "one")
	if got.DeletionTimestamp.IsZero() {
		t.Error("the cluster is not being deleted")
	}
}

// What the API server refuses reaches the user with its status and its own message. Only a
// management cluster that cannot be reached, or does not answer in time, is a bad gateway.
func TestManagementClusterAnswersKeepTheirStatus(t *testing.T) {
	t.Parallel()
	clusters := clusterv1.GroupVersion.WithResource("clusters").GroupResource()
	invalid := apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), "one",
		field.ErrorList{field.Invalid(field.NewPath("spec", "topology"), nil, "a webhook refused it")})
	for name, tc := range map[string]struct {
		err    error
		status int
		says   string
	}{
		"forbidden":       {apierrors.NewForbidden(clusters, "one", errors.New("a policy refused it")), http.StatusForbidden, "a policy refused it"},
		"invalid":         {invalid, http.StatusUnprocessableEntity, "spec.topology: Invalid value: null: a webhook refused it"},
		"conflict":        {apierrors.NewConflict(clusters, "one", errors.New("modified")), http.StatusConflict, clusterChanged},
		"not found":       {apierrors.NewNotFound(clusters, "one"), http.StatusNotFound, "not found"},
		"wrapped":         {fmt.Errorf("deleting: %w", apierrors.NewForbidden(clusters, "one", errors.New("held"))), http.StatusForbidden, "held"},
		"timeout":         {apierrors.NewTimeoutError("the request timed out", 1), http.StatusBadGateway, "did not answer"},
		"deadline":        {context.DeadlineExceeded, http.StatusBadGateway, "did not answer"},
		"no connection":   {&url.Error{Op: "Get", URL: "https://10.0.0.1:6443", Err: errors.New("connection refused")}, http.StatusBadGateway, "did not answer"},
		"webhook unreach": {apierrors.NewInternalError(errors.New(`failed calling webhook "clusters.kubehenge.dev"`)), http.StatusBadGateway, "did not answer"},
	} {
		h := newHarness(t)
		rec := httptest.NewRecorder()
		h.server.kubeError(rec, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/clusters/default/one", nil), tc.err)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.says) {
			t.Errorf("%s: status %d, %s; want %d saying %q", name, rec.Code, rec.Body, tc.status, tc.says)
		}
	}
}

// The guard's refusal reaches the user as the guard words it, as when a GitOps sync takes the
// annotation off between the interface's patch and its delete.
func TestTheDeletionGuardsRefusalReachesTheUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	h.admitClusters()
	admitted, ok := h.server.currentKube().(client.WithWatch)
	if !ok {
		t.Fatal("the admitting client does not watch")
	}
	h.server.setKube(interceptor.NewClient(admitted, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			synced := &clusterv1.Cluster{}
			if err := h.kube.Get(ctx, client.ObjectKeyFromObject(obj), synced); err != nil {
				return err
			}
			delete(synced.Annotations, provisioner.AllowDeletionAnnotation)
			return h.kube.Update(ctx, synced)
		},
	}))
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("DELETE", "/api/clusters/default/one", `{"confirm":"one"}`)
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "cluster one is protected from deletion: annotate it with kubehenge.dev/allow-deletion=true") {
		t.Errorf("refusal = %s", rec.Body)
	}
	if got := storedCluster(t, h, "one"); !got.DeletionTimestamp.IsZero() {
		t.Error("the cluster is being deleted")
	}
}
