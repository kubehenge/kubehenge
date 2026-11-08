package appliance

import (
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// MinPassphrase is OpenTofu's minimum for a pbkdf2 passphrase.
const MinPassphrase = 16

// Settings are the management cluster's inputs the setup wizard collects. The module's other
// variables keep their defaults.
type Settings struct {
	// Nodes are the Proxmox VE nodes the management cluster's VMs run on, taken in turn.
	Nodes          []string `json:"nodes"`
	ImageDatastore string   `json:"imageDatastore"`
	VMDatastore    string   `json:"vmDatastore"`
	Bridge         string   `json:"bridge"`
	VLAN           *int     `json:"vlan,omitempty"`
	// MTU is the management VMs' network interface MTU, for a network that carries less than 1500
	// or more. 0 or empty keeps the bridge's.
	MTU int `json:"mtu,omitempty"`
	// IPFamilies are the cluster's address families, "IPv4" and "IPv6", the first being the primary
	// one. Empty means IPv4 alone.
	IPFamilies []string `json:"ipFamilies,omitempty"`
	// NodeAddresses and Gateway are the IPv4 ones, set when the cluster has IPv4. NodeAddressesIPv6
	// and GatewayIPv6 are the IPv6 ones, set when it has IPv6.
	NodeAddresses     []string `json:"nodeAddresses"`
	NodeAddressesIPv6 []string `json:"nodeAddressesIPv6,omitempty"`
	APIAddress        string   `json:"apiAddress"`
	Gateway           string   `json:"gateway"`
	GatewayIPv6       string   `json:"gatewayIPv6,omitempty"`
	DNSServers        []string `json:"dnsServers"`
	// SearchDomains are appended to a name without dots, on the management cluster's nodes and in
	// the clusters' network variable.
	SearchDomains []string `json:"searchDomains,omitempty"`
	// TimeServers are the NTP servers the nodes keep their clocks with. Empty keeps Talos's default
	// public ones.
	TimeServers        []string `json:"timeServers,omitempty"`
	WorkloadDatastores []string `json:"workloadDatastores"`
	WorkloadBridges    []string `json:"workloadBridges"`
	BootstrapDatastore string   `json:"bootstrapDatastore"`
	// RegistryMirrors are endpoints tried before each registry, by registry name, for a site with
	// a pull-through cache or none of the internet.
	RegistryMirrors map[string][]string `json:"registryMirrors,omitempty"`
	// ChartRepositories are mirrors of the Helm chart repositories, which registry mirrors do not
	// reach.
	ChartRepositories *ChartRepositories `json:"chartRepositories,omitempty"`
	// ControllerImage replaces the controller image the chart was released with.
	ControllerImage string `json:"controllerImage,omitempty"`
	// PodSubnets and ServiceSubnets are the management cluster's own ranges. When unset, the first
	// candidates clear of the nodes' network are taken.
	PodSubnets     []string `json:"podSubnets,omitempty"`
	ServiceSubnets []string `json:"serviceSubnets,omitempty"`
	// ClusterOIDC are the identity providers every new workload cluster trusts unless it names its
	// own.
	ClusterOIDC []OIDCProvider `json:"clusterOIDC,omitempty"`
	// BackupTargets are the S3 stores clusters that switch backups on choose from.
	BackupTargets []BackupTarget `json:"backupTargets,omitempty"`
	// ProxmoxHA has Proxmox VE HA keep the clusters' control-plane VMs on separate nodes, with a
	// second controller token that holds Sys.Console on /.
	ProxmoxHA bool `json:"proxmoxHA,omitempty"`
	// EtcdRestore has the controller restore clusters' etcd through the VMs' guest agents, with a
	// controller token of its own that holds VM.GuestAgent.Unrestricted on the workload pool.
	EtcdRestore bool `json:"etcdRestore,omitempty"`
	// HeadlampSignIn has the shared Headlamp sign those who may download a cluster's admin
	// kubeconfig in as cluster-admin.
	HeadlampSignIn bool `json:"headlampSignIn,omitempty"`
	// HeadlampActions has the shared Headlamp offer the Helm app catalog and the Prometheus charts
	// it ships with. It acts with each user's own credentials in the cluster.
	HeadlampActions bool `json:"headlampActions,omitempty"`
	// OfferHARules offers Proxmox VE's HA node-affinity rules, their names and nodes only, to those
	// who may create or change a cluster without Sys.Audit on / of their own.
	OfferHARules bool `json:"offerHARules,omitempty"`
	// ClusterDatastoreAllocate lets a cluster choose the CSI plugin's newest release, whose token
	// then holds Datastore.Allocate on the cluster's storage.
	ClusterDatastoreAllocate bool `json:"clusterDatastoreAllocate,omitempty"`
	// TemplateRefresh is the weekly build of the node OS VM templates. Unset, it runs with the
	// module's defaults.
	TemplateRefresh *TemplateRefresh `json:"templateRefresh,omitempty"`
	// ClusterSysAudit gives each cluster the cloud controller manager's HA group labels unless the
	// cluster switches them off, its token then holding Sys.Audit on /. Unset, it is on.
	ClusterSysAudit *bool `json:"clusterSysAudit,omitempty"`
	// NodeBootLogs has the controller read nodes' boot logs through their guest agents, its token
	// then holding VM.GuestAgent.FileRead on the workload pool. Unset, it is on.
	NodeBootLogs *bool `json:"nodeBootLogs,omitempty"`
	// ExtraClusterNamespaces are more namespaces the interface creates and manages clusters in,
	// beside its own, such as one per team.
	ExtraClusterNamespaces []string `json:"extraClusterNamespaces,omitempty"`
	// APIAuditLog has the management cluster's API server log the changes people make to its
	// output, where the interface reads them for its activity page.
	APIAuditLog bool `json:"apiAuditLog,omitempty"`
	// Proxy is the HTTP(S) proxy that every download from the management side goes through. Unset,
	// they go direct.
	Proxy *Proxy `json:"proxy,omitempty"`
}

// KeepsProxySecret restores the saved proxy's password where these settings came back with the
// address the interface showed, password hidden.
func (s *Settings) KeepsProxySecret(saved *Settings) {
	if saved != nil {
		s.Proxy.KeepsSecret(saved.Proxy)
	}
}

// Redacted is a copy of the settings that is safe to send to the interface: the proxy's password
// is hidden.
func (s *Settings) Redacted() *Settings {
	if s == nil {
		return nil
	}
	out := *s
	out.Proxy = s.Proxy.Redacted()
	return &out
}

// TemplateRefresh is when and where the node OS VM templates are built again. Empty fields take the
// module's defaults: Sunday at 01:00 UTC, the first cluster storage, DHCP for the VM that builds a
// node image.
type TemplateRefresh struct {
	Enabled  bool   `json:"enabled"`
	Weekday  string `json:"weekday,omitempty"`
	Time     string `json:"time,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	// Storage holds the templates' disks, one of the cluster storages.
	Storage string `json:"storage,omitempty"`
	// Node is the Proxmox VE node the build VM runs on. Empty takes the first management node.
	Node string `json:"node,omitempty"`
	// Bridge is the build VM's bridge, a VNet written zone/vnet. Empty takes the first cluster bridge.
	Bridge string `json:"bridge,omitempty"`
	// VLAN tags the build VM's network. 0 or empty sends it untagged.
	VLAN int `json:"vlan,omitempty"`
	// Address, Gateway and DNS are the static network of the VM that prepares an image.
	Address string   `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
	// CreatorBuilds lets those who may create clusters have a missing template built.
	CreatorBuilds bool `json:"creatorBuilds,omitempty"`
	// KubeadmPatches has the weekly build also make the kubeadm template of the newest patch
	// release of each Kubernetes minor version a kubeadm cluster runs, and of the next minor
	// version. Unset, it is on.
	KubeadmPatches *bool `json:"kubeadmPatches,omitempty"`
}

// RefreshWeekdays are the days the template refresh may run on, as the module names them.
var RefreshWeekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// GivesClusterSysAudit tells whether the settings let clusters hold Sys.Audit on / for their HA group
// labels.
func (s *Settings) GivesClusterSysAudit() bool {
	return s.ClusterSysAudit == nil || *s.ClusterSysAudit
}

// ReadsNodeBootLogs tells whether the settings let the controller read nodes' boot logs through
// their guest agents.
func (s *Settings) ReadsNodeBootLogs() bool {
	return s.NodeBootLogs == nil || *s.NodeBootLogs
}

// RefreshesTemplates tells whether the settings have the templates built again every week.
func (s *Settings) RefreshesTemplates() bool {
	return s.TemplateRefresh == nil || s.TemplateRefresh.Enabled
}

// check adds what is wrong with the refresh, in the words of the setup page's fields.
func (r *TemplateRefresh) check(p *problems, workloadDatastores []string) {
	if !r.Enabled {
		return
	}
	if r.Weekday != "" && !slices.Contains(RefreshWeekdays, r.Weekday) {
		p.addf("Template refresh day: one of %s", strings.Join(RefreshWeekdays, ", "))
	}
	if r.Time != "" {
		if _, err := time.Parse("15:04", r.Time); err != nil || len(r.Time) != len("15:04") {
			p.addf("Template refresh time: HH:MM")
		}
	}
	if r.TimeZone != "" {
		if _, err := time.LoadLocation(r.TimeZone); err != nil || r.TimeZone == "Local" {
			p.addf("Template refresh time zone: an IANA time zone, such as Europe/Berlin")
		}
	}
	if r.Storage != "" && !slices.Contains(workloadDatastores, r.Storage) {
		p.addf("Template disks: one of the cluster storages")
	}
	if r.Node != "" && !provisioner.ValidHostName(r.Node) {
		p.addf("Template build node: a Proxmox VE node name")
	}
	if r.Bridge != "" && !bridgeRef.MatchString(r.Bridge) {
		p.addf("Template build bridge: a bridge, or a VNet written zone/vnet")
	}
	if r.VLAN < 0 || r.VLAN > maxVLAN {
		p.addf("Template build VLAN: 0 to %d", maxVLAN)
	}
	r.checkNetwork(p)
}

// checkNetwork adds what is wrong with the build VM's static network.
func (r *TemplateRefresh) checkNetwork(p *problems) {
	if r.Address == "" {
		if r.Gateway != "" || len(r.DNS) > 0 {
			p.addf("Template build address: needed for its gateway and DNS servers")
		}
		return
	}
	address, err := netip.ParsePrefix(r.Address)
	if err != nil || address.Addr().Is4In6() {
		p.addf("Template build address: an IPv4 address with prefix, such as 192.0.2.50/24, or an IPv6 one such as 2001:db8::50/64")
		return
	}
	gateway, err := netip.ParseAddr(r.Gateway)
	if err != nil || !gatewayReaches(address, gateway) {
		p.addf("Template build gateway: an address in %s", address.Masked())
	}
	for _, d := range r.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			p.addf("Template build DNS servers: IP addresses")
			break
		}
	}
}

