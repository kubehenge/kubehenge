package provisioner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const pinnedLater = "v1.99.9+rke2r1"

func pinStore(t *testing.T, objects ...client.Object) *PinStore {
	t.Helper()
	kube := refreshKube(t, objects...)
	return &PinStore{Reader: kube, Writer: kube, Namespace: "kubehenge-system"}
}

func TestPinStoreAddsAPinAndSaysWhoAddedIt(t *testing.T) {
	t.Parallel()
	s := pinStore(t)
	pin := ReleasePin{File: strings.Repeat("a", 64), Script: strings.Repeat("b", 64)}
	if err := s.Add(context.Background(), pinnedLater, pin, "admin@pve", time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	added, err := s.Added(context.Background())
	if err != nil || added[pinnedLater] != pin {
		t.Fatalf("added %v, %v", added, err)
	}
	cm := &corev1.ConfigMap{}
	if err := s.Reader.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: ReleasePinsName}, cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data["pins"], "# added by admin@pve at 2026-11-07T12:00:00Z") {
		t.Errorf("the pin does not say who added it: %q", cm.Data["pins"])
	}
}

func TestPinStoreRefusesWhatIsNoReleaseOrAlreadyPinned(t *testing.T) {
	t.Parallel()
	s := pinStore(t)
	pin := ReleasePin{File: strings.Repeat("a", 64), Script: strings.Repeat("b", 64)}
	if err := s.Add(context.Background(), "v1.36", pin, "admin", time.Now()); !errors.Is(err, ErrNotARelease) {
		t.Errorf("a bad version gave %v", err)
	}
	var embedded string
	for v := range DefaultReleasePins() {
		embedded = v
		break
	}
	if err := s.Add(context.Background(), embedded, pin, "admin", time.Now()); !errors.Is(err, ErrPinEmbedded) {
		t.Errorf("a version this build pins gave %v", err)
	}
}

func TestPinStoreRemovesAPinOnlyWhileNoClusterRunsIt(t *testing.T) {
	t.Parallel()
	const version = "v1.99.9+rke2r1"
	pins := version + " " + strings.Repeat("a", 64) + " " + strings.Repeat("b", 64) + "\n"
	pinsMap := &corev1.ConfigMap{Namespace: "kubehenge-system", Name: ReleasePinsName, Data: map[string]string{"pins": pins}}
	running := settledCluster("web", version, nil)
	s := pinStore(t, pinsMap, running)
	if err := s.Remove(context.Background(), version); !errors.Is(err, ErrPinInUse) {
		t.Fatalf("a release a cluster runs gave %v", err)
	}
	if err := s.Reader.Get(context.Background(), client.ObjectKeyFromObject(running), running); err != nil {
		t.Fatal(err)
	}
	if err := s.Writer.Delete(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), version); err != nil {
		t.Fatalf("removing an unused pin: %v", err)
	}
	if added, _ := s.Added(context.Background()); len(added) != 0 {
		t.Errorf("still pinned: %v", added)
	}
	if err := s.Remove(context.Background(), version); !errors.Is(err, ErrNoPin) {
		t.Errorf("removing it twice gave %v", err)
	}
}

func TestReleaseChecksumsDigestsRefuseABadVersionAndAMissingChecksumFile(t *testing.T) {
	t.Parallel()
	r, _ := newReleaseChecksums(t)
	if _, err := r.Digests(context.Background(), "1.36.6"); !errors.Is(err, ErrNotARelease) {
		t.Errorf("a bad version gave %v", err)
	}
	if _, err := r.Digests(context.Background(), "v1.99.9+rke2r1"); !errors.Is(err, errUnlisted) {
		t.Errorf("a release without a checksum file gave %v", err)
	}
}

// The patch hook installs a release only an administrator pinned, with the digests they pinned.
func TestThePatchHookReadsThePinsAnAdministratorAdded(t *testing.T) {
	t.Parallel()
	pin := ReleasePin{File: strings.Repeat("a", 64), Script: strings.Repeat("b", 64)}
	store := pinStore(t)
	r := &ReleaseChecksums{Pins: ReleasePins{}, Store: store}
	item := releaseItem(t, "RKE2ControlPlaneTemplate", pinnedLater, pinnedLater)
	if _, err := r.patch(t.Context(), item, cluster()); err == nil || !strings.Contains(err.Error(), "pins no rke2 release "+pinnedLater) {
		t.Fatalf("a release nobody pinned gave %v", err)
	}
	if err := store.Add(t.Context(), pinnedLater, pin, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	patch, err := r.patch(t.Context(), item, cluster())
	if err != nil {
		t.Fatalf("the release an administrator pinned: %v", err)
	}
	if !strings.Contains(string(patch), pin.File+" "+pin.Script) {
		t.Errorf("the patch does not carry the pinned digests: %s", patch)
	}
}

func TestChannelsAndTheValidatorAdmitAReleaseAnAdministratorPinned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"stable","latest":"v1.36.6+rke2r1"},{"id":"v1.36","latest":"v1.36.6+rke2r1"}]}`))
	}))
	defer srv.Close()
	const newer = "v1.36.6+rke2r1"
	store := pinStore(t)
	embedded := ReleasePins{"v1.36.5+rke2r1": {}}
	s := &ChannelServer{HTTP: srv.Client(), URLs: map[string]string{"rke2": srv.URL}, Pins: embedded, Store: store}
	ch, err := s.Channels(context.Background(), "rke2", templates.DefaultNodeOS)
	if err != nil || ch.Latest[36].Raw != "v1.36.5+rke2r1" {
		t.Fatalf("before the pin: %+v, %v", ch, err)
	}
	pin := ReleasePin{File: strings.Repeat("a", 64), Script: strings.Repeat("b", 64)}
	if err = store.Add(context.Background(), newer, pin, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ch, err = s.Channels(context.Background(), "rke2", templates.DefaultNodeOS); err != nil || ch.Latest[36].Raw != newer {
		t.Errorf("after the pin the channel offers %+v, %v", ch, err)
	}
	h, _ := hooksFor(t)
	h.Releases = &ReleaseChecksums{Pins: embedded, Store: store}
	v := &ClusterValidator{Hooks: h}
	c := cluster()
	c.Spec.Topology.Version = newer
	if errs := v.validatePinned(context.Background(), nil, c); len(errs) != 0 {
		t.Errorf("the validator refused the pinned release: %v", errs)
	}
	c.Spec.Topology.Version = "v1.36.7+rke2r1"
	if errs := v.validatePinned(context.Background(), nil, c); len(errs) != 1 {
		t.Errorf("the validator admitted a release nobody pinned: %v", errs)
	}
}

// The admission check and the patch hook go by the pins in the ConfigMap as well as this build's.
func TestAnAddedPinIsPinnedForTheNodesOfAReleaseThisBuildLacks(t *testing.T) {
	t.Parallel()
	pin := ReleasePin{File: strings.Repeat("a", 64), Script: strings.Repeat("b", 64)}
	store := pinStore(t)
	r := &ReleaseChecksums{Pins: ReleasePins{}, Store: store}
	before, err := r.pins(context.Background())
	if err != nil || before.pinned(pinnedLater) {
		t.Fatalf("before: %v, %v", before, err)
	}
	if err = store.Add(context.Background(), pinnedLater, pin, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := r.pins(context.Background())
	if err != nil || !after.pinned(pinnedLater) {
		t.Fatalf("after: %v, %v", after, err)
	}
}
