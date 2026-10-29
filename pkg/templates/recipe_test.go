package templates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"sigs.k8s.io/yaml"
)

// writeFile is one of a cloud-config's write_files.
type writeFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
}

// cloudConfig is the part of a bake's cloud-config the tests read.
type cloudConfig struct {
	Bootcmd    []string    `json:"bootcmd"`
	WriteFiles []writeFile `json:"write_files"`
	Runcmd     [][]string  `json:"runcmd"`
}

func parseCloudConfig(t *testing.T, userData string) (cloudConfig, map[string]string) {
	t.Helper()
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatalf("user data does not start as a cloud-config:\n%s", userData)
	}
	var c cloudConfig
	if err := yaml.UnmarshalStrict([]byte(userData), &c); err != nil {
		t.Fatalf("user data: %v\n%s", err, userData)
	}
	files := map[string]string{}
	for _, f := range c.WriteFiles {
		if f.Encoding != "b64" {
			t.Errorf("%s is written %q, want b64", f.Path, f.Encoding)
		}
		content, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		rel, ok := strings.CutPrefix(f.Path, buildDir+"/")
		if !ok {
			t.Errorf("%s is written outside %s", f.Path, buildDir)
		}
		want := "0600"
		if path.Ext(f.Path) == ".sh" {
			want = "0700"
		}
		if f.Permissions != want {
			t.Errorf("%s has mode %s, want %s", f.Path, f.Permissions, want)
		}
		files[rel] = string(content)
	}
	return c, files
}

func fakeKeys(keys []RepositoryKey) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		out[k.Name] = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n" + k.Name + "\n-----END PGP PUBLIC KEY BLOCK-----\n"
	}
	return out
}

var (
	fingerprintRE = regexp.MustCompile(`^[0-9A-F]{40}$`)
	repoKeyRE     = regexp.MustCompile(`(?m)^gpgkey=file:///etc/pki/rpm-gpg/kubehenge-([a-z-]+)\.asc$`)
)

// filesDir is the directory of a build mode's own files.
func filesDir(t *testing.T, m BuildMode) string {
	t.Helper()
	switch m {
	case ImageMode:
		return imageModeFilesDir
	case Transactional:
		return transactionalFilesDir
	}
	t.Fatalf("build mode %s", m)
	return ""
}

// Every family's recipe for each distribution: what the bake's first boot writes and runs, the
// keys it carries, and the checks that prove the node.
func TestRecipesOfEveryNodeOS(t *testing.T) {
	t.Parallel()
	bakes := []Bake{{Distribution: RKE2}, {Distribution: K3s}, {Distribution: Kubeadm, KubernetesVersion: "v1.37.1"}}
	for _, f := range Families {
		for _, r := range f.Releases() {
			for _, b := range bakes {
				b.NodeOS = NodeOS{Family: f.Name(), Release: r.Name}
				t.Run(b.NodeOS.String()+"/"+string(b.Distribution), func(t *testing.T) {
					t.Parallel()
					checkRecipe(t, f, r, b)
				})
			}
		}
	}
}

