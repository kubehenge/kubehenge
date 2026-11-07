package provisioner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const validAuditPolicy = "apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n  - level: Metadata\n"

func nodeReader(t *testing.T) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&corev1.Secret{Namespace: "team-a", Name: "siem", Data: map[string][]byte{"kubeconfig": []byte("x"), "ca": []byte("y")}},
		&corev1.Secret{Namespace: "team-b", Name: "elsewhere", Data: map[string][]byte{"ca": []byte("y")}},
		&corev1.ConfigMap{Namespace: "team-a", Name: "policy", Data: map[string]string{"policy.yaml": validAuditPolicy}},
		&corev1.ConfigMap{Namespace: "team-a", Name: "empty", Data: map[string]string{"policy.yaml": "apiVersion: audit.k8s.io/v1\nkind: Policy\n"}},
		&corev1.ConfigMap{Namespace: "team-a", Name: "wrongkey", Data: map[string]string{"other": validAuditPolicy}},
		&corev1.ConfigMap{Namespace: "team-a", Name: "notpolicy", Data: map[string]string{"policy.yaml": "kind: Pod\n"}},
		&corev1.ConfigMap{Namespace: "team-b", Name: "theirs", Data: map[string]string{"policy.yaml": validAuditPolicy}},
	).Build()
}

