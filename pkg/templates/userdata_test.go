package templates

import (
	"encoding/base64"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func parseCloudConfig(t *testing.T, s string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(s, "#cloud-config\n") {
		t.Fatalf("user data does not start with #cloud-config: %.40q", s)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("user data is not YAML: %v\n%s", err, s)
	}
	return doc
}

// as reads a decoded YAML value as T, failing the test if it is not one.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("%v is a %T, not a %T", v, v, x)
	}
	return x
}

func TestBakeUserDataForCloudImages(t *testing.T) {
	t.Parallel()
	doc := parseCloudConfig(t, mustUserData(t, Bake{}, ""))
	if upgrade, ok := doc["package_upgrade"].(bool); !ok || !upgrade {
		t.Error("templates must start with every update installed")
	}
	if !slices.Contains(as[[]any](t, doc["packages"]), any("qemu-guest-agent")) {
		t.Errorf("packages %v lack the QEMU guest agent", doc["packages"])
	}
}

func TestBakeUserDataForKubeadm(t *testing.T) {
	t.Parallel()
	key := string(readFixture(t, "pkgs.k8s.io-Release.key"))
	doc := parseCloudConfig(t, mustUserData(t, Bake{KubernetesVersion: "v1.37.1"}, key))

	sources := as[map[string]any](t, as[map[string]any](t, doc["apt"])["sources"])
	source := as[map[string]any](t, sources["kubernetes.list"])
	if got := source["source"]; got != "deb [signed-by=$KEY_FILE] https://pkgs.k8s.io/core:/stable:/v1.37/deb/ /" {
		t.Errorf("apt source = %q", got)
	}
	if strings.TrimSpace(as[string](t, source["key"])) != strings.TrimSpace(key) {
		t.Error("the pkgs.k8s.io key did not survive the YAML round trip")
	}
	packages := as[[]any](t, doc["packages"])
	for _, want := range []string{"kubelet", "kubeadm", "kubectl"} {
		if !slices.ContainsFunc(packages, func(p any) bool {
			pin, ok := p.([]any)
			return ok && pin[0] == want && pin[1] == "1.37.1-1.1"
		}) {
			t.Errorf("%s is not pinned to 1.37.1-1.1 in %v", want, packages)
		}
	}
	runcmd := as[[]any](t, doc["runcmd"])
	if last := as[[]any](t, runcmd[len(runcmd)-1]); !slices.Equal(last, []any{imagesScriptPath, "v1.37.1"}) {
		t.Errorf("the image pull is %v, want the checked pull of v1.37.1", last)
	}
	script := slices.IndexFunc(as[[]any](t, doc["write_files"]), func(f any) bool {
		return as[map[string]any](t, f)["path"] == imagesScriptPath
	})
	if script < 0 {
		t.Fatalf("no %s among the files", imagesScriptPath)
	}
	file := as[map[string]any](t, as[[]any](t, doc["write_files"])[script])
	content, err := base64.StdEncoding.DecodeString(as[string](t, file["content"]))
	if err != nil || file["encoding"] != "b64" || string(content) != imagesScript || file["permissions"] != "0700" {
		t.Errorf("%s written as %v (%v)", imagesScriptPath, file, err)
	}
}

func TestBakeUserDataForKubeadmNeedsTheKey(t *testing.T) {
	t.Parallel()
	if _, err := BakeUserData(Bake{KubernetesVersion: "v1.37.1"}, ""); err == nil {
		t.Error("a kubeadm bake without the pkgs.k8s.io key succeeded")
	}
}

func mustUserData(t *testing.T, b Bake, key string) string {
	t.Helper()
	s, err := BakeUserData(b, key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBakeChecksStartWithCloudInit(t *testing.T) {
	t.Parallel()
	for _, b := range []Bake{{}, {KubernetesVersion: "v1.37.1"}} {
		if checks := BakeChecks(b); !slices.Equal(checks[0], []string{"cloud-init", "status", "--wait"}) {
			t.Errorf("first check for %+v is %v", b, checks[0])
		}
	}
}

// networkConfig is the part of a NoCloud network-config (version 2) the tests read.
type networkConfig struct {
	Version   int                 `yaml:"version"`
	Ethernets map[string]ethernet `yaml:"ethernets"`
}

type ethernet struct {
	Match       ethernetMatch `yaml:"match"`
	DHCP4       bool          `yaml:"dhcp4"`
	Addresses   []string      `yaml:"addresses"`
	Routes      []route       `yaml:"routes"`
	Nameservers nameservers   `yaml:"nameservers"`
}

type ethernetMatch struct {
	MAC string `yaml:"macaddress"`
}

type route struct {
	To  string `yaml:"to"`
	Via string `yaml:"via"`
}

type nameservers struct {
	Addresses []string `yaml:"addresses"`
}

func TestNetworkConfig(t *testing.T) {
	t.Parallel()
	static := &Network{
		Address: netip.MustParsePrefix("192.0.2.50/24"),
		Gateway: netip.MustParseAddr("192.0.2.1"),
		DNS:     []netip.Addr{netip.MustParseAddr("192.0.2.53")},
	}
	for name, n := range map[string]*Network{"dhcp": nil, "static": static} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var doc networkConfig
			if err := yaml.Unmarshal([]byte(NetworkConfig("bc:24:11:aa:bb:cc", n)), &doc); err != nil {
				t.Fatal(err)
			}
			eth := doc.Ethernets["bake0"]
			if doc.Version != 2 || eth.Match.MAC != "bc:24:11:aa:bb:cc" {
				t.Fatalf("unexpected network config %+v", doc)
			}
			if n == nil {
				if !eth.DHCP4 {
					t.Error("no address given, but DHCP is off")
				}
				return
			}
			if !slices.Equal(eth.Addresses, []string{"192.0.2.50/24"}) || len(eth.Routes) != 1 ||
				eth.Routes[0].Via != "192.0.2.1" || !slices.Equal(eth.Nameservers.Addresses, []string{"192.0.2.53"}) {
				t.Errorf("static config %+v", eth)
			}
		})
	}
}

func TestMACAddress(t *testing.T) {
	t.Parallel()
	got, err := macAddress("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1")
	if err != nil || got != "bc:24:11:aa:bb:cc" {
		t.Errorf("macAddress() = %q, %v", got, err)
	}
	if _, err := macAddress("bridge=vmbr0"); err == nil {
		t.Error("found a MAC address where there is none")
	}
}
