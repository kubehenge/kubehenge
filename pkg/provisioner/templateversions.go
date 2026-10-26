package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// KubeadmVersionsAnnotation lists on the kubeadm ClusterClass, for each node OS, the Kubernetes
// versions a template in the template pool has been built for, newest first, as JSON: the node OS
// (almalinux-10) to its versions. The interface offers these: Cluster API clones with its own
// token, so the users who create clusters need not see the pool themselves.
const KubeadmVersionsAnnotation = "kubehenge.dev/kubeadm-versions"

// NodeOSTemplatesAnnotation lists on the RKE2 and k3s ClusterClasses the node OSes the template
// pool holds a template of their machines for, separated by commas. A cluster on another waits
// for ever for its first VM.
const NodeOSTemplatesAnnotation = "kubehenge.dev/node-os-templates"

// TalosTemplatesAnnotation lists on the Talos ClusterClass the Talos releases a template in the
// template pool is there for, newest first and separated by commas: a cluster starts on one of
// these, and its new machines move to another only once it is there. Talos is its own OS, so no
// node OS is part of it.
const TalosTemplatesAnnotation = "kubehenge.dev/talos-templates"

// TemplateVersions keeps KubeadmVersionsAnnotation, NodeOSTemplatesAnnotation,
// TalosTemplatesAnnotation, TemplateStoragesAnnotation, NodeImageBuildsAnnotation and
// SnapshotStoragesAnnotation current.
type TemplateVersions struct {
	Client         client.Client
	ClassNamespace string
	Proxmox        *proxmox.Client
	// Interval is how often the templates are looked at: a template built or deleted is offered or
	// withdrawn within about that long.
	Interval time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*TemplateVersions) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (p *TemplateVersions) Start(ctx context.Context) error {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		if err := p.publish(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("template-versions").Error(err, "recording the templates on the ClusterClasses")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *TemplateVersions) publish(ctx context.Context) error {
	return errors.Join(p.publishTemplates(ctx), p.publishSnapshotStorages(ctx))
}

func (p *TemplateVersions) publishSnapshotStorages(ctx context.Context) error {
	storages, err := snapshotStorages(ctx, p.Proxmox)
	if err != nil {
		return err
	}
	list := strings.Join(storages, ",")
	return errors.Join(
		p.annotate(ctx, "kubeadm", SnapshotStoragesAnnotation, list),
		p.annotate(ctx, "rke2", SnapshotStoragesAnnotation, list),
		p.annotate(ctx, "k3s", SnapshotStoragesAnnotation, list),
		p.annotate(ctx, "talos", SnapshotStoragesAnnotation, list),
	)
}

func (p *TemplateVersions) publishTemplates(ctx context.Context) error {
	seen, err := listPoolTemplates(ctx, p.Proxmox)
	if err != nil {
		return err
	}
	kubeadm, err := encodeKubeadmVersions(seen.kubeadm)
	if err != nil {
		return err
	}
	storages := strings.Join(seen.storages, ",")
	builds, err := seen.builds.Encode()
	if err != nil {
		return err
	}
	return errors.Join(
		p.annotate(ctx, "kubeadm", KubeadmVersionsAnnotation, kubeadm),
		p.annotate(ctx, "rke2", NodeOSTemplatesAnnotation, joinNodeOSes(seen.nodeOSes[templates.RKE2])),
		p.annotate(ctx, "k3s", NodeOSTemplatesAnnotation, joinNodeOSes(seen.nodeOSes[templates.K3s])),
		p.annotate(ctx, "kubeadm", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "rke2", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "k3s", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "talos", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "talos", TalosTemplatesAnnotation, strings.Join(seen.talos, ",")),
		p.annotate(ctx, "kubeadm", NodeImageBuildsAnnotation, builds),
		p.annotate(ctx, "rke2", NodeImageBuildsAnnotation, builds),
		p.annotate(ctx, "k3s", NodeImageBuildsAnnotation, builds),
	)
}

// encodeKubeadmVersions writes KubeadmVersionsAnnotation, the same for the same versions.
func encodeKubeadmVersions(versions map[templates.NodeOS][]kubeVersion) (string, error) {
	raw := map[string][]string{}
	for nodeOS, list := range versions {
		for _, v := range list {
			raw[nodeOS.String()] = append(raw[nodeOS.String()], v.Raw)
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", KubeadmVersionsAnnotation, err)
	}
	return string(out), nil
}

// ParseKubeadmVersions reads KubeadmVersionsAnnotation: each node OS's versions, newest first.
func ParseKubeadmVersions(s string) (map[string][]string, error) {
	out := map[string][]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("reading %s: %w", KubeadmVersionsAnnotation, err)
	}
	return out, nil
}

func joinNodeOSes(list []templates.NodeOS) string {
	out := make([]string, len(list))
	for i, o := range list {
		out[i] = o.String()
	}
	return strings.Join(out, ",")
}

// annotate sets an annotation on a distribution's ClusterClass, once the chart has made it.
func (p *TemplateVersions) annotate(ctx context.Context, distribution, key, value string) error {
	class := &clusterv1.ClusterClass{}
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.ClassNamespace, Name: ClassPrefix + distribution}, class)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the %s ClusterClass: %w", distribution, err)
	}
	if got, ok := class.Annotations[key]; ok && got == value {
		return nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, key, value)
	if err := p.Client.Patch(ctx, class, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("annotating the %s ClusterClass: %w", distribution, err)
	}
	return nil
}
