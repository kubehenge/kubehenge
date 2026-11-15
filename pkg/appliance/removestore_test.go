package appliance

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

const shortStop = 50 * time.Millisecond

func storeObjects(replicas int32) []runtime.Object {
	labels := map[string]string{"app": backupstore.Service}
	return []runtime.Object{
		&appsv1.Deployment{
			Name: backupstore.Service, Namespace: backupstore.Namespace,
			Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}},
		},
		&corev1.PersistentVolumeClaim{
			Name: backupstore.Claim, Namespace: backupstore.Namespace,
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: backupstore.Claim},
		},
	}
}

func storePod() *corev1.Pod {
	return &corev1.Pod{Name: "store-0", Namespace: backupstore.Namespace, Labels: map[string]string{"app": backupstore.Service}}
}

func attachment(pv string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		Name: "csi-0",
		Spec: storagev1.VolumeAttachmentSpec{NodeName: "cp-0", Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv}},
	}
}

// A destroy starts only once the store is stopped, with no pod left and its volume detached.
func TestStopStoreScalesTheStoreToZero(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(1), attachment("another-volume"))...)
	if err := stopStore(t.Context(), &Job{}, kube, shortStop); err != nil {
		t.Fatal(err)
	}
	d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 0 {
		t.Fatalf("replicas %d, want 0", *d.Spec.Replicas)
	}
}

func TestStopStoreWaitsForThePod(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(1), storePod())...)
	err := stopStore(t.Context(), &Job{}, kube, shortStop)
	if err == nil || !strings.Contains(err.Error(), "pod store-0 is still there") {
		t.Fatalf("error %v, want the pod named", err)
	}
}

func TestStopStoreWaitsForTheDetach(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(append(storeObjects(0), attachment(backupstore.Claim))...)
	err := stopStore(t.Context(), &Job{}, kube, shortStop)
	if err == nil || !strings.Contains(err.Error(), "still attached to node cp-0") {
		t.Fatalf("error %v, want the attachment named", err)
	}
}

func TestStopStoreWithoutAStore(t *testing.T) {
	t.Parallel()
	if err := stopStore(t.Context(), &Job{}, fake.NewClientset(), shortStop); err != nil {
		t.Fatal(err)
	}
}
