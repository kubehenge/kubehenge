package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// MakeTemplate builds a template with a user's session; the builder comes without its client and
// sources, which this adds.
type MakeTemplate func(ctx context.Context, t *pve.Ticket, b templates.Builder, tpl *templates.Template) (vmid int, err error)

type templateView struct {
	VMID int      `json:"vmid"`
	Node string   `json:"node"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
	// Line is the template a build is one of, with Build its tag and Built when it was made; a
	// Talos template has neither.
	Line  string     `json:"line"`
	Build string     `json:"build,omitempty"`
	Built *time.Time `json:"built,omitempty"`
	// Newest is the line's newest build, which new machines are cloned from once it is offered.
	Newest bool `json:"newest,omitempty"`
	// InUse names a cluster that clones from it, which keeps it from being deleted.
	InUse string `json:"inUse,omitempty"`
	// NodeOS is the operating system its machines run, such as AlmaLinux 10; a Talos template has
	// none.
	NodeOS string `json:"nodeOS,omitempty"`
}

type templatesView struct {
	Templates []templateView     `json:"templates"`
	Job       *appliance.JobView `json:"job,omitempty"`
	// Refresh is the weekly build, when it is on.
	Refresh *refreshView `json:"refresh,omitempty"`
	// NodeOSes are those templates can be built of, and those not released yet.
	NodeOSes []templates.NodeOSInfo `json:"nodeOSes"`
}

// templatesReady checks that this is an appliance whose management cluster, which made the
// template pool, exists, and that the user may look after it.
func (s *Server) templatesReady(w http.ResponseWriter, r *http.Request, u user) bool {
	if s.Appliance == nil || s.MakeTemplate == nil {
		writeError(w, http.StatusNotFound, "this interface does not build templates. Use kubehenge template create")
		return false
	}
	if s.currentKube() == nil {
		writeError(w, http.StatusConflict, "set up the management cluster first")
		return false
	}
	return s.require(w, r, u, "/", setupPrivileges...)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, u user) {
	if !s.templatesReady(w, r, u) {
		return
	}
	// Read before the list, so that a build shown as finished is in it: the page polls slowly once
	// no build runs.
	job := s.Appliance.TemplateJob()
	list, err := s.templates(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	state, err := s.refreshState(r.Context(), s.currentKube())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	needs, err := s.templateNeeds(r.Context(), list, state.soak())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	for i := range list {
		list[i].InUse = needs.of(list[i].Tags)
	}
	writeJSON(w, http.StatusOK, templatesView{Templates: list, Job: job, Refresh: state.view(s.now()), NodeOSes: templates.NodeOSes()})
}

// templates lists the VM templates in the template pool, each build beside its line's others,
// newest first.
func (s *Server) templates(ctx context.Context, t *pve.Ticket) ([]templateView, error) {
	members, err := s.Accounts.Templates(ctx, t, templatePool)
	if err != nil {
		return nil, fmt.Errorf("the templates in pool %s: %w", templatePool, err)
	}
	list := []templateView{}
	newest := map[string]time.Time{}
	for i := range members {
		m := &members[i]
		v := templateView{VMID: m.VMID, Node: m.Node, Name: m.Name, Tags: templates.ParseTags(m.Tags), Line: m.Name}
		if o, ok := templates.NodeOSOf(v.Tags); ok {
			v.NodeOS = nodeOSTitle(o)
		}
		if at, built := templates.BuildOf(v.Tags); built {
			v.Build, v.Built = templates.BuildTag(at), &at
			v.Line = strings.TrimSuffix(m.Name, "-"+v.Build)
			if key := templates.LineKey(v.Tags); at.After(newest[key]) {
				newest[key] = at
			}
		}
		list = append(list, v)
	}
	for i := range list {
		list[i].Newest = list[i].Built != nil && list[i].Built.Equal(newest[templates.LineKey(list[i].Tags)])
	}
	slices.SortFunc(list, func(a, b templateView) int {
		if c := strings.Compare(a.Line, b.Line); c != 0 || a.Built == nil || b.Built == nil {
			return c
		}
		return b.Built.Compare(*a.Built)
	})
	return list, nil
}

// templateUsers tells, by a template's tags, which cluster clones from it: the build of each line
// its machines run that its node image time picks. During an upgrade a kubeadm cluster runs two
// lines, the Kubernetes version it leaves and the one it goes to.
type templateUsers map[string]string

// of names a cluster that clones from the template with these tags, or is "".
func (t templateUsers) of(tags []string) string {
	return t[strings.Join(templates.NormaliseTags(tags), ";")]
}

func (s *Server) templateNeeds(ctx context.Context, list []templateView, soak time.Duration) (templateUsers, error) {
	builds := listedBuilds(list)
	kube := s.currentKube()
	var clusters clusterv1.ClusterList
	if err := kube.List(ctx, &clusters); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	users := templateUsers{}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !provisioner.Owns(c, s.ClassNamespace) || !provisioner.RefreshesNodes(c) {
			continue
		}
		if err := users.add(ctx, kube, c, builds, soak); err != nil {
			return nil, err
		}
	}
	return users, nil
}

// listedBuilds are the times of the listed builds by line, oldest first, as the ClusterClasses
// record them.
func listedBuilds(list []templateView) provisioner.NodeImageBuilds {
	builds := provisioner.NodeImageBuilds{}
	for i := range list {
		if t := &list[i]; t.Built != nil {
			key := templates.LineKey(t.Tags)
			builds[key] = append(builds[key], *t.Built)
		}
	}
	for key := range builds {
		slices.SortFunc(builds[key], time.Time.Compare)
	}
	return builds
}

// add records the build of each line a cluster's machines run that its node image time picks.
func (t templateUsers) add(ctx context.Context, kube client.Client, c *clusterv1.Cluster, builds provisioner.NodeImageBuilds, soak time.Duration) error {
	since, err := provisioner.NodeImageTime(c)
	if err != nil {
		return fmt.Errorf("reading cluster %s/%s's node image time: %w", c.Namespace, c.Name, err)
	}
	versions, err := machineVersions(ctx, kube, c)
	if err != nil {
		return err
	}
	dist, _ := distribution(c.Spec.Topology.ClassRef.Name)
	nodeOS, err := provisioner.ClusterNodeOS(c)
	if err != nil {
		return fmt.Errorf("reading cluster %s/%s's node OS: %w", c.Namespace, c.Name, err)
	}
	for _, v := range versions {
		line, lineErr := provisioner.MachineLine(dist, v, nodeOS)
		if lineErr != nil {
			continue
		}
		if at, ok := builds.Pick(line, since, soak); ok {
			t[strings.Join(templates.NormaliseTags(append(slices.Clone(line), templates.BuildTag(at))), ";")] = c.Namespace + "/" + c.Name
		}
	}
	return nil
}

// machineVersions are a cluster's Kubernetes version and each of its machines'.
func machineVersions(ctx context.Context, kube client.Client, c *clusterv1.Cluster) ([]string, error) {
	versions := []string{c.Spec.Topology.Version}
	var machines clusterv1.MachineList
	if err := kube.List(ctx, &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing cluster %s/%s's machines: %w", c.Namespace, c.Name, err)
	}
	for i := range machines.Items {
		if v := machines.Items[i].Spec.Version; v != "" && !slices.Contains(versions, v) {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

type templateRequest struct {
	// Distribution is rke2, k3s, kubeadm or talos.
	Distribution      string `json:"distribution"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	TalosVersion      string `json:"talosVersion,omitempty"`
	// NodeOS is the node OS as one word, such as almalinux-10; empty means the default. Talos is
	// its own OS and takes none.
	NodeOS        string   `json:"nodeOS,omitempty"`
	Node          string   `json:"node"`
	Storage       string   `json:"storage"`
	ImportStorage string   `json:"importStorage"`
	ISOStorage    string   `json:"isoStorage,omitempty"`
	Bridge        string   `json:"bridge"`
	VLAN          int      `json:"vlan,omitempty"`
	BuildAddress  string   `json:"buildAddress,omitempty"`
	BuildGateway  string   `json:"buildGateway,omitempty"`
	BuildDNS      []string `json:"buildDNS,omitempty"`
}

