package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// NodeCustomisationRevisionVariable holds, per role, a digest of the Secrets and the ConfigMap
	// that nodeCustomisation names for that role. Only Kubehenge's controller sets it: a new digest
	// changes the files the class patches into that role's bootstrap template alone, so Cluster API
	// replaces only those nodes. Cluster API passes nothing else of a Cluster, such as an
	// annotation, into a bootstrap template. Worker pools share the variable, so they roll together.
	NodeCustomisationRevisionVariable = "nodeCustomisationRevision"

	// NodeCustomisationCondition is true on a Cluster while nodeCustomisation names a Secret or a
	// ConfigMap that is missing or unusable, so that the nodes keep what they have.
	NodeCustomisationCondition = "NodeCustomisationInvalid"
	// NodeCustomisationMissing is the reason of NodeCustomisationCondition.
	NodeCustomisationMissing = "MissingReference"

	// nodeSourcesAnnotation records, per role, the digest of each source behind the revision, to
	// name the one that changed.
	nodeSourcesAnnotation = "kubehenge.dev/node-customisation-sources"

	// NodeCustomisationChangedReason is the reason of the event that says why nodes are replaced.
	NodeCustomisationChangedReason = "NodeCustomisationChanged"

	revisionHexLen = 16

	roleControlPlane = "controlPlane"
	roleWorker       = "worker"
)

var (
	allNodeRoles     = []string{roleControlPlane, roleWorker}
	controlPlaneOnly = []string{roleControlPlane}
	roleNames        = map[string]string{roleControlPlane: "control planes", roleWorker: "workers"}
)

// sources are the Secrets and the ConfigMap that nodeCustomisation names, each with the roles whose
// nodes read it and the function that reads its content. The audit webhook and the audit policy are
// read by control planes alone. Talos reads none.
func (n *NodeCustomisation) sources(namespace string) []nodeSource {
	var out []nodeSource
	for _, f := range n.Files {
		if f.Secret != nil {
			roles := f.Roles
			if len(roles) == 0 {
				roles = allNodeRoles
			}
			out = append(out, secretSource(namespace, f.Secret.Name, f.Secret.Key, fmt.Sprintf("Secret %s/%s", f.Secret.Name, f.Secret.Key), roles))
		}
	}
	if w := n.AuditWebhook; w != nil {
		key := w.Key
		if key == "" {
			key = IdentityKubeconfigKey
		}
		out = append(out, secretSource(namespace, w.Secret, key, "the audit webhook's Secret "+w.Secret, controlPlaneOnly))
	}
	if p := n.AuditPolicy; p != nil && p.ConfigMap != "" {
		out = append(out, nodeSource{label: "the audit policy's ConfigMap " + p.ConfigMap, roles: controlPlaneOnly, read: func(ctx context.Context, r client.Reader) ([]byte, error) {
			var cm corev1.ConfigMap
			if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: p.ConfigMap}, &cm); err != nil {
				return nil, fmt.Errorf("ConfigMap %s: %w", p.ConfigMap, err)
			}
			text, ok := cm.Data[auditPolicyConfigMapKey]
			if !ok {
				return nil, fmt.Errorf("ConfigMap %s has no key %s", p.ConfigMap, auditPolicyConfigMapKey)
			}
			return []byte(text), nil
		}})
	}
	return out
}

// NodeCustomisationRolls is what a plan says of a cluster whose customisation names a Secret or a
// ConfigMap, for the roles that read one.
func NodeCustomisationRolls(roles []string) string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, roleNames[r])
	}
	return "The " + strings.Join(names, " and ") + " are also replaced, one at a time, whenever a Secret or ConfigMap that node customisation names for them changes."
}

// NodeCustomisationOf reads a Cluster's nodeCustomisation variable.
func NodeCustomisationOf(c *clusterv1.Cluster) (NodeCustomisation, bool, error) {
	return optionalVariable[NodeCustomisation](c, NodeCustomisationVariable)
}

// ReferenceRoles are the roles whose nodes read a Secret or a ConfigMap of the customisation.
func (n *NodeCustomisation) ReferenceRoles() []string {
	sources := n.sources("")
	var out []string
	for _, role := range allNodeRoles {
		if slices.ContainsFunc(sources, func(s nodeSource) bool { return slices.Contains(s.roles, role) }) {
			out = append(out, role)
		}
	}
	return out
}

type nodeSource struct {
	label string
	roles []string
	read  func(context.Context, client.Reader) ([]byte, error)
}

func secretSource(namespace, name, key, label string, roles []string) nodeSource {
	return nodeSource{label: label, roles: roles, read: func(ctx context.Context, r client.Reader) ([]byte, error) {
		var s corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("Secret %s is not in namespace %s", name, namespace) //nolint:staticcheck // names a Secret
			}
			return nil, fmt.Errorf("Secret %s: %w", name, err) //nolint:staticcheck // names a Secret
		}
		data, ok := s.Data[key]
		if !ok {
			return nil, fmt.Errorf("Secret %s has no key %s", name, key) //nolint:staticcheck // names a Secret
		}
		return data, nil
	}}
}

// roleDigests maps a role to the digest of each source it reads, by label.
type roleDigests map[string]map[string]string

// nodeDigests reads every source. A source that cannot be read is a problem, and has no digest.
func nodeDigests(ctx context.Context, r client.Reader, sources []nodeSource) (roleDigests, []string) {
	digests := roleDigests{}
	var problems []string
	for _, s := range sources {
		data, err := s.read(ctx, r)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		sum := sha256.Sum256(data)
		for _, role := range s.roles {
			if digests[role] == nil {
				digests[role] = map[string]string{}
			}
			digests[role][s.label] = hex.EncodeToString(sum[:])[:revisionHexLen]
		}
	}
	return digests, problems
}

