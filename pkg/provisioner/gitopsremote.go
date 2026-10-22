package provisioner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileGitOps removes the engines the cluster no longer runs, then has the one it runs sync its
// repository: the repository's and the sign-in client's credentials in the engine's namespace, and
// the object that starts the sync. The engine chosen starts syncing only once the other is gone, so
// that two never fight over the same objects. It reports whether it waits.
func (r *Reconciler) reconcileGitOps(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) (bool, error) {
	g := &addons.GitOps
	waiting := false
	for _, engine := range []string{argoCD, flux} {
		if g.on(engine) {
			continue
		}
		w, err := r.removeGitOps(ctx, cluster, engine)
		if err != nil {
			return false, err
		}
		waiting = waiting || w
	}
	if !g.Enabled || waiting {
		return waiting, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	waiting, err = r.bootstrapGitOps(ctx, cluster, workload.client, g, &addons.gitOpsFound)
	if err != nil {
		r.Remotes.forget(cluster)
	}
	return waiting, err
}

// removeGitOps removes an engine the cluster no longer runs, leaving what it deployed running:
// first what would have the engine delete it, or hold its CRDs back, then its chart. It reports
// whether it waits for the engine to let go.
func (r *Reconciler) removeGitOps(ctx context.Context, cluster *clusterv1.Cluster, engine string) (bool, error) {
	proxy := gitOpsProxy(cluster, engine)
	there, err := r.anyProxy(ctx, cluster, proxy)
	if err != nil || !there {
		return false, err
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil {
		return false, err
	}
	if found {
		release := releaseArgoCD
		if engine == flux {
			release = releaseFlux
		}
		done, err := release(ctx, workload.client)
		if err != nil {
			r.Remotes.forget(cluster)
			return false, fmt.Errorf("removing %s: %w", addonNames[engine], err)
		}
		if !done {
			ctrl.LoggerFrom(ctx).Info("waiting for the GitOps engine to let go of what it deployed before removing it", "engine", engine)
			return true, nil
		}
	}
	return false, r.removeProxies(ctx, cluster, proxy)
}

// releaseArgoCD deletes Argo CD's ApplicationSets, leaving their Applications, and its Applications,
// without their finalizers, with which Argo CD would delete what they deployed or the CRDs would
// never go, and lifts the finalizers off its projects. It reports whether none is left.
func releaseArgoCD(ctx context.Context, c client.Client) (bool, error) {
	left := false
	for _, kind := range []struct {
		gvk    schema.GroupVersionKind
		delete []client.DeleteOption
	}{
		{applicationSetGVK, []client.DeleteOption{client.PropagationPolicy(metav1.DeletePropagationOrphan)}},
		{applicationGVK, nil},
		{appProjectGVK, nil},
	} {
		found, err := releaseArgoCDKind(ctx, c, kind.gvk, kind.delete...)
		if err != nil {
			return false, err
		}
		left = left || found
	}
	if left {
		return false, nil
	}
	return true, deleteSecrets(ctx, c, argoCDNamespace, argoCDRepositorySecret, argoCDClientSecret)
}

// releaseArgoCDKind lifts the finalizers off every object of one of Argo CD's kinds and deletes it,
// but for projects, which only lose their finalizers. It reports whether it found any to delete.
func releaseArgoCDKind(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, opts ...client.DeleteOption) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	err := c.List(ctx, list)
	if meta.IsNoMatchError(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("listing %ss: %w", gvk.Kind, err)
	}
	found := false
	for i := range list.Items {
		o := &list.Items[i]
		if len(o.GetFinalizers()) > 0 {
			patch := client.MergeFrom(o.DeepCopy())
			o.SetFinalizers(nil)
			if err := client.IgnoreNotFound(c.Patch(ctx, o, patch)); err != nil {
				return false, fmt.Errorf("lifting the finalizers of %s %s/%s: %w", gvk.Kind, o.GetNamespace(), o.GetName(), err)
			}
		}
		if gvk == appProjectGVK {
			continue
		}
		found = true
		if err := client.IgnoreNotFound(c.Delete(ctx, o, opts...)); err != nil {
			return false, fmt.Errorf("deleting %s %s/%s: %w", gvk.Kind, o.GetNamespace(), o.GetName(), err)
		}
	}
	return found, nil
}

// releaseFlux deletes the FluxInstance, on which the Flux Operator uninstalls Flux and leaves what
// Flux deployed running, and reports whether it is gone.
func releaseFlux(ctx context.Context, c client.Client) (bool, error) {
	instance := &unstructured.Unstructured{}
	instance.SetGroupVersionKind(fluxInstanceGVK)
	err := c.Get(ctx, client.ObjectKey{Namespace: fluxNamespace, Name: fluxInstanceName}, instance)
	switch {
	case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		if err = client.IgnoreNotFound(c.Delete(ctx, solverPolicy())); err != nil {
			return false, fmt.Errorf("deleting NetworkPolicy %s/%s: %w", fluxNamespace, fluxSolverPolicy, err)
		}
		return true, deleteSecrets(ctx, c, fluxNamespace, fluxPullSecret, fluxWebSecret)
	case err != nil:
		return false, fmt.Errorf("reading the FluxInstance: %w", err)
	case instance.GetDeletionTimestamp() == nil:
		if err := client.IgnoreNotFound(c.Delete(ctx, instance)); err != nil {
			return false, fmt.Errorf("deleting the FluxInstance: %w", err)
		}
	}
	return false, nil
}

func deleteSecrets(ctx context.Context, c client.Client, namespace string, names ...string) error {
	for _, name := range names {
		s := &corev1.Secret{Namespace: namespace, Name: name}
		if err := client.IgnoreNotFound(c.Delete(ctx, s)); err != nil {
			return fmt.Errorf("deleting Secret %s/%s: %w", namespace, name, err)
		}
	}
	return nil
}

// bootstrapGitOps writes the engine's credentials into the workload cluster, for Flux the network
// policy that lets certificates be issued in its namespace, and the object that has the engine sync
// the repository. It reports whether it waits: for the engine's namespace and CRDs, or for
// a credential the cluster's owner is yet to store.
func (r *Reconciler) bootstrapGitOps(ctx context.Context, cluster *clusterv1.Cluster, c client.Client, g *GitOps, state *gitOpsState) (bool, error) {
	creds, missing, err := r.repositoryCredentials(ctx, cluster, g, state)
	if err != nil {
		return false, err
	}
	if missing != "" {
		r.gitOpsWaits(ctx, cluster, missing)
		return true, nil
	}
	clientSecret, missing, err := r.clientSecret(ctx, cluster, c, g, state)
	if err != nil {
		return false, err
	}
	if missing != "" {
		r.gitOpsWaits(ctx, cluster, missing)
	}
	keep, err := gitOpsSecrets(g, state, creds, clientSecret)
	if err != nil {
		return false, err
	}
	if waits, err := writeGitOpsSecrets(ctx, c, g, keep); err != nil || waits {
		return waits, err
	}
	if g.engine() == flux {
		if waits, err := applyInWorkload(ctx, c, solverPolicy()); err != nil || waits {
			return waits, err
		}
	}
	return applyInWorkload(ctx, c, bootstrapObject(g, r.Charts.FluxVersion))
}

// gitOpsSecrets are the Secrets the controller keeps in the engine's namespace, by name: the
// repository's credentials, and the sign-in client's secret where the engine needs it written.
func gitOpsSecrets(g *GitOps, state *gitOpsState, creds map[string][]byte, clientSecret []byte) (map[string]*corev1.Secret, error) {
	keep := map[string]*corev1.Secret{}
	if creds != nil {
		s := repositorySecret(g, creds)
		keep[s.Name] = s
	}
	switch {
	case clientSecret == nil:
	case g.engine() == argoCD && g.clientSecret() == clientSecretStored:
		s := argoCDClientSecretObject(clientSecret)
		keep[s.Name] = s
	case g.fluxWebOn(state):
		config, err := fluxWebConfig(g, state.provider, string(clientSecret))
		if err != nil {
			return nil, err
		}
		s := fluxWebSecretObject(config, state.provider.CertificateAuthority)
		keep[s.Name] = s
	}
	return keep, nil
}

// writeGitOpsSecrets applies the Secrets kept and deletes the engine's other Secrets of the
// controller's, and reports whether it waits for the engine's namespace.
func writeGitOpsSecrets(ctx context.Context, c client.Client, g *GitOps, keep map[string]*corev1.Secret) (bool, error) {
	ours := []string{argoCDRepositorySecret, argoCDClientSecret}
	if g.engine() == flux {
		ours = []string{fluxPullSecret, fluxWebSecret}
	}
	for _, name := range ours {
		s, ok := keep[name]
		if !ok {
			if err := deleteSecrets(ctx, c, g.namespace(), name); err != nil {
				return false, err
			}
			continue
		}
		o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
		if err != nil {
			return false, fmt.Errorf("writing Secret %s: %w", name, err)
		}
		if waits, err := applyInWorkload(ctx, c, &unstructured.Unstructured{Object: o}); err != nil || waits {
			return waits, err
		}
	}
	return false, nil
}

// applyInWorkload applies o to the workload cluster, and reports whether it waits for its namespace
// or its CRD, which the engine's chart brings.
func applyInWorkload(ctx context.Context, c client.Client, o *unstructured.Unstructured) (bool, error) {
	err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner(fieldOwner), client.ForceOwnership)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("applying %s %s/%s: %w", o.GetKind(), o.GetNamespace(), o.GetName(), err)
	}
	return false, nil
}

