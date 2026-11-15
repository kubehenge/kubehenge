package backupstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// WakeRequestAnnotation on the store's Deployment asks the appliance to wake the store, and to keep
// it awake, with the time it was asked in RFC 3339. The controller sets it while it stores etcd
// snapshots there and takes it away once done.
const WakeRequestAnnotation = "kubehenge.dev/wake-requested"

// InterfaceWakeAnnotation is the interface's own wake request, for a look at the clusters'
// snapshots. It runs out by itself, and taking the controller's away leaves it in force.
const InterfaceWakeAnnotation = "kubehenge.dev/interface-wake-requested"

// WakeRequestLife is how long a request holds the store awake unless it is asked again. It outlasts
// one snapshot, so that a controller gone mid-snapshot leaves the store to sleep again.
const WakeRequestLife = provisioner.EtcdSnapshotTimeout + 10*time.Minute

// WakeRequested tells whether d carries a wake request still in force at now.
func WakeRequested(d *appsv1.Deployment, now time.Time) bool {
	for _, key := range []string{WakeRequestAnnotation, InterfaceWakeAnnotation} {
		if at, err := time.Parse(time.RFC3339, d.Annotations[key]); err == nil && now.Before(at.Add(WakeRequestLife)) {
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
	// Annotation carries the request, WakeRequestAnnotation where empty.
	Annotation string
	Now        func() time.Time
}

var _ provisioner.BackupVolume = (*Volume)(nil)

// Open asks for the store to be awake, and returns it once it is. A sleeping store answers
// provisioner.ErrVolumeAsleep: the appliance wakes it on the request.
func (v *Volume) Open(ctx context.Context) (provisioner.VolumeStore, error) { //nolint:ireturn // tests stand in for the store
	if err := v.request(ctx, v.now().UTC().Format(time.RFC3339)); err != nil {
		return nil, err
	}
	d, err := v.Kube.AppsV1().Deployments(Namespace).Get(ctx, Service, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("the backup volume's store: %w", err)
	}
	if d.Status.ReadyReplicas < 1 {
		return nil, provisioner.ErrVolumeAsleep
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

// Done takes the wake request away, so that the appliance may put the store back to sleep.
func (v *Volume) Done(ctx context.Context) error {
	return v.request(ctx, nil)
}

func (v *Volume) request(ctx context.Context, value any) error {
	key := v.Annotation
	if key == "" {
		key = WakeRequestAnnotation
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}})
	if err != nil {
		return fmt.Errorf("asking for the backup volume: %w", err)
	}
	if _, err := v.Kube.AppsV1().Deployments(Namespace).Patch(ctx, Service, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("asking for the backup volume: %w", err)
	}
	return nil
}

func (v *Volume) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
