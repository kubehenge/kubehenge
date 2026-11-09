package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EtcdHealthName is the ConfigMap, beside the ClusterClasses, in which the controller records which
// etcd members of the workload clusters are healthy and on which Proxmox VE node each one runs.
const EtcdHealthName = "kubehenge-etcd-health"

// EtcdHealthKey holds the record in EtcdHealthName, as JSON.
const EtcdHealthKey = "health"

// ManagementEtcdName names the management cluster among the sets of members.
const ManagementEtcdName = "management"

// EtcdMemberHealth is whether one control-plane machine's etcd member is healthy, and where it runs.
type EtcdMemberHealth struct {
	Machine string `json:"machine"`
	// Node is the Proxmox VE node the machine's VM runs on, empty where it is not known.
	Node    string `json:"node,omitempty"`
	Healthy bool   `json:"healthy"`
	// Reason says why it is not healthy.
	Reason string `json:"reason,omitempty"`
}

// EtcdSet is the etcd members of one cluster.
type EtcdSet struct {
	// Cluster is namespace/name, or ManagementEtcdName.
	Cluster string             `json:"cluster"`
	Members []EtcdMemberHealth `json:"members"`
}

// Healthy counts the healthy members.
func (s *EtcdSet) Healthy() int {
	n := 0
	for i := range s.Members {
		if s.Members[i].Healthy {
			n++
		}
	}
	return n
}

// EtcdHealth is what the controller last saw of the workload clusters' etcd members.
type EtcdHealth struct {
	Checked time.Time `json:"checked"`
	// Nodes are the Proxmox VE nodes of the cluster.
	Nodes    []string  `json:"nodes,omitempty"`
	Clusters []EtcdSet `json:"clusters"`
}

// ParseEtcdHealth reads the record out of the ConfigMap.
func ParseEtcdHealth(cm *corev1.ConfigMap) (EtcdHealth, error) {
	var h EtcdHealth
	if err := json.Unmarshal([]byte(cm.Data[EtcdHealthKey]), &h); err != nil {
		return EtcdHealth{}, fmt.Errorf("%s: %w", EtcdHealthName, err)
	}
	return h, nil
}

// ReadEtcdHealth reads the controller's record. ok is false until it has made one.
func ReadEtcdHealth(ctx context.Context, r client.Reader, namespace string) (EtcdHealth, bool, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: EtcdHealthName}, cm)
	if apierrors.IsNotFound(err) {
		return EtcdHealth{}, false, nil
	}
	if err != nil {
		return EtcdHealth{}, false, fmt.Errorf("reading %s: %w", EtcdHealthName, err)
	}
	h, err := ParseEtcdHealth(cm)
	return h, err == nil, err
}

// EtcdHealthStale is how old the record may be before a host reboot waits for a newer one: three of
// the watch's looks.
const EtcdHealthStale = 3 * time.Minute

// majority is how many of n etcd members make its quorum.
func majority(n int) int { return n/2 + 1 }

// RebootBlocker says why the Proxmox VE host node cannot go down now, or "" when it can. With node
// empty it asks that every member of every cluster is healthy. With a node it asks only whether
// stopping that node's members leaves each cluster a majority of healthy members, counting those
// already down anywhere. A degraded cluster with no member on the node does not block it.
func RebootBlocker(sets []EtcdSet, node string) string {
	if node == "" {
		return unhealthyMember(sets)
	}
	for i := range sets {
		healthy, onThis := 0, 0
		var names []string
		for _, m := range sets[i].Members {
			switch {
			case m.Node == node:
				names = append(names, m.Machine)
				if m.Healthy {
					onThis++
				}
			case m.Healthy:
				healthy++
			}
		}
		total := len(sets[i].Members)
		if len(names) > 0 && onThis > 0 && healthy < majority(total) {
			return fmt.Sprintf("%s: stopping %s would leave %d of %d etcd members healthy, and its quorum is %d (%s)",
				sets[i].Cluster, node, healthy, total, majority(total), strings.Join(names, ", "))
		}
	}
	return ""
}

// unhealthyMember names the first etcd member of any cluster that is not healthy, or is "".
func unhealthyMember(sets []EtcdSet) string {
	for i := range sets {
		for _, m := range sets[i].Members {
			if !m.Healthy {
				return fmt.Sprintf("%s: etcd member %s%s is not healthy: %s", sets[i].Cluster, m.Machine, onNodeText(m.Node), m.Reason)
			}
		}
	}
	return ""
}

func onNodeText(node string) string {
	if node == "" {
		return ""
	}
	return " on " + node
}

// RebootAnswer is whether one Proxmox VE node may go down.
type RebootAnswer struct {
	Node    string `json:"node"`
	Ready   bool   `json:"ready"`
	Blocker string `json:"blocker,omitempty"`
	// Note names a degraded cluster that does not hold this node back.
	Note string `json:"note,omitempty"`
}

