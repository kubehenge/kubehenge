package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// fakeTofu is testdata/tofu, built once for the package's tests.
var fakeTofu string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kubehenge-appliance")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeTofu = filepath.Join(dir, "tofu")
	if runtime.GOOS == "windows" {
		fakeTofu += ".exe"
	}
	build := exec.CommandContext(context.Background(), "go", "build", "-o", fakeTofu, "./testdata/tofu") //nolint:gosec // into the test's own temporary directory
	// The race detector's flags would make the fake as slow to start as the test binary.
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the fake tofu: %v\n%s", err, out)
		os.Exit(1)
	}
	m.Run()
	_ = os.RemoveAll(dir)
}

func settings(node string) *Settings {
	return &Settings{
		Nodes: []string{node}, ImageDatastore: "local", VMDatastore: "local-lvm", Bridge: "vmbr0",
		NodeAddresses: []string{"10.0.0.11/24"}, APIAddress: "10.0.0.10", Gateway: "10.0.0.1",
		DNSServers: []string{"10.0.0.1"}, WorkloadDatastores: []string{"local-lvm"}, WorkloadBridges: []string{"vmbr0"},
		BootstrapDatastore: "local",
	}
}

func TestNodesValidated(t *testing.T) {
	t.Parallel()
	for name, nodes := range map[string][]string{
		"none":                nil,
		"twice":               {"pve1", "pve1"},
		"empty":               {""},
		"more than addresses": {"pve1", "pve2"},
	} {
		s := settings("pve1")
		s.Nodes = nodes
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Proxmox VE nodes:") {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := settings("pve1")
	s.Nodes, s.NodeAddresses = []string{"pve1", "pve2", "pve3"}, []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	if err := s.Validate(); err != nil {
		t.Errorf("three nodes for three VMs: %v", err)
	}
}

func TestNetworkValidated(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		change func(*Settings)
		want   string
	}{
		"no prefix":         {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.11"} }, "Node addresses: 10.0.0.11 is no IPv4 address"},
		"IPv6 node":         {func(s *Settings) { s.NodeAddresses = []string{"fd00::11/64"} }, "Node addresses: fd00::11/64 is no IPv4"},
		"network address":   {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.0/24"} }, "network's own or broadcast"},
		"broadcast":         {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.255/24"} }, "network's own or broadcast"},
		"outside gateway":   {func(s *Settings) { s.NodeAddresses = []string{"10.0.1.11/24"} }, "outside the gateway's network"},
		"twice":             {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.11/24", "10.0.0.12/24"} }, "listed twice"},
		"two networks":      {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/16", "10.0.0.13/24"} }, "another network"},
		"gateway":           {func(s *Settings) { s.Gateway = "gateway" }, "Gateway: an IPv4 address"},
		"API not IPv4":      {func(s *Settings) { s.APIAddress = "10.0.0.10/24" }, "API address: an IPv4 address"},
		"API a node":        {func(s *Settings) { s.APIAddress = "10.0.0.11" }, "API address: unused"},
		"API elsewhere":     {func(s *Settings) { s.APIAddress = "10.9.0.10" }, "API address: inside the nodes' network, 10.0.0.0/24"},
		"DNS":               {func(s *Settings) { s.DNSServers = []string{"dns.example"} }, "DNS servers: dns.example is no IP address"},
		"VLAN":              {func(s *Settings) { v := 4095; s.VLAN = &v }, "VLAN tag: 1 to 4094"},
		"pod host bits":     {func(s *Settings) { s.PodSubnets = []string{"10.244.0.1/16"} }, "Pod ranges: 10.244.0.1/16 is no network"},
		"mirror scheme":     {func(s *Settings) { s.RegistryMirrors = map[string][]string{"docker.io": {"mirror:5000"}} }, "Registry mirrors: mirror:5000"},
		"mirror registry":   {func(s *Settings) { s.RegistryMirrors = map[string][]string{"a/b": {"https://m"}} }, `Registry mirrors: "a/b"`},
		"nothing for usage": {func(s *Settings) { s.WorkloadBridges = nil }, "What clusters may use"},
	} {
		s := settings("pve1")
		c.change(s)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	s := settings("pve1")
	s.APIAddress = s.Gateway
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "API address: unused") {
		t.Errorf("API address on the gateway: %v", err)
	}
	// A /31 has no network or broadcast address: both of its addresses are hosts.
	s = settings("pve1")
	s.NodeAddresses, s.Gateway, s.APIAddress = []string{"10.0.0.10/31"}, "10.0.0.11", "10.0.0.12"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "API address: inside the nodes' network") || strings.Contains(err.Error(), "broadcast") {
		t.Errorf("a /31: %v", err)
	}
}

func TestTemplateRefreshValidated(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		refresh TemplateRefresh
		want    string
	}{
		"weekday":           {TemplateRefresh{Weekday: time.Sunday.String()}, "Template refresh day"},
		"time":              {TemplateRefresh{Time: "1:00"}, "Template refresh time: HH:MM"},
		"time zone":         {TemplateRefresh{TimeZone: "Mars/Olympus"}, "Template refresh time zone"},
		"storage":           {TemplateRefresh{Storage: "elsewhere"}, "Template disks: one of the cluster storages"},
		"gateway alone":     {TemplateRefresh{Gateway: "10.0.0.1"}, "Template build address: needed"},
		"address":           {TemplateRefresh{Address: "10.0.0.50"}, "Template build address: an IPv4 address with prefix"},
		"gateway elsewhere": {TemplateRefresh{Address: "10.0.0.50/24", Gateway: "10.0.1.1"}, "Template build gateway: an address in 10.0.0.0/24"},
		"DNS":               {TemplateRefresh{Address: "10.0.0.50/24", Gateway: "10.0.0.1", DNS: []string{"dns"}}, "Template build DNS servers"},
	} {
		s := settings("pve1")
		c.refresh.Enabled = true
		s.TemplateRefresh = &c.refresh
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	s := settings("pve1")
	s.TemplateRefresh = &TemplateRefresh{Enabled: false, Weekday: "never"}
	if err := s.Validate(); err != nil || s.RefreshesTemplates() {
		t.Errorf("switched off, the rest is not looked at: %v", err)
	}
	s.TemplateRefresh = &TemplateRefresh{Enabled: true, Weekday: "Wed", Time: "03:30", TimeZone: "Europe/Berlin", Storage: "local-lvm", Address: "10.0.0.50/24", Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"}, CreatorBuilds: true}
	if err := s.Validate(); err != nil || !s.RefreshesTemplates() {
		t.Errorf("a full refresh: %v", err)
	}
	if !settings("pve1").RefreshesTemplates() {
		t.Error("settings saved without a refresh have it on")
	}
}

func TestModuleTemplateRefresh(t *testing.T) {
	t.Parallel()
	if got := moduleTemplateRefresh(nil); len(got) != 0 {
		t.Errorf("unset: %v, want the module's defaults", got)
	}
	got := moduleTemplateRefresh(&TemplateRefresh{Enabled: true, TimeZone: "Europe/Berlin", DNS: []string{"10.0.0.1"}})
	want := map[string]any{"enabled": true, "creator_builds": false, "time_zone": "Europe/Berlin", "dns": []string{"10.0.0.1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestClusterOIDC(t *testing.T) {
	t.Parallel()
	empty, system := "", "system:"
	for name, providers := range map[string][]OIDCProvider{
		"http issuer":    {{IssuerURL: "http://idp.example", ClientID: "k8s"}},
		"query":          {{IssuerURL: "https://idp.example/?x=1", ClientID: "k8s"}},
		"no client":      {{IssuerURL: "https://idp.example"}},
		"not a CA":       {{IssuerURL: "https://idp.example", ClientID: "k8s", CertificateAuthority: "nonsense"}},
		"issuer twice":   {{IssuerURL: "https://idp.example", ClientID: "a"}, {IssuerURL: "https://idp.example", ClientID: "b"}},
		"system: prefix": {{IssuerURL: "https://idp.example", ClientID: "k8s", GroupsPrefix: &system}},
		"nine providers": slices.Repeat([]OIDCProvider{{IssuerURL: "https://idp.example", ClientID: "k8s"}}, 9),
	} {
		s := settings("pve1")
		s.ClusterOIDC = providers
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Identity providers:") {
			t.Errorf("%s: %v", name, err)
		}
	}

	got := moduleOIDC([]OIDCProvider{
		{IssuerURL: "https://idp.example", ClientID: "k8s"},
		{IssuerURL: "https://login.example", ClientID: "lab", UsernameClaim: "sub", UsernamePrefix: &empty, Scopes: []string{"email"}},
	})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"certificate_authority":null,"client_id":"k8s","groups_claim":null,"groups_prefix":null,"issuer_url":"https://idp.example","scopes":null,"username_claim":null,"username_prefix":null},` +
		`{"certificate_authority":null,"client_id":"lab","groups_claim":null,"groups_prefix":null,"issuer_url":"https://login.example","scopes":["email"],"username_claim":"sub","username_prefix":""}]`
	if string(raw) != want {
		t.Fatalf("module providers = %s", raw)
	}
	if raw, err = json.Marshal(moduleOIDC(nil)); err != nil || string(raw) != "[]" {
		t.Fatalf("no providers = %s, %v", raw, err)
	}
}

const moduleLock = "provider \"registry.opentofu.org/bpg/proxmox\" {}\n"

func newAppliance(t *testing.T) (*Appliance, chan []byte) {
	t.Helper()
	ready := make(chan []byte, 1)
	module := filepath.Join(t.TempDir(), "management-cluster")
	if err := os.Mkdir(module, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, ".terraform.lock.hcl"), []byte(moduleLock), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Appliance{
		Dir:        t.TempDir(),
		Module:     module,
		Tofu:       fakeTofu,
		ProxmoxURL: "https://pve.example:8006",
		ProxmoxCA:  "-----BEGIN CERTIFICATE-----\n",
		Log:        slog.New(slog.DiscardHandler),
		InterfaceKubeconfig: func(_ context.Context, admin []byte, endpoint, _ string) ([]byte, error) {
			return []byte("interface for " + string(admin) + " at " + endpoint), nil
		},
	}
	return a, ready
}

func ticket() *pve.Ticket {
	return &pve.Ticket{Username: "root@pam", Ticket: "PVE:root@pam:1", CSRF: "csrf", Issued: time.Now()}
}

func waitFor(t *testing.T, a *Appliance) *JobView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if j := a.Current(); j != nil && j.State != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the apply did not finish")
	return nil
}

func TestExtraClusterNamespacesValidated(t *testing.T) {
	t.Parallel()
	for ns, want := range map[string]string{
		"kubehenge-system": "Namespaces for clusters: kubehenge-system holds",
		"clusters":         "Namespaces for clusters: clusters is the interface's own",
		"Team A":           `Namespaces for clusters: "Team A" is not a namespace name`,
	} {
		s := settings("pve1")
		s.ExtraClusterNamespaces = []string{"team-a", ns}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", ns, err, want)
		}
	}
	s := settings("pve1")
	s.ExtraClusterNamespaces = []string{"team-a", "team-a"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "team-a is named twice") {
		t.Errorf("a namespace twice: %v", err)
	}
	s.ExtraClusterNamespaces = []string{"team-a", "team-b"}
	if err := s.Validate(); err != nil {
		t.Errorf("two teams: %v", err)
	}
}

func TestApplyPassesTheExtraClusterNamespaces(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	vars := func() string {
		t.Helper()
		j := waitFor(t, a)
		if j.State != "succeeded" {
			t.Fatalf("job = %+v", j)
		}
		raw, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	ready := func(context.Context, []byte) error { return nil }
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), ready); err != nil {
		t.Fatal(err)
	}
	// The module's variable takes a list, never null.
	if v := vars(); !strings.Contains(v, `"extraClusterNamespaces": []`) {
		t.Fatalf("tfvars without an empty list of namespaces:\n%s", v)
	}
	s := settings("pve1")
	s.ExtraClusterNamespaces = []string{"team-a"}
	if err := a.Apply(s, "correct horse battery staple", ticket(), ready); err != nil {
		t.Fatal(err)
	}
	if v := vars(); !strings.Contains(v, "\"extraClusterNamespaces\": [\n      \"team-a\"\n    ]") {
		t.Fatalf("tfvars without team-a:\n%s", v)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "extra_cluster_namespaces = var.settings.extraClusterNamespaces") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	if saved := a.SavedSettings(); !slices.Equal(saved.ExtraClusterNamespaces, []string{"team-a"}) {
		t.Fatalf("saved settings = %+v", saved)
	}
}

func TestApply(t *testing.T) {
	t.Setenv("KUBEHENGE_UNRELATED_SECRET", "must not reach tofu")
	a, ready := newAppliance(t)
	if a.Ready() {
		t.Fatal("ready before any apply")
	}
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "succeeded" || j.User != "root@pam" {
		t.Fatalf("job = %+v", j)
	}
	if got := <-ready; string(got) != "interface for admin-kubeconfig at https://10.0.0.20:6443" {
		t.Fatalf("ready with %q", got)
	}
	if !a.Ready() {
		t.Fatal("not ready after the apply")
	}
	kc, err := os.Stat(a.KubeconfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && kc.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig mode %v", kc.Mode())
	}
	if !strings.Contains(strings.Join(j.Log, "\n"), "Creation complete") {
		t.Fatalf("log = %q", j.Log)
	}

	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), `source = "`+filepath.ToSlash(filepath.Dir(a.Module))+`//management-cluster"`) {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"proxmox_endpoint": "https://pve.example:8006"`) ||
		!strings.Contains(string(vars), `"proxmox_insecure": false`) || strings.Contains(string(vars), "horse") {
		t.Fatalf("tfvars: %v\n%s", err, vars)
	}
	if s := a.SavedSettings(); s == nil || !slices.Equal(s.Nodes, []string{"pve1"}) {
		t.Fatalf("saved settings = %+v", s)
	}
	if !strings.Contains(string(vars), "\"nodes\": [\n      \"pve1\"\n    ]") {
		t.Fatalf("tfvars without the nodes:\n%s", vars)
	}

	log, err := os.ReadFile(filepath.Join(a.Dir, "fake-tofu.log"))
	if err != nil {
		t.Fatal(err)
	}
	runs := strings.Split(strings.TrimSpace(string(log)), "\n")
	// init, apply, and the outputs kubeconfig, api_endpoint and talosconfig.
	if len(runs) != 5 {
		t.Fatalf("tofu ran %d times: %s", len(runs), log)
	}
	for _, run := range runs {
		var rec struct {
			Args []string `json:"args"`
			Env  []string `json:"env"`
		}
		if err := json.Unmarshal([]byte(run), &rec); err != nil {
			t.Fatal(err)
		}
		env := strings.Join(rec.Env, "\n")
		for _, want := range []string{"PROXMOX_VE_AUTH_TICKET=PVE:root@pam:1", "PROXMOX_VE_CSRF_PREVENTION_TOKEN=csrf", "TF_IN_AUTOMATION=1", `passphrase = "correct horse battery staple"`} {
			if !strings.Contains(env, want) {
				t.Errorf("tofu %v ran without %s", rec.Args, want)
			}
		}
		if strings.Contains(env, "KUBEHENGE_UNRELATED_SECRET") {
			t.Errorf("tofu %v saw the appliance's own environment", rec.Args)
		}
	}
}

func TestApplyHoldsTheProvidersToTheModulesLock(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	lock, err := os.ReadFile(filepath.Join(a.Dir, "management", ".terraform.lock.hcl"))
	if err != nil || string(lock) != moduleLock {
		t.Fatalf("the root module's lock: %v\n%s", err, lock)
	}
	log, err := os.ReadFile(filepath.Join(a.Dir, "fake-tofu.log"))
	if err != nil {
		t.Fatal(err)
	}
	inits := 0
	for run := range strings.SplitSeq(strings.TrimSpace(string(log)), "\n") {
		var rec struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal([]byte(run), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Args[0] == "init" {
			inits++
			if !slices.Contains(rec.Args, "-lockfile=readonly") {
				t.Errorf("tofu %v may change the providers' lock", rec.Args)
			}
		}
	}
	if inits == 0 {
		t.Fatal("no tofu init")
	}
}

func TestCredential(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if _, err := a.Credential(context.Background(), "correct horse battery staple", "kubeconfig"); err == nil {
		t.Fatal("a credential before any apply")
	}
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	// The fake tofu writes no state; the real one does.
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.Credential(context.Background(), "correct horse battery staple", "kubeconfig")
	if err != nil || string(got) != "admin-kubeconfig" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
	if _, err := a.Credential(context.Background(), "the wrong passphrase entirely", "kubeconfig"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := a.Credential(context.Background(), "correct horse battery staple", "terraform.tfstate"); err == nil {
		t.Fatal("handed out something other than a credential")
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Backups of the removed cluster's etcd stop with it.
	if err := os.MkdirAll(filepath.Dir(a.etcdBackupFile("keys.json")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(a.etcdBackupFile("settings.json"), EtcdBackupSettings{Target: "minio", EveryHours: 6, Keep: 2}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(a.etcdBackupFile("keys.json"), etcdBackupKeys{AccessKeyID: "AKID", SecretAccessKey: "secret"}); err != nil {
		t.Fatal(err)
	}

	before, gone := 0, 0
	step := func(_ context.Context, j *Job) error { before++; j.Add("deleting templates"); return nil }
	if err := a.Remove("the wrong passphrase entirely", ticket(), step, func() { gone++ }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "failed" || j.Kind != "remove" || before != 0 || gone != 0 || !a.Ready() {
		t.Fatalf("wrong passphrase: job %+v, before %d, gone %d", j, before, gone)
	}

	if err := a.Remove("correct horse battery staple", ticket(), step, func() { gone++ }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "succeeded" || before != 1 || gone != 1 || a.Ready() {
		t.Fatalf("remove: job %+v, before %d, gone %d, ready %v", j, before, gone, a.Ready())
	}
	log, err := os.ReadFile(filepath.Join(a.Dir, "fake-tofu.log"))
	if err != nil || !strings.Contains(string(log), `"destroy"`) {
		t.Fatalf("tofu destroy did not run: %v", err)
	}
	if s := a.SavedSettings(); s == nil || !slices.Equal(s.Nodes, []string{"pve1"}) {
		t.Fatal("the settings for the next setup are gone")
	}
	// The emptied state goes, so the next setup may choose a passphrase of its own.
	if a.HasState() {
		t.Fatal("the removed management cluster's state is still there")
	}
	if a.EtcdBackups().Settings != nil || a.EtcdBackupKeysStored() {
		t.Error("the removed management cluster's etcd backups are still set up")
	}
}

func TestTemplateBuildIsRecorded(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	release := make(chan struct{})
	if err := a.BuildTemplate(ticket(), func(_ context.Context, j *Job) error { j.Add("cloning"); <-release; return errors.New("no storage") }); err != nil {
		t.Fatal(err)
	}
	// A restart during the build finds it cut off.
	cut := &Appliance{Dir: a.Dir}
	cut.LoadLast()
	if j := cut.TemplateJob(); j == nil || j.State != "failed" || j.Error != "the appliance stopped during the run" {
		t.Fatalf("build cut off by a restart: %+v", j)
	}
	close(release)
	for deadline := time.Now().Add(10 * time.Second); a.TemplateJob().State == "running"; {
		if time.Now().After(deadline) {
			t.Fatal("the build did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	restarted := &Appliance{Dir: a.Dir}
	restarted.LoadLast()
	j := restarted.TemplateJob()
	if j == nil || j.Kind != "template" || j.State != "failed" || j.Error != "no storage" || !slices.Equal(j.Log, []string{"cloning"}) {
		t.Fatalf("restored build: %+v", j)
	}
	if restarted.Current() != nil {
		t.Fatal("a template build shows as a run of the management cluster")
	}
}

func TestApplyLogSaysWhatItLeftOut(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := a.Apply(settings("long"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	log := strings.Join(j.Log, "\n")
	if j.State != "failed" || !strings.Contains(j.Error, "tofu apply") || !strings.Contains(log, "Creation complete") ||
		!strings.Contains(log, "The rest of tofu's output is left out of this log: bufio.Scanner: token too long") {
		t.Fatalf("job = %+v", j)
	}
}

func TestApplyFails(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if err := a.Apply(settings("fail"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "failed" || !strings.Contains(j.Error, "tofu apply") || !strings.Contains(strings.Join(j.Log, "\n"), "node fail does not exist") {
		t.Fatalf("job = %+v", j)
	}
	if a.Ready() {
		t.Fatal("ready after a failed apply")
	}
	select {
	case <-ready:
		t.Fatal("ready called after a failed apply")
	default:
	}

	restarted := &Appliance{Dir: a.Dir}
	restarted.LoadLast()
	if j := restarted.Current(); j == nil || j.State != "failed" || len(j.Log) == 0 {
		t.Fatalf("after a restart: %+v", j)
	}
}

// A run switching Proxmox VE HA off saves the settings first, so the release stays due until a run
// with HA off gets through.
func TestSwitchingHAOffLastsUntilARunGetsThrough(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	apply := func(s *Settings, want string) {
		t.Helper()
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != want {
			t.Fatalf("job = %+v", j)
		}
	}
	on, off, failing := settings("pve1"), settings("pve1"), settings("fail")
	on.ProxmoxHA = true
	if a.ReleasesHA(off) {
		t.Fatal("releases HA that was never on")
	}
	apply(on, "succeeded")
	if a.ReleasesHA(on) || !a.ReleasesHA(off) {
		t.Fatal("switching HA off releases it, keeping it on does not")
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "proxmox_ha             = var.settings.proxmoxHA") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	if vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json")); err != nil || !strings.Contains(string(vars), `"proxmoxHA": true`) {
		t.Fatalf("tfvars: %v\n%s", err, vars)
	}
	apply(failing, "failed")
	if a.SavedSettings().ProxmoxHA || !a.ReleasesHA(off) {
		t.Fatal("a failed run with HA off left nothing due")
	}
	apply(off, "succeeded")
	if !a.ReleasesHA(off) {
		t.Fatal("no longer due although the release has not been made")
	}
	if err := a.HAReleased(); err != nil || a.ReleasesHA(off) {
		t.Fatalf("still due once released: %v", err)
	}
	// Switched on again before the release, the controller looks after HA again.
	apply(on, "succeeded")
	apply(failing, "failed")
	apply(on, "succeeded")
	if _, err := os.Stat(a.haReleasePath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a release still due with HA on: %v", err)
	}
}

func TestOfferingHARulesReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	for _, offer := range []bool{true, false} {
		s := settings("pve1")
		s.OfferHARules = offer
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != "succeeded" {
			t.Fatalf("job = %+v", j)
		}
		main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
		if err != nil || !strings.Contains(string(main), "offer_ha_rules         = var.settings.offerHARules") {
			t.Fatalf("main.tf: %v\n%s", err, main)
		}
		want := fmt.Sprintf(`"offerHARules": %t`, offer)
		if vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json")); err != nil || !strings.Contains(string(vars), want) {
			t.Fatalf("tfvars without %s: %v\n%s", want, err, vars)
		}
		if a.SavedSettings().OfferHARules != offer {
			t.Fatalf("saved settings do not offer the HA rules as %t", offer)
		}
	}
}

func TestClusterDatastoreAllocateReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	for _, on := range []bool{false, true} {
		s := settings("pve1")
		s.ClusterDatastoreAllocate = on
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != "succeeded" {
			t.Fatalf("job = %+v", j)
		}
		main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
		if err != nil || !strings.Contains(string(main), "cluster_datastore_allocate = var.settings.clusterDatastoreAllocate") {
			t.Fatalf("main.tf: %v\n%s", err, main)
		}
		vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
		if want := fmt.Sprintf(`"clusterDatastoreAllocate": %t`, on); err != nil || !strings.Contains(string(vars), want) {
			t.Fatalf("tfvars want %s: %v\n%s", want, err, vars)
		}
	}
}

// Clusters may hold Sys.Audit on / unless the settings switch it off.
func TestClusterSysAuditReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	for _, set := range []*bool{nil, new(false), new(true)} {
		on := set == nil || *set
		s := settings("pve1")
		s.ClusterSysAudit = set
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != "succeeded" {
			t.Fatalf("job = %+v", j)
		}
		main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
		if err != nil || !strings.Contains(string(main), "cluster_sys_audit          = var.settings.clusterSysAudit") {
			t.Fatalf("main.tf: %v\n%s", err, main)
		}
		vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
		if want := fmt.Sprintf(`"clusterSysAudit": %t`, on); err != nil || !strings.Contains(string(vars), want) {
			t.Fatalf("tfvars want %s: %v\n%s", want, err, vars)
		}
		if saved := a.SavedSettings(); saved.GivesClusterSysAudit() != on || (set == nil) != (saved.ClusterSysAudit == nil) {
			t.Errorf("the setting %t was not kept", on)
		}
	}
}

// The controller reads nodes' boot logs through the guest agent unless the settings switch it off.
func TestNodeBootLogsReachTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	for _, set := range []*bool{nil, new(false), new(true)} {
		on := set == nil || *set
		s := settings("pve1")
		s.NodeBootLogs = set
		if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if j := waitFor(t, a); j.State != "succeeded" {
			t.Fatalf("job = %+v", j)
		}
		main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
		if err != nil || !strings.Contains(string(main), "node_boot_logs             = var.settings.nodeBootLogs") {
			t.Fatalf("main.tf: %v\n%s", err, main)
		}
		vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
		if want := fmt.Sprintf(`"nodeBootLogs": %t`, on); err != nil || !strings.Contains(string(vars), want) {
			t.Fatalf("tfvars want %s: %v\n%s", want, err, vars)
		}
		if saved := a.SavedSettings(); saved.ReadsNodeBootLogs() != on || (set == nil) != (saved.NodeBootLogs == nil) {
			t.Errorf("the setting %t was not kept", on)
		}
	}
}

// Restores through the guest agent reach the module as its etcd_restore variable.
func TestEtcdRestoreReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.EtcdRestore = true
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "etcd_restore           = var.settings.etcdRestore") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	if vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json")); err != nil || !strings.Contains(string(vars), `"etcdRestore": true`) {
		t.Fatalf("tfvars: %v\n%s", err, vars)
	}
	if !a.SavedSettings().EtcdRestore {
		t.Error("the setting was not kept")
	}
}

func TestHeadlampSignInReachesTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.HeadlampSignIn = true
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "headlamp_sign_in       = var.settings.headlampSignIn") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"headlampSignIn": true`) {
		t.Fatalf("tfvars: %v\n%s", err, vars)
	}
	if !a.SavedSettings().HeadlampSignIn {
		t.Fatal("the saved settings forget the sign-in")
	}
}

func TestHeadlampActionsReachTheModule(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	s := settings("pve1")
	s.HeadlampActions = true
	if err := a.Apply(s, "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "headlamp_actions       = var.settings.headlampActions") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), `"headlampActions": true`) {
		t.Fatalf("tfvars: %v\n%s", err, vars)
	}
	if !a.SavedSettings().HeadlampActions {
		t.Fatal("the saved settings forget the actions")
	}
}

func TestJobViewCarriesTheEndOfALongLog(t *testing.T) {
	t.Parallel()
	j := &Job{Kind: "apply", State: "running"}
	for i := range 600 {
		j.Add(fmt.Sprintf("line %d", i))
	}
	v := j.view()
	if len(v.Log) != shownLines || v.Log[0] != "line 100" || v.Log[len(v.Log)-1] != "line 599" || v.Lines != 600 {
		t.Fatalf("view of 600 lines: %d shown from %q, lines %d", len(v.Log), v.Log[0], v.Lines)
	}
	if len(j.Log()) != 600 {
		t.Fatal("the job's own log was cut")
	}
	short := &Job{Kind: "apply", State: "running"}
	short.Add("one")
	if v := short.view(); len(v.Log) != 1 || v.Lines != 0 {
		t.Fatalf("view of one line: %+v", v)
	}
}

func TestJobLogIsAListEvenWithoutLines(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir()}
	a.job = &Job{Kind: "remove", State: "running"}
	raw, err := json.Marshal(a.Current())
	if err != nil || !strings.Contains(string(raw), `"log":[]`) {
		t.Fatalf("running job: %s, %v", raw, err)
	}
	// A record saved while the log still went out as null.
	err = os.MkdirAll(a.workdir(), 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(a.workdir(), "last-apply.json"), []byte(`{"kind":"remove","state":"failed","log":null}`), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Appliance{Dir: a.Dir}
	restarted.LoadLast()
	if raw, err = json.Marshal(restarted.Current()); err != nil || !strings.Contains(string(raw), `"log":[]`) {
		t.Fatalf("restored job: %s, %v", raw, err)
	}
}

func TestJSONMessageNamesTheResourceADiagnosticIsAbout(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ line, want string }{{
		line: `{"@level":"error","@message":"Error: Unable to create ACL","type":"diagnostic","diagnostic":{"summary":"Unable to create ACL","detail":"EOF","address":"module.management.proxmox_acl.clusters[\"/pool/kubehenge\"]"}}`,
		want: `error: module.management.proxmox_acl.clusters["/pool/kubehenge"]: Unable to create ACL: EOF`,
	}, {
		line: `{"@level":"warning","@message":"Warning: Deprecated","type":"diagnostic","diagnostic":{"summary":"Deprecated","detail":"use another attribute"}}`,
		want: `warning: Deprecated: use another attribute`,
	}, {
		line: `{"@level":"info","@message":"module.management.proxmox_acl.clusters: Creating...","type":"apply_start"}`,
		want: `module.management.proxmox_acl.clusters: Creating...`,
	}, {
		line: "  not json  ",
		want: "not json",
	}} {
		if got := jsonMessage(c.line); got != c.want {
			t.Errorf("jsonMessage(%s) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestFixedOnceCreated(t *testing.T) {
	t.Parallel()
	was := settings("pve1")
	for name, c := range map[string]struct {
		change func(*Settings)
		want   string
	}{
		"address changed": {func(s *Settings) { s.NodeAddresses = []string{"10.0.0.12/24"} }, "Node addresses: the nodes there are keep theirs (10.0.0.11/24)"},
		"node removed":    {func(s *Settings) { s.NodeAddresses = nil }, "Node addresses:"},
		"API address":     {func(s *Settings) { s.APIAddress = "10.0.0.9" }, "API address: stays 10.0.0.10"},
		"gateway":         {func(s *Settings) { s.Gateway = "10.0.0.254" }, "Gateway: stays 10.0.0.1"},
		"VM disks":        {func(s *Settings) { s.VMDatastore = "ceph" }, "VM disks: stays local-lvm"},
		"pod ranges":      {func(s *Settings) { s.PodSubnets = []string{"10.50.0.0/16"} }, "Pod ranges: stay 10.244.0.0/16"},
	} {
		s := settings("pve1")
		c.change(s)
		if err := s.KeepsFixed(was); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	grown := settings("pve1")
	grown.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	grown.PodSubnets = []string{"10.244.0.0/16"}
	grown.WorkloadDatastores, grown.DNSServers, grown.Bridge = []string{"ceph"}, []string{"10.0.0.2"}, "vmbr1"
	if err := grown.KeepsFixed(was); err != nil {
		t.Errorf("growing to three nodes and changing what may change: %v", err)
	}

	a, ready := newAppliance(t)
	if err := a.Apply(was, "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	moved := settings("pve1")
	moved.NodeAddresses = []string{"10.0.0.12/24"}
	if err := a.Apply(moved, "correct horse battery staple", ticket(), nil); err == nil || !strings.Contains(err.Error(), "Node addresses:") {
		t.Fatalf("moving the node of a running management cluster: %v", err)
	}
}

func TestApplyFailsWithoutAClient(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return errors.New("no client") }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "failed" || !strings.Contains(j.Error, "the interface cannot use it: no client") {
		t.Fatalf("job = %+v", j)
	}
}

func TestOneApplyAtATime(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	done := func(_ context.Context, k []byte) error { ready <- k; return nil }
	if err := a.Apply(settings("held"), "correct horse battery staple", ticket(), done); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), done); !errors.Is(err, ErrBusy) {
		t.Fatalf("second apply: %v, want ErrBusy", err)
	}
	if _, err := LockJobs(a.Dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("the updater's lock during a run: %v, want ErrBusy", err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	unlock, err := LockJobs(a.Dir)
	if err != nil {
		t.Fatalf("the updater's lock after the run: %v", err)
	}
	defer unlock()
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), done); !errors.Is(err, ErrUpgrading) {
		t.Fatalf("apply during an upgrade: %v, want ErrUpgrading", err)
	}
}

// The job's goroutine writes its log and outcome while requests read them, so that the race
// detector sees any read that skips the job's lock.
func TestJobIsReadWhileItRunsAndEnds(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	begin := make(chan struct{})
	err := a.start(ticket(), "apply", func(_ context.Context, j *Job) error {
		<-begin
		for i := range 100 {
			j.Add(fmt.Sprint("line ", i))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			<-begin
			for a.Busy() {
				_ = a.Current()
			}
		})
	}
	close(begin)
	readers.Wait()
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Errorf("state %q", j.State)
	}
}

func TestApplyRefuses(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	bad := settings("pve1")
	bad.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24"}
	if err := a.Apply(bad, "correct horse battery staple", ticket(), nil); err == nil {
		t.Fatal("two nodes accepted")
	}
	bad = settings("pve1")
	bad.NodeAddresses = []string{"10.0.0.11"}
	if err := a.Apply(bad, "correct horse battery staple", ticket(), nil); err == nil {
		t.Fatal("an address without prefix accepted")
	}
	if err := a.Apply(settings("pve1"), "too short", ticket(), nil); err == nil {
		t.Fatal("a short passphrase accepted")
	}
	if err := a.Apply(settings("pve1"), "sixteen chars\nand a newline", ticket(), nil); err == nil {
		t.Fatal("a passphrase with a newline accepted")
	}
	if a.Current() != nil {
		t.Fatal("a refused apply started a job")
	}
}

func TestRanges(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	r, err := s.ranges()
	if err != nil || r.pods[0] != "10.244.0.0/16" || r.services[0] != "10.96.0.0/12" {
		t.Fatalf("10.0.0.0/24: %+v %v", r, err)
	}
	// 10.100.0.0/24 lies inside Kubernetes' default service range.
	s.NodeAddresses, s.APIAddress, s.Gateway = []string{"10.100.0.11/24"}, "10.100.0.10", "10.100.0.1"
	if r, err = s.ranges(); err != nil || r.pods[0] != "10.244.0.0/16" || r.services[0] != "172.20.0.0/16" {
		t.Fatalf("10.100.0.0/24: %+v %v", r, err)
	}
	s.NodeAddresses = []string{"10.244.3.11/16"}
	s.APIAddress, s.Gateway = "10.244.3.10", "10.244.0.1"
	if r, err = s.ranges(); err != nil || r.pods[0] != "172.21.0.0/16" {
		t.Fatalf("10.244.0.0/16: %+v %v", r, err)
	}
	s.PodSubnets, s.ServiceSubnets = []string{"192.168.0.0/16"}, []string{"192.169.0.0/16"}
	if r, err = s.ranges(); err != nil || r.pods[0] != "192.168.0.0/16" || r.services[0] != "192.169.0.0/16" {
		t.Fatalf("set ranges: %+v %v", r, err)
	}
	s.PodSubnets, s.ServiceSubnets = nil, nil
	s.NodeAddresses, s.APIAddress, s.Gateway = []string{"0.0.0.0/0"}, "10.0.0.10", "10.0.0.1"
	if _, err = s.ranges(); err == nil {
		t.Fatal("a node network covering every candidate was accepted")
	}
}

func TestEncryptionEscapesThePassphrase(t *testing.T) {
	t.Parallel()
	got := encryption(`a"b\c${d}%{e}`)
	if !strings.Contains(got, `passphrase = "a\"b\\c$${d}%%{e}"`) {
		t.Fatalf("encryption = %s", got)
	}
	if !strings.Contains(got, "enforced = true") {
		t.Fatal("encryption is not enforced")
	}
}
