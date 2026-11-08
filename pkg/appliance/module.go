package appliance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// A line of OpenTofu's -json output can carry a whole diagnostic, past bufio's default limit.
const (
	lineBuffer = 64 << 10
	maxLine    = 4 << 20
)

// rootConfig calls the module with the wizard's settings. State encryption comes from
// TF_ENCRYPTION, so the passphrase is never written down.
const rootConfig = `# Written by Kubehenge's appliance; changes are overwritten.
variable "settings" {
  type = any
}

module "management" {
  source = %q

  proxmox_endpoint       = var.settings.proxmox_endpoint
  proxmox_ca_certificate = var.settings.proxmox_ca_certificate
  proxmox_insecure       = var.settings.proxmox_insecure
  proxmox_nodes          = var.settings.nodes
  image_datastore        = var.settings.imageDatastore
  vm_datastore           = var.settings.vmDatastore
  bridge                 = var.settings.bridge
  vlan_id                = try(var.settings.vlan, null)
  mtu                    = try(var.settings.mtu, null)
  ip_families            = try(var.settings.ipFamilies, ["IPv4"])
  node_addresses         = var.settings.nodeAddresses
  node_addresses_ipv6    = try(var.settings.nodeAddressesIPv6, [])
  appliance_addresses    = try(var.settings.applianceAddresses, [])
  api_address            = var.settings.apiAddress
  gateway                = var.settings.gateway
  gateway_ipv6           = try(var.settings.gatewayIPv6, null)
  dns_servers            = var.settings.dnsServers
  search_domains         = var.settings.searchDomains
  time_servers           = var.settings.timeServers
  workload_datastores    = var.settings.workloadDatastores
  workload_bridges       = var.settings.workloadBridges
  bootstrap_datastore    = var.settings.bootstrapDatastore
  registry_mirrors       = var.settings.registryMirrors
  chart_repositories     = var.settings.chartRepositories
  controller_image       = var.settings.controllerImage
  vm_id_range            = var.settings.vmIdRange
  pod_subnets            = var.settings.podSubnets
  service_subnets        = var.settings.serviceSubnets
  cluster_oidc           = var.settings.clusterOIDC
  backup_targets         = var.settings.backupTargets
  proxmox_ha             = var.settings.proxmoxHA
  offer_ha_rules         = var.settings.offerHARules
  template_refresh       = var.settings.templateRefresh
  etcd_restore           = var.settings.etcdRestore
  headlamp_sign_in       = var.settings.headlampSignIn
  headlamp_actions       = var.settings.headlampActions

  cluster_datastore_allocate = var.settings.clusterDatastoreAllocate
  cluster_sys_audit          = var.settings.clusterSysAudit
  node_boot_logs             = var.settings.nodeBootLogs

  extra_cluster_namespaces = var.settings.extraClusterNamespaces
  api_audit_log            = var.settings.apiAuditLog
  proxy                    = var.settings.proxy
}

output "kubeconfig" {
  value     = module.management.kubeconfig
  sensitive = true
}

output "talosconfig" {
  value     = module.management.talosconfig
  sensitive = true
}

output "api_endpoint" {
  value = module.management.api_endpoint
}
`