var proxmoxName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (req *templateRequest) builder() (templates.Builder, templates.Template, error) {
	var problems []string
	for _, f := range [...]struct{ label, value string }{
		{"Node", req.Node}, {"Template disk", req.Storage}, {"Downloaded image", req.ImportStorage}, {"Bridge", req.Bridge},
	} {
		if !proxmoxName.MatchString(f.value) {
			problems = append(problems, f.label+": choose one")
		}
	}
	if req.ISOStorage != "" && !proxmoxName.MatchString(req.ISOStorage) {
		problems = append(problems, "Cloud-init image: choose a storage")
	}
	if req.VLAN < 0 || req.VLAN > 4094 {
		problems = append(problems, "VLAN tag: 1 to 4094")
	}
	plan := templates.Request{NodeOS: templates.DefaultNodeOS}
	switch req.Distribution {
	case string(templates.RKE2), string(templates.K3s):
		plan.Distribution = templates.Distribution(req.Distribution)
	case string(templates.Kubeadm):
		plan.Distribution, plan.KubernetesVersion = templates.Kubeadm, req.KubernetesVersion
	case string(templates.Talos):
		plan = templates.Request{Distribution: templates.Talos, TalosVersion: req.TalosVersion}
		if req.NodeOS != "" {
			problems = append(problems, "Node OS: Talos is its own OS")
		}
	default:
		problems = append(problems, "For: RKE2, k3s, kubeadm or Talos")
	}
	if req.NodeOS != "" && plan.Distribution != templates.Talos {
		o, err := templates.ParseNodeOS(req.NodeOS)
		if err == nil {
			_, _, err = templates.Available(o)
		}
		if err != nil {
			problems = append(problems, "Node OS: "+err.Error())
		}
		plan.NodeOS = o
	}
	network, problem := buildNetwork(req.BuildAddress, req.BuildGateway, req.BuildDNS)
	if problem != "" {
		problems = append(problems, problem)
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return templates.Builder{}, templates.Template{}, errors.New(strings.Join(problems, ". "))
	}
	t, err := templates.Plan(&plan)
	if err != nil {
		return templates.Builder{}, templates.Template{}, fmt.Errorf("the template: %w", err)
	}
	b := templates.Builder{
		Node: req.Node, Storage: req.Storage, ImportStorage: req.ImportStorage, ISOStorage: req.ISOStorage,
		Pool: templatePool, Bridge: req.Bridge, VLAN: req.VLAN, Network: network,
	}
	return b, t, nil
}

