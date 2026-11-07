package provisioner

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// bgpPurpose is the user credential of a cluster's BGP password, shared by its routers.
	bgpPurpose = "bgp"
	// kubeVipManifestKey is the key of the control planes' kube-vip static pod in its Secret.
	kubeVipManifestKey = "kube-vip.yaml"
	// bgpPeerSecret in the workload cluster holds the password MetalLB's peers sign their sessions with.
	bgpPeerSecret = "kubehenge-bgp" //nolint:gosec // a Secret's name, not a credential
	// kubeVipAdminConf is where the pod mounts the node's admin kubeconfig, whichever file it is on the host.
	kubeVipAdminConf = "/etc/kubernetes/admin.conf"
	// kubeVipKubeconfig names the volume of that kubeconfig.
	kubeVipKubeconfig = "kubeconfig"
	capNetAdmin       = "NET_ADMIN"
)

// KubeVipSecretName is the Secret beside a cluster that holds the control planes' kube-vip static
// pod while its BGP sessions have a password. The ClusterClasses write the file from it.
func KubeVipSecretName(cluster string) string {
	return cluster + "-kube-vip"
}

// kubeVipHostKubeconfig is the admin kubeconfig a distribution's control-plane node holds.
func kubeVipHostKubeconfig(d templates.Distribution) string {
	switch d {
	case templates.RKE2:
		return "/etc/rancher/rke2/rke2.yaml"
	case templates.K3s:
		return "/etc/rancher/k3s/k3s.yaml"
	case templates.Kubeadm, templates.Talos:
		return kubeVipAdminConf
	}
	return kubeVipAdminConf
}

// KubeVipStaticPod is the manifest of kube-vip on a control-plane node, announcing the endpoint
// over BGP to the routers, each session signed with the password. It is the one the ClusterClasses
// write inline, apart from the password, which cannot go into a ClusterClass.
func KubeVipStaticPod(image, hostKubeconfig, endpoint string, bgp *BGP, password string) ([]byte, error) {
	peers := make([]string, 0, len(bgp.Peers))
	for _, p := range bgp.Peers {
		peers = append(peers, p.Address+":"+strconv.FormatUint(uint64(p.ASN), 10)+":"+password+":false")
	}
	env := []corev1.EnvVar{
		{Name: "address", Value: endpoint},
		{Name: "port", Value: "6443"},
		{Name: "vip_arp", Value: falseText},
		{Name: "bgp_enable", Value: trueText},
		{Name: "bgp_routerid", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{Name: "bgp_as", Value: strconv.FormatUint(uint64(bgp.ASN), 10)},
		{Name: "bgp_peers", Value: strings.Join(peers, ",")},
		{Name: "cp_enable", Value: trueText},
		{Name: "cp_namespace", Value: metav1.NamespaceSystem},
		{Name: "svc_enable", Value: falseText},
		{Name: "prometheus_server", Value: "127.0.0.1:2112"},
		{Name: "vip_leaderelection", Value: trueText},
		{Name: "vip_leaseduration", Value: "30"},
		{Name: "vip_renewdeadline", Value: "25"},
		{Name: "vip_retryperiod", Value: "3"},
		{Name: "vip_nodename", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
	}
	fileType := corev1.HostPathFile
	pod := corev1.Pod{
		APIVersion: "v1", Kind: "Pod", Name: "kube-vip", Namespace: metav1.NamespaceSystem,
		Spec: corev1.PodSpec{
			HostNetwork: true,
			HostAliases: []corev1.HostAlias{{IP: loopback, Hostnames: []string{"kubernetes"}}},
			SecurityContext: &corev1.PodSecurityContext{
				SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t", Level: "s0"},
			},
			Containers: []corev1.Container{{
				Name: "kube-vip", Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Args: []string{"manager"}, Env: env,
				SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
					Add: []corev1.Capability{capNetAdmin, "NET_RAW"}, Drop: []corev1.Capability{"ALL"},
				}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi"),
				}},
				VolumeMounts: []corev1.VolumeMount{{Name: kubeVipKubeconfig, MountPath: kubeVipAdminConf, ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: kubeVipKubeconfig, HostPath: &corev1.HostPathVolumeSource{Path: hostKubeconfig, Type: &fileType}}},
		},
	}
	out, err := yaml.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("writing kube-vip's manifest: %w", err)
	}
	return out, nil
}

// bgpState tells whether a cluster's BGP sessions carry a password and whether it is there to use.
type bgpState int

const (
	// bgpPlain is a cluster whose sessions carry no password.
	bgpPlain bgpState = iota
	// bgpWaiting is a cluster whose sessions need a password that its credential does not hold yet.
	bgpWaiting
	// bgpSigned is a cluster whose credential holds the password.
	bgpSigned
)

