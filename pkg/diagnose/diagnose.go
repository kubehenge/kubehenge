// Package diagnose writes a cluster's support bundle: what someone looking into why the cluster is
// stuck needs, from the management cluster and the appliance, with no Secret and no secret in it.
// The command line and the interface write the same one.
package diagnose

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/redact"
)

const (
	// logLines and logBytes bound how much of each container's log is read, its end.
	logLines = 20000
	logBytes = 32 << 20
	// apiGroupSuffix is the group of Cluster API and of each of its providers.
	apiGroupSuffix = "cluster.x-k8s.io"
	// fileMode keeps the bundle's files to whoever unpacks it.
	fileMode = 0o600
)

// ErrNotAppliance is what Journal returns where it does not run on Kubehenge's appliance.
var ErrNotAppliance = errors.New("not on Kubehenge's appliance")

var errNoClientset = errors.New("no client to find the resources and read the logs with")

// Source is where a bundle is read from.
type Source struct {
	// Kube reads the management cluster's objects.
	Kube client.Client
	// Clientset finds the Cluster API resources there and reads the providers' logs.
	Clientset kubernetes.Interface
	// ControllerNamespace is where Kubehenge's controller runs, the chart's namespace.
	ControllerNamespace string
	// Journal reads the journals of Kubehenge's units on the appliance. A nil value, or ErrNotAppliance,
	// leaves them out.
	Journal func(ctx context.Context) ([]byte, error)
	Now     func() time.Time
}

// logSource is a namespace of the management cluster whose pods' logs a bundle carries.
type logSource struct {
	namespace, selector string
}

// providerLogs are the providers that act on a cluster's VMs and add-ons.
var providerLogs = []logSource{
	{namespace: "proxmox-infrastructure-system"},
	{namespace: "helm-addon-system"},
}

// Name is the file a bundle of a cluster is saved as, at a time.
func Name(namespace, cluster string, at time.Time) string {
	return fmt.Sprintf("%s-%s-%s.tar.gz", namespace, cluster, at.UTC().Format("20060102T150405Z"))
}

// Write writes the support bundle of a cluster as a gzipped tar. It fails before writing anything
// when the cluster cannot be read. Whatever else cannot be read the bundle's manifest says.
func Write(ctx context.Context, w io.Writer, src *Source, namespace, name string) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterv1.GroupVersion.WithKind(clusterv1.ClusterKind))
	if err := src.Kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cluster); err != nil {
		return fmt.Errorf("reading cluster %s/%s: %w", namespace, name, err)
	}
	now := time.Now
	if src.Now != nil {
		now = src.Now
	}
	b := &bundle{
		src: src, namespace: namespace, cluster: name, at: now().UTC(),
		dir: namespace + "-" + name, names: map[string]bool{name: true},
	}
	gz := gzip.NewWriter(w)
	b.tar = tar.NewWriter(gz)
	b.object(cluster)
	b.objects(ctx)
	b.bootLogs(ctx)
	b.events(ctx)
	b.logs(ctx)
	b.journal(ctx)
	b.manifest()
	return errors.Join(b.err, b.tar.Close(), gz.Close())
}

type bundle struct {
	src       *Source
	namespace string
	cluster   string
	at        time.Time
	dir       string
	tar       *tar.Writer
	// names are those of the objects collected, by which the logs' lines are chosen.
	names map[string]bool
	// collected and failed are what the manifest lists.
	collected, failed []string
	// err is the first write that failed, after which the archive is broken.
	err error
}

func (b *bundle) file(name string, data []byte) {
	if b.err != nil {
		return
	}
	hdr := &tar.Header{Name: b.dir + "/" + name, Mode: fileMode, Size: int64(len(data)), ModTime: b.at, Typeflag: tar.TypeReg}
	if err := b.tar.WriteHeader(hdr); err != nil {
		b.err = fmt.Errorf("writing the bundle: %w", err)
		return
	}
	if _, err := b.tar.Write(data); err != nil {
		b.err = fmt.Errorf("writing the bundle: %w", err)
		return
	}
	b.collected = append(b.collected, name)
}

func (b *bundle) fail(what string, err error) {
	b.failed = append(b.failed, what+": "+redact.Line(err.Error()))
}

// object writes one object, with its secrets taken out, under its group and kind.
func (b *bundle) object(o *unstructured.Unstructured) {
	if o.GetKind() == "Secret" {
		return
	}
	o = o.DeepCopy()
	unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	redact.Object(o.Object)
	raw, err := yaml.Marshal(o.Object)
	if err != nil {
		b.fail(o.GetKind()+" "+o.GetName(), err)
		return
	}
	group := o.GroupVersionKind().Group
	if group == "" {
		group = "core"
	}
	b.names[o.GetName()] = true
	b.file(path.Join("objects", group, o.GetKind(), o.GetName()+".yaml"), raw)
}

