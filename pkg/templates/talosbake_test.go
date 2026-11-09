package templates

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

var testTalosPin = TalosPin{
	Imager:        "sha256:" + strings.Repeat("a", 64),
	InstallerBase: "sha256:" + strings.Repeat("b", 64),
	Extensions:    "sha256:" + strings.Repeat("c", 64),
	GuestAgent:    "10.1.2@sha256:" + strings.Repeat("d", 64),
}

func TestTalosProfileIsLiteral(t *testing.T) {
	t.Parallel()
	want := `arch: amd64
platform: nocloud
secureboot: false
version: v1.14.5
input:
  baseInstaller:
    imageRef: ghcr.io/siderolabs/installer-base@sha256:` + strings.Repeat("b", 64) + `
  systemExtensions:
    - imageRef: ghcr.io/siderolabs/qemu-guest-agent:10.1.2@sha256:` + strings.Repeat("d", 64) + `
output:
  kind: image
  outFormat: raw
  imageOptions:
    diskSize: 1306525696
    diskFormat: raw
`
	if got := talosProfile("v1.14.5", testTalosPin); got != want {
		t.Errorf("talosProfile =\n%s\nwant\n%s", got, want)
	}
}

type schematicExtensions struct {
	OfficialExtensions []string `json:"officialExtensions"`
}

type schematicCustomization struct {
	SystemExtensions schematicExtensions `json:"systemExtensions"`
}

// schematicFile is the part of a schematic the extensions check reads.
type schematicFile struct {
	Customization schematicCustomization `json:"customization"`
}

type profileExtension struct {
	ImageRef string `json:"imageRef"`
}

type profileInput struct {
	SystemExtensions []profileExtension `json:"systemExtensions"`
}

// profileFile is the part of an imager profile the extensions check reads.
type profileFile struct {
	Input profileInput `json:"input"`
}

func TestTalosProfileAndSchematicCarryTheSameExtensions(t *testing.T) {
	t.Parallel()
	var schematic schematicFile
	if err := yaml.Unmarshal([]byte(TalosSchematic), &schematic); err != nil {
		t.Fatal(err)
	}
	var profile profileFile
	if err := yaml.Unmarshal([]byte(talosProfile("v1.14.5", testTalosPin)), &profile); err != nil {
		t.Fatal(err)
	}
	baked := make([]string, 0, len(profile.Input.SystemExtensions))
	for _, e := range profile.Input.SystemExtensions {
		repository, _, _ := strings.Cut(strings.TrimPrefix(e.ImageRef, "ghcr.io/"), ":")
		baked = append(baked, repository)
	}
	want := []string{"siderolabs/qemu-guest-agent"}
	if got := schematic.Customization.SystemExtensions.OfficialExtensions; !slices.Equal(got, want) {
		t.Errorf("the schematic holds %v, want %v", got, want)
	}
	if !slices.Equal(baked, want) {
		t.Errorf("the profile holds %v, want %v", baked, want)
	}
}

func TestTalosRecipeWritesTheTemplatesDisk(t *testing.T) {
	t.Parallel()
	f, r, err := Available(DefaultNodeOS.Base())
	if err != nil {
		t.Fatal(err)
	}
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	proxy := &GuestProxy{URL: "http://proxy.example.test:3128", NoProxy: "10.0.0.0/8", CABundle: ca}
	bake := func(release string, p *GuestProxy) Bake {
		return Bake{NodeOS: DefaultNodeOS.Base(), Distribution: Talos, TalosVersion: release, Talos: testTalosPin, Proxy: p}
	}

	b := bake("v1.14.5", proxy)
	recipe, err := f.Recipe(r, &b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Image != nil || recipe.BootLog || recipe.Cleanup != nil || recipe.Log != buildLog || recipe.DiskGiB != 5 ||
		recipe.BuilderDiskGiB != 20 || recipe.MemoryMiB != 4096 || len(recipe.Checks) != 1 || !slices.Equal(recipe.Checks[0], builtCheck) {
		t.Errorf("recipe = %+v", recipe)
	}
	c, files := parseCloudConfig(t, recipe.UserData)
	for name, want := range map[string]string{
		"el/talos-imager":       "ghcr.io/siderolabs/imager@sha256:" + strings.Repeat("a", 64) + "\n",
		"el/talos-profile.yaml": talosProfile("v1.14.5", testTalosPin),
		"proxy/ca.crt":          ca,
	} {
		if files[name] != want {
			t.Errorf("%s = %q, want %q", name, files[name], want)
		}
	}
	for _, name := range []string{"el/talos.sh", "el/build.sh", "proxy/run.sh", "proxy/up.sh", "proxy/down.sh"} {
		if _, ok := files[name]; !ok {
			t.Errorf("the bake writes no %s", name)
		}
	}
	if _, ok := files["el/talos-privileged"]; ok {
		t.Error("v1.14.5's imager is marked privileged")
	}
	last := c.Runcmd[len(c.Runcmd)-1]
	if !strings.Contains(last[len(last)-1], "sh /var/lib/kubehenge-build/proxy/run.sh sh /var/lib/kubehenge-build/el/build.sh talos ") {
		t.Errorf("the build is %q", last[len(last)-1])
	}

	b = bake("v1.12.12", nil)
	recipe, err = f.Recipe(r, &b, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, files = parseCloudConfig(t, recipe.UserData)
	if _, ok := files["el/talos-privileged"]; !ok {
		t.Error("v1.12.12's imager is not marked privileged")
	}
	if _, ok := files["proxy/ca.crt"]; ok {
		t.Error("a bake with no proxy writes a proxy CA")
	}
	last = c.Runcmd[len(c.Runcmd)-1]
	if !strings.Contains(last[len(last)-1], "sh /var/lib/kubehenge-build/el/build.sh talos 2>&1") {
		t.Errorf("the build is %q", last[len(last)-1])
	}
}

func TestTheBuilderHandsTheBakeItsPin(t *testing.T) {
	t.Parallel()
	b := &Builder{Sources: &Sources{TalosPins: map[string]TalosPin{"v1.14.5": testTalosPin}}}
	good, err := Plan(&Request{Distribution: Talos, TalosVersion: "v1.14.5"})
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := b.recipe(t.Context(), good.Bake)
	if err != nil {
		t.Fatal(err)
	}
	_, files := parseCloudConfig(t, recipe.UserData)
	if want := testTalosPin.ImagerRef() + "\n"; files["el/talos-imager"] != want {
		t.Errorf("el/talos-imager = %q, want %q", files["el/talos-imager"], want)
	}
	if good.Bake.Talos != (TalosPin{}) {
		t.Error("the planned bake carries a pin")
	}

	unpinned, err := Plan(&Request{Distribution: Talos, TalosVersion: "v1.14.99"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.recipe(t.Context(), unpinned.Bake)
	if err == nil || !strings.Contains(err.Error(), "v1.14.99") || !strings.Contains(err.Error(), "pins no Talos") || !Final(err) {
		t.Errorf("an unpinned release gave %v, final %v", err, Final(err))
	}
}
