package kubehenge_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"maps"
	"math/big"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// siteCA is a self-signed certificate as PEM, so that the schema's pattern and Talos's own checks
// read a real one.
func siteCA(t *testing.T, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// siteVariables are a cluster's variables with every site setting. The builtin ranges are those
// Cluster API passes for a Cluster that names its own.
func siteVariables(t *testing.T, builtin string, extra map[string]any) map[string]any {
	t.Helper()
	v := variablesOf(t, `{
		"controlPlaneEndpoint": "192.0.2.10",
		"cni": "flannel",
		"network": {"bridge": "vmbr0", "gateway": "192.0.2.1", "prefix": 24, "addresses": ["192.0.2.20-192.0.2.30"]},
		"builtin": {"cluster": {"network": `+builtin+`}},
		"registryMirrors": [
			{"registry": "docker.io", "endpoints": ["https://mirror.example.org:5000", "http://plain.example.org"], "caBundle": `+jsonString(siteCA(t, "mirror"))+`},
			{"registry": "*", "endpoints": ["https://harbor.example.org/v2"]},
			{"registry": "ghcr.io", "endpoints": ["https://mirror.example.org:5000"], "caBundle": `+jsonString(siteCA(t, "mirror"))+`}
		],
		"trustedCAs": [`+jsonString(siteCA(t, "corp"))+`],
		"egressProxy": {"url": "http://proxy.example.org:3128", "httpsUrl": "http://secure.example.org:3129", "noProxy": [".corp.example.org", "10.10.0.0/16", "localhost"]}
	}`)
	maps.Copy(v, extra)
	return v
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const ownRanges = `{"pods": ["172.16.0.0/16"], "services": ["172.17.0.0/16"]}`

// siteCase is where a distribution's templates keep the files and the first commands of a node.
type siteCase struct {
	file, kind, commands, distribution, units string
	files                                     []string
}

var siteCases = []siteCase{
	{"kubeadm.yaml", "KubeadmControlPlaneTemplate", "preKubeadmCommands", "kubeadm", "", []string{"spec", "template", "spec", "kubeadmConfigSpec"}},
	{"kubeadm.yaml", "KubeadmConfigTemplate", "preKubeadmCommands", "kubeadm", "", []string{"spec", "template", "spec"}},
	{"rke2.yaml", "RKE2ControlPlaneTemplate", "preRKE2Commands", "rke2", "rke2-server rke2-agent", []string{"spec", "template", "spec"}},
	{"rke2.yaml", "RKE2ConfigTemplate", "preRKE2Commands", "rke2", "rke2-server rke2-agent", []string{"spec", "template", "spec"}},
	{"k3s.yaml", "KThreesControlPlaneTemplate", "preK3sCommands", "k3s", "k3s k3s-agent", []string{"spec", "template", "spec", "kthreesConfigSpec"}},
	{"k3s.yaml", "KThreesConfigTemplate", "preK3sCommands", "k3s", "k3s k3s-agent", []string{"spec", "template", "spec"}},
}

// siteScript is the node's boot script and its commands after the patches for these variables.
func siteScript(t *testing.T, c *siteCase, vars map[string]any) (string, []string) {
	t.Helper()
	spec := asMap(t, field(t, renderClass(t, c.file).patched(t, c.kind, vars), c.files...))
	file, n := fileOf(t, spec["files"], "/etc/kubehenge/node-site.sh")
	if n != 1 {
		t.Fatalf("%s %s: %d node-site.sh files, want 1", c.file, c.kind, n)
	}
	if file["permissions"] != "0700" || file["owner"] != "root:root" {
		t.Errorf("%s %s: the script is %v, want root-only", c.file, c.kind, file)
	}
	script, ok := file["content"].(string)
	if !ok {
		t.Fatalf("%s %s: no content", c.file, c.kind)
	}
	return script, commandsOf(t, spec[c.commands])
}

func TestEveryNodeTakesTheSiteSettings(t *testing.T) {
	t.Parallel()
	for _, c := range siteCases {
		t.Run(c.file+"/"+c.kind, func(t *testing.T) {
			t.Parallel()
			vars := siteVariables(t, ownRanges, nil)
			script, commands := siteScript(t, &c, vars)
			at := slices.Index(commands, "/etc/kubehenge/node-site.sh || exit 1")
			if at < 1 || commands[0] != waitClockCommand {
				t.Fatalf("%v: the site script must run after the clock wait", commands)
			}
			if commands[at+1] != "set -a; . /etc/kubehenge/site/proxy.env; set +a" {
				t.Errorf("%v: the proxy's environment must be read right after the script", commands)
			}
			for _, want := range []string{
				"dist=" + c.distribution + "\n",
				// Heredoc bodies start at the line's first column, or the bundle would carry spaces.
				"<<'KUBEHENGE_PEM'\n-----BEGIN CERTIFICATE-----\n",
				"\n-----END CERTIFICATE-----\nKUBEHENGE_PEM\n",
				"cat > \"${site}/mirror-0.pem\" <<'KUBEHENGE_PEM'",
				"cat > \"${site}/mirror-2.pem\" <<'KUBEHENGE_PEM'",
				"HTTP_PROXY='http://proxy.example.org:3128'\n",
				"HTTPS_PROXY='http://secure.example.org:3129'\n",
				"https_proxy='http://secure.example.org:3129'\n",
				// The cluster's own ranges, node network and API address join the user's exceptions, once.
				"NO_PROXY='localhost,127.0.0.1,::1,.svc,.cluster.local,192.0.2.10,192.0.2.1/24,172.16.0.0/16,172.17.0.0/16,.corp.example.org,10.10.0.0/16'\n",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("script lacks %q:\n%s", want, script)
				}
			}
			checkMirrors(t, c.distribution, script)
			for unit := range strings.FieldsSeq(c.units) {
				if !strings.Contains(script, unit) {
					t.Errorf("script never writes the environment of %s", unit)
				}
			}
			syntax(t, script)
		})
	}
}

func checkMirrors(t *testing.T, distribution, script string) {
	t.Helper()
	if distribution == "kubeadm" {
		for _, want := range []string{
			"dir=/etc/containerd/certs.d/docker.io\n",
			"dir=/etc/containerd/certs.d/_default\n",
			"dir=/etc/containerd/certs.d/ghcr.io\n",
			"[host.\"https://mirror.example.org:5000\"]",
			"[host.\"http://plain.example.org\"]",
			"ca = \"/etc/kubehenge/site/mirror-0.pem\"",
			"ca = \"/etc/kubehenge/site/mirror-2.pem\"",
			"override_path = true",
			"systemctl restart containerd",
			"EnvironmentFile=%s",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("kubeadm script lacks %q", want)
			}
		}
		return
	}
	for _, want := range []string{
		"/etc/rancher/${dist}/registries.yaml",
		"  \"docker.io\":\n    endpoint:\n      - \"https://mirror.example.org:5000\"\n      - \"http://plain.example.org\"\n",
		"  \"*\":\n    endpoint:\n      - \"https://harbor.example.org/v2\"\n",
		"  \"ghcr.io\":\n    endpoint:\n      - \"https://mirror.example.org:5000\"\n",
		"configs:\n  \"mirror.example.org:5000\":\n    tls:\n      ca_file: /etc/kubehenge/site/mirror-0.pem\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("%s script lacks %q", distribution, want)
		}
	}
	// A host shared by two registries gets one entry, with the first one's authority.
	if strings.Count(script, "\"mirror.example.org:5000\":") != 1 {
		t.Errorf("%s script names the shared mirror host more than once", distribution)
	}
}

