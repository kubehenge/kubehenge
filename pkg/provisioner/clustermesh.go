package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// ClusterMesh is a cluster's clusterMesh variable: Cilium Cluster Mesh, with the clusters of its
// namespace it links with. A link holds only while both list each other.
type ClusterMesh struct {
	Enabled bool     `json:"enabled"`
	Peers   []string `json:"peers,omitempty"`
}

const (
	clusterMeshVariable = "clusterMesh"
	// CiliumClusterIDAnnotation holds the Cilium cluster id of a cluster that runs Cilium: unique
	// among the clusters of its namespace, the ones it can link with, and fixed for its life, since
	// changing it means restarting every workload.
	CiliumClusterIDAnnotation = "kubehenge.dev/cilium-cluster-id"
	// MaxCiliumClusterName is the longest cluster name Cilium takes.
	MaxCiliumClusterName = 32
	maxCiliumClusterID   = 255
	// meshWrittenAnnotation marks a cluster Kubehenge wrote Cluster Mesh's objects into, so that
	// they are taken out again once it switches the mesh off.
	meshWrittenAnnotation = "kubehenge.dev/cluster-mesh"
	// meshPort is the clustermesh-apiserver's NodePort, which peers reach on the control-plane
	// endpoint.
	meshPort = 32379
	// meshAPIPort is the clustermesh-apiserver's own port.
	meshAPIPort = 2379
	// meshCASecret holds a namespace's Cluster Mesh CA.
	meshCASecret = "kubehenge-cluster-mesh-ca" //nolint:gosec // a Secret's name, not a credential
	// meshCAValidity is how long the CA is valid. meshCertValidity is how long what it signs is, renewed
	// with 1/meshRenewalShare of it left.
	meshCAValidity   = 10 * 365 * 24 * time.Hour
	meshCertValidity = 365 * 24 * time.Hour
	meshRenewalShare = 3
	// meshServer is the clustermesh-apiserver's name inside its cluster, which the agents dial.
	meshServer = "clustermesh-apiserver.kube-system.svc"
	// The objects Cilium reads Cluster Mesh's peers from: their connection settings for the agents,
	// which reach them through the local KVStoreMesh, and for KVStoreMesh itself, and the users
	// peers sign in to this cluster's clustermesh-apiserver as.
	meshAgentsSecret   = "cilium-clustermesh"
	meshKVStoreMesh    = "cilium-kvstoremesh"
	meshUsersConfigMap = "clustermesh-remote-users"
	meshUsersKey       = "users.yaml"
	meshFiles          = "/var/lib/cilium/clustermesh/"
	// meshCAKey holds, beside a certificate, the CA that signed it, which its peers' share.
	meshCAKey = "ca.crt"
	// meshPolicy lets the peers reach the clustermesh-apiserver where RKE2's CIS profile lets only
	// kube-system's own pods reach the pods there.
	meshPolicy = "kubehenge-clustermesh-apiserver"
)

// runsCilium tells whether the cluster runs Cilium, Kubehenge's or its distribution's own.
func runsCilium(c *clusterv1.Cluster) bool {
	return runsCNI(c, cniCilium)
}

// runsCNI tells whether the cluster runs the CNI named, Kubehenge's or its distribution's own.
func runsCNI(c *clusterv1.Cluster, name string) bool {
	cni, _, _ := optionalVariable[string](c, "cni")
	return cni == name
}

// ciliumIDs hands out Cilium cluster ids one at a time, and remembers those handed out until the
// cache shows them, so that clusters reconciled at once never get the same one.
type ciliumIDs struct {
	mu      sync.Mutex
	pending map[types.UID]ciliumID
}

type ciliumID struct {
	namespace string
	id        int
}

// CiliumClusterID is the Cilium cluster id recorded on a cluster, and whether it has one.
func CiliumClusterID(c *clusterv1.Cluster) (int, bool) {
	id, err := strconv.Atoi(c.Annotations[CiliumClusterIDAnnotation])
	return id, err == nil && id >= 1 && id <= maxCiliumClusterID
}