func checkRecipe(t *testing.T, f Family, r Release, b Bake) {
	t.Helper()
	keys := f.Keys(r, b)
	names := checkKeys(t, keys)
	recipe, err := f.Recipe(r, b, fakeKeys(keys))
	if err != nil {
		t.Fatal(err)
	}
	c, files := parseCloudConfig(t, recipe.UserData)
	dir := filesDir(t, r.Mode)
	checkGuestExec(t, c.Bootcmd)
	checkFiles(t, files, dir, keys, names)

	build := "sh " + buildDir + "/" + dir + "/build.sh " + string(b.Distribution)
	if b.Distribution == Kubeadm {
		build += " " + b.KubernetesVersion
	}
	if want := [][]string{{"sh", "-c", build + " 2>&1 | " + timestamped + " > " + buildLog}}; !slices.EqualFunc(c.Runcmd, want, slices.Equal) {
		t.Errorf("runcmd %q, want %q", c.Runcmd, want)
	}
	if len(recipe.Checks) != 1 || !slices.Equal(recipe.Checks[0], builtCheck) || recipe.Log != buildLog {
		t.Errorf("checks %q and log %q, want the build's marker and its log", recipe.Checks, recipe.Log)
	}

	node := nodeBootChecks(t, &recipe, r.Mode, b, files)
	if !slices.EqualFunc(node, nodeChecks(b), slices.Equal) {
		t.Errorf("node checks %q, want %q", node, nodeChecks(b))
	}
	if !slices.ContainsFunc(recipe.Cleanup, func(s []string) bool { return s[0] == cloudInit && slices.Contains(s, "--machine-id") }) ||
		!slices.Equal(recipe.Cleanup[len(recipe.Cleanup)-1], []string{"sync"}) {
		t.Errorf("cleanup %q leaves no fresh first boot", recipe.Cleanup)
	}
}

// checkKeys finds every key named once, fetched over https and pinned by its fingerprints.
func checkKeys(t *testing.T, keys []RepositoryKey) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, k := range keys {
		if names[k.Name] || !strings.HasPrefix(k.URL, "https://") || len(k.Fingerprints) == 0 {
			t.Errorf("key %+v: want a unique name, an https URL and its fingerprints", k)
		}
		names[k.Name] = true
		for _, fp := range k.Fingerprints {
			if !fingerprintRE.MatchString(fp) {
				t.Errorf("key %s has fingerprint %q", k.Name, fp)
			}
		}
	}
	return names
}

// checkGuestExec finds guest-exec allowed before anything else, with the embedded policy.
func checkGuestExec(t *testing.T, bootcmd []string) {
	t.Helper()
	cil, err := recipeFiles.ReadFile("node/kubehenge-qga.cil")
	if err != nil {
		t.Fatal(err)
	}
	if len(bootcmd) != 1 || !strings.Contains(bootcmd[0], base64.StdEncoding.EncodeToString(cil)) ||
		!strings.Contains(bootcmd[0], "semodule -i /run/kubehenge-qga.cil") || !strings.Contains(bootcmd[0], "--allow-rpcs=guest-exec,guest-exec-status,") {
		t.Errorf("bootcmd does not allow guest-exec: %q", bootcmd)
	}
}

// checkFiles finds the bake carrying its scripts, each of its keys and nothing else of the kind,
// and repositories that check their packages against those keys.
func checkFiles(t *testing.T, files map[string]string, dir string, keys []RepositoryKey, names map[string]bool) {
	t.Helper()
	for _, name := range []string{dir + "/build.sh", dir + "/node.sh", "node/kubehenge-qga.cil", "node/kubehenge-calico.cil", "node/images.sh", "node/kubeadm.sh"} {
		if files[name] == "" {
			t.Errorf("the bake does not carry %s", name)
		}
	}
	for name, content := range fakeKeys(keys) {
		if files["keys/"+name+".asc"] != content {
			t.Errorf("the bake does not carry the %s key", name)
		}
	}
	var repos int
	for name, content := range files {
		if strings.HasPrefix(name, "keys/") && !names[strings.TrimSuffix(strings.TrimPrefix(name, "keys/"), ".asc")] {
			t.Errorf("the bake carries %s, which no key of the recipe is", name)
		}
		if !strings.HasPrefix(name, "repos/") {
			continue
		}
		repos++
		m := repoKeyRE.FindStringSubmatch(content)
		if len(m) < 2 || !names[m[1]] || !strings.Contains(content, "\ngpgcheck=1\n") || !strings.Contains(content, "baseurl=https://") {
			t.Errorf("%s does not check its packages against a key the bake carries:\n%s", name, content)
		}
	}
	if repos == 0 {
		t.Error("the bake adds no repository")
	}
	checkScriptPaths(t, files, dir)
}