// moduleTemplateRefresh is the module's template_refresh for the settings: an empty object takes
// every default, the refresh switched on among them.
func moduleTemplateRefresh(r *TemplateRefresh) map[string]any {
	out := map[string]any{}
	if r == nil {
		return out
	}
	out["enabled"] = r.Enabled
	out["creator_builds"] = r.CreatorBuilds
	if r.KubeadmPatches != nil {
		out["kubeadm_patches"] = *r.KubeadmPatches
	}
	for key, value := range map[string]string{
		"weekday": r.Weekday, "time": r.Time, "time_zone": r.TimeZone, "storage": r.Storage,
		"address": r.Address, "gateway": r.Gateway,
		"node": r.Node, "bridge": r.Bridge,
	} {
		if value != "" {
			out[key] = value
		}
	}
	if r.VLAN > 0 {
		out["vlan"] = r.VLAN
	}
	if len(r.DNS) > 0 {
		out["dns"] = r.DNS
	}
	return out
}

// OIDCProvider is an OpenID Connect provider a cluster's API server accepts ID tokens from. Unset
// fields take the chart's defaults.
type OIDCProvider struct {
	IssuerURL            string   `json:"issuerURL"`
	ClientID             string   `json:"clientID"`
	UsernameClaim        string   `json:"usernameClaim,omitempty"`
	UsernamePrefix       *string  `json:"usernamePrefix,omitempty"`
	GroupsClaim          string   `json:"groupsClaim,omitempty"`
	GroupsPrefix         *string  `json:"groupsPrefix,omitempty"`
	Scopes               []string `json:"scopes,omitempty"`
	CertificateAuthority string   `json:"certificateAuthority,omitempty"`
}