// run applies the module with the settings, signing in to Proxmox VE as the ticket's user (a
// session, or an API token), and returns the interface's kubeconfig.
func (a *Appliance) run(ctx context.Context, j *Job, s *Settings, passphrase string, ticket *pve.Ticket) ([]byte, error) {
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		return nil, fmt.Errorf("preparing the module's working directory: %w", err)
	}
	ids, err := a.vmIDRange(ctx, ticket, s)
	if err != nil {
		return nil, err
	}
	if err = a.writeConfig(s, ids); err != nil {
		return nil, err
	}
	auth := ticketAuth(ticket)
	if ticket.Token != "" {
		auth = tokenAuth(ticket.Token)
	}
	env, err := a.env(passphrase, auth)
	if err != nil {
		return nil, err
	}
	if err = a.init(ctx, j, env); err != nil {
		return nil, err
	}
	if err = a.claimLease(ctx, j, env); err != nil {
		return nil, err
	}
	j.Add("Applying the management cluster")
	err = a.tofu(ctx, j, env, jsonMessage, "apply", "-auto-approve", "-input=false", "-json")
	if err != nil {
		a.explainWaits(ctx, j, env)
		return nil, err
	}
	admin, err := a.output(ctx, env, "kubeconfig")
	if err != nil {
		return nil, err
	}
	endpoint, err := a.output(ctx, env, "api_endpoint")
	if err != nil {
		return nil, err
	}
	j.Add("Fetching the interface's credentials")
	kubeconfig, err := a.InterfaceKubeconfig(ctx, []byte(admin), endpoint, a.TokenPath())
	if err != nil {
		return nil, fmt.Errorf("the interface's kubeconfig: %w", err)
	}
	err = writeFileAtomic(a.KubeconfigPath(), kubeconfig)
	if err != nil {
		return nil, err
	}
	talos, err := a.output(ctx, env, "talosconfig")
	if err != nil {
		return nil, err
	}
	if err = a.recordApplied(CredentialsExpire([]byte(admin), []byte(talos))); err != nil {
		return nil, err
	}
	// The cluster runs as applied either way. The backups' status says why their snapshots fail.
	if err = a.renewEtcdBackupCredential(ctx, []byte(talos)); err != nil {
		j.Add("Renewing the credential of the etcd backups failed: " + err.Error())
		a.etcdBackupFailed(err)
	}
	return kubeconfig, nil
}

// init prepares OpenTofu for the module released with the appliance. OpenTofu reads the lock file
// of the root module only, so the module's own, which holds its providers' checksums, is put there
// first, and init refuses any provider that does not match it.
func (a *Appliance) init(ctx context.Context, j *Job, env []string) error {
	lock, err := os.ReadFile(filepath.Join(a.Module, ".terraform.lock.hcl"))
	if err != nil {
		return fmt.Errorf("the module's provider lock: %w", err)
	}
	if err = writeFileAtomic(filepath.Join(a.workdir(), ".terraform.lock.hcl"), lock); err != nil {
		return err
	}
	j.Add("Preparing OpenTofu")
	return a.tofu(ctx, j, env, plainLine, "init", "-input=false", "-lockfile=readonly", "-no-color") //nolint:misspell // OpenTofu's flag
}

