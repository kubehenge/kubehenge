//go:build integration

package provisioner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationIdentitiesGoUnderTheControllersRights checks that the controller, with only the
// chart's rights, takes away the token of an identity a cluster no longer wants and passes over one
// it never had: a fake client allows every call, so only the API server shows a missing verb.
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
	w := &WorkloadIdentities{Client: controller, ClassNamespace: classNamespace}

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
}
