package provisioner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestCiliumHasTheClusterNameAndIDFromTheStart(t *testing.T) {
	t.Parallel()
	cilium := CNI{Name: cniCilium, Pods: "10.42.0.0/16", ClusterID: 3}
	_, values := proxySpecs(t, templates.K3s, cilium, &Addons{})
	agent := values["web-cni"]
	if lookup[string](t, agent, "cluster", "name") != "web" || lookup[float64](t, agent, "cluster", "id") != 3 {
		t.Errorf("Cilium does not know the cluster as web, id 3: %v", agent)
	}
	if _, ok := agent["clustermesh"]; ok {
		t.Errorf("a cluster outside the mesh runs a clustermesh-apiserver: %v", agent)
	}
	if !lookup[bool](t, agent, "rollOutCiliumPods") || !lookup[bool](t, agent, "operator", "rollOutPods") || lookup[float64](t, agent, "operator", "replicas") != 1 {
		t.Errorf("Cilium's agents or operator keep running on a configuration they no longer have: %v", agent)
	}
	if lookup[string](t, agent, "cni", "chainingMode") != "portmap" {
		t.Errorf("Cilium serves no host ports: %v", agent)
	}

	_, meshed := proxySpecs(t, templates.K3s, cilium, &Addons{ClusterMesh: ClusterMesh{Enabled: true}})
	mesh := meshed["web-cni"]
	if !lookup[bool](t, mesh, "clustermesh", "useAPIServer") {
		t.Fatalf("Cluster Mesh on, no clustermesh-apiserver: %v", mesh)
	}
	if lookup[string](t, mesh, "clustermesh", "apiserver", "service", "type") != "NodePort" ||
		lookup[float64](t, mesh, "clustermesh", "apiserver", "service", "nodePort") != meshPort {
		t.Errorf("peers cannot reach the clustermesh-apiserver on port %d: %v", meshPort, mesh)
	}
	if lookup[string](t, mesh, "clustermesh", "apiserver", "tls", "authMode") != "cluster" || lookup[bool](t, mesh, "clustermesh", "apiserver", "tls", "auto", "enabled") {
		t.Errorf("peers share one user, or the chart makes a CA of its own: %v", mesh)
	}
	if _, ok := lookup[map[string]any](t, mesh, "clustermesh")["config"]; ok {
		t.Errorf("peers go through the chart's values, so that changing them upgrades Cilium: %v", mesh)
	}

	long := cluster()
	long.Name = strings.Repeat("a", MaxCiliumClusterName+1)
	if _, err := helmChartProxies(long, &testCharts, templates.K3s, "local-lvm", cilium, &Addons{}); err == nil {
		t.Error("Cilium given a cluster name longer than it takes")
	}
}

// meshCluster is a kubeadm cluster of team-a that runs Cilium, with the mesh variable given.
func meshCluster(name string, id int, endpoint, pods, mesh string) *clusterv1.Cluster {
	c := cluster(
		clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}},
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}},
	)
	c.Name, c.UID = name, types.UID("uid-"+name)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	c.Spec.ControlPlaneEndpoint.Host = endpoint
	if pods != "" {
		c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{pods}
	}
	if id > 0 {
		c.Annotations = map[string]string{CiliumClusterIDAnnotation: strconv.Itoa(id)}
	}
	if mesh != "" {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: clusterMeshVariable, Value: apiextensionsv1.JSON{Raw: []byte(mesh)}})
	}
	return c
}

func TestReconcileGivesEveryCiliumClusterAnIDOfItsNamespace(t *testing.T) {
	t.Parallel()
	a, b := meshCluster("a", 0, "", "", ""), meshCluster("b", 0, "", "", "")
	taken := meshCluster("taken", 1, "", "", "")
	other := meshCluster("a", 0, "", "", "")
	other.Namespace, other.UID = "team-b", "uid-team-b-a"
	r, kube := newReconciler(t, &fakeCloud{}, a, b, taken, other)
	ids := map[string]string{}
	for _, c := range []*clusterv1.Cluster{a, b, other, a} {
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		got := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		ids[c.Namespace+"/"+c.Name] = got.Annotations[CiliumClusterIDAnnotation]
	}
	if want := map[string]string{"team-a/a": "2", "team-a/b": "3", "team-b/a": "1"}; !maps.Equal(ids, want) {
		t.Errorf("Cilium cluster ids %v, want %v", ids, want)
	}

	canal := cluster(clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}})
	canal.Name = "canal"
	if err := kube.Create(context.Background(), canal); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, canal); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(canal), canal); err != nil || canal.Annotations[CiliumClusterIDAnnotation] != "" {
		t.Errorf("a cluster without Cilium was given a Cilium cluster id: %v %v", canal.Annotations, err)
	}
}