var bakePathRE = regexp.MustCompile(`\b(?:el|leap|node)/[A-Za-z0-9_.+-]+`)

// checkScriptPaths finds build.sh working in the bake's root, builtCheck's marker there, and every
// file of the bake it names carried.
func checkScriptPaths(t *testing.T, files map[string]string, dir string) {
	t.Helper()
	build := files[dir+"/build.sh"]
	if !strings.Contains(build, "\ncd \"$(dirname \"$0\")/..\"\n") || !strings.Contains(build, "\ntouch built\n") {
		t.Errorf("%s/build.sh does not work in the bake's root, where the check finds its marker", dir)
	}
	for name, content := range files {
		if path.Ext(name) != ".sh" && !strings.HasSuffix(name, "Containerfile") {
			continue
		}
		for _, p := range bakePathRE.FindAllString(content, -1) {
			if _, ok := files[p]; !ok {
				t.Errorf("%s names %s, which the bake does not carry", name, p)
			}
		}
	}
}

// nodeBootChecks are the checks of the node's own boot, past what the build mode runs first.
func nodeBootChecks(t *testing.T, recipe *Recipe, m BuildMode, b Bake, files map[string]string) [][]string {
	t.Helper()
	switch m {
	case ImageMode:
		if recipe.Image == nil || recipe.Reboot || recipe.BuilderDiskGiB < 16 || recipe.DiskGiB < 8 {
			t.Fatalf("an image-mode recipe boots the image it built from its own disk: %+v", recipe)
		}
		wantNodeUserData := "#cloud-config\n{}\n"
		if b.Distribution == Kubeadm {
			wantNodeUserData = "#cloud-config\nruncmd:\n  - [" + imagesPath + ", " + b.KubernetesVersion + "]\n"
		}
		if recipe.Image.UserData != wantNodeUserData {
			t.Errorf("node user data %q, want %q", recipe.Image.UserData, wantNodeUserData)
		}
		if !strings.Contains(files[imageModeFilesDir+"/manifest.yaml"], "  - minimal/manifest.yaml\n  - standard/generic-growfs.yaml\n") {
			t.Errorf("manifest:\n%s", files[imageModeFilesDir+"/manifest.yaml"])
		}
		return recipe.Image.Checks
	case Transactional:
		if recipe.Image != nil || !recipe.Reboot {
			t.Fatalf("a transactional recipe reboots into its snapshot: %+v", recipe)
		}
		node := recipe.RebootChecks
		if b.Distribution == Kubeadm {
			if !slices.Equal(node[0], []string{imagesPath, b.KubernetesVersion}) {
				t.Errorf("the first check after the reboot is %q, want the images pulled", node[0])
			}
			node = node[1:]
		}
		return node
	}
	t.Fatalf("build mode %s", m)
	return nil
}

// distributionChecks are what nodeChecks must hold for the distribution beyond the shared ones.
func distributionChecks(t *testing.T, d Distribution) []string {
	t.Helper()
	switch d {
	case RKE2:
		return []string{"rpm -q rke2-selinux", "grep -qx rke2", "touch /usr/local/", iptablesCheck}
	case K3s:
		return []string{"rpm -q k3s-selinux", "grep -qx k3s", "touch /usr/local/"}
	case Kubeadm:
		return []string{
			"kubeadm version -o short)\" = v1.37.1", "enable_selinux = true", "/sys/module/br_netfilter",
			"/sys/module/nf_conntrack", "type: spc_t", "crictl images", "enable kubelet.service", iptablesCheck,
		}
	case Talos:
	}
	t.Fatalf("distribution %s", d)
	return nil
}

