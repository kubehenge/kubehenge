// Package ui is Kubehenge's web interface: users sign in with their Proxmox VE accounts and manage
// the clusters their Proxmox VE privileges allow, which the interface then creates, changes and
// deletes in the management cluster with its own account.
package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// Accounts is what the interface asks Proxmox VE, as the signed-in user.
type Accounts interface {
	SignIns
	Resources
	Firewalls
	Guests
	VMIDPool
	Removals
	Upgraders
}

// VMIDPool is the range Proxmox VE offers new VMs IDs from, and the IDs in use.
type VMIDPool interface {
	NextID(ctx context.Context, t *pve.Ticket) (pve.NextID, error)
	VMIDs(ctx context.Context, t *pve.Ticket) ([]int, error)
}

// SignIns are the Proxmox VE sign-ins the interface's sessions rest on.
type SignIns interface {
	SignIn(ctx context.Context, username, password string) (*pve.Ticket, *pve.Challenge, error)
	TokenUser(ctx context.Context, token string) (*pve.Ticket, error)
	Answer(ctx context.Context, c *pve.Challenge, kind, code string) (*pve.Ticket, error)
	Renew(ctx context.Context, t *pve.Ticket) (*pve.Ticket, error)
	Realms(ctx context.Context) ([]pve.Realm, error)
	OpenIDAuthURL(ctx context.Context, realm, redirect string) (string, error)
	OpenIDLogin(ctx context.Context, code, state, redirect string) (*pve.Ticket, error)
}

// Resources are what a signed-in user holds and sees in Proxmox VE.
type Resources interface {
	Privileges(ctx context.Context, t *pve.Ticket, aclPath string) (pve.Privileges, error)
	AllPrivileges(ctx context.Context, t *pve.Ticket) (map[string]pve.Privileges, error)
	Storages(ctx context.Context, t *pve.Ticket) ([]pve.Storage, error)
	Nodes(ctx context.Context, t *pve.Ticket) ([]pve.Node, error)
	Bridges(ctx context.Context, t *pve.Ticket, node string) ([]pve.Bridge, error)
	DNS(ctx context.Context, t *pve.Ticket, node string) (pve.NodeDNS, error)
	AccessNames(ctx context.Context, t *pve.Ticket) (pve.Names, error)
	NodeAddresses(ctx context.Context, t *pve.Ticket) (map[string]string, error)
	NodeAffinityRules(ctx context.Context, t *pve.Ticket) ([]pve.HARule, error)
	Network(ctx context.Context, t *pve.Ticket, bridge string) (pve.Network, error)
}

// Firewalls are the Proxmox VE firewalls a signed-in user may read.
type Firewalls interface {
	APIReach(ctx context.Context, t *pve.Ticket) (*pve.APIReach, error)
}

// Guests are the VMs a signed-in user sees in Proxmox VE, and how they run.
type Guests interface {
	PoolMembers(ctx context.Context, t *pve.Ticket, pool string) ([]pve.Member, error)
	VMLoad(ctx context.Context, t *pve.Ticket, node string, vmid int) ([]pve.LoadPoint, error)
	Templates(ctx context.Context, t *pve.Ticket, pool string) ([]pve.Member, error)
	BackupCovers(ctx context.Context, t *pve.Ticket, vmid int) (bool, error)
	ApplianceID(ctx context.Context, t *pve.Ticket, hint int, macs []string) (int, error)
	NodeTime(ctx context.Context, t *pve.Ticket, node string) (time.Time, error)
}

// Removals are what an administrator's runs remove from Proxmox VE that the management-cluster
// module does not.
type Removals interface {
	DeleteVM(ctx context.Context, t *pve.Ticket, node string, vmid int) error
	RebootVM(ctx context.Context, t *pve.Ticket, node string, vmid int) error
	ReleaseHA(ctx context.Context, t *pve.Ticket, prefix string) error
}