func TestRKE2sOwnCiliumGetsTheClusterNameIDAndMesh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rke2 := meshCluster("rke2", 0, "", "", `{"enabled":true}`)
	rke2.Spec.Topology.ClassRef.Name = "kubehenge-rke2"
	rke2.Spec.Topology.Variables = append(rke2.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "istio", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	kubeadm := meshCluster("kubeadm", 1, "", "", `{"enabled":true}`)
	r, kube := newReconciler(t, &fakeCloud{}, rke2, kubeadm)
	r.Remotes = &Remotes{Reader: kube}
	for _, c := range []*clusterv1.Cluster{rke2, kubeadm} {
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(rke2), rke2); err != nil || rke2.Annotations[CiliumClusterIDAnnotation] != "2" {
		t.Errorf("RKE2's Cilium was given no Cilium cluster id of its own: %v %v", rke2.Annotations, err)
	}

	s := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: rke2.Namespace, Name: rke2CiliumName(rke2)}, s); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(s.Data["rke2-cilium.yaml"], &config); err != nil {
		t.Fatal(err)
	}
	if config["kind"] != "HelmChartConfig" || lookup[string](t, config, "metadata", "name") != "rke2-cilium" || lookup[string](t, config, "metadata", "namespace") != "kube-system" {
		t.Fatalf("not RKE2's Cilium chart configured: %v", config)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, config, "spec", "valuesContent")), &values); err != nil {
		t.Fatal(err)
	}
	if lookup[string](t, values, "cluster", "name") != "rke2" || lookup[float64](t, values, "cluster", "id") != 2 ||
		!lookup[bool](t, values, "clustermesh", "useAPIServer") || lookup[bool](t, values, "cni", "exclusive") || !lookup[bool](t, values, "rollOutCiliumPods") {
		t.Errorf("RKE2's Cilium is not the cluster's, in the mesh, beside istio-cni: %v", values)
	}
	if _, ok := values["ipam"]; ok {
		t.Errorf("RKE2's own pod addressing is overridden: %v", values)
	}
	crs := &addonsv1.ClusterResourceSet{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: rke2.Namespace, Name: rke2.Name + "-proxmox"}, crs); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(crs.Spec.Resources, addonsv1.ResourceRef{Kind: secretKind, Name: rke2CiliumName(rke2)}) {
		t.Errorf("the configuration is not applied to the cluster: %v", crs.Spec.Resources)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: kubeadm.Namespace, Name: rke2CiliumName(kubeadm)}, s); !apierrors.IsNotFound(err) {
		t.Errorf("a cluster with Kubehenge's Cilium got RKE2's configuration: %v", err)
	}
}