// objects writes every object of the cluster in the Cluster API groups: its machines, their
// infrastructure, bootstrap configuration and control plane, its IP addresses and add-ons.
func (b *bundle) objects(ctx context.Context) {
	if b.src.Clientset == nil {
		b.fail("the Cluster API objects", errNoClientset)
		return
	}
	kinds, err := clusterKinds(b.src.Clientset)
	if err != nil {
		b.fail("finding the Cluster API resources", err)
	}
	for _, gvk := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err := b.src.Kube.List(ctx, list, client.InNamespace(b.namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: b.cluster})
		if err != nil {
			b.fail("listing "+gvk.Kind, err)
			continue
		}
		for i := range list.Items {
			b.object(&list.Items[i])
		}
	}
}

// clusterKinds are the namespaced kinds of the Cluster API groups that can be listed, each at the
// version its group prefers.
func clusterKinds(cs kubernetes.Interface) ([]schema.GroupVersionKind, error) {
	groups, lists, err := cs.Discovery().ServerGroupsAndResources()
	preferred := map[string]string{}
	for _, g := range groups {
		preferred[g.Name] = g.PreferredVersion.GroupVersion
	}
	var kinds []schema.GroupVersionKind
	seen := map[schema.GroupKind]bool{}
	for _, l := range lists {
		gv, parseErr := schema.ParseGroupVersion(l.GroupVersion)
		if parseErr != nil || gv.Group != apiGroupSuffix && !strings.HasSuffix(gv.Group, "."+apiGroupSuffix) {
			continue
		}
		if p, ok := preferred[gv.Group]; ok && p != "" && p != l.GroupVersion {
			continue
		}
		for i := range l.APIResources {
			r := &l.APIResources[i]
			gk := schema.GroupKind{Group: gv.Group, Kind: r.Kind}
			if !r.Namespaced || strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || seen[gk] || r.Kind == clusterv1.ClusterKind {
				continue
			}
			seen[gk] = true
			kinds = append(kinds, gv.WithKind(r.Kind))
		}
	}
	slices.SortFunc(kinds, func(a, b schema.GroupVersionKind) int {
		return strings.Compare(a.Group+"/"+a.Kind, b.Group+"/"+b.Kind)
	})
	if err != nil {
		return kinds, fmt.Errorf("discovery: %w", err)
	}
	return kinds, nil
}

// bootLogs writes the boot logs the controller read for the cluster's machines, if any were asked
// for.
func (b *bundle) bootLogs(ctx context.Context) {
	cm := &corev1.ConfigMap{}
	err := b.src.Kube.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: provisioner.BootLogsConfigMap(b.cluster)}, cm)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			b.fail("reading the boot logs", err)
		}
		return
	}
	raw, err := yaml.Marshal(cm.Data)
	if err != nil {
		b.fail("the boot logs", err)
		return
	}
	b.file("boot-logs.yaml", raw)
}

