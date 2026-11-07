package templates

import (
	"encoding/base64"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
)

const testProxy = "http://alice:s3cret@proxy.example.com:3128" //nolint:gosec // fake credentials

func proxyBakes() []Bake {
	return []Bake{{Distribution: RKE2}, {Distribution: K3s}, {Distribution: Kubeadm, KubernetesVersion: "v1.37.1"}}
}

func TestBakeWithoutProxyIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, f := range Families {
		for _, r := range f.Releases() {
			for _, b := range proxyBakes() {
				b.NodeOS = NodeOS{Family: f.Name(), Release: r.Name}
				plain, err := f.Recipe(r, b, fakeKeys(f.Keys(r, b)))
				if err != nil {
					t.Fatal(err)
				}
				b.Proxy = &GuestProxy{}
				empty, err := f.Recipe(r, b, fakeKeys(f.Keys(r, b)))
				if err != nil {
					t.Fatal(err)
				}
				if plain.UserData != empty.UserData || plain.Image != nil && plain.Image.UserData != empty.Image.UserData {
					t.Errorf("%s %s: an empty proxy changed the cloud-config", b.NodeOS, b.Distribution)
				}
				if strings.Contains(plain.UserData, "proxy") && !strings.Contains(plain.UserData, "kubehenge-qga") {
					t.Errorf("%s %s: the plain cloud-config mentions a proxy", b.NodeOS, b.Distribution)
				}
				_, files := parseCloudConfig(t, plain.UserData)
				for name := range files {
					if strings.HasPrefix(name, proxyDir+"/") {
						t.Errorf("%s %s: %s is written without a proxy", b.NodeOS, b.Distribution, name)
					}
				}
			}
		}
	}
}

func TestBakeWithProxy(t *testing.T) {
	t.Parallel()
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	for _, f := range Families {
		for _, r := range f.Releases() {
			for _, b := range proxyBakes() {
				b.NodeOS = NodeOS{Family: f.Name(), Release: r.Name}
				b.Proxy = &GuestProxy{URL: testProxy, NoProxy: "10.0.0.0/24,.svc", CABundle: ca}
				recipe, err := f.Recipe(r, b, fakeKeys(f.Keys(r, b)))
				if err != nil {
					t.Fatal(err)
				}
				c, files := parseCloudConfig(t, recipe.UserData)
				name := b.NodeOS.String() + "/" + string(b.Distribution)
				up := files[proxyDir+"/up.sh"]
				for _, want := range []string{
					"proxy_username=%s", "/etc/sysconfig/proxy", "/etc/environment", "/etc/profile.d/kubehenge-proxy.sh",
					"containerd.service.d/kubehenge-proxy.conf", "update-ca-trust", "update-ca-certificates",
					"'http://proxy.example.com:3128'", "'alice'", "'s3cret'", "'10.0.0.0/24,.svc'",
				} {
					if !strings.Contains(up, want) {
						t.Errorf("%s: up.sh lacks %q", name, want)
					}
				}
				if strings.Contains(up, "alice:s3cret@") && strings.Contains(strings.SplitN(up, "if [ -f /etc/dnf/dnf.conf ]", 2)[1], "printf 'proxy=%s\\n' 'http://alice") {
					t.Errorf("%s: dnf gets the credentials inside the address", name)
				}
				if files[proxyDir+"/ca.crt"] != ca {
					t.Errorf("%s: ca.crt = %q", name, files[proxyDir+"/ca.crt"])
				}
				if !strings.Contains(files[proxyDir+"/down.sh"], "kubehenge-proxy.crt") || !strings.Contains(files[proxyDir+"/down.sh"], "proxy_password") {
					t.Errorf("%s: down.sh takes nothing away", name)
				}
				last := c.Runcmd[len(c.Runcmd)-1]
				if !strings.Contains(last[len(last)-1], "sh "+path.Join(buildDir, proxyDir, "run.sh")+" sh ") {
					t.Errorf("%s: the build does not run inside run.sh: %v", name, last)
				}
				if b.Distribution == Kubeadm && recipe.Image != nil {
					ud := recipe.Image.UserData
					if !strings.Contains(ud, "/run/kubehenge-proxy/run.sh") || !strings.Contains(ud, imagesPath) {
						t.Errorf("%s: the image pull does not run through the proxy:\n%s", name, ud)
					}
				}
				if recipe.Image != nil && b.Distribution != Kubeadm && strings.Contains(recipe.Image.UserData, "proxy") {
					t.Errorf("%s: the booted image gets a proxy it does not use", name)
				}
				for _, check := range append(slices.Clone(recipe.RebootChecks), recipe.Checks...) {
					if slices.Contains(check, imagesPath) && f.Name() == "leap" && !slices.Contains(check, path.Join(buildDir, proxyDir, "run.sh")) {
						t.Errorf("%s: the reboot check pulls images without the proxy: %v", name, check)
					}
				}
			}
		}
	}
}