// MaxOIDCProviders is how many providers a cluster may trust, as the chart's schema allows.
const MaxOIDCProviders = 8

// The VLAN tags Proxmox VE accepts on a bridge port.
const (
	minVLAN = 1
	maxVLAN = 4094
)

// The MTUs of a node's interface. Proxmox VE takes one from 576, but IPv6 does not work below 1280.
const (
	minMTU = 1280
	maxMTU = 65520
)

// bridgeRef is a node's bridge by name, or a VNet as zone/vnet.
var bridgeRef = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*/)?[A-Za-z0-9][A-Za-z0-9._-]*$`)

// plainHTTPS tells whether s is an https:// URL with a host and without query, fragment or user.
func plainHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == schemeHTTPS && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

// Validate checks a provider as the API server would on start, where a mistake keeps the control
// plane from coming up.
func (p *OIDCProvider) Validate() error {
	switch {
	case !plainHTTPS(p.IssuerURL):
		return fmt.Errorf("issuer %q: an https:// URL without query, fragment or user", p.IssuerURL)
	case strings.TrimSpace(p.ClientID) == "":
		return fmt.Errorf("issuer %s: clientID is required", p.IssuerURL)
	}
	// Kubernetes keeps system: for its own users and groups, such as system:masters.
	for _, prefix := range []*string{p.UsernamePrefix, p.GroupsPrefix} {
		if prefix != nil && strings.HasPrefix(*prefix, "system:") {
			return fmt.Errorf("issuer %s: no prefix may start with %q", p.IssuerURL, "system:")
		}
	}
	if p.CertificateAuthority != "" {
		if ok := x509.NewCertPool().AppendCertsFromPEM([]byte(p.CertificateAuthority)); !ok {
			return fmt.Errorf("issuer %s: certificateAuthority holds no PEM certificate", p.IssuerURL)
		}
	}
	return nil
}

// ValidOIDC checks a list of providers: each valid, few enough, no issuer twice.
func ValidOIDC(providers []OIDCProvider) error {
	if len(providers) > MaxOIDCProviders {
		return fmt.Errorf("at most %d identity providers", MaxOIDCProviders)
	}
	seen := map[string]bool{}
	for i := range providers {
		p := &providers[i]
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.IssuerURL] {
			return fmt.Errorf("issuer %s is listed twice", p.IssuerURL)
		}
		seen[p.IssuerURL] = true
	}
	return nil
}

// ValidPassphrase checks a state passphrase: long enough for pbkdf2, printable, on one line.
func ValidPassphrase(p string) error {
	if len([]rune(p)) < MinPassphrase {
		return fmt.Errorf("the passphrase needs at least %d characters", MinPassphrase)
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return errors.New("the passphrase may hold only printable characters")
		}
	}
	return nil
}

// problems collects what is wrong with settings, in the words of the setup page's fields.
type problems []string

func (p *problems) addf(format string, a ...any) { *p = append(*p, fmt.Sprintf(format, a...)) }

func (p *problems) err() error {
	if len(*p) == 0 {
		return nil
	}
	return errors.New(strings.Join(*p, "; "))
}

// subnets are the management cluster's own pod and service ranges.
type subnets struct {
	pods, services []string
}

// The address families a cluster has. The first of IPFamilies is the primary one.
const (
	familyIPv4 = "IPv4"
	familyIPv6 = "IPv6"
)

// Candidate ranges by family, Kubernetes' and Flannel's usual ones first for IPv4. The first IPv6
// ones are the module's defaults. An IPv6 pod range is /48 to /63, since each node takes a /64 of
// it and kube-controller-manager allows 16 bits between the two. A service range is /108 to /120.
var (
	podCandidates = map[string][]string{
		familyIPv4: {"10.244.0.0/16", "172.21.0.0/16", "10.201.0.0/16"},
		familyIPv6: {"fd5e:4b1c:9a20::/56", "fd5e:4b1c:9a30::/56", "fd5e:4b1c:9a40::/56"},
	}
	serviceCandidates = map[string][]string{
		familyIPv4: {"10.96.0.0/12", "172.20.0.0/16", "10.200.0.0/16"},
		familyIPv6: {"fd5e:4b1c:9a21::/112", "fd5e:4b1c:9a31::/112", "fd5e:4b1c:9a41::/112"},
	}
)

// maxFamilies is IPv4 and IPv6 together.
const maxFamilies = 2

const (
	minIPv6PodBits, maxIPv6PodBits         = 48, 63
	minIPv6ServiceBits, maxIPv6ServiceBits = 108, 120
)

// Families are the cluster's address families, the primary one first. Settings that name none are
// IPv4 alone.
func (s *Settings) Families() []string {
	if len(s.IPFamilies) == 0 {
		return []string{familyIPv4}
	}
	return s.IPFamilies
}

// HasIPv6 tells that the cluster has an IPv6 family, alone or beside IPv4.
func (s *Settings) HasIPv6() bool { return slices.Contains(s.Families(), familyIPv6) }

// NodeCount is the number of management nodes, which the primary family's addresses give.
func (s *Settings) NodeCount() int { return len(s.addressesOf(s.Families()[0])) }

// inFamily tells whether an address belongs to the family. An IPv4-mapped IPv6 address is neither.
func inFamily(a netip.Addr, family string) bool {
	if a.Zone() != "" {
		return false
	}
	if family == familyIPv6 {
		return a.Is6() && !a.Is4In6()
	}
	return a.Is4()
}

// gatewayReaches tells whether a gateway is on the network of an address with prefix. An IPv6
// gateway is often a link-local address, which no prefix holds.
func gatewayReaches(address netip.Prefix, gateway netip.Addr) bool {
	return address.Contains(gateway) || (address.Addr().Is6() && gateway.Is6() && gateway.IsLinkLocalUnicast())
}

// familyNet is what the checks learned of one family's addresses.
type familyNet struct {
	family  string
	gateway netip.Addr
	network netip.Prefix
	nodes   map[netip.Addr]bool
}

// Validate checks what the module would otherwise only refuse after starting, or what would leave
// the management cluster unreachable. The messages name the setup page's fields.
func (s *Settings) Validate() error {
	var p problems
	families := s.checkFamilies(&p)
	s.checkNodes(&p)
	s.checkPlacement(&p)
	nets := make([]familyNet, 0, len(families))
	for _, family := range families {
		nets = append(nets, s.checkFamilyAddresses(&p, family))
	}
	if len(nets) > 0 {
		s.checkAPIAddress(&p, &nets[0])
	}
	if len(nets) == maxFamilies && len(s.NodeAddresses) != len(s.NodeAddressesIPv6) {
		label, _ := familyLabels(families[1], len(families))
		p.addf("%s: as many as the %s ones, node i taking the i-th of each", label, families[0])
	}
	s.checkServers(&p, families)
	s.checkSubnets(&p, families)
	s.checkMirrors(&p)
	s.ChartRepositories.check(&p)
	if err := ValidOIDC(s.ClusterOIDC); err != nil {
		p.addf("Identity providers: %s", err)
	}
	if err := ValidBackupTargets(s.BackupTargets); err != nil {
		p.addf("Backup targets: %s", err)
	}
	if s.TemplateRefresh != nil {
		s.TemplateRefresh.check(&p, s.WorkloadDatastores)
	}
	s.checkNamespaces(&p)
	checkProxySetting(s.Proxy, &p)
	if err := p.err(); err != nil {
		return err
	}
	_, err := s.ranges()
	return err
}

// KeepsFixed checks new settings against those the running management cluster was made with. Its
// nodes keep their addresses, and nodes are only added after them: a node given another address
// is cut off from Talos and etcd, and a node taken away leaves its etcd member behind, which costs
// the quorum. The API address, gateway, pod and service ranges and the VMs' disk storage stay as
// they are, since running nodes and pods depend on them.
func (s *Settings) KeepsFixed(was *Settings) error {
	var p problems
	if !slices.Equal(s.Families(), was.Families()) {
		p.addf("Address families: stay %s once the management cluster exists", strings.Join(was.Families(), ", "))
	}
	shown := len(was.Families())
	for _, f := range [...]string{familyIPv4, familyIPv6} {
		label, gateway := familyLabels(f, shown)
		if changesAddresses(s.addressesOf(f), was.addressesOf(f)) {
			p.addf("%s: the nodes there are keep theirs (%s), and more can only be added after them", label, strings.Join(was.addressesOf(f), ", "))
		}
		if s.gatewayOf(f) != was.gatewayOf(f) {
			p.addf("%s: stays %s once the management cluster exists", gateway, was.gatewayOf(f))
		}
	}
	// The VMs take the nodes in turn, so adding, removing or reordering one moves running etcd VMs
	// to other nodes.
	if !slices.Equal(s.Nodes, was.Nodes) {
		p.addf("Proxmox VE nodes: stay %s once the management cluster exists, or its VMs would migrate to other nodes", strings.Join(was.Nodes, ", "))
	}
	for _, f := range [...]struct{ label, now, then string }{
		{"API address", s.APIAddress, was.APIAddress},
		{"VM disks", s.VMDatastore, was.VMDatastore},
	} {
		if f.now != f.then {
			p.addf("%s: stays %s once the management cluster exists", f.label, f.then)
		}
	}
	now, err := s.ranges()
	then, thenErr := was.ranges()
	if err == nil && thenErr == nil {
		if !slices.Equal(now.pods, then.pods) {
			p.addf("Pod ranges: stay %s once the management cluster exists", strings.Join(then.pods, ", "))
		}
		if !slices.Equal(now.services, then.services) {
			p.addf("Service ranges: stay %s once the management cluster exists", strings.Join(then.services, ", "))
		}
	}
	return p.err()
}

// addressesOf are the node addresses of one family, and gatewayOf its gateway.
func (s *Settings) addressesOf(family string) []string {
	if family == familyIPv6 {
		return s.NodeAddressesIPv6
	}
	return s.NodeAddresses
}

func (s *Settings) gatewayOf(family string) string {
	if family == familyIPv6 {
		return s.GatewayIPv6
	}
	return s.Gateway
}

func (s *Settings) checkNodes(p *problems) {
	if len(s.Nodes) == 0 || len(s.Nodes) > s.NodeCount() {
		p.addf("Proxmox VE nodes: at least one, and no more than there are node addresses")
	}
	seen := map[string]bool{}
	for _, n := range s.Nodes {
		if strings.TrimSpace(n) == "" || seen[n] {
			p.addf("Proxmox VE nodes: each named once")
			break
		}
		seen[n] = true
	}
}

// checkPlacement checks where the management cluster's VMs and images go.
func (s *Settings) checkPlacement(p *problems) {
	for _, f := range [...]struct{ label, value string }{
		{"VM disks", s.VMDatastore}, {"Talos image", s.ImageDatastore}, {"Cloud-init images", s.BootstrapDatastore},
	} {
		if strings.TrimSpace(f.value) == "" {
			p.addf("%s: choose a storage", f.label)
		}
	}
	if !bridgeRef.MatchString(s.Bridge) {
		p.addf("Bridge: choose one")
	}
	if s.VLAN != nil && (*s.VLAN < minVLAN || *s.VLAN > maxVLAN) {
		p.addf("VLAN tag: %d to %d", minVLAN, maxVLAN)
	}
	if s.MTU != 0 && (s.MTU < minMTU || s.MTU > maxMTU) {
		p.addf("MTU: %d to %d", minMTU, maxMTU)
	}
}

// checkFamilies checks the address families and returns them, or nil when they are wrong.
func (s *Settings) checkFamilies(p *problems) []string {
	families := s.Families()
	valid := len(families) <= maxFamilies && len(slices.Compact(slices.Sorted(slices.Values(families)))) == len(families) &&
		!slices.ContainsFunc(families, func(f string) bool { return f != familyIPv4 && f != familyIPv6 })
	if !valid {
		p.addf("Address families: IPv4, IPv6, or both with the primary one first")
		return nil
	}
	for _, f := range [...]string{familyIPv4, familyIPv6} {
		if slices.Contains(families, f) {
			continue
		}
		// A family the cluster lacks has no field on the page, so its own name says which is meant.
		label, gateway := familyLabels(f, maxFamilies)
		if len(s.addressesOf(f)) > 0 {
			p.addf("%s: leave empty, the cluster has no %s", label, f)
		}
		if s.gatewayOf(f) != "" {
			p.addf("%s: leave empty, the cluster has no %s", gateway, f)
		}
	}
	return families
}

// familyLabels are the names of a family's node addresses and gateway on the setup page, which
// shows that many families. With both shown, each names its family.
func familyLabels(family string, shown int) (string, string) {
	if shown > 1 {
		return family + " node addresses", family + " gateway"
	}
	return "Node addresses", "Gateway"
}

// checkFamilyAddresses checks the nodes' addresses of one family against its gateway and each other,
// and returns what the API address is checked against.
func (s *Settings) checkFamilyAddresses(p *problems, family string) familyNet {
	label, gatewayLabel := familyLabels(family, len(s.Families()))
	example := "192.0.2.11/24"
	if family == familyIPv6 {
		example = "2001:db8::11/64"
	}
	gateway, err := netip.ParseAddr(s.gatewayOf(family))
	if err != nil || !inFamily(gateway, family) {
		p.addf("%s: an %s address", gatewayLabel, family)
		gateway = netip.Addr{}
	}
	out := familyNet{family: family, gateway: gateway, nodes: map[netip.Addr]bool{}}
	addresses := s.addressesOf(family)
	if n := len(addresses); n != 1 && n != 3 {
		p.addf("%s: one, or three for high availability", label)
	}
	for _, a := range addresses {
		prefix, err := netip.ParsePrefix(a)
		own := prefix.Addr() == prefix.Masked().Addr()
		switch {
		case err != nil || !inFamily(prefix.Addr(), family) || prefix.Bits() == 0:
			p.addf("%s: %s is no %s address with its prefix length, such as %s", label, a, family, example)
			continue
		case family == familyIPv4 && prefix.Bits() < 31 && (own || prefix.Addr() == lastAddr(prefix)):
			p.addf("%s: %s is its network's own or broadcast address", label, a)
		case family == familyIPv6 && prefix.Bits() < 127 && own:
			p.addf("%s: %s is its network's own address", label, a)
		case out.nodes[prefix.Addr()]:
			p.addf("%s: %s is listed twice", label, a)
		case gateway.IsValid() && !gatewayReaches(prefix, gateway):
			p.addf("%s: %s is outside the gateway's network", label, a)
		case out.network.IsValid() && prefix.Masked() != out.network:
			p.addf("%s: %s is in another network than %s", label, a, out.network)
		}
		out.nodes[prefix.Addr()] = true
		if !out.network.IsValid() {
			out.network = prefix.Masked()
		}
	}
	return out
}

// changesAddresses tells whether a node's address was changed, or a node taken away. Nodes added
// after the existing ones are fine.
func changesAddresses(now, was []string) bool {
	return len(now) < len(was) || !slices.Equal(now[:len(was)], was)
}

// checkAPIAddress checks the API address against the primary family's network.
func (s *Settings) checkAPIAddress(p *problems, primary *familyNet) {
	switch api, err := netip.ParseAddr(s.APIAddress); {
	case err != nil || !inFamily(api, primary.family):
		p.addf("API address: an %s address", primary.family)
	case primary.nodes[api] || api == primary.gateway:
		p.addf("API address: unused, neither a node address nor the gateway")
	case primary.network.IsValid() && !primary.network.Contains(api):
		p.addf("API address: inside the nodes' network, %s", primary.network)
	}
}

// checkServers checks the name servers and what clusters may use.
func (s *Settings) checkServers(p *problems, families []string) {
	if len(s.DNSServers) == 0 {
		p.addf("DNS servers: at least one")
	}
	for _, d := range s.DNSServers {
		a, err := netip.ParseAddr(d)
		switch {
		case err != nil || a.Zone() != "":
			p.addf("DNS servers: %s is no IP address", d)
		case families != nil && !slices.ContainsFunc(families, func(f string) bool { return inFamily(a, f) }):
			p.addf("DNS servers: %s is of an address family the cluster does not have", d)
		}
	}
	if len(families) == maxFamilies {
		for _, f := range families {
			if !slices.ContainsFunc(s.DNSServers, func(d string) bool { a, err := netip.ParseAddr(d); return err == nil && inFamily(a, f) }) {
				p.addf("DNS servers: at least one %s address, as the cluster has both families", f)
			}
		}
	}
	if len(s.WorkloadDatastores) == 0 || len(s.WorkloadBridges) == 0 {
		p.addf("What clusters may use: at least one storage and one bridge")
	}
	for _, b := range s.WorkloadBridges {
		if !bridgeRef.MatchString(b) {
			p.addf("What clusters may use: %q is no bridge, write a VNet as zone/vnet", b)
		}
	}
	s.checkSearchDomains(p)
	s.checkTimeServers(p)
}

// maxSearchDomains is what glibc's resolver reads, and what the clusters' network variable allows.
const maxSearchDomains = 6

// checkSearchDomains holds the domains to what the clusters' network variable accepts, so the
// management cluster and the clusters made on it resolve short names alike.
func (s *Settings) checkSearchDomains(p *problems) {
	if len(s.SearchDomains) > maxSearchDomains {
		p.addf("Search domains: at most %d", maxSearchDomains)
	}
	seen := map[string]bool{}
	for _, d := range s.SearchDomains {
		switch {
		case seen[d]:
			p.addf("Search domains: %s is listed twice", d)
		case !provisioner.ValidHostName(d) || strings.HasSuffix(d, "."):
			p.addf("Search domains: %q is no domain name", d)
		}
		seen[d] = true
	}
}

// checkTimeServers checks that each time server is a host name or an address Talos can resolve and
// reach, not a URL and not an address that leads back to the node itself.
func (s *Settings) checkTimeServers(p *problems) {
	seen := map[string]bool{}
	for _, t := range s.TimeServers {
		switch problem := provisioner.TimeServerProblem(t); {
		case seen[t]:
			p.addf("Time servers: %s is listed twice", t)
		case problem != "":
			p.addf("Time servers: %s", problem)
		}
		seen[t] = true
	}
}

// checkSubnets checks the ranges the administrator set: one per family in the families' order, each
// a network without host bits, an IPv6 one of a length Kubernetes accepts, and none overlapping the
// nodes' networks, the API address, the gateways or each other.
func (s *Settings) checkSubnets(p *problems, families []string) {
	sets := [...]rangeSet{
		{"Pod ranges", s.PodSubnets, minIPv6PodBits, maxIPv6PodBits},
		{"Service ranges", s.ServiceSubnets, minIPv6ServiceBits, maxIPv6ServiceBits},
	}
	valid := true
	for i := range sets {
		valid = sets[i].check(p, families) && valid
	}
	if !valid {
		return
	}
	reserved, err := s.reserved()
	if err != nil {
		return
	}
	for _, r := range sets[0].ranges {
		prefix := netip.MustParsePrefix(r)
		for _, other := range s.ServiceSubnets {
			if prefix.Overlaps(netip.MustParsePrefix(other)) {
				p.addf("%s: %s overlaps the service range %s", sets[0].label, r, other)
			}
		}
	}
	for i := range sets {
		for _, r := range sets[i].ranges {
			prefix := netip.MustParsePrefix(r)
			for _, t := range reserved {
				if prefix.Overlaps(t.prefix) {
					p.addf("%s: %s overlaps %s, which Talos would drop node addresses in", sets[i].label, r, t.what)
				}
			}
		}
	}
}

// rangeSet is the pod or the service ranges the administrator set, with the lengths an IPv6 range
// of its kind may have.
type rangeSet struct {
	label            string
	ranges           []string
	minBits, maxBits int
}

// check adds what is wrong with the set's ranges and tells whether there was nothing.
func (r *rangeSet) check(p *problems, families []string) bool {
	if len(r.ranges) == 0 {
		return true
	}
	valid := true
	if families != nil && len(r.ranges) != len(families) {
		p.addf("%s: one per address family, in this order: %s", r.label, strings.Join(families, ", "))
		valid = false
	}
	for i, c := range r.ranges {
		family, example := familyIPv4, "10.244.0.0/16"
		if i < len(families) {
			family = families[i]
		}
		if family == familyIPv6 {
			example = "fd5e:4b1c:9a20::/56"
		}
		prefix, err := netip.ParsePrefix(c)
		switch {
		case err != nil || prefix != prefix.Masked():
			p.addf("%s: %s is no network such as %s, without host bits", r.label, c, example)
		case i < len(families) && !inFamily(prefix.Addr(), family):
			p.addf("%s: %s is no %s network, and the ranges come one per family in this order: %s", r.label, c, family, strings.Join(families, ", "))
		case prefix.Addr().Is6() && (prefix.Bits() < r.minBits || prefix.Bits() > r.maxBits):
			p.addf("%s: %s is /%d, and an IPv6 range is /%d to /%d", r.label, c, prefix.Bits(), r.minBits, r.maxBits)
		default:
			continue
		}
		valid = false
	}
	return valid
}

func (s *Settings) checkMirrors(p *problems) {
	for _, registry := range slices.Sorted(maps.Keys(s.RegistryMirrors)) {
		if strings.TrimSpace(registry) == "" || strings.ContainsAny(registry, "/ ") {
			p.addf("Registry mirrors: %q is no registry host name", registry)
		}
		for _, e := range s.RegistryMirrors[registry] {
			if u, err := url.Parse(e); err != nil || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) || u.Host == "" {
				p.addf("Registry mirrors: %s is no http:// or https:// URL", e)
			}
		}
	}
}

func (s *Settings) checkNamespaces(p *problems) {
	for i, ns := range s.ExtraClusterNamespaces {
		if err := provisioner.ValidExtraNamespace(ns); err != nil {
			p.addf("Namespaces for clusters: %s", err)
		} else if slices.Contains(s.ExtraClusterNamespaces[:i], ns) {
			p.addf("Namespaces for clusters: %s is named twice", ns)
		}
	}
}

// ranges returns the pod and service ranges: those set, or the first candidates that overlap
// neither the nodes' networks, the API address, the gateway nor each other. Talos drops a node
// address inside them, which leaves the node unreachable.
func (s *Settings) ranges() (subnets, error) {
	reserved, err := s.reserved()
	if err != nil {
		return subnets{}, err
	}
	taken := make([]netip.Prefix, 0, len(reserved))
	for _, r := range reserved {
		taken = append(taken, r.prefix)
	}
	pick := func(set []string, candidates map[string][]string, what string) ([]string, error) {
		if len(set) > 0 {
			return set, nil
		}
		var out []string
	families:
		for _, family := range s.Families() {
			for _, c := range candidates[family] {
				prefix := netip.MustParsePrefix(c)
				if !slices.ContainsFunc(taken, prefix.Overlaps) {
					taken = append(taken, prefix)
					out = append(out, c)
					continue families
				}
			}
			return nil, fmt.Errorf("every %s range Kubehenge tries overlaps the nodes' network; set the %s ranges under Advanced", what, what)
		}
		return out, nil
	}
	pods, err := pick(s.PodSubnets, podCandidates, "pod")
	if err != nil {
		return subnets{}, err
	}
	services, err := pick(s.ServiceSubnets, serviceCandidates, "service")
	if err != nil {
		return subnets{}, err
	}
	return subnets{pods: pods, services: services}, nil
}

// reserved are the networks and addresses a pod or service range must stay clear of: the nodes'
// networks, the API address and the gateways. Talos drops a node address inside such a range.
func (s *Settings) reserved() ([]place, error) {
	var out []place
	for _, family := range s.Families() {
		for _, a := range s.addressesOf(family) {
			prefix, err := netip.ParsePrefix(a)
			if err != nil {
				return nil, fmt.Errorf("node address %s: %w", a, err)
			}
			out = append(out, place{prefix.Masked(), fmt.Sprintf("the nodes' network %s", prefix.Masked())})
		}
	}
	hosts := []struct{ what, address string }{{"the API address", s.APIAddress}}
	for _, family := range s.Families() {
		hosts = append(hosts, struct{ what, address string }{"the gateway", s.gatewayOf(family)})
	}
	for _, h := range hosts {
		ip, err := netip.ParseAddr(h.address)
		if err != nil {
			return nil, fmt.Errorf("%s is not an IP address", h.address)
		}
		out = append(out, place{netip.PrefixFrom(ip, ip.BitLen()), fmt.Sprintf("%s %s", h.what, ip)})
	}
	return out, nil
}

// lastAddr is an IPv4 prefix's last address, its broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	hostBits := p.Addr().BitLen() - p.Bits()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])|(uint32(1)<<hostBits-1))
	return netip.AddrFrom4(b)
}
