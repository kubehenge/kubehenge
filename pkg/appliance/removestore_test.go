package appliance

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

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

// A removal stops the store before its destroy, which would otherwise uninstall the CSI plugin
// under a pod still mounting the volume.
func TestRemoveStopsTheStoreBeforeTheDestroy(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	management := fake.NewClientset(storeObjects(1)...)
	stoppedAfter := -1
	management.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		stoppedAfter = len(tofuCalls(t, a))
		return false, nil, nil
	})
	a.ManagementClient = func([]byte) (kubernetes.Interface, error) { return management, nil }
	if err := a.Remove("correct horse battery staple", KeepBackups, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("remove: %+v", j)
	}
	d, err := management.AppsV1().Deployments(backupstore.Namespace).Get(t.Context(), backupstore.Service, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 0 {
		t.Fatalf("the store has %d replicas after the removal, want 0", *d.Spec.Replicas)
	}
	if stoppedAfter < 0 {
		t.Fatal("the store was never stopped")
	}
	for i, args := range tofuCalls(t, a) {
		if slices.Contains(args, "-destroy") && i < stoppedAfter {
			t.Fatalf("the destroy (tofu call %d) ran before the store was stopped (after call %d)", i, stoppedAfter)
		}
	}
}

func TestStopStoreWithoutAStore(t *testing.T) {
	t.Parallel()
	if err := stopStore(t.Context(), &Job{}, fake.NewClientset(), shortStop); err != nil {
		t.Fatal(err)
	}
}
