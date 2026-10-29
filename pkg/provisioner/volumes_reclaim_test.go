package provisioner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reclaimRig is a Reclaimer over the management cluster's clusters, each with a workload cluster
// holding a released volume of owner VM 4242 on node a, and a Proxmox VE that holds it.
type reclaimRig struct {
	r    *Reclaimer
	kube client.WithWatch
	pve  *fakePVE

	mu     sync.Mutex
	tokens []string
}

func releasedVolume(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		Name: name,
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: csiDriver, VolumeHandle: "lab/a/local/4242/vm-4242-" + name + ".raw",
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
}

func clusterNamed(name string) *clusterv1.Cluster {
	c := cluster()
	c.Name = name
	return c
}

// tokenOf is the token a cluster's credentials carry, told apart by the cluster's name.
func tokenOf(c *clusterv1.Cluster) Credentials {
	return Credentials{TokenID: webNames.TokenID(), TokenSecret: "secret-of-" + c.Name, VolumeOwner: 4242, VolumeOwnerNode: "a"}
}

// newReclaimRig makes the rig; workloads fail listing for the clusters named in failing.
func newReclaimRig(t *testing.T, clusters map[*clusterv1.Cluster]bool, failing ...string) *reclaimRig {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	volumes := make([]string, 0, len(clusters))
	objects := make([]client.Object, 0, 2*len(clusters))
	remotes := &Remotes{byCluster: map[string]remote{}}
	for c, ready := range clusters {
		objects = append(objects, c, credentialsSecret(c, tokenOf(c), ready))
		volumes = append(volumes, "local:4242/vm-4242-pvc-"+c.Name+".raw")
		built := fake.NewClientBuilder().WithScheme(scheme).WithObjects(releasedVolume("pvc-" + c.Name)).Build()
		var workload client.Client = built
		if slices.Contains(failing, c.Name) {
			workload = interceptor.NewClient(built, interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("the API server is restarting")
				},
			})
		}
		remotes.byCluster[remoteKey(c)] = remote{client: workload, until: time.Now().Add(time.Hour)}
	}
	slices.Sort(volumes)
	_, kube := newReconciler(t, &fakeCloud{}, objects...)
	rig := &reclaimRig{kube: watching(t, kube), pve: newFakePVE("a", map[string][]string{"a/local": volumes})}
	pve := serve(t, rig.pve)
	remotes.Reader = kube
	rig.r = &Reclaimer{
		Client: kube, ClassNamespace: "kubehenge-system", Remotes: remotes, Interval: time.Hour,
		Connect: func(token string) (*proxmox.Client, error) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.tokens = append(rig.tokens, token)
			return pve, nil
		},
	}
	return rig
}

func (rig *reclaimRig) signedInAs() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return slices.Sorted(slices.Values(rig.tokens))
}

func (rig *reclaimRig) volumesLeft() []string {
	rig.pve.mu.Lock()
	defer rig.pve.mu.Unlock()
	return slices.Clone(rig.pve.volumes["a/local"])
}

func (rig *reclaimRig) holds(c *clusterv1.Cluster) bool {
	rig.r.Remotes.mu.Lock()
	defer rig.r.Remotes.mu.Unlock()
	_, ok := rig.r.Remotes.byCluster[remoteKey(c)]
	return ok
}

// Each pass reclaims the released volumes of every cluster Kubehenge built whose objects are all
// in place, with that cluster's own token, and of no other.
func TestReclaimAllReclaimsOnlyReadyOwnedClustersWithTheirOwnToken(t *testing.T) {
	t.Parallel()
	web, pending, foreign, going := clusterNamed("web"), clusterNamed("pending"), clusterNamed("foreign"), clusterNamed("going")
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	going.Finalizers, going.DeletionTimestamp = []string{Finalizer}, &metav1.Time{Time: time.Now()}
	rig := newReclaimRig(t, map[*clusterv1.Cluster]bool{web: true, pending: false, foreign: true, going: true})
	nothing := clusterNamed("no-credentials")
	if err := rig.kube.Create(t.Context(), nothing); err != nil {
		t.Fatal(err)
	}

	rig.r.reclaimAll(t.Context())
	if got := rig.signedInAs(); !slices.Equal(got, []string{webNames.TokenID() + "=secret-of-web"}) {
		t.Errorf("signed in as %v", got)
	}
	left := rig.volumesLeft()
	if slices.Contains(left, "local:4242/vm-4242-pvc-web.raw") || len(left) != 3 {
		t.Errorf("volumes left %v; want all but web's", left)
	}
}

// A cluster whose reclaim fails has its client dropped, and the pass goes on to the next.
func TestReclaimAllGoesOnPastAClusterThatFails(t *testing.T) {
	t.Parallel()
	broken, web := clusterNamed("broken"), clusterNamed("web")
	rig := newReclaimRig(t, map[*clusterv1.Cluster]bool{broken: true, web: true}, "broken")
	rig.r.reclaimAll(t.Context())
	if left := rig.volumesLeft(); !slices.Equal(left, []string{"local:4242/vm-4242-pvc-broken.raw"}) {
		t.Errorf("volumes left %v", left)
	}
	if rig.holds(broken) || !rig.holds(web) {
		t.Errorf("clients kept: broken %v, web %v", rig.holds(broken), rig.holds(web))
	}

	rig = newReclaimRig(t, map[*clusterv1.Cluster]bool{broken: true}, "broken")
	if err := rig.r.reclaim(t.Context(), broken); err == nil || !strings.Contains(err.Error(), "listing persistent volumes") {
		t.Errorf("reclaiming the broken cluster: %v", err)
	}
}

func TestReclaimReportsCredentialsItCannotRead(t *testing.T) {
	t.Parallel()
	web := clusterNamed("web")
	rig := newReclaimRig(t, map[*clusterv1.Cluster]bool{web: true})
	stored := credentialsSecret(web, tokenOf(web), true)
	stored.Data["volumeOwner"] = []byte("not a number")
	if err := rig.kube.Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if err := rig.r.reclaim(t.Context(), web); err == nil || !strings.Contains(err.Error(), "volumeOwner") {
		t.Errorf("unreadable credentials: %v", err)
	}

	rig.r.Client = interceptor.NewClient(rig.kube, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("etcd is out of space")
		},
	})
	if err := rig.r.reclaim(t.Context(), web); err == nil || !strings.Contains(err.Error(), "reading the cluster's credentials") {
		t.Errorf("credentials that cannot be read: %v", err)
	}
	if got := rig.signedInAs(); len(got) != 0 {
		t.Errorf("signed in as %v", got)
	}
}

// A cluster whose kubeconfig Cluster API has not written yet has nothing to reclaim yet.
func TestReclaimWaitsForTheKubeconfig(t *testing.T) {
	t.Parallel()
	web := clusterNamed("web")
	rig := newReclaimRig(t, map[*clusterv1.Cluster]bool{web: true})
	rig.r.Remotes = &Remotes{Reader: rig.kube}
	if err := rig.r.reclaim(t.Context(), web); err != nil {
		t.Errorf("without a kubeconfig: %v", err)
	}
	if got := rig.signedInAs(); len(got) != 0 {
		t.Errorf("signed in as %v", got)
	}
}

// Started by the manager, the reclaimer looks at once, and returns once the manager stops it.
func TestReclaimerStartLooksAtOnceAndStopsWithItsContext(t *testing.T) {
	t.Parallel()
	web := clusterNamed("web")
	rig := newReclaimRig(t, map[*clusterv1.Cluster]bool{web: true})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rig.r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rig.signedInAs(); len(got) != 1 {
		t.Errorf("the first pass signed in as %v", got)
	}
}
