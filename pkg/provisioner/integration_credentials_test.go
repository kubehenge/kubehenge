//go:build integration

package provisioner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const interfaceNamespace = InterfaceNamespace

func credential(cluster, purpose string, data map[string][]byte) *corev1.Secret {
	labels := map[string]string{CredentialLabel: purpose}
	if cluster != "" {
		labels[CredentialClusterLabel] = cluster
	}
	return &corev1.Secret{
		Namespace: interfaceNamespace, Name: CredentialName(cluster, purpose), Labels: labels,
		Type: CredentialType, Data: data,
	}
}

// TestIntegrationInterfaceCredentials holds the interface's account to the credentials it stores:
// it writes and deletes those, and no other Secret.
func TestIntegrationInterfaceCredentials(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, admin := startAPIConfig(t)
	ctx := context.Background()
	ui, err := client.New(interfaceAccount(cfg), client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := &corev1.Secret{Namespace: interfaceNamespace, Name: "one-kubeconfig", Data: map[string][]byte{"value": []byte("apiVersion: v1\n")}}
	if err := admin.Create(ctx, kubeconfig); err != nil {
		t.Fatal(err)
	}
	waitForCredentialPolicy(t, ui, admin, interfaceNamespace)

	refused := func(what string, err error, message string) {
		t.Helper()
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v, want refused for %q", what, err, message)
		}
	}

	for _, p := range CredentialPurposes() {
		values := map[string][]byte{}
		for _, k := range p.Keys {
			values[k] = []byte("value of " + k)
		}
		c := credential("one", p.Name, values)
		switch err := ui.Create(ctx, c); {
		case p.ApplianceOnly:
			refused("a cluster's "+p.Name+" credential", err, "not one for "+p.Name)
		case err != nil:
			t.Fatalf("storing one's %s credential: %v", p.Name, err)
		default:
			c.Data[p.Keys[0]] = []byte("replaced")
			if err := ui.Update(ctx, c); err != nil {
				t.Fatalf("replacing one's %s credential: %v", p.Name, err)
			}
			if err := ui.Delete(ctx, c); err != nil {
				t.Fatalf("removing one's %s credential: %v", p.Name, err)
			}
		}
		shared := credential("", p.Name, values)
		err := ui.Create(ctx, shared)
		switch {
		case p.Appliance && err != nil:
			t.Fatalf("storing the %s credential for every cluster: %v", p.Name, err)
		case p.Appliance:
			if deleted := ui.Delete(ctx, shared); deleted != nil {
				t.Fatalf("removing the %s credential for every cluster: %v", p.Name, deleted)
			}
		default:
			refused("a "+p.Name+" credential for every cluster", err, "not one for "+p.Name)
		}
	}

	unlabelled := &corev1.Secret{Namespace: interfaceNamespace, Name: "one.backup", Type: CredentialType}
	refused("an unlabelled Secret", ui.Create(ctx, unlabelled), "not an unlabelled Secret")
	refused("an unknown purpose", ui.Create(ctx, credential("one", "ssh", nil)), "not one for ssh")
	named := credential("one", "backup", nil)
	named.Name = "one-kubeconfig2"
	refused("a credential under another name", ui.Create(ctx, named), "is named <cluster>.<purpose>")
	other := credential("two", "backup", nil)
	other.Name = "one.backup"
	refused("a credential under another cluster's name", ui.Create(ctx, other), "is named <cluster>.<purpose>")
	opaque := credential("one", "backup", nil)
	opaque.Type = corev1.SecretTypeOpaque
	refused("an Opaque credential", ui.Create(ctx, opaque), "of type kubehenge.dev/user-credential")
	token := credential("one", "backup", nil)
	token.Type = corev1.SecretTypeServiceAccountToken
	token.Annotations = map[string]string{corev1.ServiceAccountNameKey: "default"}
	refused("a ServiceAccount token", ui.Create(ctx, token), "of type kubehenge.dev/user-credential")
	held := credential("one", "backup", nil)
	held.Finalizers = []string{"kubehenge.dev/held"}
	refused("a credential with a finalizer", ui.Create(ctx, held), "carries no finalizers")

	taken := kubeconfig.DeepCopy()
	if err := admin.Get(ctx, client.ObjectKeyFromObject(taken), taken); err != nil {
		t.Fatal(err)
	}
	taken.Labels = map[string]string{CredentialLabel: "backup", CredentialClusterLabel: "one-kubeconfig"} //nolint:gosec // labels, not a credential
	taken.Data = map[string][]byte{"value": []byte("apiVersion: v1\nclusters: []\n")}
	refused("labelling a kubeconfig as a credential", ui.Update(ctx, taken), "changes no Secret but the credentials")
	refused("deleting a kubeconfig", ui.Delete(ctx, kubeconfig), "not an unlabelled Secret")

	elsewhere := credential("one", "backup", nil)
	elsewhere.Namespace = "team-a"
	// team-a is not among the interface's namespaces here.
	if err := ui.Create(ctx, elsewhere); !apierrors.IsForbidden(err) {
		t.Errorf("a credential outside the interface's namespace: %v, want forbidden", err)
	}

	// Anyone else, a GitOps tool or kubectl, is the policy's no concern.
	if err := admin.Delete(ctx, kubeconfig); err != nil {
		t.Errorf("the administrator deleting a kubeconfig: %v", err)
	}
}