// Server serves the interface and its API.
type Server struct {
	Accounts Accounts
	// Kube acts in the management cluster with the interface's own account, after the user's
	// Proxmox VE privileges have been checked. Nil until the appliance has set the management
	// cluster up.
	Kube client.Client
	// Probe tells which of the addresses a host answers on, for the addresses a new cluster is
	// offered. Nil probes them from where the interface runs.
	Probe func(ctx context.Context, addrs []netip.Addr) map[netip.Addr]bool
	// Appliance is set when the interface runs in Kubehenge's appliance, which creates and looks
	// after the management cluster.
	Appliance *appliance.Appliance
	// ApplianceVMID is the appliance's own guest ID in Proxmox VE as installed, for telling
	// administrators whether it is backed up. 0 when not known. With ApplianceMACs, the MAC
	// addresses of its network interfaces, the interface finds the ID again after a restore to
	// another ID or a clone.
	ApplianceVMID int
	ApplianceMACs []string
	// CertificateExpires is when the certificate the interface serves runs out. Zero when not
	// known. It is read at start, as the certificate is.
	CertificateExpires time.Time
	// CertificateIPs and CertificateNames are the addresses and DNS names the certificate is for.
	// OwnAddresses gives the addresses the machine has now. Together they tell administrators
	// when the appliance's address moved away from its certificate's. Nil OwnAddresses tells
	// nothing.
	CertificateIPs   []netip.Addr
	CertificateNames []string
	OwnAddresses     func() []netip.Addr
	// DiskUse gives the bytes taken and free on the appliance's disk, for telling administrators
	// to grow it before an upgrade or an apply runs out of room. Nil tells nothing.
	DiskUse func() (used, free uint64, err error)
	// ProxmoxInsecure tells administrators that the appliance and the clusters do not verify the
	// Proxmox VE API's certificate, as it was installed.
	ProxmoxInsecure bool
	// ProxmoxURL is the Proxmox VE API's address, whose web interface opens a machine's console.
	// Empty offers no console.
	ProxmoxURL string
	// NewKube makes the client for the management cluster from the kubeconfig the appliance wrote.
	NewKube func(kubeconfig []byte) (client.Client, error)
	// Upgrades are set in the appliance, which upgrades its own software.
	Upgrades *Upgrades
	// MakeTemplate builds VM templates in the appliance with an administrator's session.
	MakeTemplate MakeTemplate
	// Releases are the Kubernetes versions RKE2 and k3s publish, the same channels the
	// controller's automatic upgrades read. Nil leaves their version to be typed in.
	Releases     provisioner.Releases
	releaseCache releaseCache
	// PinChecks reads the checksum file and install script of an RKE2 or k3s release to pin. Nil
	// leaves pinning to kubehenge release pin and kubectl.
	PinChecks *provisioner.ReleaseChecksums
	// TalosReleases lists the Talos releases this Kubehenge pins. Nil offers only those the
	// template pool has.
	TalosReleases func(ctx context.Context) ([]string, error)
	// TalosClock asks a Talos node whether its clock is synced, for the Health page. Nil asks the
	// node's Talos API.
	TalosClock provisioner.TalosClockProbe
	// Headlamp is set when a shared Headlamp server runs beside the interface.
	Headlamp *Headlamp
	// WorkloadPool is the Proxmox VE pool every cluster's own pool nests under.
	WorkloadPool string
	// ClassNamespace holds Kubehenge's ClusterClasses.
	ClassNamespace string
	// Namespace is where the interface creates and changes clusters while the management cluster
	// lists no namespaces for it (namespaces).
	Namespace string
	// Assets is the built frontend: index.html and what it loads.
	Assets fs.FS
	// PublicURL is the address users reach the interface at, such as https://kubehenge.example;
	// OpenID identity providers send users back to it. Empty means the address the browser used.
	PublicURL string
	// TrustedProxies are the reverse proxies and load balancers in front of the interface. From
	// one of them, the sign-in limits count the client in X-Forwarded-For instead of the proxy.
	// Empty means every request counts under the address it came from.
	TrustedProxies []netip.Prefix
	// SessionMaxAge ends every session this long after sign-in.
	SessionMaxAge time.Duration
	// ConnectWorkload reaches a workload cluster's API server with the token of one of Kubehenge's
	// identities in it. Unset, it connects over HTTPS trusting the cluster's CA alone.
	ConnectWorkload func(server string, ca []byte, token string) (*Workload, error)
	// OpenStore reaches the backup targets clusters' etcd snapshots are in. Unset, over HTTPS.
	OpenStore provisioner.OpenStore
	// NewClientset reaches the management cluster for what its controller-runtime client cannot
	// read, the providers' logs a support bundle carries. Nil leaves bundles without them.
	NewClientset func() (kubernetes.Interface, error)
	Log          *slog.Logger

	// activityLog keeps what each user did: the lines logged with a user, and the API server's
	// changes when it writes them.
	activityLog *activityLog

	// prepared guards the set-up Run and the handlers each do on first use, in whichever order the
	// caller starts them.
	prepared     sync.Once
	sessions     *sessions
	limiter      *limiter
	tokens       tokenUsers
	activeKube   atomic.Pointer[client.Client]
	backups      backupCache
	applianceIDs readCache[int]
	etags        etagCache
	health       readCache[healthView]
	veleroRuns   readCache[[]runView]
	privileges   readCache[*userPrivileges]
	vmLoads      vmLoadCache
	workloads    workloadConns
	changes      changes
	notifier     notifier

	// auditWake wakes the audit follower between its ticks. auditFollower is the one it runs.
	auditWake     chan struct{}
	auditFollower atomic.Pointer[auditFollower]
	// clock is the health page's time. Unset, the system's.
	clock func() time.Time
	// demo is set by SetUpDemo: the answers say so and changes are refused.
	demo *demoState
}