// buildNetwork is the static address of the VM that prepares an image, none meaning DHCP, or what
// is wrong with it, in the build dialog's terms.
func buildNetwork(address, gateway string, dns []string) (*templates.Network, string) {
	if address == "" {
		if gateway != "" || len(dns) > 0 {
			return nil, "Gateway and DNS servers need an address"
		}
		return nil, ""
	}
	n := &templates.Network{}
	var err error
	if n.Address, err = netip.ParsePrefix(address); err != nil || !n.Address.Addr().Is4() {
		return nil, "Address: an IPv4 address with prefix length, like 192.0.2.50/24"
	}
	if n.Gateway, err = netip.ParseAddr(gateway); err != nil || !n.Address.Contains(n.Gateway) {
		return nil, "Gateway: an address in the same network"
	}
	for _, d := range dns {
		a, err := netip.ParseAddr(d)
		if err != nil {
			return nil, "DNS servers: " + d + " is not an IP address"
		}
		n.DNS = append(n.DNS, a)
	}
	if len(n.DNS) == 0 {
		return nil, "DNS servers: enter at least one"
	}
	return n, ""
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request, u user) {
	var req templateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !s.templatesReady(w, r, u) {
		return
	}
	b, t, err := req.builder()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Each build is a template of its own, which clusters move to inside their windows. A Talos
	// release's template is made once: its nodes take updates as Talos releases.
	if t.Refreshed() {
		if t, err = t.Build(s.now()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// A build takes minutes, so it gets the whole of a fresh ticket's life.
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	err = s.Appliance.BuildTemplate(ticket, func(ctx context.Context, j *appliance.Job) error { //nolint:contextcheck // the build outlives the request on purpose
		j.Add(fmt.Sprintf("Building template %s on node %s", t.Name, b.Node))
		b.Log = func(format string, a ...any) { j.Add(fmt.Sprintf(format, a...)) }
		retry := templates.Retry{Log: b.Log}
		vmid, buildErr := retry.Do(ctx, t.Name, func(ctx context.Context) (int, error) { return s.MakeTemplate(ctx, ticket, b, &t) })
		if buildErr != nil {
			return fmt.Errorf("building template %s: %w", t.Name, buildErr)
		}
		j.Add(fmt.Sprintf("Template %s is VM %d", t.Name, vmid))
		return nil
	})
	switch {
	case jobConflict(err):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("template build started", "user", u.ticket.Username, "template", t.Name, "node", b.Node)
	writeJSON(w, http.StatusAccepted, s.Appliance.TemplateJob())
}

func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, u user) {
	vmid, err := strconv.Atoi(r.PathValue("vmid"))
	if err != nil || vmid <= 0 {
		writeError(w, http.StatusNotFound, "no such template")
		return
	}
	if !s.templatesReady(w, r, u) {
		return
	}
	list, err := s.templates(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	i := slices.IndexFunc(list, func(t templateView) bool { return t.VMID == vmid })
	if i < 0 {
		writeError(w, http.StatusNotFound, "no such template")
		return
	}
	state, err := s.refreshState(r.Context(), s.currentKube())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	needs, err := s.templateNeeds(r.Context(), list, state.soak())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if c := needs.of(list[i].Tags); c != "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("cluster %s still uses %s", c, list[i].Name))
		return
	}
	if err := s.Accounts.DeleteVM(r.Context(), u.ticket, list[i].Node, vmid); err != nil {
		s.proxmoxError(w, err)
		return
	}
	s.Log.Info("deleted template", "user", u.ticket.Username, "template", list[i].Name, "vmid", vmid)
	w.WriteHeader(http.StatusNoContent)
}
