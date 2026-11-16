package backupstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// WakeRequestAnnotation begins the annotations on the store's Deployment that hold the store awake,
// with the time each runs out in RFC 3339. The controller sets one for each holder
// (WakeRequestKey) while it uses the store, and takes it away once done.
const WakeRequestAnnotation = "kubehenge.dev/wake-requested"

// InterfaceWakeAnnotation begins the appliance's own wake requests, for its backups and a look at
// the clusters' snapshots, one for each holder as the controller's.
const InterfaceWakeAnnotation = "kubehenge.dev/interface-wake-requested"

// WakeRequestLife is how long a request holds the store awake unless it is asked again. It outlasts
// the longest holder, a restore, which asks once and uses the store to its end. A request whose
// holder never lets go runs out after it, and Sleeper puts the store back to sleep.
const WakeRequestLife = provisioner.EtcdRestoreTimeout + 10*time.Minute

// LookLife is the life of a look at the snapshots, which the Backups page asks for again with each
// poll, every 30 seconds. The disk detaches a few minutes after the page closes.
const LookLife = 3 * time.Minute

// WakeRequestKey is holder's own annotation under base, one of the two above.
func WakeRequestKey(base, holder string) string {
	sum := sha256.Sum256([]byte(holder))
	return base + "-" + hex.EncodeToString(sum[:8])
}

// WakeRequested tells whether d carries a wake request still in force at now, of any holder.
func WakeRequested(d *appsv1.Deployment, now time.Time) bool {
	for key, value := range d.Annotations {
		if !strings.HasPrefix(key, WakeRequestAnnotation) && !strings.HasPrefix(key, InterfaceWakeAnnotation) {
			continue
		}
		if until, err := time.Parse(time.RFC3339, value); err == nil && now.Before(until) {
			return true
		}
	}
	return false
}

// ServiceBase is the store's address inside the management cluster.
const ServiceBase = "http://" + Service + "." + Namespace + ".svc"

// Volume is the backup volume as the controller reaches it, from inside the management cluster.
type Volume struct {
	Kube kubernetes.Interface
	HTTP *http.Client
	// Base is the store's address, ServiceBase where empty.
	Base string
	// Annotation begins each holder's request, WakeRequestAnnotation where empty.
	Annotation string
	// VMs reads where the management VMs run, to set the nodes' zones before a wake.
	VMs VMReader
	// Life is how long each request holds the store, WakeRequestLife where 0.
	Life time.Duration
	Log  *slog.Logger
	Now  func() time.Time
}

var _ provisioner.BackupVolume = (*Volume)(nil)

// Open asks for the store to be awake on behalf of holder, and returns it once it is. A sleeping
// store is started and answers provisioner.ErrVolumeAsleep until its pod is ready.
func (v *Volume) Open(ctx context.Context, holder string) (provisioner.VolumeStore, error) { //nolint:ireturn // tests stand in for the store
	if err := v.Wake(ctx, holder); err != nil {
		return nil, err
	}
	secret, err := v.Kube.CoreV1().Secrets(Namespace).Get(ctx, TokenSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the backup volume's token: %w", err)
	}
	base := v.Base
	if base == "" {
		base = ServiceBase
	}
	return &Client{Base: base, Token: string(secret.Data[TokenKey]), HTTP: v.HTTP}, nil
}

// Wake asks for the store to be awake on behalf of holder, and starts it where it sleeps. It
// answers provisioner.ErrVolumeAsleep until the store's pod is ready.
func (v *Volume) Wake(ctx context.Context, holder string) error {
	life := v.Life
	if life == 0 {
		life = WakeRequestLife
	}
	d, err := v.request(ctx, holder, v.now().Add(life).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	// A store put to sleep may still count its stopping pod as ready.
	if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
		if err = v.start(ctx, d); err != nil {
			return err
		}
		return provisioner.ErrVolumeAsleep
	}
	if d.Status.ReadyReplicas < 1 {
		return provisioner.ErrVolumeAsleep
	}
	return nil
}

// Done takes holder's wake request away, and puts the store back to the replicas its chart keeps
// between uses once no holder's is left. On a node's own storage that is none, which detaches the
// disk again, so that Proxmox VE can migrate the node VMs and a node can go into maintenance.
func (v *Volume) Done(ctx context.Context, holder string) error {
	d, err := v.request(ctx, holder, nil)
	if err != nil {
		return err
	}
	return v.sleep(ctx, d)
}

