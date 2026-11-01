package provisioner

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// gatewayAPIFiles holds upstream's standard-channel install file of the Gateway API release the
// controller carries, as `make gateway-api` downloads and checks it.
//
//go:embed all:gatewayapi
var gatewayAPIFiles embed.FS

const (
	// bundleVersionAnnotation names the Gateway API release each of its objects comes from.
	bundleVersionAnnotation = "gateway.networking.k8s.io/bundle-version"
	// gatewayAPIAnnotation records on a Cluster the oldest Gateway API release among the CRDs its
	// workload cluster has, once it has every one Kubehenge applies, or once the distribution's own
	// Gateway API chart is installed and holds them.
	gatewayAPIAnnotation = "kubehenge.dev/gateway-api"
	// gatewayAPIInterval is how often a workload cluster's Gateway API is looked at: a distribution
	// that stops shipping the CRDs does so in an upgrade, which is seen only in the workload cluster.
	gatewayAPIInterval = time.Minute
	// Where Helm marks the objects of a release, and the Secrets it keeps each revision in.
	helmReleaseName      = "meta.helm.sh/release-name"
	helmReleaseNamespace = "meta.helm.sh/release-namespace"
	// maxHelmRelease bounds a release decompressed: a Secret holds at most 1 MiB, and gzip shrinks
	// a manifest of CRDs about tenfold.
	maxHelmRelease = 64 << 20
)

var (
	crdGVK       = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	helmChartGVK = schema.GroupVersionKind{Group: "helm.cattle.io", Version: "v1", Kind: "HelmChart"}
	// The kinds upstream's install file holds: its CRDs, and the admission policy that refuses
	// experimental CRDs over standard ones and releases older than its own minimum.
	gatewayAPIKinds = []schema.GroupVersionKind{
		crdGVK,
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"},
	}
)

// GatewayAPI is one release of the Gateway API's standard channel, as upstream publishes its install
// file: the CRDs and their admission policy, in the order they are applied.
type GatewayAPI struct {
	// Version is the release, as its tag names it.
	Version string
	release *version.Version
	objects []gatewayAPIObject
}

// gatewayAPIObject is one object of the install file, kept as its document until it is applied.
type gatewayAPIObject struct {
	gvk  schema.GroupVersionKind
	name string
	doc  []byte
}

// LoadGatewayAPI reads the Gateway API release the controller was built with.
func LoadGatewayAPI() (*GatewayAPI, error) {
	found, err := fs.Glob(gatewayAPIFiles, "gatewayapi/standard-install-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("finding the Gateway API's install file: %w", err)
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("the controller carries %d Gateway API install files, not one: build it with make", len(found))
	}
	raw, err := gatewayAPIFiles.ReadFile(found[0])
	if err != nil {
		return nil, fmt.Errorf("reading the Gateway API's install file: %w", err)
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(path.Base(found[0]), "standard-install-"), ".yaml")
	return parseGatewayAPI(tag, raw)
}

// manifestMetadata is what a manifest's object says of itself.
type manifestMetadata struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

// manifestHead is what tells the objects of a manifest apart.
type manifestHead struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   manifestMetadata `json:"metadata"`
}

// manifestObjects splits a manifest into its documents, with what each holds. Documents that hold
// only comments are left out.
func manifestObjects(manifest []byte) ([]manifestHead, [][]byte, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	var (
		heads []manifestHead
		docs  [][]byte
	)
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return heads, docs, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("splitting the manifest: %w", err)
		}
		var head manifestHead
		if err := yaml.Unmarshal(doc, &head); err != nil {
			return nil, nil, fmt.Errorf("reading a document of the manifest: %w", err)
		}
		if head.Kind == "" {
			continue
		}
		heads, docs = append(heads, head), append(docs, doc)
	}
}