func (a *Appliance) writeConfig(s *Settings, ids VMIDRange) error {
	ranges, err := s.ranges()
	if err != nil {
		return err
	}
	dir := a.workdir()
	// OpenTofu copies a module given by absolute path into .terraform; <package>//<subdir> copies
	// the whole directory, so the module still finds the charts it installs beside it.
	module := filepath.ToSlash(filepath.Dir(a.Module)) + "//" + filepath.Base(a.Module)
	err = writeFileAtomic(filepath.Join(dir, "main.tf"), fmt.Appendf(nil, rootConfig, module))
	if err != nil {
		return err
	}
	own, err := a.addresses()
	if err != nil {
		return err
	}
	values := map[string]any{
		"applianceAddresses":     own,
		"proxmox_endpoint":       a.ProxmoxURL,
		"proxmox_ca_certificate": a.ProxmoxCA,
		"proxmox_insecure":       a.ProxmoxInsecure,
		"nodes":                  s.Nodes,
		"imageDatastore":         s.ImageDatastore,
		"vmDatastore":            s.VMDatastore,
		"bridge":                 s.Bridge,
		"vlan":                   s.VLAN,
		"mtu":                    nilIfZero(s.MTU),
		"nodeAddresses":          append([]string{}, s.NodeAddresses...),
		"apiAddress":             s.APIAddress,
		"gateway":                nilIfEmpty(s.Gateway),
		"dnsServers":             s.DNSServers,
		"searchDomains":          append([]string{}, s.SearchDomains...),
		"timeServers":            append([]string{}, s.TimeServers...),
		"workloadDatastores":     s.WorkloadDatastores,
		"workloadBridges":        s.WorkloadBridges,
		"bootstrapDatastore":     s.BootstrapDatastore,
		"registryMirrors":        nonNilMirrors(s.RegistryMirrors),
		"chartRepositories":      moduleChartRepositories(s.ChartRepositories),
		"controllerImage":        nilIfEmpty(s.ControllerImage),
		"vmIdRange":              ids,
		"podSubnets":             ranges.pods,
		"serviceSubnets":         ranges.services,
		"clusterOIDC":            moduleOIDC(s.ClusterOIDC),
		"backupTargets":          moduleBackupTargets(s.BackupTargets),
		"proxmoxHA":              s.ProxmoxHA,
		"offerHARules":           s.OfferHARules,
		"templateRefresh":        moduleTemplateRefresh(s.TemplateRefresh),
		"etcdRestore":            s.EtcdRestore,
		"headlampSignIn":         s.HeadlampSignIn,
		"headlampActions":        s.HeadlampActions,

		"clusterDatastoreAllocate": s.ClusterDatastoreAllocate,
		"clusterSysAudit":          s.GivesClusterSysAudit(),
		"nodeBootLogs":             s.ReadsNodeBootLogs(),
		"apiAuditLog":              s.APIAuditLog,
		"proxy":                    moduleProxy(s, a.ProxmoxURL),
		// The module takes no null for a list.
		"extraClusterNamespaces": append([]string{}, s.ExtraClusterNamespaces...),
	}
	if s.HasIPv6() {
		// An IPv4 cluster leaves these out, so its variables stay as they were and the module's
		// defaults apply.
		values["ipFamilies"] = s.Families()
		values["nodeAddressesIPv6"] = append([]string{}, s.NodeAddressesIPv6...)
		values["gatewayIPv6"] = nilIfEmpty(s.GatewayIPv6)
	}
	raw, err := json.MarshalIndent(map[string]any{"settings": values}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the module's variables: %w", err)
	}
	err = writeFileAtomic(filepath.Join(dir, "terraform.tfvars.json"), raw)
	if err != nil {
		return err
	}
	settings, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the settings: %w", err)
	}
	// Saved settings with HA off no longer tell that HA was on, which a run that fails after taking
	// the HA token away would forget. Switched on again, the controller looks after what it left in
	// HA itself.
	switch {
	case a.ReleasesHA(s):
		err = writeFileAtomic(a.haReleasePath(), nil)
	case s.ProxmoxHA:
		err = a.HAReleased()
	}
	if err != nil {
		return fmt.Errorf("recording whether Proxmox VE HA is being switched off: %w", err)
	}
	return writeFileAtomic(filepath.Join(dir, "settings.json"), settings)
}

// ticketAuth has the module's Proxmox VE provider act with a user's session, or with the API token
// that signed the user in, which has no ticket.
func ticketAuth(t *pve.Ticket) []string {
	if t.Token != "" {
		return tokenAuth(t.Token)
	}
	return []string{"PROXMOX_VE_AUTH_TICKET=" + t.Ticket, "PROXMOX_VE_CSRF_PREVENTION_TOKEN=" + t.CSRF}
}

// tokenAuth has it act with an API token, user@realm!name=secret.
func tokenAuth(token string) []string {
	return []string{"PROXMOX_VE_API_TOKEN=" + token}
}