// syntax holds a script to the shell's own parser, which is where a stray quote or a missing
// heredoc terminator shows.
func syntax(t *testing.T, script string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Log("no sh to parse the script with")
		return
	}
	cmd := exec.CommandContext(t.Context(), sh, "-n") //nolint:gosec // sh is the PATH's own shell, parsing without running
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v: %s\n%s", err, out, script)
	}
}

func TestNodesChangeNothingWithoutSiteSettings(t *testing.T) {
	t.Parallel()
	for _, c := range siteCases {
		spec := asMap(t, field(t, renderClass(t, c.file).patched(t, c.kind, variablesOf(t, bareNet)), c.files...))
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"node-site", "kubehenge/site", "registries.yaml", "proxy.env"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("%s %s: %q without any site setting", c.file, c.kind, banned)
			}
		}
		// A proxy without an address (the exceptions extend the management side's, for add-ons)
		// and empty lists give nodes nothing either.
		empty := variablesOf(t, `{"registryMirrors": [], "trustedCAs": [], "egressProxy": {"noProxy": ["a.example.org"]}}`)
		spec = asMap(t, field(t, renderClass(t, c.file).patched(t, c.kind, empty), c.files...))
		if raw, err = json.Marshal(spec); err != nil || strings.Contains(string(raw), "node-site") {
			t.Errorf("%s %s: empty settings write the site script (%v)", c.file, c.kind, err)
		}
	}
}

