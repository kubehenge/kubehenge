//go:build integration

package provisioner

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationIdentitiesGoUnderTheControllersRights checks that the controller, with only the
// chart's rights, takes away the token of an identity a cluster no longer wants and passes over one
// it never had, and keeps a mounted identity's token in the autoscalers' namespace: a fake client
// allows every call, so only the API server shows a missing verb.
func TestIntegrationIdentitiesGoUnderTheControllersRights(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, admin, _ := startAPIConfig(t)
	ctx := context.Background()
	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + classNamespace + ":kubehenge-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + classNamespace, "system:authenticated"},
	}
	controller, err := client.New(as, client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	cluster := backupsCluster(`{"enabled":false,"target":"store"}`)
	err = client.IgnoreAlreadyExists(admin.Create(ctx, &corev1.Namespace{Name: cluster.Namespace}))
	if err != nil {
		t.Fatal(err)
	}
	w := &WorkloadIdentities{Client: controller, ClassNamespace: classNamespace, MountNamespace: autoscalerNamespace}

	if _, err = w.removeUnwanted(ctx, cluster); err != nil {
		t.Fatalf("with no backup identities to take away: %v", err)
	}
	for _, id := range []Identity{IdentityBackups, IdentityRestores} {
		s := &corev1.Secret{Namespace: cluster.Namespace, Name: IdentitySecretName(cluster.Name, id)}
		if err = admin.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	wanted, err := w.removeUnwanted(ctx, cluster)
	if err != nil {
		t.Fatalf("taking the backup identities away: %v", err)
	}
	for _, id := range wanted {
		if id.id == IdentityBackups || id.id == IdentityRestores {
			t.Errorf("%s still wanted with backups off", id.id)
		}
	}
	for _, id := range []Identity{IdentityBackups, IdentityRestores} {
		err := admin.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: IdentitySecretName(cluster.Name, id)}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("%s's token is still there: %v", id, err)
		}
	}
	checkMountedIdentityUnderTheControllersRights(ctx, t, w, admin)
}

// checkMountedIdentityUnderTheControllersRights has the controller write the autoscaler identity's
// token into the autoscalers' namespace, make it again, and take it away once the cluster stops
// autoscaling and once the cluster is gone.
func checkMountedIdentityUnderTheControllersRights(ctx context.Context, t *testing.T, w *WorkloadIdentities, admin client.Client) {
	t.Helper()
	cluster, _ := autoscaledCluster()
	key := client.ObjectKeyFromObject(cluster)
	secret := client.ObjectKey{Namespace: autoscalerNamespace, Name: mountedSecretName(key, IdentityAutoscaler)}
	id := identityOf(IdentityAutoscaler)
	workload := remote{config: &rest.Config{Host: "https://10.0.0.20:6443", CAData: []byte("the cluster CA")}}
	write := func(token string) {
		t.Helper()
		if err := w.writeToken(ctx, cluster, workload, id, token, time.Now().Add(tokenLifetime)); err != nil {
			t.Fatalf("writing the autoscaler identity's token: %v", err)
		}
		s := &corev1.Secret{}
		if err := admin.Get(ctx, secret, s); err != nil {
			t.Fatal(err)
		}
		if string(s.Data[IdentityTokenKey]) != token || len(s.Data[IdentityKubeconfigKey]) == 0 {
			t.Errorf("the autoscaler identity's Secret holds %q, want token %s and a kubeconfig", s.Data, token)
		}
	}
	gone := func(when string) {
		t.Helper()
		if err := admin.Get(ctx, secret, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("the autoscaler identity's token is still there %s: %v", when, err)
		}
	}
	write("first")
	write("renewed")

	cluster.Spec.Topology.Workers.MachineDeployments[0].Metadata.Annotations = nil
	wanted, err := w.removeUnwanted(ctx, cluster)
	if err != nil {
		t.Fatalf("taking the autoscaler identity away: %v", err)
	}
	if slices.Contains(wanted, id) {
		t.Error("the autoscaler identity still wanted without an autoscaled pool")
	}
	gone("with no autoscaled pool")

	write("again")
	if _, err := w.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("looking at a cluster that is gone: %v", err)
	}
	gone("once the cluster is gone")
}