// revisionsOf is one digest of the sources' digests for each role that reads any.
func revisionsOf(digests roleDigests) map[string]string {
	out := map[string]string{}
	for role, labels := range digests {
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(labels[k])
			b.WriteString("\n")
		}
		sum := sha256.Sum256([]byte(b.String()))
		out[role] = hex.EncodeToString(sum[:])[:revisionHexLen]
	}
	return out
}

// ensureNodeCustomisation keeps what nodeCustomisation names in step with the nodes. It mirrors a
// ConfigMap policy into the Secret the bootstrap reads, marks the Cluster while a reference is
// missing, and writes the digest of the referenced content into nodeCustomisationRevision, which
// replaces the nodes that read it when it changes: a rotated token or an edited audit policy
// reaches them.
func (r *Reconciler) ensureNodeCustomisation(ctx context.Context, cluster *clusterv1.Cluster) error {
	if distributionOf(cluster) == templates.Talos {
		return nil
	}
	n, _, err := optionalVariable[NodeCustomisation](cluster, NodeCustomisationVariable)
	if err != nil {
		return err
	}
	key := client.ObjectKeyFromObject(cluster)
	sources := n.sources(cluster.Namespace)
	if _, had, _ := optionalVariable[map[string]string](cluster, NodeCustomisationRevisionVariable); len(sources) == 0 && !had {
		return nil
	}
	digests, problems := nodeDigests(ctx, r.Reader, sources)
	if len(problems) > 0 {
		cond := &metav1.Condition{Status: metav1.ConditionTrue, Reason: NodeCustomisationMissing, Message: strings.Join(problems, ". ")}
		before, markErr := markCluster(ctx, r.Client, r.Reader, key, NodeCustomisationCondition, cond)
		if markErr == nil && r.Events != nil && newlyMarked(before, cond) {
			r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, NodeCustomisationCondition, "WatchNodeCustomisation", "%s", shorten(cond.Message, watchEventMax))
		}
		return markErr
	}
	if meta.FindStatusCondition(cluster.Status.Conditions, NodeCustomisationCondition) != nil {
		if _, err := markCluster(ctx, r.Client, r.Reader, key, NodeCustomisationCondition, nil); err != nil {
			return err
		}
	}
	if err := r.ensureAuditPolicy(ctx, cluster); err != nil {
		return err
	}
	return r.writeRevision(ctx, key, digests)
}

// writeRevision sets the revision variable and the record of its sources on the Cluster, or takes
// both away once nothing is referenced, and says in an event which source changed for which role.
func (r *Reconciler) writeRevision(ctx context.Context, key client.ObjectKey, digests roleDigests) error {
	var changed map[string][]string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live clusterv1.Cluster
		if err := r.Reader.Get(ctx, key, &live); err != nil {
			return client.IgnoreNotFound(err)
		}
		var differs bool
		var setErr error
		if changed, differs, setErr = setRevision(&live, digests); setErr != nil || !differs {
			return setErr
		}
		return r.Client.Update(ctx, &live)
	})
	if err != nil {
		return fmt.Errorf("writing the node customisation revision of %s: %w", key, err)
	}
	if len(changed) > 0 && r.Events != nil {
		var live clusterv1.Cluster
		if r.Reader.Get(ctx, key, &live) == nil {
			for _, role := range allNodeRoles {
				if labels := changed[role]; len(labels) > 0 {
					r.Events.Eventf(&live, nil, corev1.EventTypeNormal, NodeCustomisationChangedReason, "RollNodes",
						"%s changed: %s are replaced one at a time.", strings.Join(labels, " and "), roleNames[role])
				}
			}
		}
	}
	return nil
}

// setRevision puts the revision and the record of its sources on the Cluster, or takes both away
// once nothing is referenced. It returns the sources that changed and whether the Cluster differs.
func setRevision(live *clusterv1.Cluster, digests roleDigests) (map[string][]string, bool, error) {
	want := revisionsOf(digests)
	had, _, _ := optionalVariable[map[string]string](live, NodeCustomisationRevisionVariable)
	if maps.Equal(had, want) {
		return nil, false, nil
	}
	var before roleDigests
	_ = json.Unmarshal([]byte(live.Annotations[nodeSourcesAnnotation]), &before)
	live.Spec.Topology.Variables = slices.DeleteFunc(live.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
		return v.Name == NodeCustomisationRevisionVariable
	})
	if len(want) == 0 {
		delete(live.Annotations, nodeSourcesAnnotation)
		return changedSources(before, digests), true, nil
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return nil, false, fmt.Errorf("encoding the revision: %w", err)
	}
	sources, err := json.Marshal(digests)
	if err != nil {
		return nil, false, fmt.Errorf("encoding the sources: %w", err)
	}
	live.Spec.Topology.Variables = append(live.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: NodeCustomisationRevisionVariable, Value: apiextensionsv1.JSON{Raw: raw}})
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[nodeSourcesAnnotation] = string(sources)
	return changedSources(before, digests), true, nil
}

// changedSources are, per role, the sources whose content differs from the record, by label.
// Nothing is named the first time, when there is no record to differ from.
func changedSources(before, now roleDigests) map[string][]string {
	if len(before) == 0 {
		return nil
	}
	out := map[string][]string{}
	for role, labels := range now {
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			if before[role][k] != labels[k] {
				out[role] = append(out[role], k)
			}
		}
	}
	for role, labels := range before {
		for _, k := range slices.Sorted(maps.Keys(labels)) {
			if _, ok := now[role][k]; !ok {
				out[role] = append(out[role], k)
			}
		}
	}
	return out
}
