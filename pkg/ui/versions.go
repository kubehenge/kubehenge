package ui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

type versionsView struct {
	// Versions are the newest release of each minor version a new cluster may start on, newest
	// first.
	Versions []string `json:"versions"`
	// Older are those of the minor versions before, offered only as the next step of a cluster
	// already on one.
	Older []string `json:"older,omitempty"`
	// Stable is the one its publisher calls stable, when it names one.
	Stable string `json:"stable,omitempty"`
	// Error says why there are none to offer. The version is then typed in.
	Error string `json:"error,omitempty"`
}

// releaseCache keeps each distribution's channels a while: every wizard asks, and they change a
// few times a month.
type releaseCache struct {
	mu   sync.Mutex
	seen map[string]cachedRelease
}

type cachedRelease struct {
	view    versionsView
	fetched time.Time
}

const releaseCacheFor = 10 * time.Minute

func (s *Server) versions(w http.ResponseWriter, r *http.Request, _ user) {
	switch d := r.PathValue("distribution"); d {
	case string(templates.Kubeadm):
		nodeOS := templates.DefaultNodeOS
		if q := r.URL.Query().Get("nodeOS"); q != "" {
			o, err := templates.ParseNodeOS(q)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			nodeOS = o
		}
		writeJSON(w, http.StatusOK, s.kubeadmVersions(r.Context(), nodeOS))
	case string(templates.RKE2), string(templates.K3s), string(templates.Talos):
		writeJSON(w, http.StatusOK, s.publishedVersions(r.Context(), d))
	default:
		writeError(w, http.StatusNotFound, "no such distribution")
	}
}

// talosReleasesView is what a Talos cluster may run.
type talosReleasesView struct {
	// Templates are the releases the template pool has a template of, newest first: a cluster
	// starts on one of these.
	Templates []string `json:"templates"`
	// Releases are the newest release of each minor version the Image Factory builds and Talos's
	// compatibility tables in this Kubehenge know, newest first, which a template can be built of
	// and a cluster upgraded to.
	Releases []string `json:"releases"`
	// Default is the release Kubehenge makes templates of unless another is asked for.
	Default string `json:"default"`
	// Kubernetes are the Kubernetes minor versions, such as "1.36", each of the releases above and
	// the templates' runs, oldest first.
	Kubernetes map[string][]string `json:"kubernetes"`
	// Error says why Releases is empty.
	Error string `json:"error,omitempty"`
}

// talosReleasesCacheKey keeps the Image Factory's releases in the release cache beside the
// distributions' channels.
const talosReleasesCacheKey = "talos-releases"

func (s *Server) talosReleases(w http.ResponseWriter, r *http.Request, _ user) {
	view := talosReleasesView{Templates: []string{}, Releases: []string{}, Default: templates.DefaultTalosVersion}
	if kube := s.currentKube(); kube != nil {
		class := &clusterv1.ClusterClass{}
		err := kube.Get(r.Context(), client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.ClassPrefix + string(templates.Talos)}, class)
		if err == nil {
			view.Templates = append(view.Templates, provisioner.ClassTalosTemplates(class)...)
		}
	}
	releases, err := s.cachedTalosReleases(r.Context())
	if err != nil {
		view.Error = err.Error()
	}
	view.Releases = releases
	view.Kubernetes = map[string][]string{}
	for _, v := range slices.Concat(view.Templates, view.Releases, []string{view.Default}) {
		minors, err := templates.TalosKubernetesMinors(v)
		if err != nil {
			continue
		}
		view.Kubernetes[v] = make([]string, 0, len(minors))
		for _, m := range minors {
			view.Kubernetes[v] = append(view.Kubernetes[v], fmt.Sprintf("1.%d", m))
		}
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) cachedTalosReleases(ctx context.Context) ([]string, error) {
	if s.TalosReleases == nil {
		return []string{}, errors.New("this interface does not read the Talos releases")
	}
	c := &s.releaseCache
	c.mu.Lock()
	cached, ok := c.seen[talosReleasesCacheKey]
	c.mu.Unlock()
	if ok && time.Since(cached.fetched) < releaseCacheFor {
		return cached.view.Versions, nil
	}
	all, err := s.TalosReleases(ctx)
	if err != nil {
		s.Log.WarnContext(ctx, "reading the Talos releases", "error", err)
		return []string{}, fmt.Errorf("the Talos releases could not be read: %w", err)
	}
	newest := map[int]string{}
	for _, v := range all {
		minor, err := templates.TalosMinor(v)
		// A release newer than the compatibility tables Kubehenge was built with is refused by the
		// cluster checks, so it is not offered.
		if err != nil {
			continue
		}
		if _, err := templates.TalosKubernetesMinors(v); err != nil {
			continue
		}
		if old, ok := newest[minor]; !ok || talosPatch(old) < talosPatch(v) {
			newest[minor] = v
		}
	}
	minors := slices.Sorted(maps.Keys(newest))
	slices.Reverse(minors)
	out := make([]string, 0, len(minors))
	for _, m := range minors {
		out = append(out, newest[m])
	}
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]cachedRelease{}
	}
	c.seen[talosReleasesCacheKey] = cachedRelease{view: versionsView{Versions: out}, fetched: time.Now()}
	c.mu.Unlock()
	return out, nil
}

// talosPatch is a v1.x.y release's patch number.
func talosPatch(v string) int {
	p, _ := strconv.Atoi(v[strings.LastIndex(v, ".")+1:])
	return p
}