// ciliumClusterID is the cluster's Cilium cluster id, given to it now if it has none: the lowest
// that no other cluster of its namespace has.
func (r *Reconciler) ciliumClusterID(ctx context.Context, cluster *clusterv1.Cluster) (int, error) {
	if id, ok := CiliumClusterID(cluster); ok {
		return id, nil
	}
	r.ids.mu.Lock()
	defer r.ids.mu.Unlock()
	if p, ok := r.ids.pending[cluster.UID]; ok {
		return p.id, nil
	}
	list := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return 0, fmt.Errorf("listing the clusters of namespace %s: %w", cluster.Namespace, err)
	}
	used := map[int]bool{}
	// Those the cache shows without an id may have been given one it does not show yet.
	without := map[types.UID]bool{}
	for i := range list.Items {
		c := &list.Items[i]
		if id, ok := CiliumClusterID(c); ok {
			used[id] = true
		} else {
			without[c.UID] = true
		}
	}
	for uid, p := range r.ids.pending {
		if p.namespace != cluster.Namespace {
			continue
		}
		if !without[uid] {
			delete(r.ids.pending, uid)
			continue
		}
		used[p.id] = true
	}
	id := 1
	for used[id] {
		id++
	}
	if id > maxCiliumClusterID {
		return 0, fmt.Errorf("every Cilium cluster id, 1 to %d, is taken in namespace %s", maxCiliumClusterID, cluster.Namespace)
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[CiliumClusterIDAnnotation] = strconv.Itoa(id)
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return 0, fmt.Errorf("recording the cluster's Cilium cluster id: %w", err)
	}
	if r.ids.pending == nil {
		r.ids.pending = map[types.UID]ciliumID{}
	}
	r.ids.pending[cluster.UID] = ciliumID{cluster.Namespace, id}
	return id, nil
}

// clusterMeshValues run the clustermesh-apiserver, which peers reach on a NodePort of the
// control-plane endpoint, with KVStoreMesh caching the peers' state for the agents. Kubehenge writes
// its certificates and peers itself, so that the mesh CA's key never goes into values and a peer
// added or taken away restarts nothing.
func clusterMeshValues() map[string]any {
	return map[string]any{
		"useAPIServer": true,
		"apiserver": map[string]any{
			"service":     map[string]any{"type": string(corev1.ServiceTypeNodePort), "nodePort": meshPort},
			"tls":         map[string]any{"authMode": "cluster", "auto": map[string]any{"enabled": false}},
			"resources":   reserved(cpu10m, "32Mi"),
			etcdKey:       map[string]any{"resources": reserved(cpu10m, memory64Mi)},
			"kvstoremesh": map[string]any{"resources": reserved(cpu10m, "32Mi")},
		},
	}
}

// MeshPeer is a cluster another lists in its clusterMesh variable.
type MeshPeer struct {
	Name string `json:"name"`
	// Refused says why the two are not linked. It is empty once they are.
	Refused string `json:"refused,omitempty"`
}

// meshMember is what a cluster's links depend on.
type meshMember struct {
	name, endpoint string
	mesh           ClusterMesh
	cilium         bool
	id             int
	pods, services []string
}

func memberOf(c *clusterv1.Cluster, classes []clusterv1.ClusterClass) meshMember {
	m := meshMember{name: c.Name, endpoint: c.Spec.ControlPlaneEndpoint.Host}
	m.mesh, _, _ = optionalVariable[ClusterMesh](c, clusterMeshVariable)
	m.cilium = runsCilium(c)
	m.id, _ = CiliumClusterID(c)
	m.pods, m.services = ClusterRanges(c, classes)
	return m
}

// ClusterRanges are a cluster's pod and service ranges: its own, or else its class's defaults.
func ClusterRanges(c *clusterv1.Cluster, classes []clusterv1.ClusterClass) ([]string, []string) {
	pods, services := c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks
	i := slices.IndexFunc(classes, func(class clusterv1.ClusterClass) bool { return class.Name == c.Spec.Topology.ClassRef.Name })
	if i < 0 {
		return pods, services
	}
	if def := classes[i].Annotations[DefaultPodsAnnotation]; len(pods) == 0 && def != "" {
		pods = []string{def}
	}
	if def := classes[i].Annotations[DefaultServicesAnnotation]; len(services) == 0 && def != "" {
		services = []string{def}
	}
	return pods, services
}