// env is all OpenTofu gets: nothing of the appliance's own environment beyond the path and any
// proxy, so no other credential reaches it.
func (a *Appliance) env(passphrase string, auth []string) ([]string, error) {
	cache := filepath.Join(a.Dir, "plugins")
	if err := os.MkdirAll(cache, privateDir); err != nil {
		return nil, fmt.Errorf("preparing OpenTofu's plugin cache: %w", err)
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + a.Dir,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"TF_PLUGIN_CACHE_DIR=" + cache,
		// More tries than OpenTofu's own, which spells of 504s from GitHub's release downloads
		// outlast. Each backs off longer, about two minutes in all.
		"TF_PROVIDER_DOWNLOAD_RETRY=8",
		"TF_REGISTRY_DISCOVERY_RETRY=8",
		"TF_ENCRYPTION=" + encryption(passphrase),
	}
	env = append(env, auth...)
	// The setting wins over the service's own environment, which only the installer sets.
	if saved := a.SavedSettings(); saved != nil && saved.Proxy.Enabled() {
		env = append(env, saved.Proxy.Env(saved.NoProxyList(a.ProxmoxURL))...)
		if saved.Proxy.CABundle != "" {
			path := filepath.Join(a.workdir(), "proxy-ca.pem")
			if err := writeFileAtomic(path, []byte(saved.Proxy.CABundle)); err != nil {
				return nil, err
			}
			// Go reads this file beside the system's certificate directories, so public hosts stay trusted.
			env = append(env, "SSL_CERT_FILE="+path)
		}
		return env, nil
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

// moduleProxy is the module's proxy variable: empty strings for none, which the module reads as no
// proxy.
func moduleProxy(s *Settings, proxmoxURL string) map[string]any {
	out := map[string]any{"url": "", "no_proxy": "", moduleCABundle: ""}
	if !s.Proxy.Enabled() {
		return out
	}
	out["url"] = s.Proxy.FullURL()
	out["no_proxy"] = strings.Join(s.NoProxyList(proxmoxURL), ",")
	out[moduleCABundle] = s.Proxy.CABundle
	return out
}

// tofuGrace is how long OpenTofu has between an interrupt and a kill, to finish the operations in
// flight and write its state. A killed apply loses the objects it made since its last write, and
// the next run then fails on them as already there.
const tofuGrace = 5 * time.Minute

// tofu runs one OpenTofu command in the working directory, logging into the job what message makes
// of each line of its output. A job with a deadline interrupts it tofuGrace before, so that it
// stops while the sign-in it runs on still holds, and kills it only at the deadline.
func (a *Appliance) tofu(ctx context.Context, j *Job, env []string, message func(line string) string, args ...string) error {
	// OpenTofu echoes a failed request's proxy now and then, and the log goes to the interface.
	if saved := a.SavedSettings(); saved != nil {
		j.Hide(saved.Proxy.Secrets()...)
	}
	run := ctx
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		run, cancel = context.WithDeadline(ctx, deadline.Add(-tofuGrace))
		defer cancel()
	}
	cmd := exec.CommandContext(run, a.Tofu, append([]string{"-chdir=" + a.workdir()}, args...)...) //nolint:gosec // the binary and arguments are the appliance's own
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = tofuGrace
	cmd.Env = env
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tofu %s: %w", args[0], err)
	}
	cmd.Stderr = cmd.Stdout
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("tofu %s: %w", args[0], err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, lineBuffer), maxLine)
	var failures []string
	for sc.Scan() {
		if d := errorDiagnostic(sc.Text()); d != "" {
			failures = append(failures, d)
		}
		if line := message(sc.Text()); line != "" {
			j.Add(line)
		}
	}
	// Whatever stopped the scan, tofu runs on and its outcome below still counts. The log says what
	// it is missing rather than ending as though tofu had said nothing more.
	if scanErr := sc.Err(); scanErr != nil {
		j.Add(fmt.Sprintf("The rest of tofu's output is left out of this log: %v", scanErr))
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	switch {
	case err != nil && errors.Is(context.Cause(ctx), ErrServiceStopped):
		return fmt.Errorf("tofu %s: interrupted as %w, so run it again: %w", args[0], ErrServiceStopped, err)
	case err != nil && run.Err() != nil && ctx.Err() == nil:
		return fmt.Errorf("tofu %s: interrupted before the Proxmox VE sign-in it runs on expires, so run it again: %w", args[0], err)
	case err != nil && len(failures) > 0:
		return fmt.Errorf("tofu %s: %w: %s", args[0], err, strings.Join(failures, "; "))
	case err != nil:
		return fmt.Errorf("tofu %s: %w", args[0], err)
	}
	return nil
}

// errorDiagnostic is the summary and detail of a line of OpenTofu's -json output that reports an
// error, or "" for any other line. The job's error carries them, as the log scrolls out of view.
func errorDiagnostic(line string) string {
	var m tofuMessage
	if json.Unmarshal([]byte(line), &m) != nil || m.Type != "diagnostic" || m.Level != "error" {
		return ""
	}
	d := m.Diagnostic
	text := strings.TrimSpace(d.Summary)
	if detail := strings.TrimSpace(d.Detail); detail != "" {
		text += ": " + detail
	}
	if d.Address != "" {
		text = d.Address + ": " + text
	}
	return text
}