func TestNodeCustomisationIsRefusedWhereItMeetsWhatKubehengeKeeps(t *testing.T) {
	t.Parallel()
	reader := nodeReader(t)
	for name, tc := range map[string]struct {
		class, value, want string
	}{
		"fine on kubeadm": {"kubeadm", `{"files":[{"path":"/etc/audit/rules.d/50-co.rules","permissions":"0640","content":"-w /etc/passwd -p wa","roles":["controlPlane","worker"]},{"path":"/etc/pki/ca-trust/source/anchors/co.crt","secret":{"name":"siem","key":"ca"}}],` +
			`"commands":[{"run":"augenrules --load"}],"kubeletArgs":[{"name":"event-qps","value":"50"}],"apiServerArgs":[{"name":"audit-log-maxage","value":"7"},{"name":"request-timeout","value":"30s"}],` +
			`"controllerManagerArgs":[{"name":"concurrent-gc-syncs","value":"30"}],"schedulerArgs":[{"name":"kube-api-qps","value":"60"}],"auditPolicy":{"configMap":"policy"},"auditWebhook":{"secret":"siem"}}`, ""},
		"fine on talos": {"talos", `{"files":[{"path":"/var/etc/co.conf","content":"x","roles":["worker"]}],"kubeletArgs":[{"name":"event-qps","value":"50"}],"apiServerArgs":[{"name":"request-timeout","value":"30s"}],"auditPolicy":{"policy":{"apiVersion":"audit.k8s.io/v1","kind":"Policy","rules":[{"level":"Metadata"}]}}}`, ""},
		"fine on rke2":  {"rke2", `{"files":[{"path":"/etc/audit/auditd.conf","content":"x"}],"auditWebhook":{"secret":"siem","key":"kubeconfig"}}`, ""},
		"fine on k3s":   {"k3s", `{"commands":[{"run":"true","roles":["worker"]}]}`, ""},

		"path under /etc/kubernetes": {"kubeadm", `{"files":[{"path":"/etc/kubernetes/pki/ca.crt","content":"x"}]}`, "/etc/kubernetes/pki/ca.crt is a path Kubehenge keeps"},
		"the kubeadm audit policy":   {"kubeadm", `{"files":[{"path":"/etc/kubernetes/audit-policy.yaml","content":"x"}]}`, "is a path Kubehenge keeps"},
		"rke2 config directory":      {"rke2", `{"files":[{"path":"/etc/rancher/rke2/config.yaml.d/90-x.yaml","content":"x"}]}`, "Kubehenge writes under /etc/rancher"},
		"k3s manifests":              {"k3s", `{"files":[{"path":"/var/lib/rancher/k3s/server/manifests/x.yaml","content":"x"}]}`, "Kubehenge writes under /var/lib/rancher"},
		"kubehenge's own scripts":    {"kubeadm", `{"files":[{"path":"/etc/kubehenge/node-ip.sh","content":"x"}]}`, "Kubehenge writes under /etc/kubehenge"},
		"the sysctl file":            {"rke2", `{"files":[{"path":"/etc/sysctl.d/90-kubehenge.conf","content":"x"}]}`, "Kubehenge writes under /etc/sysctl.d/90-kubehenge.conf"},
		"a read-only path":           {"kubeadm", `{"files":[{"path":"/usr/bin/x","content":"x"}]}`, "read-only"},
		"a traversing path":          {"kubeadm", `{"files":[{"path":"/etc/audit/../kubernetes/x","content":"x"}]}`, "without . or .. parts"},
		"a relative path":            {"kubeadm", `{"files":[{"path":"etc/x","content":"x"}]}`, "absolute path"},
		"a directory":                {"kubeadm", `{"files":[{"path":"/etc/x/","content":"x"}]}`, "closing slash"},
		"talos outside /var":         {"talos", `{"files":[{"path":"/etc/audit/x","content":"x"}]}`, "Talos writes machine files under /var and /etc/cri/conf.d only"},
		"talos etcd data":            {"talos", `{"files":[{"path":"/var/lib/etcd/x","content":"x"}]}`, "Talos keeps /var/lib/etcd for itself"},
		"talos secret file":          {"talos", `{"files":[{"path":"/var/x","secret":{"name":"siem","key":"ca"}}]}`, "Talos reads no Secret"},
		"talos commands":             {"talos", `{"commands":[{"run":"true"}]}`, "Talos has no shell"},
		"talos webhook":              {"talos", `{"auditWebhook":{"secret":"siem"}}`, "Talos reads no Secret"},
		"talos configmap policy":     {"talos", `{"auditPolicy":{"configMap":"policy"}}`, "Talos reads no ConfigMap"},
		"an unknown role":            {"kubeadm", `{"files":[{"path":"/etc/x","content":"x","roles":["etcd"]}]}`, "etcd"},
		"an unknown command role":    {"kubeadm", `{"commands":[{"run":"true","roles":["master"]}]}`, "master"},

		"anonymous auth":          {"kubeadm", `{"apiServerArgs":[{"name":"anonymous-auth","value":"true"}]}`, "anonymous-auth is an argument Kubehenge sets for the API server"},
		"authorization mode":      {"rke2", `{"apiServerArgs":[{"name":"authorization-mode","value":"AlwaysAllow"}]}`, "authorization-mode is an argument Kubehenge sets"},
		"oidc":                    {"k3s", `{"apiServerArgs":[{"name":"oidc-issuer-url","value":"x"}]}`, "oidc-issuer-url is an argument Kubehenge sets"},
		"the cis profile":         {"kubeadm", `{"kubeletArgs":[{"name":"seccomp-default","value":"false"}]}`, "seccomp-default is an argument Kubehenge sets for the kubelet"},
		"protect kernel defaults": {"talos", `{"kubeletArgs":[{"name":"protect-kernel-defaults","value":"false"}]}`, "protect-kernel-defaults"},
		"node labels":             {"rke2", `{"kubeletArgs":[{"name":"node-labels","value":"a=b"}]}`, "node-labels"},
		"a leader election flag":  {"kubeadm", `{"controllerManagerArgs":[{"name":"leader-elect-lease-duration","value":"5s"}]}`, "leader-elect-lease-duration"},
		"the scheduler's kubecfg": {"kubeadm", `{"schedulerArgs":[{"name":"kubeconfig","value":"x"}]}`, "kubeconfig is an argument Kubehenge sets for the scheduler"},
		"the audit log path":      {"kubeadm", `{"apiServerArgs":[{"name":"audit-log-path","value":"-"}]}`, "Use auditPolicy"},
		"the audit policy flag":   {"rke2", `{"auditPolicy":{"configMap":"policy"},"apiServerArgs":[{"name":"audit-policy-file","value":"/x"}]}`, "Use auditPolicy"},
		"the webhook flag":        {"kubeadm", `{"apiServerArgs":[{"name":"audit-webhook-config-file","value":"/x"}]}`, "Use auditWebhook"},
		"rotation without audit":  {"kubeadm", `{"apiServerArgs":[{"name":"audit-log-maxage","value":"7"}]}`, "audit-log-maxage is an argument Kubehenge sets"},

		"secret in another namespace": {"kubeadm", `{"files":[{"path":"/etc/x","secret":{"name":"elsewhere","key":"ca"}}]}`, "Secret elsewhere is not in namespace team-a"},
		"missing secret":              {"rke2", `{"auditWebhook":{"secret":"nothing"}}`, "Secret nothing is not in namespace team-a"},
		"missing secret key":          {"kubeadm", `{"files":[{"path":"/etc/x","secret":{"name":"siem","key":"nokey"}}]}`, "Secret siem has no key nokey"},
		"webhook default key":         {"k3s", `{"auditWebhook":{"secret":"elsewhere"}}`, "Secret elsewhere is not in namespace team-a"},
		"webhook secret key":          {"kubeadm", `{"auditWebhook":{"secret":"siem","key":"nokey"}}`, "Secret siem has no key nokey"},
		"configmap in another ns":     {"kubeadm", `{"auditPolicy":{"configMap":"theirs"}}`, "ConfigMap theirs is not in namespace team-a"},
		"configmap without the key":   {"kubeadm", `{"auditPolicy":{"configMap":"wrongkey"}}`, "ConfigMap wrongkey has no key policy.yaml"},
		"a policy without rules":      {"kubeadm", `{"auditPolicy":{"configMap":"empty"}}`, "no rules"},
		"not a policy":                {"kubeadm", `{"auditPolicy":{"configMap":"notpolicy"}}`, "Policy of audit.k8s.io/v1"},
	} {
		c := cluster(variableOf(NodeCustomisationVariable, tc.value))
		c.Spec.Topology.ClassRef.Name = ClassPrefix + tc.class
		errs := (&ClusterValidator{Hooks: &Hooks{Reader: reader}}).validateNodeCustomisation(context.Background(), nil, c)
		switch {
		case tc.want == "" && len(errs) > 0:
			t.Errorf("%s: %v", name, errs)
		case tc.want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, errs, tc.want)
		}
	}
}

func TestNodeCustomisationIsCheckedOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	value := `{"files":[{"path":"/etc/x","secret":{"name":"gone","key":"k"}}]}`
	old := cluster(variableOf(NodeCustomisationVariable, value))
	old.Spec.Topology.ClassRef.Name = ClassPrefix + "kubeadm"
	now := old.DeepCopy()
	v := &ClusterValidator{Hooks: &Hooks{Reader: nodeReader(t)}}
	if errs := v.validateNodeCustomisation(context.Background(), old, now); len(errs) > 0 {
		t.Errorf("a variable that did not change was checked again: %v", errs)
	}
	if errs := v.validateNodeCustomisation(context.Background(), nil, now); len(errs) == 0 {
		t.Error("a new cluster with a missing Secret was let through")
	}
}

func TestEveryProtectedFlagIsRefusedByItsComponent(t *testing.T) {
	t.Parallel()
	for component, flags := range protectedFlags {
		for flag := range flags {
			if !ProtectedNodeFlag(component, flag) {
				t.Errorf("%s %s", component, flag)
			}
		}
	}
	if ProtectedNodeFlag(componentKubelet, "event-qps") || ProtectedNodeFlag(componentAPIServer, "request-timeout") {
		t.Error("an argument Kubehenge leaves alone is refused")
	}
}

func TestReconcileMirrorsTheAuditPolicyConfigMap(t *testing.T) {
	t.Parallel()
	c := cluster(variableOf(NodeCustomisationVariable, `{"auditPolicy":{"configMap":"policy"}}`))
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "rke2"
	cm := &corev1.ConfigMap{Namespace: "team-a", Name: "policy", Data: map[string]string{"policy.yaml": validAuditPolicy}}
	r, kube := newReconciler(t, &fakeCloud{}, c, cm)
	r.Reader = kube
	if err := r.ensureAuditPolicy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		s := &corev1.Secret{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-audit-policy"}, s); err != nil {
			t.Fatal(err)
		}
		if len(s.OwnerReferences) != 1 {
			t.Errorf("the mirror outlives its cluster: %v", s.OwnerReferences)
		}
		return string(s.Data["audit-policy.yaml"])
	}
	if got := read(); got != validAuditPolicy {
		t.Errorf("mirrored %q", got)
	}
	cm.Data["policy.yaml"] = strings.ReplaceAll(validAuditPolicy, "Metadata", "RequestResponse")
	if err := kube.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAuditPolicy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := read(); !strings.Contains(got, "RequestResponse") {
		t.Errorf("the mirror did not follow the ConfigMap: %q", got)
	}
}
