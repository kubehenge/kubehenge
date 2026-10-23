//go:build e2e

// End-to-end test of the templates on a Proxmox VE host: each is created, found again instead of
// being built twice, cloned and booted the way Cluster API's Proxmox VE provider does it (a full
// clone with a NoCloud ISO), checked from inside, and deleted, leaving nothing behind.
//
//	go test -tags e2e -timeout 2h ./pkg/templates/
//
// Settings come from the environment: KUBEHENGE_PROXMOX_URL, KUBEHENGE_PROXMOX_TOKEN and optionally
// KUBEHENGE_PROXMOX_CA; KUBEHENGE_E2E_NODE, _STORAGE, _IMPORT_STORAGE, _POOL, _BRIDGE; and, where the
// bridge has no DHCP, KUBEHENGE_E2E_ADDRESS (with prefix), _GATEWAY and _DNS (comma-separated) for the
// VMs that bake and boot images. KUBEHENGE_E2E_KUBERNETES_VERSION picks the kubeadm templates' version.
// Talos is made, and kubeadm, whose template carries the most, on every node OS available; one whose
// CPU level no node of the host reaches is skipped, saying so. RKE2's and k3s's templates are made
// and booted by the workload cluster tests.

package templates

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func env(t *testing.T, name, fallback string) string {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		return v
	}
	if fallback == "" {
		t.Fatalf("%s must be set", name)
	}
	return fallback
}

func e2eBuilder(t *testing.T) *Builder {
	t.Helper()
	client, err := pve.Connect(pve.Config{
		URL:    env(t, "KUBEHENGE_PROXMOX_URL", ""),
		Token:  env(t, "KUBEHENGE_PROXMOX_TOKEN", ""),
		CAFile: os.Getenv("KUBEHENGE_PROXMOX_CA"),
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &Builder{
		Client:        client,
		Sources:       DefaultSources(),
		Node:          env(t, "KUBEHENGE_E2E_NODE", ""),
		Storage:       env(t, "KUBEHENGE_E2E_STORAGE", "local-lvm"),
		ImportStorage: env(t, "KUBEHENGE_E2E_IMPORT_STORAGE", "local"),
		Pool:          os.Getenv("KUBEHENGE_E2E_POOL"),
		Bridge:        env(t, "KUBEHENGE_E2E_BRIDGE", "vmbr0"),
		Log:           t.Logf,
	}
	if a := os.Getenv("KUBEHENGE_E2E_ADDRESS"); a != "" {
		n := &Network{Address: netip.MustParsePrefix(a), Gateway: netip.MustParseAddr(env(t, "KUBEHENGE_E2E_GATEWAY", ""))}
		for d := range strings.SplitSeq(env(t, "KUBEHENGE_E2E_DNS", ""), ",") {
			n.DNS = append(n.DNS, netip.MustParseAddr(strings.TrimSpace(d)))
		}
		b.Network = n
	}
	return b
}

// The templates are built one after another: each bake is a VM of its own, and the Proxmox VE host
// the test runs against need not have room for several at once.
//
//nolint:paralleltest // serial on purpose, as above
func TestTemplatesEndToEnd(t *testing.T) {
	b := e2eBuilder(t)
	kubernetes := env(t, "KUBEHENGE_E2E_KUBERNETES_VERSION", "v1.37.1")
	nodeOSes := AvailableNodeOSes()
	requests := append(make([]Request, 0, 1+len(nodeOSes)), Request{Distribution: Talos})
	for _, o := range nodeOSes {
		nodeOS, err := ParseNodeOS(o)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, Request{Distribution: Kubeadm, NodeOS: nodeOS, KubernetesVersion: kubernetes})
	}
	for _, req := range requests {
		name := string(req.Distribution)
		if req.Distribution != Talos {
			name += "/" + req.NodeOS.String()
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tmpl, err := Plan(&req)
			if err != nil {
				t.Fatal(err)
			}
			if tmpl.Bake != nil {
				if err = b.checkCPU(ctx, tmpl.Bake.NodeOS); err != nil {
					t.Skip(err)
				}
			}
			// A node OS template is made as a build of its line, as clusters select it.
			if tmpl.Refreshed() {
				if tmpl, err = tmpl.Build(time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = b.Delete(ctx, &tmpl); err != nil {
				t.Fatal(err)
			}

			vmid, err := b.Create(ctx, &tmpl)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, derr := b.Delete(context.Background(), &tmpl); derr != nil {
					t.Errorf("deleting the template: %v", derr)
				}
				assertNoLeftovers(t, b)
			})
			assertTemplateShape(t, b, vmid, &tmpl)

			again, err := b.Create(ctx, &tmpl)
			if err != nil || again != vmid {
				t.Fatalf("a second create gave VM %d, %v; want the existing %d", again, err, vmid)
			}

			if tmpl.Bake != nil {
				bootClone(t, b, vmid, *tmpl.Bake)
			}
		})
	}
}