// parseGatewayAPI reads upstream's standard-channel install file of the release tag, every object
// of which has to name that release.
func parseGatewayAPI(tag string, raw []byte) (*GatewayAPI, error) {
	v, err := version.ParseSemantic(tag)
	if err != nil {
		return nil, fmt.Errorf("the Gateway API's release %q: %w", tag, err)
	}
	heads, docs, err := manifestObjects(raw)
	if err != nil {
		return nil, fmt.Errorf("the Gateway API's install file: %w", err)
	}
	g := &GatewayAPI{Version: tag, release: v}
	crds := 0
	for i, h := range heads {
		gvk := schema.FromAPIVersionAndKind(h.APIVersion, h.Kind)
		if !slices.Contains(gatewayAPIKinds, gvk) {
			return nil, fmt.Errorf("the Gateway API's install file holds a %s, which Kubehenge does not apply", gvk)
		}
		if got := h.Metadata.Annotations[bundleVersionAnnotation]; got != tag {
			return nil, fmt.Errorf("the Gateway API's %s %s is of release %q, not %s", h.Kind, h.Metadata.Name, got, tag)
		}
		if gvk == crdGVK {
			crds++
		}
		g.objects = append(g.objects, gatewayAPIObject{gvk: gvk, name: h.Metadata.Name, doc: docs[i]})
	}
	if crds == 0 {
		return nil, errors.New("the Gateway API's install file holds no CRD")
	}
	return g, nil
}

// older reports whether a release named by an object's annotation comes before v: an object that
// names none, or none Kubehenge can read, counts as older.
func older(named string, v *version.Version) bool {
	got, err := version.ParseSemantic(named)
	return err != nil || got.LessThan(v)
}

// GatewayAPIReconciler keeps the Gateway API's CRDs in every workload cluster, with the admission
// policy upstream ships beside them, so that Gateways and HTTPRoutes work everywhere and Istio's
// waypoints can be deployed. An object of the Gateway API stays with the Helm release that marks it
// as its own while that release's latest revision ships it, and becomes Kubehenge's once it does
// not: RKE2 and k3s ship the CRDs in their Traefik CRD charts in some releases, and Helm keeps them
// when a later chart stops shipping them. A distribution release with a Gateway API chart of its own
// holds all of the Gateway API there, adopting what the Traefik CRD chart left, and Kubehenge then
// applies none of it. Kubehenge never puts an older release over a newer one.
type GatewayAPIReconciler struct {
	Client         client.Client
	ClassNamespace string
	// Remotes reaches the workload clusters.
	Remotes *Remotes
	// Bundle is the release Kubehenge applies.
	Bundle *GatewayAPI

	mu sync.Mutex
	// shipped holds what each Helm release's latest revision ships, by Cluster and release, so
	// that a revision is read from its Secret only once.
	shipped map[types.NamespacedName]map[string]helmShipment
	// holders is who each Cluster's Gateway API was last seen held by, so that only a change is
	// logged.
	holders map[types.NamespacedName]string
}

// helmShipment is the objects a revision of a Helm release holds, by the revision's Secret.
type helmShipment struct {
	secret  types.UID
	objects []string
}

// distributionCharts are the Helm releases in kube-system a distribution ships the Gateway API in:
// its Traefik CRD chart, which holds the CRDs in some releases, and the Gateway API chart of its
// own that later releases have.
type distributionCharts struct {
	traefikCRD, gatewayAPI string
}

// chartsOf is the charts a distribution ships the Gateway API in, if it has any.
func chartsOf(d templates.Distribution) (distributionCharts, bool) {
	switch d {
	case templates.RKE2:
		return distributionCharts{traefikCRD: "rke2-traefik-crd", gatewayAPI: "rke2-gateway-api-crd"}, true
	case templates.K3s:
		return distributionCharts{traefikCRD: "traefik-crd", gatewayAPI: "gateway-api-crd"}, true
	case templates.Kubeadm, templates.Talos:
	}
	return distributionCharts{}, false
}