const (
	// headlampSyncEvery is how soon a new cluster shows in Headlamp.
	headlampSyncEvery = 30 * time.Second
	// failuresAllowed failed sign-ins or wrong passphrases within failureWindow hold their source
	// back for the rest of it.
	failuresAllowed = 5
	failureWindow   = 15 * time.Minute
	// addressFailuresAllowed failed sign-ins from one address, whichever users they name, hold it
	// back: more than one user's, since several people may share an address behind NAT.
	addressFailuresAllowed = 20
)

// Run prepares the server and keeps its sessions, upgrades and Headlamp up to date until ctx ends.
// It returns once each of them has stopped.
func (s *Server) Run(ctx context.Context) {
	s.init()
	var wg sync.WaitGroup
	wg.Go(func() { s.autoUpgrade(ctx) })
	wg.Go(func() { s.watchChanges(ctx) })
	wg.Go(func() { s.backUpManagementEtcd(ctx) })
	wg.Go(func() { s.storeClusterSpecs(ctx) })
	wg.Go(func() { s.notifyLoop(ctx) })
	if s.Headlamp != nil {
		wg.Go(func() { s.syncHeadlamp(ctx, headlampSyncEvery) })
	}
	wg.Go(func() { s.followAuditLog(ctx, auditFollowEvery) })
	s.sessions.pruneEvery(ctx, time.Minute)
	wg.Wait()
}