func TestMeshPeersLinkOnlyWhatBothSidesAgreeOn(t *testing.T) {
	t.Parallel()
	self := meshCluster("a", 1, "10.0.0.10", "10.244.0.0/16", `{"enabled":true,"peers":["b","c","d","e","f","g","h","i","missing"]}`)
	calico := meshCluster("e", 5, "10.0.0.14", "10.5.0.0/16", `{"enabled":true,"peers":["a"]}`)
	calico.Spec.Topology.Variables[1].Value.Raw = []byte(`"calico"`)
	gone := meshCluster("i", 9, "10.0.0.18", "10.9.0.0/16", `{"enabled":true,"peers":["a"]}`)
	gone.DeletionTimestamp = new(metav1.Now())
	elsewhere := meshCluster("missing", 10, "10.0.0.19", "10.10.0.0/16", `{"enabled":true,"peers":["a"]}`)
	elsewhere.Namespace = "team-b"
	services := meshCluster("j", 11, "10.0.0.20", "10.96.0.0/16", `{"enabled":true,"peers":["a"]}`)
	services.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.11.0.0/16"}
	clusters := []clusterv1.Cluster{
		*self,
		*meshCluster("b", 2, "10.0.0.11", "10.2.0.0/16", `{"enabled":true,"peers":["a"]}`),
		*meshCluster("c", 3, "10.0.0.12", "10.3.0.0/16", `{"enabled":true,"peers":["b"]}`),
		*meshCluster("d", 4, "10.0.0.13", "10.4.0.0/16", `{"enabled":false,"peers":["a"]}`),
		*calico,
		*meshCluster("f", 1, "10.0.0.15", "10.6.0.0/16", `{"enabled":true,"peers":["a"]}`),
		*meshCluster("g", 7, "10.0.0.16", "", `{"enabled":true,"peers":["a"]}`),
		*meshCluster("h", 8, "", "10.8.0.0/16", `{"enabled":true,"peers":["a"]}`),
		*gone,
		*elsewhere,
		*services,
	}
	self.Spec.Topology.Variables[2].Value.Raw = []byte(`{"enabled":true,"peers":["b","c","d","e","f","g","h","i","j","missing","b"]}`)
	classes := []clusterv1.ClusterClass{{
		Name:        "kubehenge-kubeadm",
		Annotations: map[string]string{DefaultPodsAnnotation: "10.244.0.0/16", DefaultServicesAnnotation: "10.96.0.0/16"},
	}}
	got := map[string]string{}
	for _, p := range MeshPeers(self, clusters, classes) {
		got[p.Name] = p.Refused
	}
	want := map[string]string{
		"b":       "",
		"c":       "it does not list this cluster",
		"d":       "Cluster Mesh is off there",
		"e":       "it does not run Cilium",
		"f":       "both have Cilium cluster id 1",
		"g":       "the pod ranges 10.244.0.0/16 and 10.244.0.0/16 overlap",
		"h":       "waiting for a control-plane endpoint",
		"i":       "no cluster of that name in the namespace",
		"j":       "this cluster's service range 10.96.0.0/16 overlaps its pod range 10.96.0.0/16",
		"missing": "no cluster of that name in the namespace",
	}
	if !maps.Equal(got, want) {
		t.Errorf("links:\n got %v\nwant %v", got, want)
	}
}

func TestMeshPeersOfLooksAgainAtWhatAChangeLinksOrUnlinks(t *testing.T) {
	t.Parallel()
	a := meshCluster("a", 1, "", "", `{"enabled":true,"peers":["b"]}`)
	b := meshCluster("b", 2, "", "", `{"enabled":true,"peers":["c"]}`)
	c := meshCluster("c", 3, "", "", `{"enabled":true,"peers":["a"]}`)
	off := meshCluster("off", 4, "", "", `{"enabled":false,"peers":["b"]}`)
	r, _ := newReconciler(t, &fakeCloud{}, a, b, c, off)
	requests := r.meshPeersOf(context.Background(), b)
	names := make([]string, 0, len(requests))
	for _, req := range requests {
		names = append(names, req.Name)
	}
	if strings.Join(names, ",") != "a,c" {
		t.Errorf("a change to b looks again at %v, want a (listing b) and c (listed by b)", names)
	}
}

// meshWritten is a workload cluster a's Cluster Mesh objects were written into, linked with b and
// c, by a CA of team-a.
type meshWritten struct {
	workload client.Client
	ca       *meshCA
	links    []meshLink
	now      time.Time
}

func writtenMesh(t *testing.T) *meshWritten {
	t.Helper()
	now := time.Now()
	certPEM, keyPEM, err := newMeshCA("team-a", now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := parseMeshCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	m := &meshWritten{fake.NewClientBuilder().WithScheme(remoteScheme).Build(), ca, []meshLink{{"c", "10.0.0.12"}, {"b", "10.0.0.11"}}, now}
	m.write(t, "10.0.0.10", m.links, now)
	return m
}

func (m *meshWritten) write(t *testing.T, endpoint string, links []meshLink, now time.Time) {
	t.Helper()
	if err := writeMesh(context.Background(), m.workload, "a", endpoint, m.ca, links, now); err != nil {
		t.Fatal(err)
	}
}

func (m *meshWritten) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := m.workload.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: name}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (m *meshWritten) server(t *testing.T) *x509.Certificate {
	t.Helper()
	s := m.secret(t, "clustermesh-apiserver-server-cert")
	pair, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	return pair.Leaf
}