func TestNoProxyHoldsTheClustersOwnRangesOrTheDistributionsDefaults(t *testing.T) {
	t.Parallel()
	for _, c := range []siteCase{siteCases[0], siteCases[2], siteCases[4]} {
		defaults := map[string]string{"kubeadm": "10.244.0.0/16,10.96.0.0/12", "rke2": "10.42.0.0/16,10.43.0.0/16", "k3s": "10.42.0.0/16,10.43.0.0/16"}[c.distribution]
		for builtin, want := range map[string]string{
			`{}`:                           defaults,
			`{"pods": [], "services": []}`: defaults,
			ownRanges:                      "172.16.0.0/16,172.17.0.0/16",
			`{"pods": ["172.16.0.0/16", "172.16.0.0/16"], "services": ["172.17.0.0/16"]}`: "172.16.0.0/16,172.17.0.0/16",
		} {
			script, _ := siteScript(t, &c, siteVariables(t, builtin, nil))
			line := "NO_PROXY='localhost,127.0.0.1,::1,.svc,.cluster.local,192.0.2.10,192.0.2.1/24," + want + ",.corp.example.org,10.10.0.0/16'\n"
			if !strings.Contains(script, line) {
				t.Errorf("%s with %s: script lacks %q:\n%s", c.distribution, builtin, line, script)
			}
		}
	}
}

// A node's proxy leaves out the node network of each of the cluster's address families, and no
// IPv4 network where the cluster has none.
func TestNoProxyHoldsTheNodeNetworkOfEachFamily(t *testing.T) {
	t.Parallel()
	const (
		ipv6     = `"ipv6": {"addresses": ["2001:db8::20-2001:db8::30"], "prefix": 64, "gateway": "2001:db8::1"}`
		ranges   = `{"pods": ["fd5e:4b1c:9a20::/56"], "services": ["fd5e:4b1c:9a21::/112"]}`
		dual     = `{"pods": ["172.16.0.0/16", "fd5e:4b1c:9a20::/56"], "services": ["172.17.0.0/16", "fd5e:4b1c:9a21::/112"]}`
		prologue = "NO_PROXY='localhost,127.0.0.1,::1,.svc,.cluster.local,192.0.2.10,"
		epilogue = ",.corp.example.org,10.10.0.0/16'\n"
	)
	for name, c := range map[string]struct{ network, builtin, want string }{
		"IPv6 only": {
			`{"ipFamilies": ["IPv6"], "bridge": "vmbr0", ` + ipv6 + `}`, ranges,
			"2001:db8::1/64,fd5e:4b1c:9a20::/56,fd5e:4b1c:9a21::/112",
		},
		"dual-stack": {
			`{"ipFamilies": ["IPv4", "IPv6"], "bridge": "vmbr0", "gateway": "192.0.2.1", "prefix": 24, "addresses": ["192.0.2.20-192.0.2.30"], ` + ipv6 + `}`, dual,
			"192.0.2.1/24,2001:db8::1/64,172.16.0.0/16,fd5e:4b1c:9a20::/56,172.17.0.0/16,fd5e:4b1c:9a21::/112",
		},
	} {
		var network map[string]any
		if err := json.Unmarshal([]byte(c.network), &network); err != nil {
			t.Fatal(err)
		}
		script, _ := siteScript(t, &siteCases[0], siteVariables(t, c.builtin, map[string]any{"network": network}))
		if line := prologue + c.want + epilogue; !strings.Contains(script, line) {
			t.Errorf("%s: script lacks %q:\n%s", name, line, script)
		}
	}
}