// Handler returns the interface's routes.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	// The appliance's updater asks it whether the interface serves on the software it installed.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/realms", s.realms)
	mux.HandleFunc("POST /api/session", s.signIn)
	mux.HandleFunc("POST /api/session/second-factor", s.secondFactor)
	mux.HandleFunc("POST /api/session/token", s.signInWithToken)
	mux.HandleFunc("GET /api/openid/start", s.openIDStart)
	mux.HandleFunc("GET /api/openid/callback", s.openIDCallback)
	mux.HandleFunc("GET /api/session", s.signedIn(s.whoAmI))
	mux.HandleFunc("DELETE /api/session", s.signedIn(s.signOut))

	mux.HandleFunc("GET /api/proxmox/storages", s.signedIn(s.storages))
	mux.HandleFunc("GET /api/proxmox/nodes", s.signedIn(s.nodes))
	mux.HandleFunc("GET /api/proxmox/nodes/{node}/bridges", s.signedIn(s.bridges))
	mux.HandleFunc("GET /api/proxmox/nodes/{node}/dns", s.signedIn(s.dns))
	mux.HandleFunc("POST /api/proxmox/nodes/{node}/bridges/{bridge}/free", s.signedIn(s.freeAddresses))
	mux.HandleFunc("GET /api/proxmox/ha-rules", s.signedIn(s.haRules))

	s.applianceRoutes(mux)

	mux.HandleFunc("GET /api/classes", s.managed(s.classes))
	mux.HandleFunc("GET /api/versions/{distribution}", s.signedIn(s.versions))
	mux.HandleFunc("GET /api/talos/releases", s.signedIn(s.talosReleases))
	mux.HandleFunc("GET /api/clusters", s.managed(s.listClusters))
	mux.HandleFunc("GET /api/fleet/health", s.managed(s.fleetHealth))
	mux.HandleFunc("GET /api/runs", s.managed(s.runs))
	mux.HandleFunc("GET /api/activity", s.managed(s.activity))
	mux.HandleFunc("GET /api/events", s.managed(s.changeStream))
	mux.HandleFunc("POST /api/clusters", s.managed(s.createCluster))
	mux.HandleFunc("POST /api/cluster-specs", s.managed(s.createFromSpec))
	s.clusterRoutes(mux)
	mux.HandleFunc("GET /api/credentials/{purpose}", s.managed(s.applianceCredential))
	mux.HandleFunc("PUT /api/credentials/{purpose}", s.managed(s.storeApplianceCredential))
	mux.HandleFunc("DELETE /api/credentials/{purpose}", s.managed(s.removeApplianceCredential))
	mux.HandleFunc("GET /api/notifications", s.managed(s.notifications))
	mux.HandleFunc("POST /api/notifications/test", s.managed(s.testNotifications))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such API")
	})
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("/", s.assets)
	if s.demo != nil {
		return withRequestID(s.withClientAddress(securityHeaders(s.demoGuard(mux))))
	}
	return withRequestID(s.withClientAddress(securityHeaders(mux)))
}

// healthz tells a monitor that the interface answers, unauthenticated. It says nothing more, so it
// tells no one else anything about the appliance.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "ok\n")
}

// applianceRoutes are the routes of the appliance, its management cluster and its templates.
func (s *Server) applianceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", s.signedIn(s.setupStatus))
	mux.HandleFunc("POST /api/setup", s.signedIn(s.setup))
	mux.HandleFunc("POST /api/setup/check", s.signedIn(s.setupCheck))
	mux.HandleFunc("POST /api/setup/credential", s.signedIn(s.credential))
	mux.HandleFunc("POST /api/setup/remove", s.signedIn(s.remove))
	mux.HandleFunc("POST /api/setup/take-over", s.signedIn(s.takeOver))
	mux.HandleFunc("GET /api/setup/health", s.signedIn(s.managementHealth))
	mux.HandleFunc("GET /api/appliance/reboot-ready", s.signedIn(s.rebootReady))
	mux.HandleFunc("GET /api/appliance/upgrades", s.signedIn(s.upgradesStatus))
	mux.HandleFunc("POST /api/appliance/upgrades", s.signedIn(s.upgrade))
	mux.HandleFunc("PUT /api/appliance/upgrades/settings", s.signedIn(s.upgradeSettings))
	mux.HandleFunc("PUT /api/appliance/upgrades/management", s.signedIn(s.keepUnattended))
	mux.HandleFunc("DELETE /api/appliance/upgrades/management", s.signedIn(s.forgetUnattended))
	mux.HandleFunc("GET /api/appliance/etcd-backups", s.signedIn(s.managementBackups))
	mux.HandleFunc("PUT /api/appliance/etcd-backups", s.signedIn(s.configureManagementBackups))
	mux.HandleFunc("DELETE /api/appliance/etcd-backups", s.signedIn(s.stopManagementBackups))
	mux.HandleFunc("GET /api/appliance/etcd-backups/snapshots", s.signedIn(s.managementSnapshots))
	mux.HandleFunc("POST /api/appliance/etcd-backups/snapshots", s.signedIn(s.takeManagementSnapshot))
	mux.HandleFunc("POST /api/appliance/etcd-backups/snapshots/{name}/restore", s.signedIn(s.restoreManagementEtcd))
	mux.HandleFunc("POST /api/appliance/etcd-backups/warning/dismissal", s.signedIn(s.dismissBackupWarning))
	mux.HandleFunc("POST /api/appliance/etcd-backups/snapshots/{name}/recreate", s.signedIn(s.recreateManagement))
	mux.HandleFunc("POST /api/appliance/export", s.signedIn(s.exportManagement))
	mux.HandleFunc("POST /api/appliance/import", s.signedIn(s.importManagement))
	mux.HandleFunc("GET /api/templates", s.signedIn(s.listTemplates))
	mux.HandleFunc("POST /api/templates", s.signedIn(s.createTemplate))
	mux.HandleFunc("DELETE /api/templates/{vmid}", s.signedIn(s.deleteTemplate))
	mux.HandleFunc("POST /api/templates/refresh", s.signedIn(s.refreshNow))
	mux.HandleFunc("GET /api/release-pins", s.signedIn(s.listPins))
	mux.HandleFunc("POST /api/release-pins/check", s.signedIn(s.checkPin))
	mux.HandleFunc("POST /api/release-pins", s.signedIn(s.addPin))
	mux.HandleFunc("DELETE /api/release-pins/{version}", s.signedIn(s.removePin))
	mux.HandleFunc("GET /api/template-builds", s.managed(s.templateBuilds))
	mux.HandleFunc("POST /api/template-builds", s.managed(s.requestTemplateBuild))
}