// MeshPeers says, for each cluster the cluster lists, whether the two are linked: both in one
// namespace and listing each other, both running Cilium with Cluster Mesh on, with Cilium cluster
// ids of their own, control-plane endpoints to reach each other on, and pod ranges apart from each
// other's pods and services, which a packet for a peer's pod would otherwise be taken for. classes
// give the default ranges of clusters that name none.
func MeshPeers(cluster *clusterv1.Cluster, clusters []clusterv1.Cluster, classes []clusterv1.ClusterClass) []MeshPeer {
	self := memberOf(cluster, classes)
	byName := map[string]*clusterv1.Cluster{}
	for i := range clusters {
		c := &clusters[i]
		if c.Namespace == cluster.Namespace && c.Name != cluster.Name && c.DeletionTimestamp.IsZero() {
			byName[c.Name] = c
		}
	}
	names := slices.Compact(slices.Sorted(slices.Values(self.mesh.Peers)))
	peers := make([]MeshPeer, 0, len(names))
	for _, name := range names {
		p := MeshPeer{Name: name, Refused: "no cluster of that name in the namespace"}
		if c, ok := byName[name]; ok {
			p.Refused = meshRefusal(&self, new(memberOf(c, classes)))
		}
		peers = append(peers, p)
	}
	return peers
}

// meshRefusal says why self does not link with peer, or "" when it does.
func meshRefusal(self, peer *meshMember) string {
	switch {
	case !peer.cilium:
		return "it does not run Cilium"
	case !peer.mesh.Enabled:
		return "Cluster Mesh is off there"
	case !slices.Contains(peer.mesh.Peers, self.name):
		return "it does not list this cluster"
	case self.id == 0 || peer.id == 0:
		return "waiting for a Cilium cluster id"
	case self.id == peer.id:
		return fmt.Sprintf("both have Cilium cluster id %d", self.id)
	case self.endpoint == "" || peer.endpoint == "":
		return "waiting for a control-plane endpoint"
	}
	if len(self.pods) == 0 || len(peer.pods) == 0 {
		return "a pod range is unknown"
	}
	for _, c := range []struct {
		what string
		a, b []string
	}{
		{"the pod ranges %s and %s overlap", self.pods, peer.pods},
		{"this cluster's pod range %s overlaps its service range %s", self.pods, peer.services},
		{"this cluster's service range %s overlaps its pod range %s", self.services, peer.pods},
	} {
		if a, b, ok := overlapping(c.a, c.b); ok {
			return fmt.Sprintf(c.what, a, b)
		}
	}
	return ""
}

// overlapping finds a range of a that overlaps one of b. One that is no network overlaps all.
func overlapping(a, b []string) (string, string, bool) {
	for _, x := range a {
		for _, y := range b {
			px, errX := netip.ParsePrefix(x)
			py, errY := netip.ParsePrefix(y)
			if errX != nil || errY != nil || px.Overlaps(py) {
				return x, y, true
			}
		}
	}
	return "", "", false
}

// meshLink is a peer a cluster is linked with, and where its clustermesh-apiserver is.
type meshLink struct {
	name, endpoint string
}

// meshCluster links a cluster that runs Cilium with Cluster Mesh on to its peers, and takes what it
// wrote for the mesh out of a cluster that switched it off. It reports whether it waits for the
// cluster's kubeconfig.
func (r *Reconciler) meshCluster(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) (bool, error) {
	on := runsCilium(cluster) && addons.ClusterMesh.Enabled
	if !on && cluster.Annotations[meshWrittenAnnotation] == "" {
		return false, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil {
		return false, err
	}
	if !on {
		if found {
			if err = removeMesh(ctx, workload.client); err != nil {
				r.Remotes.forget(cluster)
				return false, err
			}
		}
		return false, r.unmarkMesh(ctx, cluster)
	}
	if !found {
		return true, nil
	}
	links, err := r.meshLinks(ctx, cluster)
	if err != nil {
		return false, err
	}
	ca, err := r.meshCA(ctx, cluster.Namespace)
	if err != nil {
		return false, err
	}
	if err = r.markMesh(ctx, cluster); err != nil {
		return false, err
	}
	err = writeMesh(ctx, workload.client, cluster.Name, cluster.Spec.ControlPlaneEndpoint.Host, ca, links, time.Now())
	// RKE2's CIS profile lets only kube-system's own pods reach the pods there.
	if err == nil && distributionOf(cluster) == templates.RKE2 && addons.CIS {
		err = writeMeshPolicy(ctx, workload.client)
	} else if err == nil {
		err = removeFromSystem(ctx, workload.client, &networkingv1.NetworkPolicy{Name: meshPolicy})
	}
	if err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	return false, nil
}

// meshLinks are the peers the cluster is linked with. Each it lists and is not linked with is
// reported on the cluster, with why.
func (r *Reconciler) meshLinks(ctx context.Context, cluster *clusterv1.Cluster) ([]meshLink, error) {
	list := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("listing the clusters of namespace %s: %w", cluster.Namespace, err)
	}
	classes := &clusterv1.ClusterClassList{}
	if err := r.Client.List(ctx, classes, client.InNamespace(r.ClassNamespace)); err != nil {
		return nil, fmt.Errorf("listing the ClusterClasses: %w", err)
	}
	endpoints := map[string]string{}
	for i := range list.Items {
		endpoints[list.Items[i].Name] = list.Items[i].Spec.ControlPlaneEndpoint.Host
	}
	var links []meshLink
	for _, p := range MeshPeers(cluster, list.Items, classes.Items) {
		if p.Refused == "" {
			links = append(links, meshLink{p.Name, endpoints[p.Name]})
			continue
		}
		if r.Events != nil {
			r.Events.Eventf(cluster, nil, corev1.EventTypeNormal, "ClusterMeshNotLinked", "LinkClusterMesh", "not linked with %s: %s", p.Name, p.Refused)
		}
	}
	return links, nil
}