func TestTalosNodesTakeTheSiteSettings(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	vars := siteVariables(t, ownRanges, nil)
	for kind, role := range map[string]machine.Type{"TalosControlPlaneTemplate": machine.TypeControlPlane, "TalosConfigTemplate": machine.TypeWorker} {
		docs := talosConfig(t, role, class.strategicPatches(t, kind, vars))
		env := asJSON(t, field(t, docs["v1alpha1"], "machine", "env"))
		for _, want := range []string{
			`"HTTP_PROXY":"http://proxy.example.org:3128"`,
			`"HTTPS_PROXY":"http://secure.example.org:3129"`,
			`"NO_PROXY":"localhost,127.0.0.1,::1,.svc,.cluster.local,192.0.2.10,192.0.2.1/24,172.16.0.0/16,172.17.0.0/16,.corp.example.org,10.10.0.0/16"`,
		} {
			if !strings.Contains(env, want) {
				t.Errorf("%s: machine.env %s lacks %s", kind, env, want)
			}
		}
		if got := asJSON(t, docs["RegistryMirrorConfig/docker.io"]["endpoints"]); got != `[{"url":"https://mirror.example.org:5000"},{"url":"http://plain.example.org"}]` {
			t.Errorf("%s: docker.io endpoints %s", kind, got)
		}
		if got := asJSON(t, docs["RegistryMirrorConfig/*"]["endpoints"]); got != `[{"overridePath":true,"url":"https://harbor.example.org/v2"}]` {
			t.Errorf("%s: wildcard endpoints %s", kind, got)
		}
		if _, ok := docs["RegistryMirrorConfig/ghcr.io"]; !ok {
			t.Errorf("%s: no mirror for ghcr.io", kind)
		}
		// One authority document per mirror host, though two registries use it.
		if _, ok := docs["RegistryTLSConfig/mirror.example.org:5000"]["ca"].(string); !ok {
			t.Errorf("%s: no authority for the mirror host: %v", kind, docs)
		}
		if _, ok := docs["TrustedRootsConfig/kubehenge"]["certificates"].(string); !ok {
			t.Errorf("%s: no trusted roots", kind)
		}
		bare := talosConfig(t, role, class.strategicPatches(t, kind, variablesOf(t, ntpNetwork)))
		for key := range bare {
			if strings.HasPrefix(key, "RegistryMirrorConfig") || strings.HasPrefix(key, "RegistryTLSConfig") || strings.HasPrefix(key, "TrustedRootsConfig") {
				t.Errorf("%s: %s without site settings", kind, key)
			}
		}
		if got := asJSON(t, field(t, bare["v1alpha1"], "machine", "env")); got != "null" {
			t.Errorf("%s: machine.env %s without a proxy", kind, got)
		}
	}
}