// clusterRoutes are the routes of one cluster.
func (s *Server) clusterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}", s.managed(s.getCluster))
	mux.HandleFunc("PATCH /api/clusters/{namespace}/{name}", s.managed(s.changeCluster))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/plan", s.managed(s.planCluster))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}", s.managed(s.deleteCluster))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/spec", s.managed(s.clusterSpec))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/kubeconfig", s.managed(s.kubeconfig))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/talosconfig", s.managed(s.talosconfig))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/support-bundle", s.managed(s.supportBundle))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/machines/{machine}/boot-log", s.managed(s.bootLog))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/machines/{machine}/boot-log", s.managed(s.askBootLog))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/talos-upgrade-failure", s.managed(s.retryTalosUpgrade))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/addon-changes", s.managed(s.applyAddonChanges))
	mux.HandleFunc("PUT /api/clusters/{namespace}/{name}/machines/{machine}/replacement", s.managed(s.replaceMachine))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/machines/{machine}/replacement", s.managed(s.keepMachine))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/machines/{machine}", s.managed(s.deleteMachine))
	mux.HandleFunc("PUT /api/clusters/{namespace}/{name}/machines/{machine}/forgotten", s.managed(s.forgetMachine))
	mux.HandleFunc("PUT /api/clusters/{namespace}/{name}/machines/{machine}/cordon", s.managed(s.cordonMachine))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/machines/{machine}/cordon", s.managed(s.uncordonMachine))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/machines/{machine}/reboot", s.managed(s.rebootMachine))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/health", s.managed(s.clusterHealth))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/pod-security-check", s.managed(s.checkPodSecurity))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/upgrade-checks", s.managed(s.upgradeChecks))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/upgrade-checks/accept", s.managed(s.acknowledgeUpgrade))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/activity", s.managed(s.clusterActivity))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/events", s.managed(s.runEvents))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/pods/{podNamespace}/{pod}/logs", s.managed(s.podLogs))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/backups", s.managed(s.clusterBackups))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/backups", s.managed(s.backUp))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/backups/{backup}", s.managed(s.deleteBackup))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/restores", s.managed(s.restore))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/restores/{restore}/results", s.managed(s.showRestoreResults))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/restores/{restore}/log", s.managed(s.showRestoreLog))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/backups/{backup}/log", s.managed(s.showBackupLog))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/certificates/ca", s.managed(s.ingressCA))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/etcd-snapshots", s.managed(s.etcdSnapshots))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/etcd-snapshots", s.managed(s.takeEtcdSnapshot))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/etcd-defrag", s.managed(s.defragEtcd))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/etcd-snapshots/{snapshot}", s.managed(s.downloadEtcdSnapshot))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/etcd-snapshots/{snapshot}/restore-plan", s.managed(s.etcdRestorePlan))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/etcd-restores", s.managed(s.restoreEtcd))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/control-plane-recovery", s.managed(s.controlPlaneRecovery))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/control-plane-recovery", s.managed(s.recoverControlPlane))
	mux.HandleFunc("POST /api/clusters/{namespace}/{name}/abandon-deletion", s.managed(s.abandonDeletion))
	mux.HandleFunc("GET /api/clusters/{namespace}/{name}/credentials/{purpose}", s.managed(s.clusterCredential))
	mux.HandleFunc("PUT /api/clusters/{namespace}/{name}/credentials/{purpose}", s.managed(s.storeClusterCredential))
	mux.HandleFunc("DELETE /api/clusters/{namespace}/{name}/credentials/{purpose}", s.managed(s.removeClusterCredential))
}

