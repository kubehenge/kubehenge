package kubehenge_test

import "testing"

// A role's revision changes the files of that role's templates alone, which is what has Cluster API
// replace only those nodes when a Secret or ConfigMap they read changes.
func TestRevisionChangesOnlyItsRolesBootstrap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, cp, worker string }{
		{"kubeadm.yaml", "KubeadmControlPlaneTemplate", "KubeadmConfigTemplate"},
		{"rke2.yaml", "RKE2ControlPlaneTemplate", "RKE2ConfigTemplate"},
		{"k3s.yaml", "KThreesControlPlaneTemplate", "KThreesConfigTemplate"},
	} {
		class := renderClass(t, c.file)
		render := func(kind, revision string) string {
			return asJSON(t, class.patched(t, kind, variablesOf(t, `{"nodeCustomisationRevision": `+revision+`}`)))
		}
		for kind, own := range map[string]string{c.cp: "controlPlane", c.worker: "worker"} {
			other := map[string]string{"controlPlane": "worker", "worker": "controlPlane"}[own]
			base := render(kind, `{"`+own+`": "aaaa"}`)
			if base == render(kind, `{}`) || base == render(kind, `{"`+own+`": "bbbb"}`) {
				t.Errorf("%s %s: its own revision does not reach the template", c.file, kind)
			}
			if render(kind, `{"`+other+`": "aaaa"}`) != render(kind, `{}`) {
				t.Errorf("%s %s: the %s revision reaches it", c.file, kind, other)
			}
		}
	}
}
