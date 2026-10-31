package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func TestGitOpsNeedsTheKubeconfigRight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"), cluster("default", "two"))
	on := `{"enabled":true,"repository":{"url":"https://git.example.org/a.git"}}`
	withGitOps := func(name string) string {
		return strings.Replace(strings.Replace(createBody, `"web"`, `"`+name+`"`, 1), `"storage":`, `"gitops":`+on+`,"storage":`, 1)
	}
	dev := h.browser()
	dev.signIn("dev@pve")
	rec := dev.do("POST", "/api/clusters", withGitOps("a"))
	expect(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Sys.Modify on /pool/kubehenge") {
		t.Fatalf("refusal = %s", rec.Body)
	}
	expect(t, dev.do("POST", "/api/clusters", strings.Replace(createBody, `"storage":`, `"gitops":{"enabled":false},"storage":`, 1)), http.StatusCreated)
	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("POST", "/api/clusters", withGitOps("b")), http.StatusCreated)

	// dev holds Sys.Modify on one's pool, not on two's. Switching off needs no more than a change.
	change := `{"variables":{"gitops":` + on + `}}`
	expect(t, dev.do("PATCH", "/api/clusters/default/one", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", change), http.StatusForbidden)
	expect(t, admin.do("PATCH", "/api/clusters/default/two", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", change), http.StatusOK)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"gitops":{"enabled":true,"repository":{"url":"https://git.example.org/b.git"}}}}`), http.StatusForbidden)
	expect(t, dev.do("PATCH", "/api/clusters/default/two", `{"variables":{"gitops":{"enabled":false}}}`), http.StatusOK)
}

func TestClusterShowsItsDeployKey(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: gitopsVariable, Value: apiextensionsv1.JSON{
		Raw: []byte(`{"enabled":true,"knownHosts":"git.example.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"}`),
	}})
	keys := &corev1.Secret{
		Namespace: "default", Name: "one-gitops",
		Data: map[string][]byte{
			"identity":     []byte("PRIVATE"),
			"identity.pub": []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl kubehenge default/one\n"),
		},
	}
	h := newHarness(t, c, keys)
	v := h.browser()
	v.signIn("viewer@pve")
	rec := v.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Fatal("the deploy key's private half is shown")
	}
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.GitOps == nil || !strings.HasPrefix(view.GitOps.DeployKey, "ssh-ed25519 ") || !strings.HasPrefix(view.GitOps.DeployKeyFingerprint, "SHA256:") ||
		len(view.GitOps.HostKeys) != 1 || view.GitOps.HostKeys[0] != "ssh-ed25519 "+view.GitOps.DeployKeyFingerprint {
		t.Errorf("GitOps %+v", view.GitOps)
	}
}