// output reads one output of the last apply.
func (a *Appliance) output(ctx context.Context, env []string, name string) (string, error) {
	cmd := exec.CommandContext(ctx, a.Tofu, "-chdir="+a.workdir(), "output", "-raw", name) //nolint:gosec // the binary and arguments are the appliance's own
	cmd.Env = env
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		stderr := strings.TrimSpace(string(exit.Stderr))
		if saved := a.SavedSettings(); saved != nil {
			j := &Job{}
			j.Hide(saved.Proxy.Secrets()...)
			if j.hidden != nil {
				stderr = j.hidden.Replace(stderr)
			}
		}
		return "", fmt.Errorf("tofu output %s: %s", name, stderr)
	}
	if err != nil {
		return "", fmt.Errorf("tofu output %s: %w", name, err)
	}
	return string(out), nil
}

// moduleOIDC names the providers as the module's cluster_oidc does. Null takes its default.
func moduleOIDC(providers []OIDCProvider) []map[string]any {
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		var scopes any
		if len(p.Scopes) > 0 {
			scopes = p.Scopes
		}
		out = append(out, map[string]any{
			"issuer_url":            p.IssuerURL,
			"client_id":             p.ClientID,
			"username_claim":        nilIfEmpty(p.UsernameClaim),
			"username_prefix":       p.UsernamePrefix,
			"groups_claim":          nilIfEmpty(p.GroupsClaim),
			"groups_prefix":         p.GroupsPrefix,
			"scopes":                scopes,
			"certificate_authority": nilIfEmpty(p.CertificateAuthority),
		})
	}
	return out
}

// nonNilMirrors and nilIfEmpty give the module its own defaults: no mirrors, the chart's image.
func nonNilMirrors(m map[string][]string) map[string][]string {
	if m == nil {
		return map[string][]string{}
	}
	return m
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// encryption is OpenTofu's state and plan encryption with a key derived from the passphrase.
func encryption(passphrase string) string {
	return fmt.Sprintf(`key_provider "pbkdf2" "passphrase" {
  passphrase = "%s"
}
method "aes_gcm" "passphrase" {
  keys = key_provider.pbkdf2.passphrase
}
state {
  method   = method.aes_gcm.passphrase
  enforced = true
}
plan {
  method   = method.aes_gcm.passphrase
  enforced = true
}
`, hclEscape(passphrase))
}

// hclEscape makes s safe inside an HCL quoted string, where ${ and %{ start templates.
func hclEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "${", "$${", "%{", "%%{").Replace(s)
}

// plainLine is a line of OpenTofu's plain output, as it is.
func plainLine(line string) string { return strings.TrimRight(line, " \r") }

// tofuMessage is the part of a line of OpenTofu's -json output a job's log shows.
type tofuMessage struct {
	Level      string         `json:"@level"`
	Message    string         `json:"@message"`
	Type       string         `json:"type"`
	Diagnostic tofuDiagnostic `json:"diagnostic"`
}

type tofuDiagnostic struct {
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
	Address string `json:"address"`
}

// jsonMessage is the human part of a line of OpenTofu's -json output: its message, or a
// diagnostic's whole text with the resource it is about. OpenTofu prints an apply's diagnostics
// only once the apply has ended, so the address is what ties one to the resource that failed.
func jsonMessage(line string) string {
	var m tofuMessage
	if json.Unmarshal([]byte(line), &m) != nil {
		return strings.TrimSpace(line)
	}
	if m.Type == "diagnostic" && m.Diagnostic.Detail != "" {
		if m.Diagnostic.Address != "" {
			return fmt.Sprintf("%s: %s: %s: %s", m.Level, m.Diagnostic.Address, m.Diagnostic.Summary, m.Diagnostic.Detail)
		}
		return fmt.Sprintf("%s: %s: %s", m.Level, m.Diagnostic.Summary, m.Diagnostic.Detail)
	}
	return m.Message
}