func TestPullUserData(t *testing.T) {
	t.Parallel()
	if got, want := pullUserData(nil, "v1.37.1"), "#cloud-config\nruncmd:\n  - ["+imagesPath+", v1.37.1]\n"; got != want {
		t.Errorf("without a proxy: %q, want %q", got, want)
	}
	if got := pullUserData(&GuestProxy{}, "v1.37.1"); got != pullUserData(nil, "v1.37.1") {
		t.Errorf("an empty proxy: %q", got)
	}
	with := pullUserData(&GuestProxy{URL: testProxy}, "v1.37.1")
	if pullUserData(&GuestProxy{URL: testProxy}, "v1.37.1") != with {
		t.Error("the cloud-config differs between renders")
	}
	if !strings.Contains(with, "/run/kubehenge-proxy/up.sh") || !strings.Contains(with, "/run/kubehenge-proxy/down.sh") {
		t.Errorf("scripts missing:\n%s", with)
	}
	if strings.Contains(with, "s3cret") {
		t.Error("the credentials are in the cloud-config unencoded")
	}
	if strings.Contains(with, "ca.crt") {
		t.Error("an authority is written without one given")
	}
}

func TestProxyRefusesLineBreaks(t *testing.T) {
	t.Parallel()
	b := Bake{NodeOS: NodeOS{Family: Families[0].Name(), Release: Families[0].Releases()[0].Name}, Distribution: RKE2, Proxy: &GuestProxy{URL: "http://proxy.example.com\nrm -rf /"}}
	f, r, err := Available(b.NodeOS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Recipe(r, b, fakeKeys(f.Keys(r, b))); err == nil {
		t.Error("an address with a line break was accepted")
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"plain": "'plain'", "it's": `'it'\''s'`, "": "''", "$(x) `y`": "'$(x) `y`'"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestProxyFromEnvironment(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", CAFileEnv} {
		t.Setenv(name, "")
	}
	if p, err := ProxyFromEnvironment(); p.Enabled() || err != nil {
		t.Fatalf("without variables: %+v, %v", p, err)
	}
	ca := t.TempDir() + "/ca.crt"
	if err := os.WriteFile(ca, []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_PROXY", "http://http-only:1")
	t.Setenv("NO_PROXY", "a.example.com")
	p, err := ProxyFromEnvironment()
	if err != nil || p.URL != "http://http-only:1" || p.NoProxy != "a.example.com" || p.CABundle != "" {
		t.Fatalf("got %+v, %v", p, err)
	}
	t.Setenv("HTTPS_PROXY", testProxy)
	t.Setenv(CAFileEnv, ca)
	p, err = ProxyFromEnvironment()
	if err != nil || p.URL != testProxy || p.CABundle != "PEM" {
		t.Fatalf("got %+v, %v", p, err)
	}
	t.Setenv(CAFileEnv, ca+".missing")
	if _, err = ProxyFromEnvironment(); err == nil {
		t.Error("a missing authority file was ignored")
	}
}

func TestPullUserDataFilesDecode(t *testing.T) {
	t.Parallel()
	ud := pullUserData(&GuestProxy{URL: testProxy, CABundle: "PEM"}, "v1.37.1")
	var encoded []string
	for line := range strings.SplitSeq(ud, "\n") {
		if content, ok := strings.CutPrefix(strings.TrimSpace(line), "content: "); ok {
			encoded = append(encoded, content)
		}
	}
	if len(encoded) != 4 {
		t.Fatalf("%d files, want run.sh, down.sh, up.sh and ca.crt", len(encoded))
	}
	for _, e := range encoded {
		if _, err := base64.StdEncoding.DecodeString(e); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// inOrder fails unless each marker is found in text, each after the one before it.
func inOrder(t *testing.T, name, text string, markers ...string) {
	t.Helper()
	pos := 0
	for _, m := range markers {
		i := strings.Index(text[pos:], m)
		if i < 0 {
			t.Errorf("%s: %q is missing or out of order", name, m)
			return
		}
		pos += i + len(m)
	}
}

func recipeScript(t *testing.T, name string) string {
	t.Helper()
	content, err := recipeFiles.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// TestProxyRunsInOrder walks a proxied bake step by step. The proxy goes up before the build and the
// authority is trusted where the build installs packages (the container of an EL build, the snapshot
// of a Leap one). Everything with a credential is gone before the image is what clones boot.
func TestProxyRunsInOrder(t *testing.T) {
	t.Parallel()
	inOrder(t, "run.sh", proxyRun, `sh "${dir}/up.sh"`, "/etc/profile.d/kubehenge-proxy.sh", `"$@"`, `sh "${dir}/down.sh"`)

	// EL: podman passes the exported proxy on to the container's dnf. The build adds the authority
	// to the container's trust, and the anchor leaves with the packages installed.
	inOrder(t, "el/node.sh", recipeScript(t, "el/node.sh"),
		"proxy/ca.crt", "update-ca-trust", "dnf -y install", `proxy/down.sh`, "rm -rf /var/cache/*")

	// Leap: the snapshot is built from a copy of /etc and nothing of /var. The authority is trusted
	// in it before zypper runs, and every proxy file is taken out of it before it is the root.
	inOrder(t, "leap/node.sh", recipeScript(t, "leap/node.sh"),
		"proxy/ca.crt", "update-ca-certificates", "zypper --non-interactive install", `proxy/down.sh`, `rm -rf "${files}"`)
	inOrder(t, "leap/build.sh", recipeScript(t, "leap/build.sh"),
		`cp -a . "${staged}"`, "transactional-update", `rm -rf "${staged}"`, "touch built")

	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	family := leap{}
	for _, r := range family.Releases() {
		for _, b := range proxyBakes() {
			b.NodeOS = NodeOS{Family: family.Name(), Release: r.Name}
			b.Proxy = &GuestProxy{URL: testProxy, CABundle: ca}
			recipe, err := family.Recipe(r, b, fakeKeys(family.Keys(r, b)))
			if err != nil {
				t.Fatal(err)
			}
			name := b.NodeOS.String() + "/" + string(b.Distribution)
			c, files := parseCloudConfig(t, recipe.UserData)
			// A snapshot leaves /var alone, so the scripts the pull after the reboot runs are still
			// there. parseCloudConfig has refused any file written outside buildDir.
			if !strings.HasPrefix(buildDir, "/var/") {
				t.Fatalf("%s: the bake's files are in %s, which a snapshot may replace", name, buildDir)
			}
			for _, want := range []string{"up.sh", "down.sh", "run.sh", "ca.crt"} {
				if _, ok := files[proxyDir+"/"+want]; !ok {
					t.Errorf("%s: %s is not written", name, want)
				}
			}
			if !recipe.Reboot {
				t.Errorf("%s: no reboot into the snapshot", name)
			}
			last := c.Runcmd[len(c.Runcmd)-1]
			inOrder(t, name+" build", last[len(last)-1], "run.sh", "leap/build.sh")
			if b.Distribution == Kubeadm {
				if len(recipe.RebootChecks) == 0 || !slices.Contains(recipe.RebootChecks[0], path.Join(buildDir, proxyDir, "run.sh")) {
					t.Errorf("%s: the pull after the reboot does not run first, through run.sh: %v", name, recipe.RebootChecks)
				}
			}
			removed := slices.IndexFunc(recipe.Cleanup, func(step []string) bool {
				return slices.Equal(step, []string{"rm", "-rf", path.Join(buildDir, proxyDir)})
			})
			if removed < 0 {
				t.Errorf("%s: the proxy scripts stay in the template: %v", name, recipe.Cleanup)
			}
		}
	}
}