func (s *Server) init() {
	s.prepared.Do(func() {
		s.sessions = newSessions(s.Accounts, s.SessionMaxAge)
		s.limiter = newLimiter(failureWindow)
		s.auditWake = make(chan struct{}, 1)
		if s.Kube != nil {
			s.setKube(s.Kube)
		}
		if s.Log == nil {
			s.Log = slog.New(slog.DiscardHandler)
		}
		var dir string
		if s.Appliance != nil {
			dir = s.Appliance.Dir
		}
		s.activityLog = newActivityLog(dir)
		s.Log = withActivity(withRequestIDs(s.Log), s.activityLog)
	})
}

// setKube switches the interface to a management cluster. The first one wakes the audit follower,
// so the API servers' logs are followed from the moment there is a cluster to read them from.
func (s *Server) setKube(c client.Client) {
	if s.activeKube.Swap(&c) == nil {
		select {
		case s.auditWake <- struct{}{}:
		default:
		}
	}
}

func (s *Server) currentKube() client.Client {
	if p := s.activeKube.Load(); p != nil {
		return *p
	}
	return nil
}

// hsts keeps browsers on HTTPS for a year once they have reached the interface or Headlamp, which
// serve nothing else, so that a plain-HTTP address typed in or followed later never reaches whoever
// sits on the network. Subdomains are left alone: the interface's host name may be the parent of
// others it does not serve.
const hsts = "max-age=31536000"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", hsts)
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// assets serves the frontend. Paths that name no file get index.html, where the frontend's own
// router takes over.
func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, err := s.Assets.Open(name)
	if err == nil {
		if st, statErr := f.Stat(); statErr != nil || st.IsDir() {
			_ = f.Close()
			f, err = nil, fs.ErrNotExist
		}
	}
	if err != nil {
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
		if f, err = s.Assets.Open(name); err != nil {
			http.Error(w, "the interface was built without its frontend", http.StatusNotFound)
			return
		}
	}
	h := w.Header()
	if strings.HasPrefix(name, "assets/") {
		// Vite names these by the hash of their content.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	}
	// The build writes compressed forms beside each text file, so none is compressed per request.
	served := name
	h.Add("Vary", "Accept-Encoding")
	if g, e, ok := s.compressed(r, name); ok {
		_ = f.Close()
		f, served = g, name+e.suffix
		h.Set("Content-Encoding", e.coding)
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		_, _ = io.Copy(w, f)
		return
	}
	if tag, err := s.etag(served); err == nil {
		h.Set("ETag", tag)
	}
	// ServeContent answers a known ETag with 304 and serves ranges. The Content-Type set above
	// keeps it from guessing.
	http.ServeContent(w, r, "", time.Time{}, rs)
}

type assetEncoding struct{ coding, suffix string }

// precompressed are the forms the build writes beside each text asset, by preference.
var precompressed = []assetEncoding{{"br", ".br"}, {"gzip", ".gz"}}

// compressed opens the preferred form of an asset the request accepts. Not ok when there is none.
func (s *Server) compressed(r *http.Request, name string) (fs.File, assetEncoding, bool) {
	for _, e := range precompressed {
		if !accepts(r, e.coding) {
			continue
		}
		if f, err := s.Assets.Open(name + e.suffix); err == nil {
			return f, e, true
		}
	}
	return nil, assetEncoding{}, false
}