func assertTemplateShape(t *testing.T, b *Builder, vmid int, tmpl *Template) {
	t.Helper()
	node, err := b.Client.Node(context.Background(), b.Node)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(context.Background(), vmid)
	if err != nil {
		t.Fatal(err)
	}
	c := vm.VirtualMachineConfig
	if !bool(c.Template) {
		t.Error("not a template")
	}
	if !slices.Equal(ParseTags(c.Tags), tmpl.Tags) {
		t.Errorf("tags %q, want %v", c.Tags, tmpl.Tags)
	}
	if c.Boot != "order=scsi0" {
		t.Errorf("boot %q, want order=scsi0: capmox appends its ISO to it", c.Boot)
	}
	if len(c.Nets) != 0 || len(c.IDEs) != 0 {
		t.Errorf("template has NICs %v or IDE devices %v; capmox adds its own", c.Nets, c.IDEs)
	}
	if c.Cores == nil || *c.Cores != templateCores || int(c.Memory) != templateMemoryMB {
		t.Errorf("template has %v cores and %d MiB, want %d and %d", c.Cores, c.Memory, templateCores, templateMemoryMB)
	}
	if !strings.HasPrefix(c.Agent, "enabled=1") && c.Agent != "1" {
		t.Errorf("guest agent %q, want enabled", c.Agent)
	}
}

// bootClone does what Cluster API's Proxmox VE provider does with a template and checks the clone
// from inside: cloud-init runs afresh and finishes without warnings, and the clone is not a copy of
// the bake VM's identity.
func bootClone(t *testing.T, b *Builder, template int, bake Bake) {
	t.Helper()
	ctx := context.Background()
	node, err := b.Client.Node(ctx, b.Node)
	if err != nil {
		t.Fatal(err)
	}
	tvm, err := node.VirtualMachine(ctx, template)
	if err != nil {
		t.Fatal(err)
	}
	cloneID, task, err := tvm.Clone(ctx, &proxmox.VirtualMachineCloneOptions{Full: true, Name: "kubehenge-e2e-clone", Storage: b.Storage})
	if err = waitTask(ctx, task, err, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	vm, err := node.VirtualMachine(ctx, cloneID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.discard(context.Background(), node, vm) })

	task, err = vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: "net0", Value: "virtio,bridge=" + b.Bridge},
		proxmox.VirtualMachineOption{Name: "cores", Value: 2},
		proxmox.VirtualMachineOption{Name: "memory", Value: 2048},
	)
	if err = waitTask(ctx, task, err, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	// As Cluster API's Proxmox VE provider gives a machine the disk its class asks.
	task, err = vm.ResizeDisk(ctx, "scsi0", "+8G")
	if err = waitTask(ctx, task, err, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if vm, err = node.VirtualMachine(ctx, cloneID); err != nil {
		t.Fatal(err)
	}
	mac, err := macAddress(vm.VirtualMachineConfig.Nets["net0"])
	if err != nil {
		t.Fatal(err)
	}
	iso, err := b.isoStorage(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	userData := "#cloud-config\nwrite_files:\n  - path: /run/kubehenge-e2e\n    content: clone\n"
	metaData := fmt.Sprintf("instance-id: kubehenge-e2e-%d\nlocal-hostname: kubehenge-e2e-%d\n", cloneID, cloneID)
	seed := map[string]string{"user-data": userData, "meta-data": metaData, "network-config": NetworkConfig(mac, b.Network)}
	if err = attachSeed(ctx, node, iso, vm, "ide0", seed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s, serr := node.Storage(context.Background(), iso); serr == nil {
			_ = deleteVolume(context.Background(), s, fmt.Sprintf("%s:iso/"+proxmox.UserDataISOFormat, iso, cloneID))
		}
	})

	task, err = vm.Start(ctx)
	if err = waitTask(ctx, task, err, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = vm.WaitForAgent(ctx, 600); err != nil {
		t.Fatal(err)
	}
	// The node's own checks first, cloud-init's at their head: it finished without warnings.
	checks := append(nodeChecks(bake), [][]string{
		{"test", "-s", "/run/kubehenge-e2e"},
		{"sh", "-c", fmt.Sprintf("test \"$(hostname)\" = kubehenge-e2e-%d", cloneID)},
		// The bake's boots left none of their identity: this one is the clone's own.
		{"sh", "-c", fmt.Sprintf("test \"$(cat /var/lib/cloud/data/instance-id)\" = kubehenge-e2e-%d", cloneID)},
		{"sh", "-c", "test -s /etc/machine-id && ! grep -q uninitialized /etc/machine-id"},
		{"sh", "-c", fmt.Sprintf(`. /etc/os-release && test "${ID}" = %q`, osReleaseID[bake.NodeOS.Family])},
		// The root file system grew to the disk the clone was given, beyond the template's.
		{"sh", "-c", `root=$(findmnt -n -o TARGET /sysroot || echo /); disk=$(lsblk -dbno SIZE /dev/sda); fs=$(df -B1 --output=size "${root}" | tail -n 1); test $((fs * 10)) -gt $((disk * 8))`},
	}...)
	for _, c := range checks {
		if err := b.exec(ctx, vm, c, 20*time.Minute); err != nil {
			t.Error(err)
		}
	}
}

// osReleaseID is each family's ID in /etc/os-release.
var osReleaseID = map[string]string{almaLinuxName: "almalinux", rockyName: "rocky", leapName: "opensuse-leap"}

func assertNoLeftovers(t *testing.T, b *Builder) {
	t.Helper()
	ctx := context.Background()
	node, err := b.Client.Node(ctx, b.Node)
	if err != nil {
		t.Fatal(err)
	}
	stores := []string{b.ImportStorage}
	if iso, err := b.isoStorage(ctx, node); err == nil {
		stores = append(stores, iso)
	}
	for _, name := range stores {
		s, err := node.Storage(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		content, err := s.GetContent(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range content {
			if strings.Contains(c.Volid, "/template-") || strings.Contains(c.Volid, "/user-data-") {
				t.Errorf("left behind on %s: %s", name, c.Volid)
			}
		}
	}
}
