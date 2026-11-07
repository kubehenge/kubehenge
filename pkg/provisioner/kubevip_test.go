package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const bgpTestPassword = "s3cret-pass"

// signedBGP is a loadBalancer variable in BGP mode whose sessions carry a password.
func signedBGP() string {
	return `{"enabled":true,"mode":"bgp","addresses":["198.51.100.0/28"],"bgp":{"asn":65001,"authenticated":true,"peers":[` +
		peerOf("10.0.0.1", "64512") + `,` + peerOf("10.0.1.1", "64513") + `]}}`
}

// bgpCredential is the web cluster's bgp credential holding this password.
func bgpCredential(password string) *corev1.Secret {
	return &corev1.Secret{
		Namespace: "team-a", Name: CredentialName("web", bgpPurpose),
		Labels: map[string]string{CredentialLabel: bgpPurpose, CredentialClusterLabel: "web"},
		Type:   CredentialType,
		Data:   map[string][]byte{passwordKey: []byte(password)},
	}
}

func TestBGPIsACredentialPurposeOfACluster(t *testing.T) {
	t.Parallel()
	p, ok := CredentialPurposeNamed(bgpPurpose)
	if !ok || !slices.Equal(p.Keys, []string{passwordKey}) || p.Appliance {
		t.Errorf("purpose %+v", p)
	}
}

func TestKubeVipStaticPodSignsEverySession(t *testing.T) {
	t.Parallel()
	bgp := &BGP{ASN: 65001, Authenticated: true, Peers: []BGPPeer{{Address: "10.0.0.1", ASN: 64512}, {Address: "10.0.1.1", ASN: 64513}}}
	for _, tc := range []struct {
		d    templates.Distribution
		host string
	}{
		{templates.RKE2, "/etc/rancher/rke2/rke2.yaml"}, {templates.K3s, "/etc/rancher/k3s/k3s.yaml"}, {templates.Kubeadm, "/etc/kubernetes/admin.conf"},
	} {
		d, host := tc.d, tc.host
		t.Run(string(d), func(t *testing.T) {
			t.Parallel()
			out, err := KubeVipStaticPod(kubeVipImage(testCharts.KubeVipImage), kubeVipHostKubeconfig(d), "10.0.0.5", bgp, bgpTestPassword)
			if err != nil {
				t.Fatal(err)
			}
			var pod corev1.Pod
			if err := yaml.UnmarshalStrict(out, &pod); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if want := "10.0.0.1:64512:" + bgpTestPassword + ":false,10.0.1.1:64513:" + bgpTestPassword + ":false"; env["bgp_peers"] != want {
				t.Errorf("bgp_peers %q, want %q", env["bgp_peers"], want)
			}
			if env["address"] != "10.0.0.5" || env["vip_arp"] != falseText || env["bgp_enable"] != trueText || env["bgp_as"] != "65001" {
				t.Errorf("env %v", env)
			}
			if got := pod.Spec.Volumes[0].HostPath.Path; got != host {
				t.Errorf("kubeconfig %q, want %q", got, host)
			}
			if kubeadm := d == templates.Kubeadm; kubeadm != strings.Contains(string(out), "path: /etc/kubernetes/admin.conf") {
				t.Errorf("the line kube-vip-credentials.sh rewrites is missing or on the wrong distribution:\n%s", out)
			}
		})
	}
}

func TestReconcileKeepsTheBGPPasswordOutOfTheClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", signedBGP())
	c.Namespace = "team-a"
	r, kube := newReconciler(t, &fakeCloud{}, c, bgpCredential(bgpTestPassword))
	lb, _, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
	if err != nil {
		t.Fatal(err)
	}

	if held, err := r.reconcileKubeVip(ctx, c, lb); err != nil || held {
		t.Fatalf("held %v, %v", held, err)
	}
	s := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: KubeVipSecretName("web")}, s); err != nil {
		t.Fatal(err)
	}
	manifest := string(s.Data[kubeVipManifestKey])
	if !strings.Contains(manifest, "10.0.0.1:64512:"+bgpTestPassword+":false") || !strings.Contains(manifest, "/etc/rancher/rke2/rke2.yaml") {
		t.Errorf("manifest:\n%s", manifest)
	}

	// Authentication switched off takes the Secret, and the password with it.
	unsigned := lb
	unsigned.BGP = &BGP{ASN: lb.BGP.ASN, Peers: lb.BGP.Peers}
	if held, err := r.reconcileKubeVip(ctx, c, unsigned); err != nil || held {
		t.Fatalf("held %v, %v", held, err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(s), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Secret stays: %v", err)
	}
}

func TestReconcileHoldsTheClusterUntilTheBGPPasswordIsUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wrongType := bgpCredential(bgpTestPassword)
	wrongType.Type = corev1.SecretTypeOpaque
	for name, objs := range map[string][]client.Object{
		"no credential":    nil,
		"empty password":   {bgpCredential("")},
		"colon":            {bgpCredential("a:b")},
		"comma":            {bgpCredential("a,b")},
		"space":            {bgpCredential("a b")},
		"not a credential": {wrongType},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", signedBGP())
			c.Namespace = "team-a"
			r, kube := newReconciler(t, &fakeCloud{}, append([]client.Object{c}, objs...)...)
			lb, _, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
			if err != nil {
				t.Fatal(err)
			}
			if held, err := r.reconcileKubeVip(ctx, c, lb); err != nil || !held {
				t.Fatalf("held %v, %v", held, err)
			}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: KubeVipSecretName("web")}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Errorf("a manifest was written: %v", err)
			}
		})
	}
}

func TestMetalLBPeersNameTheSharedPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", signedBGP())
	c.Namespace = "team-a"
	r, _ := newReconciler(t, &fakeCloud{}, c, bgpCredential(bgpTestPassword))
	workload := newLBWorkload(t, &corev1.Namespace{Name: metalLBNamespace})
	workload.install()
	lb, _, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := parseAddresses(lb.Addresses)
	if err != nil {
		t.Fatal(err)
	}

	if waiting, err := r.applyMetalLB(ctx, c, workload, spans, lb); err != nil || waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	s := &corev1.Secret{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: metalLBNamespace, Name: bgpPeerSecret}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data[passwordKey]) != bgpTestPassword || s.Type != corev1.SecretTypeBasicAuth {
		t.Errorf("Secret %q of type %s", s.Data[passwordKey], s.Type)
	}
	for _, o := range metalLBPool(spans, lb) {
		if o.GetKind() != bgpPeerKind {
			continue
		}
		if got := lookup[map[string]any](t, o.Object, "spec", "passwordSecret"); got["name"] != bgpPeerSecret || got["namespace"] != metalLBNamespace {
			t.Errorf("peer %s: passwordSecret %v", o.GetName(), got)
		}
	}
	// The password itself is in no object MetalLB or the chart is given.
	for _, o := range metalLBPool(spans, lb) {
		if strings.Contains(string(mustJSON(t, o.Object)), bgpTestPassword) {
			t.Errorf("%s holds the password", o.GetName())
		}
	}
	if strings.Contains(string(mustJSON(t, metalLBValues(lb))), bgpTestPassword) {
		t.Error("MetalLB's chart values hold the password")
	}

	// Authentication off: the peers name no Secret and it is deleted.
	lb.BGP.Authenticated = false
	if waiting, err := r.applyMetalLB(ctx, c, workload, spans, lb); err != nil || waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	if err := workload.Get(ctx, client.ObjectKeyFromObject(s), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Secret stays: %v", err)
	}
	for _, o := range metalLBPool(spans, lb) {
		if _, has := lookup[map[string]any](t, o.Object, "spec")["passwordSecret"]; has {
			t.Errorf("peer %s still names a password", o.GetName())
		}
	}
}

// Unticking "Use a BGP password" takes the password out of both Secrets and the peers, through a
// whole reconcile of the cluster.
func TestReconcileRemovesTheBGPPasswordWhenTheTickGoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", signedBGP())
	c.Namespace = "team-a"
	withAddons(c, map[string]string{"storage": `"local-lvm"`})
	r, kube := newReconciler(t, &fakeCloud{}, c, bgpCredential(bgpTestPassword))
	workload := newLBWorkload(t, &corev1.Namespace{Name: metalLBNamespace})
	workload.install()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	run := func() {
		t.Helper()
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
	}
	missing := func(cl client.Client, key client.ObjectKey) bool {
		return apierrors.IsNotFound(cl.Get(ctx, key, &corev1.Secret{}))
	}
	manifest := client.ObjectKey{Namespace: "team-a", Name: KubeVipSecretName("web")}
	peers := client.ObjectKey{Namespace: metalLBNamespace, Name: bgpPeerSecret}

	run()
	if missing(kube, manifest) || missing(workload, peers) {
		t.Fatal("a signed cluster has no Secret to keep its password in")
	}
	setLoadBalancer(t, kube, c, strings.Replace(signedBGP(), `"authenticated":true,`, "", 1))
	run()
	if !missing(kube, manifest) {
		t.Error("the kube-vip manifest, with the password, stays")
	}
	if !missing(workload, peers) {
		t.Error("MetalLB's password Secret stays")
	}
	names := workload.announcing(bgpPeerKind)
	if len(names) == 0 {
		t.Error("the peers are gone")
	}
	for _, name := range names {
		o := workload.announcers[bgpPeerKind+"/"+name]
		if _, has := lookup[map[string]any](t, o.Object, "spec")["passwordSecret"]; has {
			t.Errorf("peer %s still names a password", name)
		}
	}
}

func TestMetalLBWaitsForTheBGPPasswordBeforeItsPeers(t *testing.T) {
	t.Parallel()
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", signedBGP())
	c.Namespace = "team-a"
	r, _ := newReconciler(t, &fakeCloud{}, c)
	workload := newLBWorkload(t)
	workload.install()
	lb, _, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
	if err != nil {
		t.Fatal(err)
	}
	spans, _ := parseAddresses(lb.Addresses)
	if waiting, err := r.applyMetalLB(context.Background(), c, workload, spans, lb); err != nil || !waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	if got := workload.announcing(bgpPeerKind); len(got) != 0 {
		t.Errorf("peers %v name a password that is not there", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