// repositoryCredentials are what the engine reads the repository with, nil when the controller
// writes none, or what is missing of them.
func (r *Reconciler) repositoryCredentials(ctx context.Context, cluster *clusterv1.Cluster, g *GitOps, state *gitOpsState) (map[string][]byte, string, error) {
	switch g.auth() {
	case gitAuthKey:
		if len(state.keys[identityKey]) == 0 {
			return nil, "the deploy key", nil
		}
		creds := map[string][]byte{identityKey: state.keys[identityKey]}
		if g.engine() == flux {
			if state.knownHosts == "" {
				return nil, "the repository host's SSH keys", nil
			}
			creds[identityPubKey], creds[knownHostsKey] = state.keys[identityPubKey], []byte(state.knownHosts)
		}
		return creds, "", nil
	case gitAuthToken:
		stored, ok, err := r.credential(ctx, cluster, gitOpsPurpose)
		if err != nil || !ok {
			return nil, "the repository's token, stored as the cluster's " + gitOpsPurpose + " credential", err
		}
		creds := map[string][]byte{usernameKey: stored[usernameKey], passwordKey: stored[passwordKey]}
		return creds, "", nil
	}
	return nil, "", nil
}

// clientSecret is the secret of the client the engine's interface signs in as, where the controller
// writes it into the cluster, or what is missing of it.
func (r *Reconciler) clientSecret(ctx context.Context, cluster *clusterv1.Cluster, c client.Client, g *GitOps, state *gitOpsState) ([]byte, string, error) {
	needed := g.engine() == argoCD || (g.UI && g.Hostname != "" && state.provider != nil)
	if !needed {
		return nil, "", nil
	}
	switch g.clientSecret() {
	case clientSecretStored:
		stored, ok, err := r.credential(ctx, cluster, clientPurpose)
		if err != nil || !ok {
			return nil, "the sign-in client's secret, stored as the cluster's " + clientPurpose + " credential", err
		}
		return stored[clientSecretKey], "", nil
	case clientSecretExisting:
		if g.engine() == argoCD {
			// Argo CD reads the owner's Secret itself, by the name in its configuration.
			return nil, "", nil
		}
		s := &corev1.Secret{}
		err := c.Get(ctx, client.ObjectKey{Namespace: fluxNamespace, Name: g.SignIn.ExistingSecret}, s)
		if apierrors.IsNotFound(err) || (err == nil && len(s.Data[clientSecretKey]) == 0) {
			return nil, fmt.Sprintf("Secret %s/%s with the sign-in client's %s", fluxNamespace, g.SignIn.ExistingSecret, clientSecretKey), nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("reading the sign-in client's secret: %w", err)
		}
		return s.Data[clientSecretKey], "", nil
	}
	if g.engine() == flux {
		return nil, "a client secret, which Flux's web interface signs in with", nil
	}
	return nil, "", nil
}

// gitOpsWaits reports on the cluster what its GitOps waits for.
func (r *Reconciler) gitOpsWaits(ctx context.Context, cluster *clusterv1.Cluster, missing string) {
	ctrl.LoggerFrom(ctx).Info("GitOps waits for " + missing)
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "GitOpsWaiting", "SyncRepository", "GitOps waits for %s", missing)
	}
}