// publishedVersions are RKE2's or k3s's releases, the same whatever the node OS, or the Kubernetes
// releases Talos runs, on Talos's own OS.
func (s *Server) publishedVersions(ctx context.Context, distribution string) versionsView {
	if s.Releases == nil {
		return versionsView{Versions: []string{}, Error: "this interface does not read release channels"}
	}
	c := &s.releaseCache
	c.mu.Lock()
	cached, ok := c.seen[distribution]
	c.mu.Unlock()
	if ok && time.Since(cached.fetched) < releaseCacheFor {
		return cached.view
	}
	ch, err := s.Releases.Channels(ctx, distribution, templates.DefaultNodeOS)
	if err != nil {
		s.Log.WarnContext(ctx, "reading release channels", "distribution", distribution, "error", err)
		return versionsView{Versions: []string{}, Error: "the release channels could not be read: " + err.Error()}
	}
	o := ch.Versions()
	view := versionsView{Versions: o.Current, Older: o.Older, Stable: o.Stable}
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]cachedRelease{}
	}
	c.seen[distribution] = cachedRelease{view: view, fetched: time.Now()}
	c.mu.Unlock()
	return view
}

// missingTemplate says why no cluster of the class on the node OS, or none of that version ("" for
// any), can be built yet: its machines are cloned from a template the controller did not find in
// the template pool, and would wait for ever. Until the controller has looked, it says nothing.
func missingTemplate(class *clusterv1.ClusterClass, distribution, version string, nodeOS templates.NodeOS) string {
	const buildsIt = ". An administrator builds it in Administration"
	title := nodeOSTitle(nodeOS)
	switch distribution {
	case string(templates.Kubeadm):
		raw, ok := class.Annotations[provisioner.KubeadmVersionsAnnotation]
		if !ok {
			return ""
		}
		recorded, err := provisioner.ParseKubeadmVersions(raw)
		if err != nil {
			return ""
		}
		versions := recorded[nodeOS.String()]
		switch {
		case len(versions) == 0:
			return "no kubeadm VM template on " + title + " yet" + buildsIt
		case version != "" && !slices.Contains(versions, version):
			return "no kubeadm " + version + " VM template on " + title + " yet" + buildsIt
		}
	case string(templates.RKE2), string(templates.K3s):
		recorded, ok := class.Annotations[provisioner.NodeOSTemplatesAnnotation]
		if ok && !slices.Contains(strings.Split(recorded, ","), nodeOS.String()) {
			return "no " + distributionTitle(distribution) + " VM template on " + title + " yet" + buildsIt
		}
	case string(templates.Talos):
		recorded, ok := class.Annotations[provisioner.TalosTemplatesAnnotation]
		switch {
		case !ok:
		case recorded == "":
			return "no Talos VM template yet" + buildsIt
		case version != "" && !slices.Contains(strings.Split(recorded, ","), version):
			return "no Talos " + version + " VM template yet" + buildsIt
		}
	}
	return ""
}

// nodeOSTitle is a node OS as people read it, such as AlmaLinux 10.
func nodeOSTitle(o templates.NodeOS) string {
	if f, r, err := templates.ReleaseOf(o); err == nil {
		return f.Title() + " " + r.Name
	}
	return o.String()
}

func distributionTitle(d string) string {
	switch templates.Distribution(d) {
	case templates.RKE2:
		return "RKE2"
	case templates.K3s:
		return "k3s"
	case templates.Kubeadm, templates.Talos:
	}
	return d
}

var kubeadmVersion = regexp.MustCompile(`^v1\.(\d+)\.(\d+)$`)

// kubeadmVersions are the versions a kubeadm cluster on the node OS can run: those a template of it
// has been built for in the template pool, as the controller records them on the kubeadm class.
// Cluster API clones with its own token, so they are offered whatever the user may see in
// Proxmox VE.
func (s *Server) kubeadmVersions(ctx context.Context, nodeOS templates.NodeOS) versionsView {
	kube := s.currentKube()
	if kube == nil {
		return versionsView{Versions: []string{}, Error: "the management cluster is not set up yet"}
	}
	class := &clusterv1.ClusterClass{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.ClassPrefix + string(templates.Kubeadm)}, class); err != nil {
		s.Log.WarnContext(ctx, "reading the kubeadm ClusterClass", "error", err)
		return versionsView{Versions: []string{}, Error: "the kubeadm cluster class could not be read"}
	}
	raw, ok := class.Annotations[provisioner.KubeadmVersionsAnnotation]
	if !ok {
		return versionsView{Versions: []string{}, Error: "templates not checked yet. Try again in a minute"}
	}
	recorded, err := provisioner.ParseKubeadmVersions(raw)
	if err != nil {
		s.Log.WarnContext(ctx, "reading the kubeadm ClusterClass", "error", err)
		return versionsView{Versions: []string{}, Error: "the kubeadm cluster class could not be read"}
	}
	type version struct{ minor, patch int }
	newest := map[int]version{}
	for _, raw := range recorded[nodeOS.String()] {
		if v := kubeadmVersion.FindStringSubmatch(raw); v != nil {
			minor, _ := strconv.Atoi(v[1])
			patch, _ := strconv.Atoi(v[2])
			if old, ok := newest[minor]; !ok || old.patch < patch {
				newest[minor] = version{minor, patch}
			}
		}
	}
	list := make([]version, 0, len(newest))
	for _, v := range newest {
		list = append(list, v)
	}
	slices.SortFunc(list, func(a, b version) int { return cmp.Compare(b.minor, a.minor) })
	view := versionsView{Versions: []string{}}
	for _, v := range list {
		view.Versions = append(view.Versions, "v1."+strconv.Itoa(v.minor)+"."+strconv.Itoa(v.patch))
	}
	if len(view.Versions) == 0 {
		view.Error = "no kubeadm VM template on " + nodeOSTitle(nodeOS) + " yet. An administrator builds it in Administration"
	}
	return view
}
