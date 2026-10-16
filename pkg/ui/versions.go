package ui

import (
	"cmp"
	"context"
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
	// Error says why there are none to offer; the version is then typed in.
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
		writeJSON(w, http.StatusOK, s.kubeadmVersions(r.Context()))
	case string(templates.RKE2), string(templates.K3s):
		writeJSON(w, http.StatusOK, s.publishedVersions(r.Context(), d))
	default:
		writeError(w, http.StatusNotFound, "no such distribution")
	}
}

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
	ch, err := s.Releases.Channels(ctx, distribution)
	if err != nil {
		s.Log.Warn("reading release channels", "distribution", distribution, "error", err)
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

// missingTemplate says why no cluster of the class, or none of that version ("" for any), can be
// built yet: its machines are cloned from a template the controller did not find in the template
// pool, and would wait for ever. Until the controller has looked, it says nothing.
func missingTemplate(class *clusterv1.ClusterClass, distribution, version string) string {
	switch distribution {
	case string(templates.Kubeadm):
		recorded, ok := class.Annotations[provisioner.KubeadmVersionsAnnotation]
		switch {
		case !ok:
		case recorded == "":
			return "there is no kubeadm VM template yet; an administrator builds one on the Administration page"
		case version != "" && !slices.Contains(strings.Split(recorded, ","), version):
			return "there is no kubeadm VM template of " + version + " yet; an administrator builds it on the Administration page"
		}
	case string(templates.RKE2), string(templates.K3s):
		if class.Annotations[provisioner.CloudImageTemplateAnnotation] == "false" {
			return "there is no VM template for RKE2 and k3s yet; an administrator builds it on the Administration page"
		}
	}
	return ""
}

var kubeadmVersion = regexp.MustCompile(`^v1\.(\d+)\.(\d+)$`)

// kubeadmVersions are the versions a kubeadm cluster can run: those a template of the classes' OS
// has been built for in the template pool, as the controller records them on the kubeadm class.
// Cluster API clones with its own token, so they are offered whatever the user may see in
// Proxmox VE.
func (s *Server) kubeadmVersions(ctx context.Context) versionsView {
	kube := s.currentKube()
	if kube == nil {
		return versionsView{Versions: []string{}, Error: "the management cluster is not set up yet"}
	}
	class := &clusterv1.ClusterClass{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.ClassPrefix + string(templates.Kubeadm)}, class); err != nil {
		s.Log.Warn("reading the kubeadm ClusterClass", "error", err)
		return versionsView{Versions: []string{}, Error: "the kubeadm ClusterClass could not be read"}
	}
	recorded, ok := class.Annotations[provisioner.KubeadmVersionsAnnotation]
	if !ok {
		return versionsView{Versions: []string{}, Error: "Kubehenge's controller has not looked at the templates yet; try again in a minute"}
	}
	type version struct{ minor, patch int }
	newest := map[int]version{}
	for raw := range strings.SplitSeq(recorded, ",") {
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
		view.Error = "no kubeadm template yet; an administrator builds one on the Administration page"
	}
	return view
}
