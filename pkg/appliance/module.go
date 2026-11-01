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
  node_addresses         = var.settings.nodeAddresses
  api_address            = var.settings.apiAddress
  gateway                = var.settings.gateway
  dns_servers            = var.settings.dnsServers
  workload_datastores    = var.settings.workloadDatastores
  workload_bridges       = var.settings.workloadBridges
  bootstrap_datastore    = var.settings.bootstrapDatastore
  registry_mirrors       = var.settings.registryMirrors
  controller_image       = var.settings.controllerImage
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

// run applies the module with the settings, signing in to Proxmox VE as auth says (ticketAuth or
// tokenAuth), and returns the interface's kubeconfig.
func (a *Appliance) run(ctx context.Context, j *Job, s *Settings, passphrase string, auth []string) ([]byte, error) {
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		return nil, fmt.Errorf("preparing the module's working directory: %w", err)
	}
	if err := a.writeConfig(s); err != nil {
		return nil, err
	}
	env, err := a.env(passphrase, auth)
	if err != nil {
		return nil, err
	}
	if err = a.init(ctx, j, env); err != nil {
		return nil, err
	}
	j.Add("Applying the management cluster")
	err = a.tofu(ctx, j, env, jsonMessage, "apply", "-auto-approve", "-input=false", "-json")
	if err != nil {
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

func (a *Appliance) writeConfig(s *Settings) error {
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
	vars := map[string]any{"settings": map[string]any{
		"proxmox_endpoint":       a.ProxmoxURL,
		"proxmox_ca_certificate": a.ProxmoxCA,
		"proxmox_insecure":       a.ProxmoxInsecure,
		"nodes":                  s.Nodes,
		"imageDatastore":         s.ImageDatastore,
		"vmDatastore":            s.VMDatastore,
		"bridge":                 s.Bridge,
		"vlan":                   s.VLAN,
		"nodeAddresses":          s.NodeAddresses,
		"apiAddress":             s.APIAddress,
		"gateway":                s.Gateway,
		"dnsServers":             s.DNSServers,
		"workloadDatastores":     s.WorkloadDatastores,
		"workloadBridges":        s.WorkloadBridges,
		"bootstrapDatastore":     s.BootstrapDatastore,
		"registryMirrors":        nonNilMirrors(s.RegistryMirrors),
		"controllerImage":        nilIfEmpty(s.ControllerImage),
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
		// The module takes no null for a list.
		"extraClusterNamespaces": append([]string{}, s.ExtraClusterNamespaces...),
	}}
	raw, err := json.MarshalIndent(vars, "", "  ")
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

// ticketAuth has the module's Proxmox VE provider act with a user's session.
func ticketAuth(t *pve.Ticket) []string {
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
		"TF_ENCRYPTION=" + encryption(passphrase),
	}
	env = append(env, auth...)
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

// tofu runs one OpenTofu command in the working directory, logging into the job what message makes
// of each line of its output.
func (a *Appliance) tofu(ctx context.Context, j *Job, env []string, message func(line string) string, args ...string) error {
	cmd := exec.CommandContext(ctx, a.Tofu, append([]string{"-chdir=" + a.workdir()}, args...)...) //nolint:gosec // the binary and arguments are the appliance's own
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
	for sc.Scan() {
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
	if err != nil {
		return fmt.Errorf("tofu %s: %w", args[0], err)
	}
	return nil
}

// output reads one output of the last apply.
func (a *Appliance) output(ctx context.Context, env []string, name string) (string, error) {
	cmd := exec.CommandContext(ctx, a.Tofu, "-chdir="+a.workdir(), "output", "-raw", name) //nolint:gosec // the binary and arguments are the appliance's own
	cmd.Env = env
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return "", fmt.Errorf("tofu output %s: %s", name, strings.TrimSpace(string(exit.Stderr)))
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