func TestNodeChecks(t *testing.T) {
	t.Parallel()
	for _, b := range []Bake{{Distribution: RKE2}, {Distribution: K3s}, {Distribution: Kubeadm, KubernetesVersion: "v1.37.1"}} {
		checks := nodeChecks(b)
		if !slices.Equal(checks[0], []string{cloudInit, "status", "--wait"}) {
			t.Errorf("%s: the first check is %q, want cloud-init finished without warnings", b.Distribution, checks[0])
		}
		if !slices.Equal(checks[len(checks)-1], []string{"sh", "-c", avcDenied}) {
			t.Errorf("%s: the last check is %q, want no denials", b.Distribution, checks[len(checks)-1])
		}
		all := fmt.Sprint(checks)
		for _, want := range append([]string{"getenforce", "kubehenge-qga", "/usr/kubehenge", "modinfo br_netfilter", "enable qemu-guest-agent.service"}, distributionChecks(t, b.Distribution)...) {
			if !strings.Contains(all, want) {
				t.Errorf("%s: no check of %s in %q", b.Distribution, want, checks)
			}
		}
	}
}

// RKE2's Cilium installs portmap into /opt/cni/bin from a confined container, which an image-mode
// node keeps in /var/opt, where RKE2 would make the directory with /var/opt's label.
func TestImageModeRKE2NodesLetContainersInstallCNIPlugins(t *testing.T) {
	t.Parallel()
	if _, r, err := ReleaseOf(DefaultNodeOS); err != nil || r.Mode != ImageMode {
		t.Fatalf("the default node OS is not image mode: %+v, %v", r, err)
	}
	const check = "stat -L -c %C /opt/cni/bin | grep -q ':container_file_t:'"
	if all := fmt.Sprint(nodeChecks(Bake{NodeOS: DefaultNodeOS, Distribution: RKE2})); !strings.Contains(all, check) {
		t.Errorf("no check of /opt/cni/bin's label in %s", all)
	}
	for _, b := range []Bake{{Distribution: RKE2}, {NodeOS: DefaultNodeOS, Distribution: K3s}} {
		if all := fmt.Sprint(nodeChecks(b)); strings.Contains(all, "/opt/cni/bin") {
			t.Errorf("%s on %v is checked for the label of /opt/cni/bin, which nothing set: %s", b.Distribution, b.NodeOS, all)
		}
	}
	cil, err := recipeFiles.ReadFile("node/kubehenge-cni.cil")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cil), `(filecon "/var/opt/cni(/.*)?" any (system_u object_r container_file_t`) {
		t.Errorf("node/kubehenge-cni.cil does not label /var/opt/cni for containers:\n%s", cil)
	}
	script, err := recipeFiles.ReadFile("el/node.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"semodule -i /usr/share/kubehenge/kubehenge-cni.cil", "d /var/opt/cni/bin 0755 "} {
		if !strings.Contains(string(script), want) {
			t.Errorf("el/node.sh does not hold %q", want)
		}
	}
}

// The Tigera operator runs confined and reads the MTU calico-node writes into /var/lib/calico on
// the host; every family's node labels it for containers to read before Calico writes there.
func TestNodesLetTheTigeraOperatorReadCalicosMTU(t *testing.T) {
	t.Parallel()
	cil, err := recipeFiles.ReadFile("node/kubehenge-calico.cil")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cil), `(filecon "/var/lib/calico(/.*)?" any (system_u object_r container_ro_file_t`) {
		t.Errorf("node/kubehenge-calico.cil does not label /var/lib/calico read-only for containers:\n%s", cil)
	}
	for _, name := range []string{"el/node.sh", "leap/node.sh"} {
		script, err := recipeFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"semodule -i /usr/share/kubehenge/kubehenge-calico.cil", "d /var/lib/calico 0755 "} {
			if !strings.Contains(string(script), want) {
				t.Errorf("%s does not hold %q", name, want)
			}
		}
	}
	for _, d := range []Distribution{RKE2, K3s, Kubeadm} {
		if all := fmt.Sprint(nodeChecks(Bake{Distribution: d, KubernetesVersion: "v1.37.1"})); !strings.Contains(all, "/var/lib/calico | grep -q ':container_ro_file_t:'") {
			t.Errorf("%s: no check of /var/lib/calico's label in %s", d, all)
		}
	}
}