// SetupWithManager registers the reconciler for Clusters.
func (r *GatewayAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("gateway-api").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the Gateway API reconciler: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler. A workload cluster is reached once its control plane
// is initialised, so that a cluster still being built returns no errors.
func (r *GatewayAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetCluster(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() {
		r.forgetCluster(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	if initialised := cluster.Status.Initialization.ControlPlaneInitialized; initialised == nil || !*initialised {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	ready, owner, err := distributionGatewayAPI(ctx, workload.client, distributionOf(cluster))
	if err != nil {
		r.Remotes.forget(cluster)
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	installed, applied, err := r.ensure(ctx, req.NamespacedName, workload.client, owner)
	if err != nil {
		r.Remotes.forget(cluster)
		return ctrl.Result{}, err
	}
	// What was applied is recorded once it is seen in the cluster.
	if applied {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	if err := r.record(ctx, cluster, installed); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: gatewayAPIInterval}, nil
}

// ensure applies each object of the bundle the workload cluster lacks, or has in an older release
// that no Helm release ships any more. While owner, the Helm release of the distribution's own
// Gateway API chart, holds the Gateway API, it applies nothing: an object that chart does not ship
// would mix a second owner's release into the distribution's. It returns the oldest release among
// the cluster's CRDs of the bundle, and whether it applied anything.
func (r *GatewayAPIReconciler) ensure(ctx context.Context, key types.NamespacedName, workload client.Client, owner string) (string, bool, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("cluster", key)
	oldest, applied := "", false
	var held []string
	for i := range r.Bundle.objects {
		o := &r.Bundle.objects[i]
		has, err := r.has(ctx, key, workload, o, owner)
		if err != nil {
			return "", false, err
		}
		if has.holder != "" {
			held = append(held, o.gvk.Kind+" "+o.name+": "+has.holder)
		}
		switch {
		case has.present && (owner != "" || has.holder != "" || !older(has.release, r.Bundle.release)):
			oldest = oldestOf(oldest, has.release, o)
		case owner == "":
			if err := applyGatewayAPIObject(ctx, workload, o); err != nil {
				return "", false, err
			}
			had := "nothing"
			if has.present {
				had = cmp.Or(has.release, "no release")
			}
			log.Info("applied an object of the Gateway API", "kind", o.gvk.Kind, "name", o.name, "release", r.Bundle.Version, "had", had)
			applied = true
		}
	}
	r.logHolders(ctx, key, owner, held)
	return oldest, applied, nil
}

// gatewayAPIHas is what a workload cluster has of an object of the bundle: whether it is there, the
// Gateway API release it names, and the Helm release, as namespace/name, whose latest revision
// ships it.
type gatewayAPIHas struct {
	present bool
	release string
	holder  string
}

// has reads what the workload cluster has of an object of the bundle. Which Helm release ships it
// is looked up only where that decides something, for an object older than the bundle, or where it
// tells which objects the distribution's own chart holds.
func (r *GatewayAPIReconciler) has(ctx context.Context, key types.NamespacedName, workload client.Client, o *gatewayAPIObject, owner string) (gatewayAPIHas, error) {
	got := &metav1.PartialObjectMetadata{}
	got.SetGroupVersionKind(o.gvk)
	if err := workload.Get(ctx, client.ObjectKey{Name: o.name}, got); err != nil {
		if apierrors.IsNotFound(err) {
			return gatewayAPIHas{}, nil
		}
		return gatewayAPIHas{}, fmt.Errorf("reading the Gateway API's %s %s: %w", o.gvk.Kind, o.name, err)
	}
	has := gatewayAPIHas{present: true, release: got.Annotations[bundleVersionAnnotation]}
	if owner == "" && !older(has.release, r.Bundle.release) {
		return has, nil
	}
	holder, err := r.shippedBy(ctx, key, workload, o.gvk.GroupKind(), got)
	has.holder = holder
	return has, err
}

// logHolders logs which Helm releases hold a cluster's Gateway API whenever that changes, so that a
// cluster whose objects Kubehenge leaves to the distribution is told apart from one it skipped.
func (r *GatewayAPIReconciler) logHolders(ctx context.Context, key types.NamespacedName, owner string, held []string) {
	seen := owner + "\n" + strings.Join(held, "\n")
	r.mu.Lock()
	previous, known := r.holders[key]
	if r.holders == nil {
		r.holders = map[types.NamespacedName]string{}
	}
	r.holders[key] = seen
	r.mu.Unlock()
	if previous == seen || (!known && owner == "" && len(held) == 0) {
		return
	}
	log := ctrl.LoggerFrom(ctx).WithValues("cluster", key)
	if owner != "" {
		log.Info("the distribution's own chart holds the Gateway API, so Kubehenge applies none of it", "release", owner, "heldBy", held)
		return
	}
	log.Info("leaving the objects of the Gateway API that Helm releases ship to those releases", "heldBy", held)
}

// oldestOf is the older of oldest and what a CRD of the bundle names, which stands for no release
// Kubehenge can read when it names none. Objects other than CRDs leave it as it is.
func oldestOf(oldest, named string, o *gatewayAPIObject) string {
	if o.gvk != crdGVK {
		return oldest
	}
	if named == "" {
		named = "unknown"
	}
	if oldest == "" {
		return named
	}
	if v, err := version.ParseSemantic(oldest); err == nil && older(named, v) {
		return named
	}
	return oldest
}

func applyGatewayAPIObject(ctx context.Context, workload client.Client, o *gatewayAPIObject) error {
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(o.doc, &u.Object); err != nil {
		return fmt.Errorf("reading the Gateway API's %s %s: %w", o.gvk.Kind, o.name, err)
	}
	// Forced, as a CRD a distribution's Helm release has left behind is that release's.
	if err := workload.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying the Gateway API's %s %s: %w", o.gvk.Kind, o.name, err)
	}
	return nil
}

// shippedBy is the Helm release, as namespace/name, that marks an object of kind gk as its own and
// whose latest revision still holds it, or "" when there is none: Helm keeps an object a chart no
// longer ships when the chart asks it to, as the distributions' Traefik CRD charts do with the
// Gateway API's CRDs, and leaves it to nobody then.
func (r *GatewayAPIReconciler) shippedBy(ctx context.Context, key types.NamespacedName, workload client.Client, gk schema.GroupKind, o *metav1.PartialObjectMetadata) (string, error) {
	release, namespace := o.Annotations[helmReleaseName], o.Annotations[helmReleaseNamespace]
	if release == "" || namespace == "" {
		return "", nil
	}
	latest, err := latestHelmRevision(ctx, workload, namespace, release)
	if err != nil || latest == nil || strings.HasPrefix(latest.Labels["status"], "uninstall") {
		return "", err
	}
	holder := namespace + "/" + release
	objects, err := r.revisionObjects(ctx, key, workload, holder, latest)
	if err != nil || !slices.Contains(objects, objectRef(gk, o.Name)) {
		return "", err
	}
	return holder, nil
}

// objectRef names an object of a Helm release by its kind and name, whichever version of its API
// the chart writes it in.
func objectRef(gk schema.GroupKind, name string) string {
	return gk.String() + "/" + name
}

// distributionGatewayAPI reports whether a distribution's charts that ship the Gateway API are
// installed, and the Helm release of its own Gateway API chart, as namespace/name, where it has one:
// that chart adopts the CRDs others left and holds all of the Gateway API, so Kubehenge leaves it
// to the chart. Kubehenge applies nothing before: Helm refuses to install a chart over objects it
// does not own, and Traefik's own chart refuses to install without its CRD chart, so objects
// applied first would leave the cluster without an ingress controller.
func distributionGatewayAPI(ctx context.Context, workload client.Client, d templates.Distribution) (bool, string, error) {
	charts, has := chartsOf(d)
	if !has {
		return true, "", nil
	}
	deployed, err := helmReleaseDeployed(ctx, workload, metav1.NamespaceSystem, charts.traefikCRD)
	if err != nil || !deployed {
		return false, "", err
	}
	// The distribution creates every HelmChart of a release in one pass over its manifests, long
	// before Helm has installed the first of them, so a Gateway API chart its release has is there
	// once the Traefik CRD chart is installed.
	chart := &metav1.PartialObjectMetadata{}
	chart.SetGroupVersionKind(helmChartGVK)
	err = workload.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: charts.gatewayAPI}, chart)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading the distribution's HelmChart %s: %w", charts.gatewayAPI, err)
	}
	deployed, err = helmReleaseDeployed(ctx, workload, metav1.NamespaceSystem, charts.gatewayAPI)
	return deployed, metav1.NamespaceSystem + "/" + charts.gatewayAPI, err
}

// helmReleaseDeployed reports whether a Helm release's latest revision is deployed.
func helmReleaseDeployed(ctx context.Context, workload client.Client, namespace, release string) (bool, error) {
	latest, err := latestHelmRevision(ctx, workload, namespace, release)
	return latest != nil && latest.Labels["status"] == "deployed", err
}

// latestHelmRevision is the Secret of a Helm release's latest revision, or nil while it has none.
func latestHelmRevision(ctx context.Context, workload client.Client, namespace, release string) (*metav1.PartialObjectMetadata, error) {
	revisions := &metav1.PartialObjectMetadataList{}
	revisions.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	err := workload.List(ctx, revisions, client.InNamespace(namespace), client.MatchingLabels{"owner": "helm", "name": release})
	if err != nil {
		return nil, fmt.Errorf("listing the revisions of Helm release %s/%s: %w", namespace, release, err)
	}
	return latestRevision(revisions.Items), nil
}

// latestRevision is the Secret of a Helm release's latest revision, the one with the highest version.
func latestRevision(secrets []metav1.PartialObjectMetadata) *metav1.PartialObjectMetadata {
	var (
		latest *metav1.PartialObjectMetadata
		newest int
	)
	for i := range secrets {
		v, err := strconv.Atoi(secrets[i].Labels["version"])
		if err == nil && (latest == nil || v > newest) {
			latest, newest = &secrets[i], v
		}
	}
	return latest
}

// revisionObjects are the objects a Helm revision holds, by objectRef, read from its Secret the
// first time.
func (r *GatewayAPIReconciler) revisionObjects(ctx context.Context, key types.NamespacedName, workload client.Client, release string, revision *metav1.PartialObjectMetadata) ([]string, error) {
	r.mu.Lock()
	known, ok := r.shipped[key][release]
	r.mu.Unlock()
	if ok && known.secret == revision.UID {
		return known.objects, nil
	}
	secret := &corev1.Secret{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: revision.Namespace, Name: revision.Name}, secret); err != nil {
		return nil, fmt.Errorf("reading the Helm release %s: %w", release, err)
	}
	objects, err := helmReleaseObjects(secret.Data["release"])
	if err != nil {
		return nil, fmt.Errorf("the Helm release %s: %w", release, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shipped == nil {
		r.shipped = map[types.NamespacedName]map[string]helmShipment{}
	}
	if r.shipped[key] == nil {
		r.shipped[key] = map[string]helmShipment{}
	}
	r.shipped[key][release] = helmShipment{secret: revision.UID, objects: objects}
	return objects, nil
}

func (r *GatewayAPIReconciler) forgetCluster(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.shipped, key)
	delete(r.holders, key)
}

// helmReleaseObjects are the objects a Helm release's manifest holds, by objectRef. Helm stores a
// release as its JSON, gzipped and in base64, within the Secret's own encoding.
func helmReleaseObjects(stored []byte) ([]string, error) {
	manifest, err := helmReleaseManifest(stored)
	if err != nil {
		return nil, err
	}
	heads, _, err := manifestObjects([]byte(manifest))
	if err != nil {
		return nil, err
	}
	objects := make([]string, 0, len(heads))
	for _, h := range heads {
		gk := schema.FromAPIVersionAndKind(h.APIVersion, h.Kind).GroupKind()
		objects = append(objects, objectRef(gk, h.Metadata.Name))
	}
	return objects, nil
}

// helmReleaseManifest is the manifest of a Helm release as its Secret stores it, gzipped unless an
// older Helm stored it.
func helmReleaseManifest(stored []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(string(stored))
	if err != nil {
		return "", fmt.Errorf("decoding the release: %w", err)
	}
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		zr, zerr := gzip.NewReader(bytes.NewReader(raw))
		if zerr != nil {
			return "", fmt.Errorf("decompressing the release: %w", zerr)
		}
		raw, err = io.ReadAll(io.LimitReader(zr, maxHelmRelease))
		if closeErr := zr.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", fmt.Errorf("decompressing the release: %w", err)
		}
	}
	var release struct {
		Manifest string `json:"manifest"`
	}
	if err := json.Unmarshal(raw, &release); err != nil {
		return "", fmt.Errorf("reading the release: %w", err)
	}
	return release.Manifest, nil
}

// record marks the Cluster with the Gateway API release its workload cluster has.
func (r *GatewayAPIReconciler) record(ctx context.Context, cluster *clusterv1.Cluster, installed string) error {
	if installed == "" || cluster.Annotations[gatewayAPIAnnotation] == installed {
		return nil
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[gatewayAPIAnnotation] = installed
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("recording the cluster's Gateway API release: %w", err)
	}
	return nil
}

// gatewayAPIWaits holds back what needs the Gateway API in the workload cluster until the cluster
// has it: Traefik's chart creates a Gateway, which an API server without the CRDs refuses, and the
// Add-on Provider for Helm puts each install after a failed one further off.
func gatewayAPIWaits(_ context.Context, cluster *clusterv1.Cluster) (bool, error) {
	_, has := cluster.Annotations[gatewayAPIAnnotation]
	return !has, nil
}