// markMesh records on the cluster that Kubehenge writes Cluster Mesh's objects into it, before it
// does, so that they are found again once it switches the mesh off.
func (r *Reconciler) markMesh(ctx context.Context, cluster *clusterv1.Cluster) error {
	if cluster.Annotations[meshWrittenAnnotation] != "" {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[meshWrittenAnnotation] = strconv.FormatBool(true)
	return r.patchMeshMark(ctx, cluster, patch)
}

// unmarkMesh records on the cluster that Cluster Mesh's objects are out of it.
func (r *Reconciler) unmarkMesh(ctx context.Context, cluster *clusterv1.Cluster) error {
	if cluster.Annotations[meshWrittenAnnotation] == "" {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	delete(cluster.Annotations, meshWrittenAnnotation)
	return r.patchMeshMark(ctx, cluster, patch)
}

func (r *Reconciler) patchMeshMark(ctx context.Context, cluster *clusterv1.Cluster, patch client.Patch) error {
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("recording the cluster's Cluster Mesh objects: %w", err)
	}
	return nil
}

// meshPeersOf are the clusters to look at again when one changes: those of its namespace with
// Cluster Mesh on that list it or that it lists, since a link needs both.
func (r *Reconciler) meshPeersOf(ctx context.Context, obj client.Object) []ctrl.Request {
	changed, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	mesh, _, _ := optionalVariable[ClusterMesh](changed, clusterMeshVariable)
	list := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, list, client.InNamespace(changed.Namespace)); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for i := range list.Items {
		c := &list.Items[i]
		if c.Name == changed.Name {
			continue
		}
		theirs, _, _ := optionalVariable[ClusterMesh](c, clusterMeshVariable)
		if theirs.Enabled && (slices.Contains(theirs.Peers, changed.Name) || slices.Contains(mesh.Peers, c.Name)) {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		}
	}
	return requests
}

// meshCA signs a namespace's Cluster Mesh certificates. Its key stays in the management cluster, so
// that no workload cluster can make another's certificates. Names repeat across namespaces, and a
// cluster's certificates are its name's, so every namespace has a CA of its own, trusted only by its
// clusters.
type meshCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// meshCA is the namespace's CA, made the first time one of its clusters needs it.
func (r *Reconciler) meshCA(ctx context.Context, namespace string) (*meshCA, error) {
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: meshCASecret}, s)
	if apierrors.IsNotFound(err) {
		s, err = r.storeMeshCA(ctx, namespace)
	} else if err != nil {
		err = fmt.Errorf("reading the Cluster Mesh CA of namespace %s: %w", namespace, err)
	}
	if err != nil {
		return nil, err
	}
	return parseMeshCA(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
}

func (r *Reconciler) storeMeshCA(ctx context.Context, namespace string) (*corev1.Secret, error) {
	certPEM, keyPEM, err := newMeshCA(namespace, time.Now())
	if err != nil {
		return nil, err
	}
	s := &corev1.Secret{
		Namespace: namespace, Name: meshCASecret,
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM},
	}
	if err := r.Client.Create(ctx, s); err != nil {
		return nil, fmt.Errorf("storing the Cluster Mesh CA of namespace %s: %w", namespace, err)
	}
	return s, nil
}

