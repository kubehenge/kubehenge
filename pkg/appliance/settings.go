package appliance

import (
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
)

// MinPassphrase is OpenTofu's minimum for a pbkdf2 passphrase.
const MinPassphrase = 16

// Settings are the management cluster's inputs the setup wizard collects; the module's other
// variables keep their defaults.
type Settings struct {
	// Nodes are the Proxmox VE nodes the management cluster's VMs run on, taken in turn.
	Nodes              []string `json:"nodes"`
	ImageDatastore     string   `json:"imageDatastore"`
	VMDatastore        string   `json:"vmDatastore"`
	Bridge             string   `json:"bridge"`
	VLAN               *int     `json:"vlan,omitempty"`
	NodeAddresses      []string `json:"nodeAddresses"`
	APIAddress         string   `json:"apiAddress"`
	Gateway            string   `json:"gateway"`
	DNSServers         []string `json:"dnsServers"`
	WorkloadDatastores []string `json:"workloadDatastores"`
	WorkloadBridges    []string `json:"workloadBridges"`
	BootstrapDatastore string   `json:"bootstrapDatastore"`
	// RegistryMirrors are endpoints tried before each registry, by registry name, for a site with
	// a pull-through cache or none of the internet.
	RegistryMirrors map[string][]string `json:"registryMirrors,omitempty"`
	// ControllerImage replaces the controller image the chart was released with.
	ControllerImage string `json:"controllerImage,omitempty"`
	// PodSubnets and ServiceSubnets are the management cluster's own ranges; when unset, the first
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
	// OfferHARules offers Proxmox VE's HA node-affinity rules, their names and nodes only, to those
	// who may create or change a cluster without Sys.Audit on / of their own.
	OfferHARules bool `json:"offerHARules,omitempty"`
	// ClusterDatastoreAllocate lets a cluster choose the CSI plugin's newest release, whose token
	// then holds Datastore.Allocate on the cluster's storage.
	ClusterDatastoreAllocate bool `json:"clusterDatastoreAllocate,omitempty"`
	// TemplateRefresh is the weekly build of the Ubuntu VM templates; unset, it runs with the
	// module's defaults.
	TemplateRefresh *TemplateRefresh `json:"templateRefresh,omitempty"`
	// ClusterSysAudit lets a cluster choose the cloud controller manager's HA group labels, whose
	// token then holds Sys.Audit on /.
	ClusterSysAudit bool `json:"clusterSysAudit,omitempty"`
}

// TemplateRefresh is when and where the Ubuntu VM templates are built again. Empty fields take the
// module's defaults: Sunday at 01:00 UTC, the first cluster storage, DHCP for the VM that
// prepares an image.
type TemplateRefresh struct {
	Enabled  bool   `json:"enabled"`
	Weekday  string `json:"weekday,omitempty"`
	Time     string `json:"time,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	// Storage holds the templates' disks, one of the cluster storages.
	Storage string `json:"storage,omitempty"`
	// Address, Gateway and DNS are the static network of the VM that prepares an image.
	Address string   `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
	// CreatorBuilds lets those who may create clusters have a missing template built.
	CreatorBuilds bool `json:"creatorBuilds,omitempty"`
}