// The patches the spc_t check reads are those node/kubeadm.sh writes, one per control-plane pod.
func TestKubeadmPatchesRunTheControlPlaneAsSPC(t *testing.T) {
	t.Parallel()
	script, err := recipeFiles.ReadFile("node/kubeadm.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		if !strings.Contains(string(script), pod) {
			t.Errorf("node/kubeadm.sh writes no patch for %s", pod)
		}
	}
	if !strings.Contains(string(script), `\n      type: spc_t\n`) {
		t.Error("node/kubeadm.sh's patches do not run the pods as spc_t")
	}
}

func TestRecipeRefuses(t *testing.T) {
	t.Parallel()
	alma, release, err := ReleaseOf(DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	leapFamily, leapRelease, err := ReleaseOf(NodeOS{Family: leapName, Release: "16.1"})
	if err != nil {
		t.Fatal(err)
	}
	kubeadm := Bake{NodeOS: DefaultNodeOS, Distribution: Kubeadm, KubernetesVersion: "v1.37.1"}
	for name, try := range map[string]func() error{
		"a missing key": func() error {
			keys := fakeKeys(alma.Keys(release, kubeadm))
			delete(keys, "kubernetes")
			_, err := alma.Recipe(release, kubeadm, keys)
			return err
		},
		"a Kubernetes version that is none": func() error {
			b := kubeadm
			b.KubernetesVersion = "1.37"
			_, err := alma.Recipe(release, b, fakeKeys(alma.Keys(release, kubeadm)))
			return err
		},
		"Talos": func() error {
			_, err := alma.Recipe(release, Bake{Distribution: Talos}, nil)
			return err
		},
		"Talos on Leap": func() error {
			_, err := leapFamily.Recipe(leapRelease, Bake{Distribution: Talos}, nil)
			return err
		},
	} {
		if try() == nil {
			t.Errorf("a recipe with %s", name)
		}
	}
}

func TestRepositoryFile(t *testing.T) {
	t.Parallel()
	got := almaLinux10EL.containerd.file()
	want := `[kubehenge-containerd]
name=AlmaLinux EPEL 10 for x86-64-v2 (containerd)
baseurl=https://epel.repo.almalinux.org/$releasever${releasever_minor:+z}/x86_64_v2/
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/kubehenge-almalinux-epel.asc
includepkgs=containerd runc
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	repo := kubernetesRepository("v1.37.1")
	if repo.baseURL != "https://pkgs.k8s.io/core:/stable:/v1.37/rpm/" || repo.key.URL != repo.baseURL+"repodata/repomd.xml.key" ||
		!strings.Contains(repo.file(), "\nrepo_gpgcheck=1\n") || !slices.Equal(repo.key.Fingerprints, []string{KubernetesPackagesKeyFingerprint}) {
		t.Errorf("the Kubernetes repository %+v with key %+v", repo, repo.key)
	}
}

// RKE2's and k3s's policies come only from repositories that sign their metadata, k3s's from its
// testing channel, which alone has it for EL 10 and signs its metadata for SUSE.
func TestPolicyRepositoriesSignTheirMetadata(t *testing.T) {
	t.Parallel()
	for _, r := range []*repository{rke2SELinux, k3sSELinux, rke2SELinuxSUSE, k3sSELinuxSUSE} {
		if !r.signedMetadata {
			t.Errorf("%s does not check its metadata", r.baseURL)
		}
	}
	for _, r := range []*repository{k3sSELinux, k3sSELinuxSUSE} {
		if !strings.HasPrefix(r.baseURL, "https://rpm-testing.rancher.io/k3s/testing/") || r.key != rancherTestingKey {
			t.Errorf("k3s's policy comes from %s with key %s", r.baseURL, r.key.Name)
		}
	}
}

func TestManifest(t *testing.T) {
	t.Parallel()
	want := `releasever: 10
repos:
  - baseos
  - appstream
variables:
  distro: "almalinux10"
packages:
  - almalinux-repos
  - cloud-utils-growpart
include:
  - minimal/manifest.yaml
  - standard/generic-growfs.yaml
`
	if got := almaLinux10EL.manifest(Kubeadm); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// RKE2 and k3s install into /usr/local at boot, so it lives in /var on their nodes.
	for _, d := range []Distribution{RKE2, K3s} {
		if got := almaLinux10EL.manifest(d); !strings.Contains(got, "\nopt-usrlocal: \"var\"\n") {
			t.Errorf("%s's manifest:\n%s", d, got)
		}
	}
	if got := rocky10EL.manifest(Kubeadm); !strings.Contains(got, "  - rocky-repos\n") || !strings.Contains(got, `distro: "rocky10"`) {
		t.Errorf("Rocky Linux's manifest:\n%s", got)
	}
}

// cloud-init's NetworkManager renderer, EL's, brings up no network for a route to "default".
func TestNetworkConfigRoutesByPrefix(t *testing.T) {
	t.Parallel()
	for gateway, want := range map[string]string{"10.0.0.1": "      - to: 0.0.0.0/0\n        via: 10.0.0.1\n", "fd00::1": "      - to: ::/0\n        via: fd00::1\n"} {
		gw := netip.MustParseAddr(gateway)
		prefix := "10.0.0.20/24"
		if gw.Is6() {
			prefix = "fd00::20/64"
		}
		got := NetworkConfig("bc:24:11:00:00:01", &Network{Address: netip.MustParsePrefix(prefix), Gateway: gw})
		if !strings.Contains(got, want) || strings.Contains(got, "default") {
			t.Errorf("got\n%s\nwant a route\n%s", got, want)
		}
	}
}

// guestAgent answers guest-exec as an EL cloud image's agent does until the bake's first boot
// allows it: refused a number of times, then run.
type guestAgent struct {
	t       *testing.T
	mu      sync.Mutex
	refuse  int
	started int
}

func (g *guestAgent) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var data any
	switch p := strings.TrimPrefix(r.URL.Path, "/api2/json"); p {
	case "/nodes/pve1/status", "/nodes/pve1/qemu/100/config":
		data = map[string]any{}
	case "/nodes/pve1/qemu/100/status/current":
		data = map[string]any{"vmid": 100, "status": "running"}
	case "/nodes/pve1/qemu/100/agent/exec":
		g.started++
		if g.started <= g.refuse {
			http.Error(w, `{"data":null,"message":"Agent error: Command guest-exec has been disabled: the command is not allowed"}`, http.StatusInternalServerError)
			return
		}
		data = map[string]any{"pid": 7}
	case "/nodes/pve1/qemu/100/agent/exec-status":
		data = map[string]any{"exited": 1, "exitcode": 0}
	default:
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		g.t.Error(err)
	}
}

func TestWaitForExecRetriesUntilTheAgentRunsCommands(t *testing.T) {
	t.Parallel()
	agent := &guestAgent{t: t, refuse: 3}
	srv := httptest.NewServer(http.HandlerFunc(agent.serve))
	t.Cleanup(srv.Close)
	client := proxmox.NewClient(srv.URL + "/api2/json")
	node, err := client.Node(t.Context(), "pve1")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{Client: client, execRetry: time.Millisecond}
	if err := b.waitForExec(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if agent.started != 4 {
		t.Errorf("guest-exec tried %d times, want 4", agent.started)
	}

	agent.mu.Lock()
	agent.refuse, agent.started = 1<<30, 0
	agent.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := b.waitForExec(ctx, vm); err == nil {
		t.Error("an agent that never runs commands was waited for no longer")
	}
}
