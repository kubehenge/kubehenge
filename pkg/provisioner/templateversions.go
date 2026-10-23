package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeadmVersionsAnnotation lists on the kubeadm ClusterClass the Kubernetes versions a template
// in the template pool has been built for, newest first and separated by commas. The interface
// offers these: Cluster API clones with its own token, so the users who create clusters need not
// see the pool themselves.
const KubeadmVersionsAnnotation = "kubehenge.dev/kubeadm-versions"

// CloudImageTemplateAnnotation says on the RKE2 and k3s ClusterClasses, "true" or "false", whether
// the template pool holds the template their machines are cloned from. Without it a cluster of
// theirs waits for ever for its first VM.
const CloudImageTemplateAnnotation = "kubehenge.dev/cloud-image-template"

// TemplateVersions keeps KubeadmVersionsAnnotation, CloudImageTemplateAnnotation,
// TemplateStoragesAnnotation, NodeImageBuildsAnnotation and SnapshotStoragesAnnotation current.
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
	)
}

func (p *TemplateVersions) publishTemplates(ctx context.Context) error {
	seen, err := listPoolTemplates(ctx, p.Proxmox)
	if err != nil {
		return err
	}
	raw := make([]string, len(seen.kubeadm))
	for i, v := range seen.kubeadm {
		raw[i] = v.Raw
	}
	cloudImage := strconv.FormatBool(seen.cloudImage)
	storages := strings.Join(seen.storages, ",")
	builds, err := seen.builds.Encode()
	if err != nil {
		return err
	}
	return errors.Join(
		p.annotate(ctx, "kubeadm", KubeadmVersionsAnnotation, strings.Join(raw, ",")),
		p.annotate(ctx, "rke2", CloudImageTemplateAnnotation, cloudImage),
		p.annotate(ctx, "k3s", CloudImageTemplateAnnotation, cloudImage),
		p.annotate(ctx, "kubeadm", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "rke2", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "k3s", TemplateStoragesAnnotation, storages),
		p.annotate(ctx, "kubeadm", NodeImageBuildsAnnotation, builds),
		p.annotate(ctx, "rke2", NodeImageBuildsAnnotation, builds),
		p.annotate(ctx, "k3s", NodeImageBuildsAnnotation, builds),
	)
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