// Sleeper puts the backup volume's store back to sleep every Every once no holder's request is in
// force, as when a holder never let go or its request ran out. Done does it at once for a holder
// that lets go. It runs on the controller's leader alone.
type Sleeper struct {
	Volume *Volume
	Every  time.Duration
}

// Start runs until ctx ends. A management cluster without the store, or a controller not allowed
// to read it, has nothing to put to sleep.
func (s *Sleeper) Start(ctx context.Context) error {
	tick := time.NewTicker(s.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		err := s.Volume.sleep(ctx, nil)
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) && ctx.Err() == nil && s.Volume.Log != nil {
			s.Volume.Log.Warn("the backup volume's store could not be put to sleep", "error", err)
		}
	}
}

// sleep scales the store to its idle replicas unless a holder's request is in force. d is the
// Deployment as last read, or nil to read it.
func (v *Volume) sleep(ctx context.Context, d *appsv1.Deployment) error {
	// The scale holds only while the Deployment is as read, so that a holder asking in between
	// keeps the store awake. Any other change, as of its status, is read again.
	var err error
	return retry.RetryOnConflict(retry.DefaultRetry, func() error { //nolint:wrapcheck // rest says what failed
		if d == nil {
			if d, err = v.Kube.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{}); err != nil {
				return fmt.Errorf("the backup volume's store: %w", err)
			}
		}
		read := d
		d = nil
		return v.rest(ctx, read)
	})
}

// rest scales the store to its idle replicas as d reads it, unless a holder's request is in force.
func (v *Volume) rest(ctx context.Context, d *appsv1.Deployment) error {
	idle, ok := d.Annotations[IdleReplicas]
	if !ok || WakeRequested(d, v.now()) {
		return nil
	}
	n, err := strconv.ParseInt(idle, 10, 32)
	if err != nil {
		return fmt.Errorf("the backup volume's store: %s %q is no number", IdleReplicas, idle)
	}
	if d.Spec.Replicas != nil && int64(*d.Spec.Replicas) == n {
		return nil
	}
	return v.scale(ctx, d.ResourceVersion, n)
}

// start starts the sleeping store. Its pod must land on a node VM on the Proxmox VE node that holds
// the disk, so the nodes' zone labels are set first.
func (v *Volume) start(ctx context.Context, d *appsv1.Deployment) error {
	if v.VMs == nil {
		return errors.New("the backup volume's store sleeps, and this client cannot read where the management VMs run to wake it")
	}
	unknown, err := FollowZones(ctx, v.Kube, v.VMs, d.Annotations[VolumeZone], v.Log)
	if err != nil {
		return err
	}
	if len(unknown) > 0 && v.Log != nil {
		v.Log.Warn("Proxmox VE lists no VM for these management nodes, whose zone labels stay as they are", "nodes", unknown)
	}
	return v.scale(ctx, "", 1)
}

// IdleReplicas annotates the store's Deployment with the replicas it keeps between uses. The storage
// chart sets it: 0 on a node's own storage, 1 on shared storage.
const IdleReplicas = "kubehenge.dev/idle-replicas"

// scale sets the store's replicas through the Deployment's scale, which changes nothing else of it.
// version, where set, refuses the change once the Deployment changed since.
func (v *Volume) scale(ctx context.Context, version string, replicas int64) error {
	s := map[string]any{"spec": map[string]any{"replicas": replicas}}
	if version != "" {
		s["metadata"] = map[string]any{"resourceVersion": version}
	}
	patch, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("scaling the backup volume's store to %d: %w", replicas, err)
	}
	_, err = v.Kube.AppsV1().Deployments(Namespace).Patch(ctx, Service, types.MergePatchType, patch, metav1.PatchOptions{}, "scale")
	if err != nil {
		return fmt.Errorf("scaling the backup volume's store to %d: %w", replicas, err)
	}
	return nil
}

// request sets or takes away holder's wake request, and returns the Deployment as it is after.
func (v *Volume) request(ctx context.Context, holder string, value any) (*appsv1.Deployment, error) {
	base := v.Annotation
	if base == "" {
		base = WakeRequestAnnotation
	}
	key := WakeRequestKey(base, holder)
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
	if err != nil {
		return nil, fmt.Errorf("asking for the backup volume: %w", err)
	}
	d, err := v.Kube.AppsV1().Deployments(Namespace).Patch(ctx, Service, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("asking for the backup volume: %w", err)
	}
	return d, nil
}

func (v *Volume) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