func TestEveryClassValidatesTheSiteSettings(t *testing.T) {
	t.Parallel()
	good := siteCA(t, "good")
	valid := func(v string) any {
		var out any
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		class := renderClass(t, file)
		check := func(name, value string, ok bool, contains string) {
			t.Helper()
			// Inside an object, as every variable sits in a Cluster: the API server's validator
			// takes a root without properties for a resource.
			wrapped := &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"v": *class.variables[name]}}
			errs := validateVariable(t, wrapped, map[string]any{"v": valid(value)}, nil)
			switch {
			case ok && len(errs) > 0:
				t.Errorf("%s %s %s: %v", file, name, value, errs)
			case !ok && len(errs) == 0:
				t.Errorf("%s %s %s was accepted", file, name, value)
			case !ok && !strings.Contains(errs.ToAggregate().Error(), contains):
				t.Errorf("%s %s %s: %v lacks %q", file, name, value, errs, contains)
			}
		}
		goodJSON := jsonString(good)
		check("registryMirrors", `[{"registry":"docker.io","endpoints":["https://m.example.org:5000"],"caBundle":`+goodJSON+`}]`, true, "")
		check("registryMirrors", `[{"registry":"*","endpoints":["http://m.example.org","https://h.example.org/v2"]}]`, true, "")
		check("registryMirrors", `[{"registry":"registry.example.org:5000","endpoints":["https://m.example.org/prefix/v2"]}]`, true, "")
		for _, bad := range []string{"m.example.org", "ftp://m.example.org", "https://m.example.org/path", "https://m.example.org/", "https://user@m.example.org", "https://m.example.org?a=b", "https://m'$(x).example.org", "https:// m.example.org"} {
			check("registryMirrors", `[{"registry":"docker.io","endpoints":[`+jsonString(bad)+`]}]`, false, "endpoints")
		}
		check("registryMirrors", `[{"registry":"Docker.io","endpoints":["https://m.example.org"]}]`, false, "registry")
		check("registryMirrors", `[{"registry":"docker.io","endpoints":[]}]`, false, "endpoints")
		check("registryMirrors", `[{"registry":"docker.io","endpoints":["https://m.example.org"],"caBundle":"not a certificate"}]`, false, "caBundle")
		check("registryMirrors", `[{"registry":"docker.io","endpoints":["https://m.example.org"],"caBundle":`+jsonString("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")+`}]`, false, "caBundle")
		check("registryMirrors", `[{"registry":"docker.io","endpoints":["https://m.example.org"],"caBundle":`+jsonString(strings.ReplaceAll(good, "\n", "\r\n"))+`}]`, false, "caBundle")
		check("trustedCAs", `[`+goodJSON+`]`, true, "")
		check("trustedCAs", `[`+jsonString(good+good)+`]`, true, "")
		check("trustedCAs", `["not a certificate"]`, false, "in body")
		check("trustedCAs", `[`+jsonString(good+"\nEOF\n")+`]`, false, "in body")
		check("egressProxy", `{"url":"http://p.example.org:3128","httpsUrl":"https://q.example.org:3129","noProxy":[".corp","10.0.0.0/8","fd00::/8","*.example.org"]}`, true, "")
		check("egressProxy", `{"url":"http://[2001:db8::1]:3128"}`, true, "")
		check("egressProxy", `{"httpsUrl":"http://q.example.org"}`, false, "httpsUrl comes with url")
		check("egressProxy", `{"disabled":true,"httpsUrl":"http://q.example.org"}`, false, "goes direct")
		check("egressProxy", `{"url":"http://p.example.org","noProxy":["a'b"]}`, false, "noProxy")
		check("egressProxy", `{"url":"http://p.example.org","noProxy":["a b"]}`, false, "noProxy")
		check("egressProxy", `{"url":"http://p'$(x).example.org"}`, false, "url")
		check("egressProxy", `{"url":"http://user:pw@p.example.org"}`, false, "url")
		// The exceptions of a cluster without a proxy of its own extend the management side's.
		check("egressProxy", `{"noProxy":["a.example.org"]}`, true, "")
		_, offers := class.variables["releaseMirror"]
		if agent := file == "rke2.yaml" || file == "k3s.yaml"; offers != agent {
			t.Errorf("%s: a release mirror of its own offered = %v", file, offers)
		}
		if !offers {
			continue
		}
		check("releaseMirror", `{"url":"https://m.example.org/github","caBundle":`+goodJSON+`}`, true, "")
		check("releaseMirror", `{"url":"http://10.0.0.5:8944"}`, true, "")
		check("releaseMirror", `{"url":"https://[2001:db8::5]:8944/a/b"}`, true, "")
		for _, bad := range []string{"m.example.org", "ftp://m.example.org", "https://m.example.org/", "https://user:pw@m.example.org", "https://m.example.org?a=b", "https://m'$(x).example.org", "https:// m.example.org"} {
			check("releaseMirror", `{"url":`+jsonString(bad)+`}`, false, "url")
		}
		check("releaseMirror", `{"caBundle":`+goodJSON+`}`, false, "url")
		check("releaseMirror", `{"url":"http://m.example.org","caBundle":`+goodJSON+`}`, false, "https:// mirror")
		check("releaseMirror", `{"url":"https://m.example.org","caBundle":`+jsonString("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")+`}`, false, "caBundle")
		check("releaseMirror", `{"url":"https://m.example.org","caBundle":`+jsonString(strings.Repeat(good, 65536/len(good)+1))+`}`, false, "caBundle")
	}
}
