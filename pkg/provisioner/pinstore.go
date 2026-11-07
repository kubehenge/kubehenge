package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReleasePinsName is the ConfigMap, in the namespace of Kubehenge's ClusterClasses, that holds the
// release pins an administrator added beside the ones this build carries. Its key "pins" is a pins
// file: a line "<version> <checksum file's SHA-256> <install script's SHA-256>" for each release,
// which may end in a comment saying who added it.
const ReleasePinsName = "kubehenge-release-pins"

const releasePinsKey = "pins"

// Errors a pin change is refused with.
var (
	ErrNotARelease = errors.New("not an RKE2 or k3s release")
	ErrPinInUse    = errors.New("a cluster runs it")
	ErrPinEmbedded = errors.New("this Kubehenge release carries it")
	ErrNoPin       = errors.New("not pinned by an administrator")
)

// PinStore reads and changes the pins administrators added.
type PinStore struct {
	Reader client.Reader
	// Writer is needed only to add or remove a pin.
	Writer    client.Writer
	Namespace string
}

// Added are the pins administrators added, none when the ConfigMap is not there.
func (s *PinStore) Added(ctx context.Context) (ReleasePins, error) {
	cm := &corev1.ConfigMap{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ReleasePinsName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return ReleasePins{}, nil
		}
		return nil, fmt.Errorf("reading ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	pins, err := ParseReleasePins(cm.Data[releasePinsKey])
	if err != nil {
		return nil, fmt.Errorf("ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	return pins, nil
}

// Add records the pin of version with who added it and when. It is refused for a version that is
// no RKE2 or k3s release and for one this build already pins.
func (s *PinStore) Add(ctx context.Context, version string, pin ReleasePin, by string, at time.Time) error {
	if distributionReleaseOf(version) == nil {
		return fmt.Errorf("%q: %w such as v1.36.6+rke2r1 or v1.36.6+k3s1", version, ErrNotARelease)
	}
	if DefaultReleasePins().pinned(version) {
		return fmt.Errorf("%s: %w", version, ErrPinEmbedded)
	}
	cm := &corev1.ConfigMap{}
	err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ReleasePinsName}, cm)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return fmt.Errorf("reading ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	file := cm.Data[releasePinsKey]
	if _, err = ParseReleasePins(file); err != nil {
		return fmt.Errorf("ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	if file != "" && !strings.HasSuffix(file, "\n") {
		file += "\n"
	}
	file += fmt.Sprintf("%s %s %s # added by %s at %s\n", version, pin.File, pin.Script, strings.Join(strings.Fields(by), "_"), at.UTC().Format(time.RFC3339))
	if _, err = ParseReleasePins(file); err != nil {
		return err
	}
	cm.Namespace, cm.Name = s.Namespace, ReleasePinsName
	cm.Data = map[string]string{releasePinsKey: file}
	if create {
		err = s.Writer.Create(ctx, cm)
	} else {
		err = s.Writer.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("saving ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	return nil
}

// Remove deletes the pin of version, unless a cluster runs the release.
func (s *PinStore) Remove(ctx context.Context, version string) error {
	cm := &corev1.ConfigMap{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ReleasePinsName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s: %w", version, ErrNoPin)
		}
		return fmt.Errorf("reading ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	var kept []string
	found := false
	for line := range strings.Lines(cm.Data[releasePinsKey]) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == version {
			found = true
			continue
		}
		kept = append(kept, line)
	}
	if !found {
		return fmt.Errorf("%s: %w", version, ErrNoPin)
	}
	running, err := s.runningCluster(ctx, version)
	if err != nil {
		return err
	}
	if running != "" {
		return fmt.Errorf("%s: %w (%s)", version, ErrPinInUse, running)
	}
	cm.Data = map[string]string{releasePinsKey: strings.Join(kept, "")}
	if err := s.Writer.Update(ctx, cm); err != nil {
		return fmt.Errorf("saving ConfigMap %s/%s: %w", s.Namespace, ReleasePinsName, err)
	}
	return nil
}

// runningCluster names a cluster that runs version, as its topology or any of its machines, or "".
func (s *PinStore) runningCluster(ctx context.Context, version string) (string, error) {
	var clusters clusterv1.ClusterList
	if err := s.Reader.List(ctx, &clusters); err != nil {
		return "", fmt.Errorf("listing the clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		versions := []string{c.Spec.Topology.Version}
		if cp := c.Status.ControlPlane; cp != nil {
			for _, v := range cp.Versions {
				versions = append(versions, v.Version)
			}
		}
		if w := c.Status.Workers; w != nil {
			for _, v := range w.Versions {
				versions = append(versions, v.Version)
			}
		}
		if slices.Contains(versions, version) {
			return c.Namespace + "/" + c.Name, nil
		}
	}
	return "", nil
}

// Digests reads the checksum file and install script of an RKE2 or k3s release as they are now,
// for an administrator to see before pinning them. It is refused for a version that is no release
// and for one whose checksum file is missing or lists a file its nodes install.
func (r *ReleaseChecksums) Digests(ctx context.Context, version string) (ReleasePin, error) {
	release := distributionReleaseOf(version)
	if release == nil {
		return ReleasePin{}, fmt.Errorf("%q: %w such as v1.36.6+rke2r1 or v1.36.6+k3s1", version, ErrNotARelease)
	}
	sums, err := r.read(ctx, release, version)
	if err != nil {
		if errors.Is(err, errUnlisted) {
			return ReleasePin{}, fmt.Errorf("%s has no usable checksum file: %w", version, err)
		}
		return ReleasePin{}, err
	}
	return ReleasePin(sums), nil
}

// pins are the pins a node may install: this build's, then the administrators'.
func (r *ReleaseChecksums) pins(ctx context.Context) (ReleasePins, error) {
	if r.Store == nil {
		return r.Pins, nil
	}
	added, err := r.Store.Added(ctx)
	if err != nil {
		return nil, err
	}
	all := ReleasePins{}
	for v, p := range r.Pins {
		all[v] = p
	}
	for v, p := range added {
		all[v] = p
	}
	return all, nil
}