// RefreshWeekdays are the days the template refresh may run on, as the module names them.
var RefreshWeekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

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
	if err != nil || !address.Addr().Is4() {
		p.addf("Template build address: an IPv4 address with prefix, such as 192.0.2.50/24")
		return
	}
	gateway, err := netip.ParseAddr(r.Gateway)
	if err != nil || !address.Contains(gateway) {
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
	for key, value := range map[string]string{
		"weekday": r.Weekday, "time": r.Time, "time_zone": r.TimeZone, "storage": r.Storage,
		"address": r.Address, "gateway": r.Gateway,
	} {
		if value != "" {
			out[key] = value
		}
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

// plainHTTPS tells whether s is an https:// URL with a host and without query, fragment or user.
func plainHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
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

// Candidate ranges, Kubernetes' and Flannel's usual ones first.
var (
	podCandidates     = []string{"10.244.0.0/16", "172.21.0.0/16", "10.201.0.0/16"}
	serviceCandidates = []string{"10.96.0.0/12", "172.20.0.0/16", "10.200.0.0/16"}
)

// Validate checks what the module would otherwise only refuse after starting, or what would leave
// the management cluster unreachable. The messages name the setup page's fields.
func (s *Settings) Validate() error {
	var p problems
	s.checkNodes(&p)
	s.checkPlacement(&p)
	gateway, err := netip.ParseAddr(s.Gateway)
	if err != nil || !gateway.Is4() {
		p.addf("Gateway: an IPv4 address")
	}
	network, nodes := s.checkNodeAddresses(&p, gateway)
	s.checkAPIAddress(&p, gateway, network, nodes)
	s.checkServers(&p)
	s.checkSubnets(&p)
	s.checkMirrors(&p)
	if err = ValidOIDC(s.ClusterOIDC); err != nil {
		p.addf("Identity providers: %s", err)
	}
	if err = ValidBackupTargets(s.BackupTargets); err != nil {
		p.addf("Backup targets: %s", err)
	}
	if s.TemplateRefresh != nil {
		s.TemplateRefresh.check(&p, s.WorkloadDatastores)
	}
	err = p.err()
	if err != nil {
		return err
	}
	_, err = s.ranges()
	return err
}

// KeepsFixed checks new settings against those the running management cluster was made with. Its
// nodes keep their addresses, and nodes are only added after them: a node given another address
// is cut off from Talos and etcd, and a node taken away leaves its etcd member behind, which costs
// the quorum. The API address, gateway, pod and service ranges and the VMs' disk storage stay as
// they are, since running nodes and pods depend on them.
func (s *Settings) KeepsFixed(was *Settings) error {
	var p problems
	if len(s.NodeAddresses) < len(was.NodeAddresses) || !slices.Equal(s.NodeAddresses[:len(was.NodeAddresses)], was.NodeAddresses) {
		p.addf("Node addresses: the nodes there are keep theirs (%s), and more can only be added after them", strings.Join(was.NodeAddresses, ", "))
	}
	for _, f := range [...]struct{ label, now, then string }{
		{"API address", s.APIAddress, was.APIAddress},
		{"Gateway", s.Gateway, was.Gateway},
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

func (s *Settings) checkNodes(p *problems) {
	if len(s.Nodes) == 0 || len(s.Nodes) > len(s.NodeAddresses) {
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
	if strings.TrimSpace(s.Bridge) == "" {
		p.addf("Bridge: choose one")
	}
	if s.VLAN != nil && (*s.VLAN < minVLAN || *s.VLAN > maxVLAN) {
		p.addf("VLAN tag: %d to %d", minVLAN, maxVLAN)
	}
}

// checkNodeAddresses checks the nodes' addresses against the gateway and each other, and returns
// the network they share and the addresses themselves.
func (s *Settings) checkNodeAddresses(p *problems, gateway netip.Addr) (netip.Prefix, map[netip.Addr]bool) {
	if n := len(s.NodeAddresses); n != 1 && n != 3 {
		p.addf("Node addresses: one, or three for high availability")
	}
	var network netip.Prefix
	nodes := map[netip.Addr]bool{}
	for _, a := range s.NodeAddresses {
		prefix, err := netip.ParsePrefix(a)
		switch {
		case err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0:
			p.addf("Node addresses: %s is no IPv4 address with its prefix length, such as 192.0.2.11/24", a)
			continue
		case prefix.Bits() < 31 && (prefix.Addr() == prefix.Masked().Addr() || prefix.Addr() == lastAddr(prefix)):
			p.addf("Node addresses: %s is its network's own or broadcast address", a)
		case nodes[prefix.Addr()]:
			p.addf("Node addresses: %s is listed twice", a)
		case gateway.IsValid() && !prefix.Contains(gateway):
			p.addf("Node addresses: %s is outside the gateway's network", a)
		case network.IsValid() && prefix.Masked() != network:
			p.addf("Node addresses: %s is in another network than %s", a, network)
		}
		nodes[prefix.Addr()] = true
		if !network.IsValid() {
			network = prefix.Masked()
		}
	}
	return network, nodes
}

func (s *Settings) checkAPIAddress(p *problems, gateway netip.Addr, network netip.Prefix, nodes map[netip.Addr]bool) {
	switch api, err := netip.ParseAddr(s.APIAddress); {
	case err != nil || !api.Is4():
		p.addf("API address: an IPv4 address")
	case nodes[api] || api == gateway:
		p.addf("API address: unused, neither a node address nor the gateway")
	case network.IsValid() && !network.Contains(api):
		p.addf("API address: inside the nodes' network, %s", network)
	}
}

// checkServers checks the name servers and what clusters may use.
func (s *Settings) checkServers(p *problems) {
	if len(s.DNSServers) == 0 {
		p.addf("DNS servers: at least one")
	}
	for _, d := range s.DNSServers {
		if _, err := netip.ParseAddr(d); err != nil {
			p.addf("DNS servers: %s is no IP address", d)
		}
	}
	if len(s.WorkloadDatastores) == 0 || len(s.WorkloadBridges) == 0 {
		p.addf("What clusters may use: at least one storage and one bridge")
	}
}

func (s *Settings) checkSubnets(p *problems) {
	for _, f := range [...]struct {
		label  string
		ranges []string
	}{{"Pod ranges", s.PodSubnets}, {"Service ranges", s.ServiceSubnets}} {
		for _, r := range f.ranges {
			if prefix, err := netip.ParsePrefix(r); err != nil || prefix != prefix.Masked() {
				p.addf("%s: %s is no network such as 10.244.0.0/16, without host bits", f.label, r)
			}
		}
	}
}

func (s *Settings) checkMirrors(p *problems) {
	for _, registry := range slices.Sorted(maps.Keys(s.RegistryMirrors)) {
		if strings.TrimSpace(registry) == "" || strings.ContainsAny(registry, "/ ") {
			p.addf("Registry mirrors: %q is no registry host name", registry)
		}
		for _, e := range s.RegistryMirrors[registry] {
			if u, err := url.Parse(e); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				p.addf("Registry mirrors: %s is no http:// or https:// URL", e)
			}
		}
	}
}

// ranges returns the pod and service ranges: those set, or the first candidates that overlap
// neither the nodes' networks, the API address, the gateway nor each other. Talos drops a node
// address inside them, which leaves the node unreachable.
func (s *Settings) ranges() (subnets, error) {
	var taken []netip.Prefix
	for _, a := range s.NodeAddresses {
		prefix, err := netip.ParsePrefix(a)
		if err != nil {
			return subnets{}, fmt.Errorf("nodeAddresses: %s: %w", a, err)
		}
		taken = append(taken, prefix.Masked())
	}
	for _, a := range []string{s.APIAddress, s.Gateway} {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return subnets{}, fmt.Errorf("%s is not an IP address", a)
		}
		taken = append(taken, netip.PrefixFrom(ip, ip.BitLen()))
	}
	pick := func(set, candidates []string, what string) ([]string, error) {
		if len(set) > 0 {
			return set, nil
		}
		for _, c := range candidates {
			prefix := netip.MustParsePrefix(c)
			if !slices.ContainsFunc(taken, prefix.Overlaps) {
				taken = append(taken, prefix)
				return []string{c}, nil
			}
		}
		return nil, fmt.Errorf("every %s range Kubehenge tries overlaps the nodes' network; set the %s ranges under Advanced", what, what)
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

// lastAddr is a prefix's last address, its broadcast address in IPv4.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	hostBits := p.Addr().BitLen() - p.Bits()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])|(uint32(1)<<hostBits-1))
	return netip.AddrFrom4(b)
}
