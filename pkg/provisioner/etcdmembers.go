package provisioner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ EtcdMaintenance = &EtcdDialer{}

// etcdPodAccess reaches the etcd members of a kubeadm, RKE2 or k3s cluster through their pods.
type etcdPodAccess struct {
	tls    *tls.Config
	config *rest.Config
	pods   []corev1.Pod
}

func (d *EtcdDialer) podAccess(ctx context.Context, cluster *clusterv1.Cluster) (*etcdPodAccess, error) {
	ca, err := etcdCA(ctx, d.Reader, cluster)
	if err != nil {
		return nil, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	tlsConfig, err := etcdClientTLS(ca, now())
	if err != nil {
		return nil, err
	}
	workload, found, err := d.Remotes.get(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the cluster has no kubeconfig yet")
	}
	pods, err := etcdPods(ctx, workload.client, distributionOf(cluster))
	if err != nil {
		d.Remotes.forget(cluster)
		return nil, err
	}
	if len(pods) == 0 {
		return nil, errNoEtcdMembers
	}
	return &etcdPodAccess{tls: tlsConfig, config: workload.config, pods: pods}, nil
}

// with runs fn with a client to the member in pod, closed afterwards.
func (a *etcdPodAccess) with(pod *corev1.Pod, fn func(*clientv3.Client) error) error {
	namespace, name := pod.Namespace, pod.Name
	cli, err := newEtcdClient(func(context.Context) (net.Conn, error) { return portForward(a.config, namespace, name, etcdPort) }, a.tls)
	if err != nil {
		return err
	}
	return errors.Join(fn(cli), cli.Close())
}

// Members reads the members through the pods that are running and ready.
func (d *EtcdDialer) Members(ctx context.Context, cluster *clusterv1.Cluster) ([]EtcdMember, error) {
	if isTalos(cluster) {
		return d.talosMembers(ctx, cluster)
	}
	a, err := d.podAccess(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var members []EtcdMember
	var errs []error
	for i := range a.pods {
		pod := &a.pods[i]
		err := a.with(pod, func(cli *clientv3.Client) error {
			m, err := readEtcdMember(ctx, cli, pod.Name)
			if err == nil {
				members = append(members, m)
			}
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("member %s: %w", pod.Name, err))
		}
	}
	return members, errors.Join(errs...)
}

// readEtcdMember reads the member cli reaches: its status, and the cluster's alarms that are its own.
func readEtcdMember(ctx context.Context, cli *clientv3.Client, name string) (EtcdMember, error) {
	st, err := cli.Status(ctx, etcdEndpoint)
	if err != nil {
		return EtcdMember{}, fmt.Errorf("reading the status: %w", err)
	}
	alarms, err := cli.AlarmList(ctx)
	if err != nil {
		return EtcdMember{}, fmt.Errorf("reading the alarms: %w", err)
	}
	id := st.Header.GetMemberId()
	m := EtcdMember{
		Name: name, ID: id, Leader: st.Leader == id, Learner: st.IsLearner,
		DBSize: st.DbSize, DBSizeInUse: st.DbSizeInUse, Errors: slices.Clone(st.Errors),
	}
	if st.Leader == 0 {
		m.Errors = append(m.Errors, "no leader")
	}
	for _, a := range alarms.Alarms {
		if a.GetMemberID() == id {
			m.Alarms = append(m.Alarms, a.GetAlarm().String())
		}
	}
	return m, nil
}

// Defragment defragments the member in the named pod, or on the named control-plane machine.
func (d *EtcdDialer) Defragment(ctx context.Context, cluster *clusterv1.Cluster, member string) error {
	if isTalos(cluster) {
		return d.talosEach(ctx, cluster, member, func(n TalosNode) error { return n.EtcdDefragment(ctx) })
	}
	a, err := d.podAccess(ctx, cluster)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(a.pods, func(p corev1.Pod) bool { return p.Name == member })
	if i < 0 {
		return fmt.Errorf("etcd member %s is not running and ready", member)
	}
	return a.with(&a.pods[i], func(cli *clientv3.Client) error {
		if _, err := cli.Defragment(ctx, etcdEndpoint); err != nil {
			return fmt.Errorf("defragmenting: %w", err)
		}
		return nil
	})
}

// Disarm clears every alarm through the first member that answers.
func (d *EtcdDialer) Disarm(ctx context.Context, cluster *clusterv1.Cluster) error {
	if isTalos(cluster) {
		return d.talosEach(ctx, cluster, "", func(n TalosNode) error { return n.EtcdDisarm(ctx) })
	}
	a, err := d.podAccess(ctx, cluster)
	if err != nil {
		return err
	}
	var errs []error
	for i := range a.pods {
		err := a.with(&a.pods[i], func(cli *clientv3.Client) error {
			alarms, err := cli.AlarmList(ctx)
			if err != nil {
				return fmt.Errorf("reading the alarms: %w", err)
			}
			for _, alarm := range alarms.Alarms {
				if _, err := cli.AlarmDisarm(ctx, (*clientv3.AlarmMember)(alarm)); err != nil {
					return fmt.Errorf("disarming: %w", err)
				}
			}
			return nil
		})
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// talosControlPlanes are a Talos cluster's control-plane machines that have an address and are not
// going, by name, with the talosconfig that reaches them.
func (d *EtcdDialer) talosControlPlanes(ctx context.Context, cluster *clusterv1.Cluster) ([]clusterv1.Machine, []byte, error) {
	if d.Talos == nil {
		return nil, nil, errors.New("this controller does not reach Talos nodes")
	}
	raw, err := readTalosconfig(ctx, d.Reader, cluster.Namespace, cluster.Name)
	if err != nil {
		return nil, nil, err
	}
	var machines clusterv1.MachineList
	err = d.Reader.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return nil, nil, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	ready := slices.DeleteFunc(machines.Items, func(m clusterv1.Machine) bool {
		return machineAddress(&m) == "" || !m.DeletionTimestamp.IsZero()
	})
	slices.SortFunc(ready, func(a, b clusterv1.Machine) int { return strings.Compare(a.Name, b.Name) })
	if len(ready) == 0 {
		return nil, nil, errNoEtcdMembers
	}
	return ready, raw, nil
}

func (d *EtcdDialer) talosMembers(ctx context.Context, cluster *clusterv1.Cluster) ([]EtcdMember, error) {
	machines, raw, err := d.talosControlPlanes(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var members []EtcdMember
	var errs []error
	for i := range machines {
		m, err := d.talosMember(ctx, raw, &machines[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("member %s: %w", machines[i].Name, err))
			continue
		}
		members = append(members, m)
	}
	return members, errors.Join(errs...)
}

func (d *EtcdDialer) talosMember(ctx context.Context, raw []byte, machine *clusterv1.Machine) (EtcdMember, error) {
	node, err := d.Talos(ctx, raw, machineAddress(machine))
	if err != nil {
		return EtcdMember{}, err
	}
	defer func() { _ = node.Close() }()
	m, err := node.EtcdMember(ctx)
	if err != nil {
		return EtcdMember{}, fmt.Errorf("reading the etcd member: %w", err)
	}
	m.Name = machine.Name
	return m, nil
}

// talosEach runs fn on the Talos node of the named control-plane machine, or on the first that
// answers when none is named.
func (d *EtcdDialer) talosEach(ctx context.Context, cluster *clusterv1.Cluster, machine string, fn func(TalosNode) error) error {
	machines, raw, err := d.talosControlPlanes(ctx, cluster)
	if err != nil {
		return err
	}
	var errs []error
	for i := range machines {
		if machine != "" && machines[i].Name != machine {
			continue
		}
		node, err := d.Talos(ctx, raw, machineAddress(&machines[i]))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = fn(node)
		_ = node.Close()
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return fmt.Errorf("etcd member %s is not among the control-plane machines", machine)
	}
	return errors.Join(errs...)
}