// accepts tells whether the request accepts a content coding, not refused with q=0.
func accepts(r *http.Request, coding string) bool {
	for _, field := range r.Header.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(field, ",") {
			name, params, _ := strings.Cut(part, ";")
			if !strings.EqualFold(strings.TrimSpace(name), coding) {
				continue
			}
			q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
			if !ok {
				return true
			}
			v, err := strconv.ParseFloat(q, 64)
			return err == nil && v > 0
		}
	}
	return false
}

type etagCache struct {
	mu     sync.Mutex
	byName map[string]string
}

// etagBytes of an asset's SHA-256 tell its versions apart.
const etagBytes = 16

// etag is a strong validator for an asset, from its content. Worked out once per file.
func (s *Server) etag(name string) (string, error) {
	c := &s.etags
	c.mu.Lock()
	tag, ok := c.byName[name]
	c.mu.Unlock()
	if ok {
		return tag, nil
	}
	raw, err := fs.ReadFile(s.Assets, name)
	if err != nil {
		return "", fmt.Errorf("asset %s: %w", name, err)
	}
	sum := sha256.Sum256(raw)
	tag = `"` + hex.EncodeToString(sum[:etagBytes]) + `"`
	c.mu.Lock()
	if c.byName == nil {
		c.byName = map[string]string{}
	}
	c.byName[name] = tag
	c.mu.Unlock()
	return tag, nil
}

// user is a signed-in request's caller.
type user struct {
	session *session
	ticket  *pve.Ticket
	// kube is the management cluster, for handlers behind managed.
	kube client.Client
	// namespaces are those the interface creates and changes clusters in, for handlers behind
	// managed.
	namespaces managedNamespaces
}

// versionHeader carries the running release on answers to signed-in users.
const versionHeader = "X-Kubehenge-Version"

// managed is signedIn for what needs the management cluster, which an appliance has only once it
// has been set up.
func (s *Server) managed(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request, u user) {
		if u.kube = s.currentKube(); u.kube == nil {
			writeError(w, http.StatusServiceUnavailable, "the management cluster is not set up yet")
			return
		}
		var err error
		if u.namespaces, err = s.namespaces(r.Context(), u.kube); err != nil {
			s.Log.ErrorContext(r.Context(), "management cluster request failed", "error", err)
			writeError(w, http.StatusBadGateway, "the management cluster did not answer")
			return
		}
		next(w, r, u)
	})
}

// signedIn lets only signed-in users through, and only with the session's CSRF token on anything
// but a read.
func (s *Server) signedIn(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), tokenPrefix); ok {
			if ticket := s.tokenUser(w, r, token); ticket != nil {
				w.Header().Set(versionHeader, release.Version)
				next(w, r, user{ticket: ticket})
			}
			return
		}
		sess := s.sessions.lookup(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sess.checkCSRF(r) {
			writeError(w, http.StatusForbidden, "missing or wrong CSRF token")
			return
		}
		ticket := s.sessions.signedIn(r.Context(), sess)
		if ticket == nil {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		// A page loaded before an upgrade learns from this that it is out of date. Only those signed
		// in see the version, so it tells no one else what to attack.
		w.Header().Set(versionHeader, release.Version)
		next(w, r, user{session: sess, ticket: ticket})
	}
}

// require checks that the user holds every privilege on a Proxmox VE path, and answers the request
// when not.
func (s *Server) require(w http.ResponseWriter, r *http.Request, u user, aclPath string, privs ...string) bool {
	held, err := s.Accounts.Privileges(r.Context(), u.ticket, aclPath)
	if err != nil {
		s.proxmoxError(w, r, err)
		return false
	}
	for _, p := range privs {
		if !held[p] {
			writeError(w, http.StatusForbidden, missingPrivilege(u, p, aclPath))
			return false
		}
	}
	return true
}

// nodesDisagree is what a refused, freshly issued ticket says: the nodes behind Proxmox VE's address
// keep clocks over five minutes apart.
const nodesDisagree = "Proxmox VE nodes disagree about the time. Compare timedatectl on each node and read pvecm status."

// missingPrivilege is what a refusal says when the user lacks priv on aclPath.
func missingPrivilege(u user, priv, aclPath string) string {
	return fmt.Sprintf("%s needs %s on %s", u.ticket.Username, priv, aclPath)
}

