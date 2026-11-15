package backupstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// WakeRequestAnnotation begins the annotations on the store's Deployment that ask the appliance to
// wake the store, and to keep it awake, with the time each was asked in RFC 3339. The controller
// sets one for each holder (WakeRequestKey) while it uses the store, and takes it away once done.
const WakeRequestAnnotation = "kubehenge.dev/wake-requested"

// InterfaceWakeAnnotation begins the interface's own wake requests, for a look at the clusters'
// snapshots, one for each holder as the controller's.
const InterfaceWakeAnnotation = "kubehenge.dev/interface-wake-requested"

// WakeRequestLife is how long a request holds the store awake unless it is asked again. It outlasts
// one snapshot, so that a controller gone mid-snapshot leaves the store to sleep again.
const WakeRequestLife = provisioner.EtcdSnapshotTimeout + 10*time.Minute

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
		if at, err := time.Parse(time.RFC3339, value); err == nil && now.Before(at.Add(WakeRequestLife)) {
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
	Now        func() time.Time
}

var _ provisioner.BackupVolume = (*Volume)(nil)

// Open asks for the store to be awake on behalf of holder, and returns it once it is. A sleeping
// store answers provisioner.ErrVolumeAsleep: the appliance wakes it on the request.
func (v *Volume) Open(ctx context.Context, holder string) (provisioner.VolumeStore, error) { //nolint:ireturn // tests stand in for the store
	if err := v.request(ctx, holder, v.now().UTC().Format(time.RFC3339)); err != nil {
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

// Done takes holder's wake request away. The appliance puts the store back to sleep once no
// holder's is left.
func (v *Volume) Done(ctx context.Context, holder string) error {
	return v.request(ctx, holder, nil)
}

func (v *Volume) request(ctx context.Context, holder string, value any) error {
	base := v.Annotation
	if base == "" {
		base = WakeRequestAnnotation
	}
	key := WakeRequestKey(base, holder)
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