func TestWriteMeshSignsEachCertificateWithTheNamespacesCA(t *testing.T) {
	t.Parallel()
	m := writtenMesh(t)
	ca := m.ca
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	for name, want := range map[string]struct {
		cn    string
		usage x509.ExtKeyUsage
	}{
		"clustermesh-apiserver-server-cert": {meshServer, x509.ExtKeyUsageServerAuth},
		"clustermesh-apiserver-admin-cert":  {"admin-a", x509.ExtKeyUsageClientAuth},
		"clustermesh-apiserver-local-cert":  {"local-a", x509.ExtKeyUsageClientAuth},
		"clustermesh-apiserver-remote-cert": {"remote-a", x509.ExtKeyUsageClientAuth},
	} {
		s := m.secret(t, name)
		pair, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(s.Data["ca.crt"], ca.certPEM) || s.Type != corev1.SecretTypeTLS {
			t.Errorf("%s does not carry the namespace's CA", name)
		}
		if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{want.usage}}); err != nil || pair.Leaf.Subject.CommonName != want.cn {
			t.Errorf("%s: %s, %v; want %s", name, pair.Leaf.Subject.CommonName, err, want.cn)
		}
	}
	server := m.server(t)
	for _, host := range []string{"10.0.0.10", meshServer, "127.0.0.1"} {
		if err := server.VerifyHostname(host); err != nil {
			t.Errorf("the server's certificate does not name %s: %v", host, err)
		}
	}
	if err := server.VerifyHostname("10.0.0.11"); err == nil {
		t.Error("the server's certificate names a peer's endpoint")
	}

	// Rewritten, a certificate stays until a third of its life is left. Then it is renewed.
	before := m.secret(t, "clustermesh-apiserver-remote-cert").Data["tls.crt"]
	m.write(t, "10.0.0.10", m.links, m.now.Add(time.Hour))
	if !bytes.Equal(m.secret(t, "clustermesh-apiserver-remote-cert").Data["tls.crt"], before) {
		t.Error("a certificate far from its end was replaced")
	}
	m.write(t, "10.0.0.10", m.links, m.now.Add(250*24*time.Hour))
	if bytes.Equal(m.secret(t, "clustermesh-apiserver-remote-cert").Data["tls.crt"], before) {
		t.Error("a certificate with less than a third of its life left was kept")
	}
	m.write(t, "10.0.0.20", m.links, m.now.Add(250*24*time.Hour))
	if m.server(t).VerifyHostname(net.ParseIP("10.0.0.20").String()) != nil {
		t.Error("the server's certificate does not follow the endpoint")
	}
}