// proxmoxError answers a request whose call to Proxmox VE as the signed-in user failed. A refusal
// carries Proxmox VE's own reason, which the user's own ticket earned.
func (s *Server) proxmoxError(w http.ResponseWriter, r *http.Request, err error) {
	if apiErr, ok := errors.AsType[*pve.APIError](err); ok && pveAnswered(apiErr) {
		s.Log.ErrorContext(r.Context(), "proxmox VE refused a request", "error", err)
		writeError(w, http.StatusBadGateway, "Proxmox VE refused: "+refusalReason(apiErr))
		return
	}
	s.anonymousProxmoxError(w, r, err)
}

// anonymousProxmoxError answers a request whose call to Proxmox VE was made for someone not yet
// signed in. It tells a stranger nothing of Proxmox VE's reasons.
func (s *Server) anonymousProxmoxError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pve.ErrNodesDisagree):
		writeError(w, http.StatusUnauthorized, nodesDisagree)
	case errors.Is(err, pve.ErrAuthentication):
		writeError(w, http.StatusUnauthorized, "session expired. Sign in again")
	case errors.Is(err, pve.ErrForbidden):
		writeError(w, http.StatusForbidden, "Proxmox VE denied permission")
	default:
		s.Log.ErrorContext(r.Context(), "proxmox VE request failed", "error", err)
		writeError(w, http.StatusBadGateway, "Proxmox VE did not answer")
	}
}

// pveAnswered tells whether Proxmox VE itself gave the refusal. A gateway in front of it answering
// 502 to 504 means it did not.
func pveAnswered(e *pve.APIError) bool {
	code, _, _ := strings.Cut(e.Status, " ")
	switch code {
	case "502", "503", "504":
		return false
	}
	return true
}

// maxReasonLength keeps Proxmox VE's reason to one line of the interface.
const maxReasonLength = 120

var (
	// pathPattern matches a file or API path in a reason, which says nothing to the user.
	pathPattern = regexp.MustCompile(`\s*['"(]?/[\w.\-/]+['")]?`)
	spaces      = regexp.MustCompile(`\s+`)
)

// refusalReason is the reason Proxmox VE gave in its status line, without the status code and any
// path, and with the first parameter it refused. It is cut to maxReasonLength.
func refusalReason(e *pve.APIError) string {
	code, reason, _ := strings.Cut(e.Status, " ")
	if len(e.Errors) > 0 {
		keys := slices.Sorted(maps.Keys(e.Errors))
		reason = strings.TrimSuffix(strings.TrimSpace(reason), ".") + ": " + keys[0] + " " + e.Errors[keys[0]]
	}
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, reason)
	reason = strings.TrimSpace(spaces.ReplaceAllString(pathPattern.ReplaceAllString(reason, ""), " "))
	if reason == "" {
		return "status " + code
	}
	if runes := []rune(reason); len(runes) > maxReasonLength {
		reason = string(runes[:maxReasonLength-1]) + "…"
	}
	return reason
}

const maxBody = 64 << 10

// readJSON decodes a request body. Only application/json is accepted: a form on another site can
// send a POST without the preflight a JSON request needs.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request: "+decodeProblem(err))
		return false
	}
	return true
}

// decodeProblem says what is wrong with a request body without Go's type and package names.
func decodeProblem(err error) string {
	var typed *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &typed) && typed.Field != "":
		return fmt.Sprintf("%s: wrong type of value (%s)", typed.Field, typed.Value)
	case errors.As(err, &typed):
		return fmt.Sprintf("the body: wrong type of value (%s)", typed.Value)
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "the body is not valid JSON"
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("the body is larger than %d bytes", tooLarge.Limit)
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

// writeJSON encodes the answer before the status goes out, so one that cannot be encoded is an
// error rather than half an answer.
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "the answer could not be encoded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

// writeError answers with the API's own message, and the request's id for the user to quote.
func writeError(w http.ResponseWriter, status int, message string) {
	answer := map[string]string{"error": message}
	if id := w.Header().Get(requestIDHeader); id != "" {
		answer["requestId"] = id
	}
	writeJSON(w, status, answer)
}