// event is how the bundle writes one of the cluster's events.
type event struct {
	Last    time.Time `json:"last"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Object  string    `json:"object"`
	Count   int32     `json:"count,omitempty"`
	Source  string    `json:"source,omitempty"`
	Message string    `json:"message"`
}

// events writes the events of the objects collected, the oldest first.
func (b *bundle) events(ctx context.Context) {
	var list corev1.EventList
	if err := b.src.Kube.List(ctx, &list, client.InNamespace(b.namespace)); err != nil {
		b.fail("listing the events", err)
		return
	}
	var out []event
	for i := range list.Items {
		e := &list.Items[i]
		if !b.names[e.InvolvedObject.Name] {
			continue
		}
		last := e.LastTimestamp.Time
		if last.IsZero() {
			last = e.EventTime.Time
		}
		if last.IsZero() {
			last = e.CreationTimestamp.Time
		}
		source := e.Source.Component
		if source == "" {
			source = e.ReportingController
		}
		out = append(out, event{
			Last: last, Type: e.Type, Reason: e.Reason, Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count: e.Count, Source: source, Message: redact.Text(e.Message),
		})
	}
	slices.SortStableFunc(out, func(x, y event) int { return x.Last.Compare(y.Last) })
	raw, err := yaml.Marshal(out)
	if err != nil {
		b.fail("the events", err)
		return
	}
	b.file("events.yaml", raw)
}

// logs writes the lines of the controller's and the providers' logs that name the cluster's
// objects.
func (b *bundle) logs(ctx context.Context) {
	if b.src.Clientset == nil {
		b.fail("the logs", errNoClientset)
		return
	}
	sources := append([]logSource{{namespace: b.src.ControllerNamespace, selector: "app.kubernetes.io/name=kubehenge-controller"}}, providerLogs...)
	for _, s := range sources {
		pods, err := b.src.Clientset.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector})
		if err != nil {
			b.fail("listing the pods in "+s.namespace, err)
			continue
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			for j := range p.Spec.Containers {
				b.podLog(ctx, p, p.Spec.Containers[j].Name)
			}
		}
	}
}

func (b *bundle) podLog(ctx context.Context, p *corev1.Pod, container string) {
	what := "the log of " + p.Namespace + "/" + p.Name + " " + container
	stream, err := b.src.Clientset.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{
		Container: container, TailLines: new(int64(logLines)), LimitBytes: new(int64(logBytes)), Timestamps: true,
	}).Stream(ctx)
	if err != nil {
		b.fail(what, err)
		return
	}
	defer func() { _ = stream.Close() }()
	kept, err := b.filter(io.LimitReader(stream, logBytes))
	if err != nil {
		b.fail(what, err)
	}
	b.file(path.Join("logs", p.Namespace, p.Name+"_"+container+".log"), kept)
}

// journal writes the lines of the appliance's journals that name the cluster's objects.
func (b *bundle) journal(ctx context.Context) {
	if b.src.Journal == nil {
		return
	}
	raw, err := b.src.Journal(ctx)
	if errors.Is(err, ErrNotAppliance) {
		return
	}
	if err != nil {
		b.fail("reading the appliance's journals", err)
		return
	}
	kept, err := b.filter(strings.NewReader(string(raw)))
	if err != nil {
		b.fail("the appliance's journals", err)
	}
	b.file("logs/appliance/kubehenge.log", kept)
}

// quoted finds the strings of a JSON log line.
var quoted = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// filter keeps the lines that name one of the cluster's objects, with their secrets taken out. A
// line counts only when it names the object exactly, as a string of its own, together with the
// cluster's namespace or as namespace/name: a line about another cluster, in another namespace or
// whose objects' names start the same, is never kept.
func (b *bundle) filter(r io.Reader) ([]byte, error) {
	var out strings.Builder
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), logBytes)
	for scan.Scan() {
		if line := scan.Text(); b.mentions(line) {
			out.WriteString(redact.Line(line))
			out.WriteByte('\n')
		}
	}
	if err := scan.Err(); err != nil {
		return []byte(out.String()), fmt.Errorf("reading the log: %w", err)
	}
	return []byte(out.String()), nil
}

// mentions tells whether a log line names one of the cluster's objects.
func (b *bundle) mentions(line string) bool {
	inNamespace, named := false, false
	for _, m := range quoted.FindAllStringSubmatch(line, -1) {
		s := m[1]
		if ns, name, ok := strings.Cut(s, "/"); ok && ns == b.namespace && b.names[name] {
			return true
		}
		inNamespace = inNamespace || s == b.namespace
		named = named || b.names[s]
	}
	return inNamespace && named
}

// manifest writes what the bundle holds and what it could not read.
func (b *bundle) manifest() {
	var s strings.Builder
	fmt.Fprintf(&s, "Support bundle of cluster %s/%s, written %s.\n\n", b.namespace, b.cluster, b.at.Format(time.RFC3339))
	s.WriteString("It holds no Secret, and secrets in what it holds are replaced by " + redact.Mark + ".\n")
	s.WriteString("The logs hold only the lines that name one of the objects under objects/.\n\n")
	s.WriteString("Collected:\n")
	for _, c := range b.collected {
		s.WriteString("  " + c + "\n")
	}
	if len(b.failed) > 0 {
		s.WriteString("\nNot collected:\n")
		for _, f := range b.failed {
			s.WriteString("  " + f + "\n")
		}
	}
	b.file("manifest.txt", []byte(s.String()))
}

// applianceUnit is the interface's unit, which only Kubehenge's appliance has.
const applianceUnit = "/etc/systemd/system/kubehenge.service"

// ApplianceJournal reads the journal of the interface's unit on Kubehenge's appliance, and returns
// ErrNotAppliance anywhere else.
func ApplianceJournal(ctx context.Context) ([]byte, error) {
	for _, p := range []string{"/run/systemd/system", applianceUnit} {
		if _, err := os.Stat(p); err != nil {
			return nil, ErrNotAppliance
		}
	}
	out, err := exec.CommandContext(ctx, "journalctl", "--no-pager", "--output", "short-iso", "--lines", "50000", "--unit", "kubehenge.service").Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	return out, nil
}