func parseMeshCA(certPEM, keyPEM []byte) (*meshCA, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("reading the Cluster Mesh CA: %w", err)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the Cluster Mesh CA's key is no ECDSA key")
	}
	return &meshCA{cert: pair.Leaf, key: key, certPEM: certPEM}, nil
}

func newMeshCA(namespace string, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("making the Cluster Mesh CA's key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Kubehenge Cluster Mesh CA, namespace " + namespace},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(meshCAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("making the Cluster Mesh CA: %w", err)
	}
	return encodePair(der, key)
}

func serialNumber() (*big.Int, error) {
	const serialBits = 128
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
	if err != nil {
		return nil, fmt.Errorf("making a serial number: %w", err)
	}
	return serial, nil
}

func encodePair(der []byte, key *ecdsa.PrivateKey) ([]byte, []byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding a key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// meshCert is one of the certificates a cluster's clustermesh-apiserver and agents use, in the
// Secret Cilium's chart reads it from. Its users are named by the common name.
type meshCert struct {
	secret, commonName string
	dns                []string
	ips                []net.IP
	usage              x509.ExtKeyUsage
}

// meshCerts are a cluster's: the server's, for its own agents and for peers, which reach it on the
// endpoint. The admin one sets up its store. The local one is what its agents read KVStoreMesh's cache with.
// The remote one is what its KVStoreMesh reads the peers with.
func meshCerts(name, endpoint string) []meshCert {
	dns := []string{meshServer}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}
	if ip := net.ParseIP(endpoint); ip != nil {
		ips = append(ips, ip)
	} else if endpoint != "" {
		dns = append(dns, endpoint)
	}
	return []meshCert{
		{"clustermesh-apiserver-server-cert", meshServer, dns, ips, x509.ExtKeyUsageServerAuth},
		{"clustermesh-apiserver-admin-cert", "admin-" + name, nil, nil, x509.ExtKeyUsageClientAuth},
		{"clustermesh-apiserver-local-cert", "local-" + name, nil, nil, x509.ExtKeyUsageClientAuth},
		{"clustermesh-apiserver-remote-cert", "remote-" + name, nil, nil, x509.ExtKeyUsageClientAuth},
	}
}

// issue signs a new certificate for c, valid no longer than the CA.
func (ca *meshCA) issue(c *meshCert, now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("making a key for %s: %w", c.commonName, err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(meshCertValidity)
	if ca.cert.NotAfter.Before(notAfter) {
		notAfter = ca.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: c.commonName},
		DNSNames:     c.dns,
		IPAddresses:  c.ips,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{c.usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("signing a certificate for %s: %w", c.commonName, err)
	}
	return encodePair(der, key)
}

// current reports whether a Secret holds c's certificate with its key, signed by the CA, with more
// than a third of its life left.
func (ca *meshCA) current(s *corev1.Secret, c *meshCert, now time.Time) bool {
	if !bytes.Equal(s.Data[meshCAKey], ca.certPEM) {
		return false
	}
	pair, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return false
	}
	cert := pair.Leaf
	if cert.CheckSignatureFrom(ca.cert) != nil || cert.Subject.CommonName != c.commonName ||
		!slices.Equal(cert.DNSNames, c.dns) || !slices.EqualFunc(cert.IPAddresses, c.ips, net.IP.Equal) ||
		!slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{c.usage}) {
		return false
	}
	return now.Before(cert.NotAfter.Add(-cert.NotAfter.Sub(cert.NotBefore) / meshRenewalShare))
}

// meshEndpoint is a peer's connection settings, in the files Cilium's chart mounts: prefix names the
// certificate it signs in with, local- for the agents and common- for KVStoreMesh.
func meshEndpoint(url, prefix string) ([]byte, error) {
	config, err := yaml.Marshal(map[string]any{
		"endpoints":       []string{url},
		"trusted-ca-file": meshFiles + prefix + "etcd-client-ca.crt",
		"key-file":        meshFiles + prefix + "etcd-client.key",
		"cert-file":       meshFiles + prefix + "etcd-client.crt",
	})
	if err != nil {
		return nil, fmt.Errorf("writing a Cluster Mesh endpoint: %w", err)
	}
	return config, nil
}

// writeMesh writes a cluster's Cluster Mesh objects into kube-system: its certificates, renewed when
// due, and its links. The agents reach every peer through the local KVStoreMesh, which reads the
// peers' clustermesh-apiservers. Each peer signs in as its remote user, which may read this
// cluster's state and nothing else.
func writeMesh(ctx context.Context, workload client.Client, name, endpoint string, ca *meshCA, links []meshLink, now time.Time) error {
	if err := writeMeshCerts(ctx, workload, name, endpoint, ca, now); err != nil {
		return err
	}
	return writeMeshLinks(ctx, workload, links)
}