// bgpSecret is the state of a cluster's BGP password, and the password once there is one.
type bgpSecret struct {
	password string
	state    bgpState
}

// bgpPassword reads the password the cluster's routers' sessions share, and says through an event
// what is missing when the sessions need one that it cannot give.
func (r *Reconciler) bgpPassword(ctx context.Context, cluster *clusterv1.Cluster, lb LoadBalancer) (bgpSecret, error) {
	if !lb.bgp() || lb.BGP == nil || !lb.BGP.Authenticated {
		return bgpSecret{}, nil
	}
	name := CredentialName(cluster.Name, bgpPurpose)
	s := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s); err != nil && !apierrors.IsNotFound(err) {
		return bgpSecret{state: bgpWaiting}, fmt.Errorf("reading the BGP credential %s: %w", name, err)
	}
	// A credential that is not there leaves s empty, which fails the checks below.
	password := string(s.Data[passwordKey])
	if s.Type != CredentialType || s.Labels[CredentialLabel] != bgpPurpose || password == "" || strings.ContainsAny(password, ":,\n\r\t ") {
		r.eventf(cluster, "NoBGPPassword", "the BGP sessions need a password in the credential %s: set one in the cluster's load balancer, without colons, commas or spaces", name)
		return bgpSecret{state: bgpWaiting}, nil
	}
	return bgpSecret{password: password, state: bgpSigned}, nil
}

func (r *Reconciler) eventf(cluster *clusterv1.Cluster, reason, format string, args ...any) {
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, reason, "ReconcileLoadBalancer", format, args...)
	}
}

// reconcileKubeVip keeps the Secret the control planes' bootstrap reads kube-vip's static pod from
// while the cluster's BGP sessions have a password, and takes it away when they have none. It
// reports whether the cluster is held back, as its control planes cannot start without the Secret.
func (r *Reconciler) reconcileKubeVip(ctx context.Context, cluster *clusterv1.Cluster, lb LoadBalancer) (bool, error) {
	p, err := r.bgpPassword(ctx, cluster, lb)
	switch {
	case err != nil:
		return false, err
	case p.state == bgpWaiting:
		return true, nil
	case p.state == bgpPlain:
		s := &corev1.Secret{Namespace: cluster.Namespace, Name: KubeVipSecretName(cluster.Name)}
		if err := r.Client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("deleting Secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		return false, nil
	}
	return false, r.writeKubeVipSecret(ctx, cluster, lb, p.password)
}

func (r *Reconciler) writeKubeVipSecret(ctx context.Context, cluster *clusterv1.Cluster, lb LoadBalancer, password string) error {
	s := &corev1.Secret{Namespace: cluster.Namespace, Name: KubeVipSecretName(cluster.Name)}
	endpoint, err := variable[string](cluster, "controlPlaneEndpoint")
	if err != nil {
		return err
	}
	manifest, err := KubeVipStaticPod(kubeVipImage(r.Charts.KubeVipImage), kubeVipHostKubeconfig(distributionOf(cluster)), endpoint, lb.BGP, password)
	if err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		s.Type = corev1.SecretTypeOpaque
		s.Data = map[string][]byte{kubeVipManifestKey: manifest}
		return controllerutil.SetControllerReference(cluster, s, r.Client.Scheme())
	})
	if err != nil {
		return fmt.Errorf("writing Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// kubeVipImage is the image reference of kube-vip's pinned release.
func kubeVipImage(i Image) string {
	return i.Registry + "/" + i.Repository + ":" + i.Tag + "@sha256:" + i.Digest
}

// deleteBGPPeerSecret takes the password away from the workload cluster once MetalLB's peers sign
// nothing with it.
func deleteBGPPeerSecret(ctx context.Context, workload client.Client) error {
	s := &corev1.Secret{Namespace: metalLBNamespace, Name: bgpPeerSecret}
	if err := workload.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// applyBGPPeerSecret keeps the password MetalLB's peers sign their sessions with in the workload
// cluster, and reports whether it waits for the namespace MetalLB's chart makes.
func applyBGPPeerSecret(ctx context.Context, workload client.Client, password string) (bool, error) {
	s := &corev1.Secret{Namespace: metalLBNamespace, Name: bgpPeerSecret}
	_, err := controllerutil.CreateOrUpdate(ctx, workload, s, func() error {
		s.Type = corev1.SecretTypeBasicAuth
		s.Data = map[string][]byte{passwordKey: []byte(password)}
		return nil
	})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("writing Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return false, nil
}