// RebootReadiness is the answer for the whole cluster of hosts and for each node.
type RebootReadiness struct {
	Checked time.Time `json:"checked"`
	// Ready tells whether every etcd member of every cluster is healthy.
	Ready   bool   `json:"ready"`
	Blocker string `json:"blocker,omitempty"`
	// Nodes answers for each Proxmox VE node by itself.
	Nodes []RebootAnswer `json:"nodes"`
}

// ReadyForReboot answers, from the workload clusters' record and the management cluster's members,
// whether a host reboot may begin. It is the one answer the CLI, the interface and its guard share.
// A record older than EtcdHealthStale, or none, blocks every node.
func ReadyForReboot(record EtcdHealth, haveRecord bool, management *EtcdSet, now time.Time) RebootReadiness {
	r := RebootReadiness{Checked: record.Checked}
	nodes := slices.Clone(record.Nodes)
	sets := slices.Clone(record.Clusters)
	if management != nil {
		sets = append([]EtcdSet{*management}, sets...)
		for i := range management.Members {
			if n := management.Members[i].Node; n != "" && !slices.Contains(nodes, n) {
				nodes = append(nodes, n)
			}
		}
	}
	slices.Sort(nodes)
	var unknown string
	switch {
	case !haveRecord:
		unknown = "The controller has not recorded the clusters' etcd members yet."
	case now.Sub(record.Checked) > EtcdHealthStale:
		unknown = "The controller has not checked the clusters' etcd members since " + record.Checked.UTC().Format(time.RFC3339) + "."
	}
	fleet := unknown
	if fleet == "" {
		fleet = RebootBlocker(sets, "")
	}
	r.Ready, r.Blocker = fleet == "", fleet
	for _, n := range nodes {
		a := RebootAnswer{Node: n, Blocker: unknown}
		if unknown == "" {
			a.Blocker = RebootBlocker(sets, n)
			if a.Blocker == "" {
				a.Note = fleet
			}
		}
		a.Ready = a.Blocker == ""
		r.Nodes = append(r.Nodes, a)
	}
	return r
}

// For picks the answer for one node. A node outside the cluster is refused.
func (r *RebootReadiness) For(node string) RebootAnswer {
	if node == "" {
		return RebootAnswer{Ready: r.Ready, Blocker: r.Blocker}
	}
	i := slices.IndexFunc(r.Nodes, func(a RebootAnswer) bool { return a.Node == node })
	if i < 0 {
		return RebootAnswer{Node: node, Blocker: "Proxmox VE node " + node + " holds none of Kubehenge's control planes, or is not in the cluster."}
	}
	return r.Nodes[i]
}

// managementNameIndex reads the number a management node's name ends in: the module names its VMs
// <cluster>-cp-<i>, and puts VM i on the i-th of its Proxmox VE nodes, taken in turn.
var managementNameIndex = regexp.MustCompile(`-cp-(\d+)$`)

// ManagementEtcd reads the management cluster's etcd members from its control-plane nodes. Talos
// runs etcd as a service, so a member counts as healthy when its node is Ready and its kube-apiserver
// pod is Ready, whose readiness probe includes the etcd check of the member that apiserver is
// connected to. pveNodes are the Proxmox VE nodes the module's VMs take in turn.
func ManagementEtcd(ctx context.Context, r client.Reader, pveNodes []string) (EtcdSet, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.HasLabels{"node-role.kubernetes.io/control-plane"}); err != nil {
		return EtcdSet{}, fmt.Errorf("listing the management cluster's control-plane nodes: %w", err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(metav1.NamespaceSystem)); err != nil {
		return EtcdSet{}, fmt.Errorf("listing the management cluster's apiserver pods: %w", err)
	}
	// Static pods are named after their component and node, whatever labels the distribution gives them.
	pods.Items = slices.DeleteFunc(pods.Items, func(p corev1.Pod) bool { return !strings.HasPrefix(p.Name, "kube-apiserver-") })
	set := EtcdSet{Cluster: ManagementEtcdName}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		m := EtcdMemberHealth{Machine: n.Name}
		if at := managementNameIndex.FindStringSubmatch(n.Name); at != nil && len(pveNodes) > 0 {
			index, _ := strconv.Atoi(at[1])
			m.Node = pveNodes[index%len(pveNodes)]
		}
		m.Healthy, m.Reason = managementMemberHealth(n, pods.Items)
		set.Members = append(set.Members, m)
	}
	slices.SortFunc(set.Members, func(a, b EtcdMemberHealth) int { return strings.Compare(a.Machine, b.Machine) })
	return set, nil
}

func managementMemberHealth(n *corev1.Node, pods []corev1.Pod) (bool, string) {
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if !ready {
		return false, "its node is not ready"
	}
	i := slices.IndexFunc(pods, func(p corev1.Pod) bool { return p.Spec.NodeName == n.Name })
	switch {
	case i < 0:
		return false, "its kube-apiserver pod is missing"
	case !podReady(&pods[i]):
		return false, "its kube-apiserver pod is not ready, which includes etcd's health"
	}
	return true, ""
}
