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
	// InUse names a cluster that clones from it, which keeps it from being deleted.
	InUse string `json:"inUse,omitempty"`
}

type templatesView struct {
	Templates []templateView     `json:"templates"`
	Job       *appliance.JobView `json:"job,omitempty"`
}

// templatesReady checks that this is an appliance whose management cluster, which made the
// template pool, exists, and that the user may look after it.
func (s *Server) templatesReady(w http.ResponseWriter, r *http.Request, u user) bool {
	if s.Appliance == nil || s.MakeTemplate == nil {
		writeError(w, http.StatusNotFound, "this interface was given its management cluster; make templates with kubehenge template create")
		return false
	}
	if s.currentKube() == nil {
		writeError(w, http.StatusConflict, "set up the management cluster first; it makes the templates' pool")
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
	needs, err := s.templateNeeds(r.Context())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	for i := range list {
		list[i].InUse = needs.of(list[i].Tags)
	}
	writeJSON(w, http.StatusOK, templatesView{Templates: list, Job: job})
}

// templates lists the VM templates in the template pool.
func (s *Server) templates(ctx context.Context, t *pve.Ticket) ([]templateView, error) {
	members, err := s.Accounts.Templates(ctx, t, templatePool)
	if err != nil {
		return nil, fmt.Errorf("the templates in pool %s: %w", templatePool, err)
	}
	list := []templateView{}
	for _, m := range members {
		list = append(list, templateView{VMID: m.VMID, Node: m.Node, Name: m.Name, Tags: templates.ParseTags(m.Tags)})
	}
	slices.SortFunc(list, func(a, b templateView) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// templateUsers tells, by a template's tags, which cluster clones from it. The ClusterClasses
// select templates of the default OS: RKE2 and k3s share one cloud image, kubeadm takes the one of
// each machine's Kubernetes version, which during an upgrade is two.
type templateUsers struct {
	cloudImage string
	// kubeadm names a cluster by each Kubernetes version its machines run.
	kubeadm map[string]string
}

// of names a cluster that clones from the template with these tags, or is "".
func (t templateUsers) of(tags []string) string {
	if !slices.Contains(tags, templates.DefaultOS) {
		return ""
	}
	if slices.Contains(tags, "cloud-image") {
		return t.cloudImage
	}
	if !slices.Contains(tags, string(templates.Kubeadm)) {
		return ""
	}
	for _, tag := range tags {
		if v, ok := strings.CutPrefix(tag, "kubernetes-"); ok && t.kubeadm[v] != "" {
			return t.kubeadm[v]
		}
	}
	return ""
}

func (s *Server) templateNeeds(ctx context.Context) (templateUsers, error) {
	kube := s.currentKube()
	var clusters clusterv1.ClusterList
	if err := kube.List(ctx, &clusters); err != nil {
		return templateUsers{}, fmt.Errorf("listing the clusters: %w", err)
	}
	users := templateUsers{kubeadm: map[string]string{}}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !provisioner.Owns(c, s.ClassNamespace) {
			continue
		}
		name := c.Namespace + "/" + c.Name
		d, _ := distribution(c.Spec.Topology.ClassRef.Name)
		switch templates.Distribution(d) {
		case templates.RKE2, templates.K3s:
			users.cloudImage = name
		case templates.Kubeadm:
			if err := addKubeadmVersions(ctx, kube, c, users.kubeadm); err != nil {
				return templateUsers{}, err
			}
		case templates.Talos:
			// Talos templates are not of the default OS, the only ones this tells about.
		}
	}
	return users, nil
}

// addKubeadmVersions names a kubeadm cluster by its Kubernetes version and each of its machines'.
func addKubeadmVersions(ctx context.Context, kube client.Client, c *clusterv1.Cluster, byVersion map[string]string) error {
	name := c.Namespace + "/" + c.Name
	byVersion[c.Spec.Topology.Version] = name
	var machines clusterv1.MachineList
	if err := kube.List(ctx, &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return fmt.Errorf("listing cluster %s's machines: %w", name, err)
	}
	for i := range machines.Items {
		if v := machines.Items[i].Spec.Version; v != "" {
			byVersion[v] = name
		}
	}
	return nil
}

type templateRequest struct {
	// Distribution is rke2 for the cloud image RKE2 and k3s share, or kubeadm.
	Distribution      string   `json:"distribution"`
	KubernetesVersion string   `json:"kubernetesVersion,omitempty"`
	Node              string   `json:"node"`
	Storage           string   `json:"storage"`
	ImportStorage     string   `json:"importStorage"`
	ISOStorage        string   `json:"isoStorage,omitempty"`
	Bridge            string   `json:"bridge"`
	VLAN              int      `json:"vlan,omitempty"`
	BuildAddress      string   `json:"buildAddress,omitempty"`
	BuildGateway      string   `json:"buildGateway,omitempty"`
	BuildDNS          []string `json:"buildDNS,omitempty"`
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
		problems = append(problems, "Cloud-init image: a storage name")
	}
	if req.VLAN < 0 || req.VLAN > 4094 {
		problems = append(problems, "VLAN tag: 1 to 4094")
	}
	var plan templates.Request
	switch req.Distribution {
	case string(templates.RKE2):
		plan = templates.Request{Distribution: templates.RKE2}
	case string(templates.Kubeadm):
		plan = templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: req.KubernetesVersion}
	default:
		problems = append(problems, "For: RKE2 and k3s, or kubeadm")
	}
	network, problem := buildNetwork(req.BuildAddress, req.BuildGateway, req.BuildDNS)
	if problem != "" {
		problems = append(problems, problem)
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return templates.Builder{}, templates.Template{}, errors.New(strings.Join(problems, "; "))
	}
	t, err := templates.Plan(plan)
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
		return nil, "Address: an IPv4 address with its prefix length, such as 192.0.2.50/24"
	}
	if n.Gateway, err = netip.ParseAddr(gateway); err != nil || !n.Address.Contains(n.Gateway) {
		return nil, "Gateway: an address inside the address's network"
	}
	for _, d := range dns {
		a, err := netip.ParseAddr(d)
		if err != nil {
			return nil, "DNS servers: " + d + " is no IP address"
		}
		n.DNS = append(n.DNS, a)
	}
	if len(n.DNS) == 0 {
		return nil, "DNS servers: at least one, with an address"
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
	// A build takes minutes, so it gets the whole of a fresh ticket's life.
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	err = s.Appliance.BuildTemplate(ticket, func(ctx context.Context, j *appliance.Job) error { //nolint:contextcheck // the build outlives the request on purpose
		j.Add(fmt.Sprintf("Building template %s on node %s", t.Name, b.Node))
		b.Log = func(format string, a ...any) { j.Add(fmt.Sprintf(format, a...)) }
		vmid, buildErr := s.MakeTemplate(ctx, ticket, b, &t)
		if buildErr != nil {
			return buildErr
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
		writeError(w, http.StatusNotFound, "no template of Kubehenge's has that ID")
		return
	}
	needs, err := s.templateNeeds(r.Context())
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if c := needs.of(list[i].Tags); c != "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("cluster %s clones its machines from %s", c, list[i].Name))
		return
	}
	if err := s.Accounts.DeleteVM(r.Context(), u.ticket, list[i].Node, vmid); err != nil {
		s.proxmoxError(w, err)
		return
	}
	s.Log.Info("deleted template", "user", u.ticket.Username, "template", list[i].Name, "vmid", vmid)
	w.WriteHeader(http.StatusNoContent)
}