func TestWriteMeshLinksThePeers(t *testing.T) {
	t.Parallel()
	m := writtenMesh(t)
	ctx := context.Background()
	kvstoremesh := m.secret(t, "cilium-kvstoremesh").Data
	var b map[string]any
	if err := yaml.Unmarshal(kvstoremesh["b"], &b); err != nil {
		t.Fatal(err)
	}
	if lookup[string](t, b, "endpoints", 0) != "https://10.0.0.11:32379" || b["cert-file"] != "/var/lib/cilium/clustermesh/common-etcd-client.crt" {
		t.Errorf("KVStoreMesh does not reach b on its endpoint as this cluster's remote user: %v", b)
	}
	agents := m.secret(t, "cilium-clustermesh").Data
	var agentB map[string]any
	if err := yaml.Unmarshal(agents["b"], &agentB); err != nil {
		t.Fatal(err)
	}
	if lookup[string](t, agentB, "endpoints", 0) != "https://clustermesh-apiserver.kube-system.svc:2379" || agentB["key-file"] != "/var/lib/cilium/clustermesh/local-etcd-client.key" {
		t.Errorf("the agents do not read b through the local KVStoreMesh: %v", agentB)
	}
	if len(kvstoremesh) != 2 || len(agents) != 2 {
		t.Errorf("peers %v and %v, want b and c", kvstoremesh, agents)
	}
	users := &corev1.ConfigMap{}
	if err := m.workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "clustermesh-remote-users"}, users); err != nil {
		t.Fatal(err)
	}
	if want := "users:\n- name: remote-b\n  role: remote\n- name: remote-c\n  role: remote\n"; users.Data["users.yaml"] != want {
		t.Errorf("users %q, want %q", users.Data["users.yaml"], want)
	}

	// A peer unlinked goes, its user too.
	m.write(t, "10.0.0.10", m.links[1:], m.now)
	if _, ok := m.secret(t, "cilium-kvstoremesh").Data["c"]; ok {
		t.Error("an unlinked peer is still read")
	}
	if err := m.workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "clustermesh-remote-users"}, users); err != nil || strings.Contains(users.Data["users.yaml"], "remote-c") {
		t.Errorf("an unlinked peer still signs in: %q %v", users.Data["users.yaml"], err)
	}
}

func TestRemoveMeshTakesOutWhatWasWritten(t *testing.T) {
	t.Parallel()
	m := writtenMesh(t)
	ctx := context.Background()
	workload := m.workload
	policy := &networkingv1.NetworkPolicy{}
	if err := writeMeshPolicy(ctx, workload); err != nil {
		t.Fatal(err)
	}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: meshPolicy}, policy); err != nil {
		t.Fatal(err)
	}
	if rules := policy.Spec.Ingress; policy.Spec.PodSelector.MatchLabels["k8s-app"] != "clustermesh-apiserver" ||
		len(rules) != 1 || len(rules[0].From) != 0 || len(rules[0].Ports) != 1 || rules[0].Ports[0].Port.IntValue() != meshAPIPort {
		t.Errorf("peers are not let through to the clustermesh-apiserver's port from anywhere: %+v", policy.Spec)
	}

	if err := removeMesh(ctx, workload); err != nil {
		t.Fatal(err)
	}
	left := &corev1.SecretList{}
	if err := workload.List(ctx, left); err != nil || len(left.Items) != 0 {
		t.Errorf("left behind: %v %v", left.Items, err)
	}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: meshPolicy}, policy); !apierrors.IsNotFound(err) {
		t.Errorf("the NetworkPolicy was left behind: %v", err)
	}
	if err := removeMesh(ctx, workload); err != nil {
		t.Errorf("removing again: %v", err)
	}
}

func TestValidateClusterMesh(t *testing.T) {
	t.Parallel()
	long := meshCluster(strings.Repeat("a", MaxCiliumClusterName+1), 0, "", "", "")
	calico := meshCluster("a", 0, "", "", `{"enabled":true}`)
	calico.Spec.Topology.Variables[1].Value.Raw = []byte(`"calico"`)
	withID := meshCluster("a", 2, "", "", "")
	moved := meshCluster("a", 3, "", "", "")
	for _, tc := range []struct {
		name     string
		old, new *clusterv1.Cluster
		want     string
	}{
		{"a new Cilium cluster with a long name", nil, long, "may not be more than 32"},
		{"an existing one", long, long, ""},
		{"Cluster Mesh beside Calico", nil, calico, "Cluster Mesh needs Cilium, and this cluster runs calico"},
		{"listing itself", nil, meshCluster("a", 0, "", "", `{"enabled":true,"peers":["a"]}`), "cannot list itself"},
		{"a Cilium cluster id changed", withID, moved, "cannot change once set"},
		{"a Cilium cluster id given", meshCluster("a", 0, "", "", ""), withID, ""},
		{"meshed", nil, meshCluster("a", 0, "", "", `{"enabled":true,"peers":["b"]}`), ""},
	} {
		errs := validateClusterMesh(tc.old, tc.new)
		if got := errs.ToAggregate(); (got == nil) != (tc.want == "") || got != nil && !strings.Contains(got.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, got, tc.want)
		}
	}
}