func writeMeshCerts(ctx context.Context, workload client.Client, name, endpoint string, ca *meshCA, now time.Time) error {
	for _, c := range meshCerts(name, endpoint) {
		s := &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: c.secret}
		_, err := controllerutil.CreateOrUpdate(ctx, workload, s, func() error {
			if ca.current(s, &c, now) {
				return nil
			}
			certPEM, keyPEM, err := ca.issue(&c, now)
			if err != nil {
				return err
			}
			s.Type = corev1.SecretTypeTLS
			s.Data = map[string][]byte{meshCAKey: ca.certPEM, corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
			return nil
		})
		if err != nil {
			return fmt.Errorf("writing %s: %w", c.secret, err)
		}
	}
	return nil
}

func writeMeshLinks(ctx context.Context, workload client.Client, links []meshLink) error {
	agents, kvstoremesh := map[string][]byte{}, map[string][]byte{}
	users := make([]map[string]string, 0, len(links))
	links = slices.SortedFunc(slices.Values(links), func(a, b meshLink) int { return strings.Compare(a.name, b.name) })
	for _, l := range links {
		var err error
		if agents[l.name], err = meshEndpoint("https://"+net.JoinHostPort(meshServer, strconv.Itoa(meshAPIPort)), "local-"); err != nil {
			return err
		}
		if kvstoremesh[l.name], err = meshEndpoint("https://"+net.JoinHostPort(l.endpoint, strconv.Itoa(meshPort)), "common-"); err != nil {
			return err
		}
		users = append(users, map[string]string{"name": "remote-" + l.name, "role": "remote"})
	}
	for secret, data := range map[string]map[string][]byte{meshAgentsSecret: agents, meshKVStoreMesh: kvstoremesh} {
		s := &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: secret}
		if _, err := controllerutil.CreateOrUpdate(ctx, workload, s, func() error { s.Data = data; return nil }); err != nil {
			return fmt.Errorf("writing %s: %w", secret, err)
		}
	}
	usersYAML, err := yaml.Marshal(map[string]any{"users": users})
	if err != nil {
		return fmt.Errorf("writing the Cluster Mesh users: %w", err)
	}
	cm := &corev1.ConfigMap{Namespace: metav1.NamespaceSystem, Name: meshUsersConfigMap}
	_, err = controllerutil.CreateOrUpdate(ctx, workload, cm, func() error {
		cm.Data = map[string]string{meshUsersKey: string(usersYAML)}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing %s: %w", meshUsersConfigMap, err)
	}
	return nil
}

// writeMeshPolicy lets the peers through to the clustermesh-apiserver from anywhere: a peer's
// connection reaches the pod from whichever node took it on the NodePort.
func writeMeshPolicy(ctx context.Context, workload client.Client) error {
	policy := &networkingv1.NetworkPolicy{Namespace: metav1.NamespaceSystem, Name: meshPolicy}
	tcp := corev1.ProtocolTCP
	_, err := controllerutil.CreateOrUpdate(ctx, workload, policy, func() error {
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "clustermesh-apiserver"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: new(intstr.FromInt32(meshAPIPort))}},
			}},
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing %s: %w", meshPolicy, err)
	}
	return nil
}

// removeMesh takes a cluster's Cluster Mesh objects out of it.
func removeMesh(ctx context.Context, workload client.Client) error {
	certs := meshCerts("", "")
	objs := slices.Grow([]client.Object{
		&corev1.Secret{Name: meshAgentsSecret},
		&corev1.Secret{Name: meshKVStoreMesh},
		&corev1.ConfigMap{Name: meshUsersConfigMap},
		&networkingv1.NetworkPolicy{Name: meshPolicy},
	}, len(certs))
	for _, c := range certs {
		objs = append(objs, &corev1.Secret{Name: c.secret})
	}
	return removeFromSystem(ctx, workload, objs...)
}

// removeFromSystem deletes objects of kube-system, those already gone too.
func removeFromSystem(ctx context.Context, workload client.Client, objs ...client.Object) error {
	for _, o := range objs {
		o.SetNamespace(metav1.NamespaceSystem)
		if err := workload.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s: %w", o.GetName(), err)
		}
	}
	return nil
}
