package templates

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// Builder creates templates on one Proxmox VE node through its API, without SSH.
type Builder struct {
	Client  *proxmox.Client
	Sources *Sources
	Node    string
	// ImportStorage is a file storage with the `import` content type, where images are downloaded.
	ImportStorage string
	// Storage holds the templates' disks; on a Proxmox VE cluster it should be shared, so clones can
	// start on any node.
	Storage string
	// ISOStorage receives the cloud-init ISO of a bake; empty means the node's first ISO storage.
	ISOStorage string
	Pool       string
	// Bridge, VLAN and Network connect a VM being baked, which needs to reach package mirrors.
	Bridge  string
	VLAN    int
	Network *Network
	Log     func(format string, args ...any)
	// KeepOnFailure leaves a failed bake's VM in place for inspection.
	KeepOnFailure bool
}

// Network is a static address for a VM being baked; without it the VM asks DHCP.
type Network struct {
	Address netip.Prefix
	Gateway netip.Addr
	DNS     []netip.Addr
}

// ErrNoTemplate is Find's answer when no template carries the tags.
var ErrNoTemplate = errors.New("no template carries these tags")

const (
	cpuType = "x86-64-v2-AES"
	// Templates are deliberately small: a clone that already matches its machine's size never
	// boots under Cluster API's Proxmox VE provider (its issue 848), and every clone is resized anyway.
	templateCores    = 1
	templateMemoryMB = 512
	bakeCores        = 2
	bakeMemoryMB     = 2048
	bakeDevice       = "ide2"
)

// How long each Proxmox VE task or guest command may take.
const (
	configTimeout     = 2 * time.Minute  // a VM configuration change
	shortTaskTimeout  = 5 * time.Minute  // start, stop, disk resize, volume removal
	longTaskTimeout   = 10 * time.Minute // conversion to a template, shutdown, deletion, clean-up steps
	transferTimeout   = 30 * time.Minute // downloading an image, importing it as a disk
	bakeTimeout       = 45 * time.Minute // installing updates and packages in the guest
	selectableTimeout = 2 * time.Minute  // a new template showing in the cluster's resources
)

// Most tasks, such as a configuration change, end within a fraction of a second; a download or a
// disk import takes minutes. So polling asks again soon at first and less often the longer one runs.
const (
	pollFirst   = 100 * time.Millisecond
	pollLongest = 2 * time.Second
	pollGrowth  = 2
)

func backoff(interval time.Duration) time.Duration {
	return min(pollGrowth*interval, pollLongest)
}

// bakeDiskSize is the disk a baked template gets. Ubuntu's cloud image is about 3.5 GB, too small
// for its own updates; cloud-init grows the file system to the disk on first boot. Clones can only
// grow it further, so it stays modest.
func bakeDiskSize(b Bake) string {
	if b.KubernetesVersion != "" {
		return "12G" // room for the control-plane images pulled ahead
	}
	return "8G"
}

// Find returns the template in the builder's pool whose tags are exactly t's, or ErrNoTemplate.
func (b *Builder) Find(ctx context.Context, t *Template) (*proxmox.ClusterResource, error) {
	resources, err := b.vms(ctx)
	if err != nil {
		return nil, err
	}
	var found []*proxmox.ClusterResource
	for _, r := range resources {
		if r.Template == 1 && b.inPool(r) && slices.Equal(ParseTags(r.Tags), t.Tags) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return nil, ErrNoTemplate
	case 1:
		return found[0], nil
	default:
		return nil, fmt.Errorf("%d templates carry the tags %s; Cluster API needs exactly one", len(found), strings.Join(t.Tags, ";"))
	}
}

// List returns every Kubehenge template in the builder's pool.
func (b *Builder) List(ctx context.Context) ([]*proxmox.ClusterResource, error) {
	resources, err := b.vms(ctx)
	if err != nil {
		return nil, err
	}
	var out []*proxmox.ClusterResource
	for _, r := range resources {
		if r.Template == 1 && b.inPool(r) && slices.Contains(ParseTags(r.Tags), kubehengeTag) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y *proxmox.ClusterResource) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// Delete removes the template with t's tags, if there is one, and returns its VM ID or 0.
func (b *Builder) Delete(ctx context.Context, t *Template) (int, error) {
	existing, err := b.Find(ctx, t)
	if errors.Is(err, ErrNoTemplate) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := vmID(existing)
	if err != nil {
		return 0, err
	}
	vm, err := b.vm(ctx, existing.Node, id)
	if err != nil {
		return 0, err
	}
	task, err := vm.Delete(ctx, &proxmox.VirtualMachineDeleteOptions{Purge: true, DestroyUnreferencedDisks: true})
	err = waitTask(ctx, task, err, longTaskTimeout)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// vmID reads a VM ID from the cluster's resource list, where it is unsigned.
func vmID(r *proxmox.ClusterResource) (int, error) {
	const largest = 999999999 // Proxmox VE's own upper bound
	if r.VMID > largest {
		return 0, fmt.Errorf("VM ID %d is outside Proxmox VE's range", r.VMID)
	}
	return int(r.VMID), nil
}

// Create makes the template unless one with its tags exists, and returns its VM ID.
func (b *Builder) Create(ctx context.Context, t *Template) (_ int, err error) {
	existing, err := b.Find(ctx, t)
	switch {
	case err == nil:
		b.logf("template %s already exists as VM %d", strings.Join(t.Tags, ";"), existing.VMID)
		return vmID(existing)
	case !errors.Is(err, ErrNoTemplate):
		return 0, err
	}
	err = b.checkNotInProgress(ctx, t)
	if err != nil {
		return 0, err
	}

	image, err := b.Sources.Resolve(ctx, t.Image)
	if err != nil {
		return 0, err
	}
	node, err := b.Client.Node(ctx, b.Node)
	if err != nil {
		return 0, fmt.Errorf("reading node %s: %w", b.Node, err)
	}
	importStorage, err := node.Storage(ctx, b.ImportStorage)
	if err != nil {
		return 0, fmt.Errorf("reading storage %s: %w", b.ImportStorage, err)
	}
	volume := fmt.Sprintf("%s:import/%s", b.ImportStorage, image.FileName)
	err = b.download(ctx, importStorage, image, volume)
	if err != nil {
		return 0, err
	}
	// The template owns a copy of the disk, so the download is only needed while creating it.
	defer func() {
		if derr := deleteVolume(context.WithoutCancel(ctx), importStorage, volume); derr != nil {
			b.logf("could not remove %s: %v", volume, derr)
		}
	}()

	cluster, err := b.Client.Cluster(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the Proxmox VE cluster: %w", err)
	}
	vmid, err := cluster.NextID(ctx)
	if err != nil {
		return 0, fmt.Errorf("choosing a VM ID: %w", err)
	}
	b.logf("creating VM %d %s from %s", vmid, t.Name, image.URL)
	err = b.createVM(ctx, node, vmid, t, volume)
	if err != nil {
		return 0, err
	}
	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return 0, fmt.Errorf("reading VM %d: %w", vmid, err)
	}
	defer func() {
		if err != nil && !b.KeepOnFailure {
			b.discard(context.WithoutCancel(ctx), node, vm)
		}
	}()
	err = b.finish(ctx, node, vm, vmid, t)
	if err != nil {
		return 0, err
	}
	b.logf("template %s is VM %d", strings.Join(t.Tags, ";"), vmid)
	return vmid, nil
}

// inPool tells whether a VM is in the builder's pool, any VM when it has none. The management
// cluster's Proxmox VE provider may clone only from its template pool, so a template elsewhere with
// the same tags neither stands in for one there nor competes with it.
func (b *Builder) inPool(r *proxmox.ClusterResource) bool {
	return b.Pool == "" || r.Pool == b.Pool
}

// finish bakes the new VM if the template asks for it, gives it its final tags and size, and turns
// it into the template.
func (b *Builder) finish(ctx context.Context, node *proxmox.Node, vm *proxmox.VirtualMachine, vmid int, t *Template) error {
	if t.Bake != nil {
		if err := b.bake(ctx, node, vm, *t.Bake); err != nil {
			return fmt.Errorf("baking %s: %w", t.Name, err)
		}
	}
	task, err := vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: "tags", Value: strings.Join(t.Tags, ";")},
		proxmox.VirtualMachineOption{Name: "cores", Value: templateCores},
		proxmox.VirtualMachineOption{Name: "memory", Value: templateMemoryMB},
		proxmox.VirtualMachineOption{Name: "boot", Value: "order=scsi0"},
	)
	err = waitTask(ctx, task, err, configTimeout)
	if err != nil {
		return err
	}
	task, err = vm.ConvertToTemplate(ctx)
	err = waitTask(ctx, task, err, longTaskTimeout)
	if err != nil {
		return err
	}
	return b.waitSelectable(ctx, t, vmid)
}

// vms lists the cluster's VMs and templates.
func (b *Builder) vms(ctx context.Context) (proxmox.ClusterResources, error) {
	cluster, err := b.Client.Cluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the Proxmox VE cluster: %w", err)
	}
	resources, err := cluster.Resources(ctx, "vm")
	if err != nil {
		return nil, fmt.Errorf("listing the cluster's VMs: %w", err)
	}
	return resources, nil
}

func (b *Builder) vm(ctx context.Context, nodeName string, id int) (*proxmox.VirtualMachine, error) {
	node, err := b.Client.Node(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("reading node %s: %w", nodeName, err)
	}
	vm, err := node.VirtualMachine(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading VM %d: %w", id, err)
	}
	return vm, nil
}

// checkNotInProgress refuses to start while a VM of the template's name exists that is not yet a
// template: another run is building it, or a failed one left it for inspection. Two builds would
// end in two templates with the same tags, which Cluster API cannot choose between.
func (b *Builder) checkNotInProgress(ctx context.Context, t *Template) error {
	resources, err := b.vms(ctx)
	if err != nil {
		return err
	}
	for _, r := range resources {
		if r.Name == t.Name && r.Template != 1 {
			return fmt.Errorf("VM %d on %s is already named %s: another run is building it, or a failed run left it; wait, or remove it", r.VMID, r.Node, t.Name)
		}
	}
	return nil
}

// waitSelectable waits until the cluster's resource view, which Cluster API selects templates
// from and Proxmox VE refreshes every few seconds, shows the new template.
func (b *Builder) waitSelectable(ctx context.Context, t *Template, vmid int) error {
	deadline := time.Now().Add(selectableTimeout)
	for {
		found, err := b.Find(ctx, t)
		if err != nil && !errors.Is(err, ErrNoTemplate) {
			return err
		}
		if err == nil {
			if id, idErr := vmID(found); idErr == nil && id == vmid {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("template VM %d does not show in the cluster's resources", vmid)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for template VM %d to show: %w", vmid, ctx.Err())
		case <-time.After(pollLongest):
		}
	}
}

func (b *Builder) download(ctx context.Context, storage *proxmox.Storage, image Resolved, volume string) error {
	// A file of the same name is a leftover from an interrupted run. Talos images carry no checksum
	// to compare it against, so it is fetched again rather than trusted.
	if err := deleteVolume(ctx, storage, volume); err != nil {
		return err
	}
	b.logf("downloading %s", image.URL)
	var task *proxmox.Task
	var err error
	if image.Checksum == "" {
		task, err = storage.DownloadURL(ctx, "import", image.FileName, image.URL)
	} else {
		task, err = storage.DownloadURLWithHash(ctx, "import", image.FileName, image.URL, image.Checksum, image.ChecksumAlgorithm)
	}
	return waitTask(ctx, task, err, transferTimeout)
}

func (b *Builder) createVM(ctx context.Context, node *proxmox.Node, vmid int, t *Template, volume string) error {
	cores, memory := templateCores, templateMemoryMB
	if t.Bake != nil {
		cores, memory = bakeCores, bakeMemoryMB
	}
	options := []proxmox.VirtualMachineOption{
		{Name: "name", Value: t.Name},
		{Name: "description", Value: "Kubehenge template. Created by `kubehenge template create`; Cluster API selects it by its tags."},
		// Not the final tags: a VM that fails half way must not look like the template.
		{Name: "tags", Value: "kubehenge;kubehenge-building"},
		{Name: "ostype", Value: "l26"},
		{Name: "machine", Value: "q35"},
		{Name: "cpu", Value: cpuType},
		{Name: "cores", Value: cores},
		{Name: "memory", Value: memory},
		{Name: "scsihw", Value: "virtio-scsi-pci"},
		{Name: "scsi0", Value: fmt.Sprintf("%s:0,import-from=%s,discard=on,ssd=1", b.Storage, volume)},
		{Name: "boot", Value: "order=scsi0"},
		{Name: "serial0", Value: "socket"},
		{Name: "vga", Value: "serial0"},
		{Name: "agent", Value: "enabled=1"},
	}
	if b.Pool != "" {
		options = append(options, proxmox.VirtualMachineOption{Name: "pool", Value: b.Pool})
	}
	if t.Bake != nil {
		net := "virtio,bridge=" + b.Bridge
		if b.VLAN > 0 {
			net += fmt.Sprintf(",tag=%d", b.VLAN)
		}
		options = append(options, proxmox.VirtualMachineOption{Name: "net0", Value: net})
	}
	task, err := node.NewVirtualMachine(ctx, vmid, options...)
	return waitTask(ctx, task, err, transferTimeout)
}

func (b *Builder) bake(ctx context.Context, node *proxmox.Node, vm *proxmox.VirtualMachine, bake Bake) error {
	isoStorage, err := b.attachCloudInit(ctx, node, vm, bake)
	if err != nil {
		return err
	}
	defer func() {
		iso := fmt.Sprintf("iso/"+proxmox.UserDataISOFormat, vm.VMID)
		if storage, serr := node.Storage(context.WithoutCancel(ctx), isoStorage); serr == nil {
			if derr := deleteVolume(context.WithoutCancel(ctx), storage, isoStorage+":"+iso); derr != nil {
				b.logf("could not remove the cloud-init ISO: %v", derr)
			}
		}
	}()

	task, err := vm.Start(ctx)
	err = waitTask(ctx, task, err, shortTaskTimeout)
	if err != nil {
		return err
	}
	b.logf("VM %d is installing updates; waiting for its guest agent", vm.VMID)
	if err = vm.WaitForAgent(ctx, int(bakeTimeout.Seconds())); err != nil {
		return fmt.Errorf("the guest agent did not start: %w", err)
	}
	err = b.check(ctx, vm, bake)
	if err != nil {
		return err
	}
	for _, step := range BakeCleanup {
		if err := b.exec(ctx, vm, step, longTaskTimeout); err != nil {
			return err
		}
	}
	return detach(ctx, vm)
}

// attachCloudInit grows the VM's disk for the bake and gives it the bake's cloud-init ISO, on the
// storage it returns.
func (b *Builder) attachCloudInit(ctx context.Context, node *proxmox.Node, vm *proxmox.VirtualMachine, bake Bake) (string, error) {
	key := ""
	if bake.KubernetesVersion != "" {
		var err error
		if key, err = b.Sources.KubernetesPackagesKey(ctx, minorSeries(bake.KubernetesVersion)); err != nil {
			return "", err
		}
	}
	userData, err := BakeUserData(bake, key)
	if err != nil {
		return "", err
	}
	mac, err := macAddress(vm.VirtualMachineConfig.Nets["net0"])
	if err != nil {
		return "", err
	}
	isoStorage, err := b.isoStorage(ctx, node)
	if err != nil {
		return "", err
	}
	task, err := vm.ResizeDisk(ctx, "scsi0", bakeDiskSize(bake))
	if err = waitTask(ctx, task, err, shortTaskTimeout); err != nil {
		return "", fmt.Errorf("growing the disk: %w", err)
	}
	metaData := fmt.Sprintf("instance-id: kubehenge-bake-%d\nlocal-hostname: kubehenge-bake-%d\n", vm.VMID, vm.VMID)
	if err = vm.CloudInit(ctx, bakeDevice, userData, metaData, "", NetworkConfig(mac, b.Network), proxmox.WithCloudInitStorage(isoStorage)); err != nil {
		return "", fmt.Errorf("attaching the cloud-init ISO: %w", err)
	}
	return isoStorage, nil
}

// check runs the bake's checks in the guest.
func (b *Builder) check(ctx context.Context, vm *proxmox.VirtualMachine, bake Bake) error {
	for _, check := range BakeChecks(bake) {
		err := b.exec(ctx, vm, check, bakeTimeout)
		if err == nil {
			continue
		}
		if check[0] == cloudInit {
			// cloud-init's own account of what failed, which its exit code does not carry.
			if out, lerr := b.output(ctx, vm, []string{cloudInit, "status", "--long"}); lerr == nil {
				return fmt.Errorf("%w\n%s", err, out)
			}
		}
		return err
	}
	return nil
}

// detach shuts the baked VM down and removes the cloud-init ISO's drive and the network it
// needed for the bake.
func detach(ctx context.Context, vm *proxmox.VirtualMachine) error {
	task, err := vm.Shutdown(ctx)
	err = waitTask(ctx, task, err, longTaskTimeout)
	if err != nil {
		return err
	}
	if err = vm.Ping(ctx); err != nil {
		return fmt.Errorf("reading VM %d after shutdown: %w", vm.VMID, err)
	}
	if !vm.IsStopped() {
		return fmt.Errorf("VM %d did not stop after shutdown", vm.VMID)
	}
	task, err = vm.Config(ctx, proxmox.VirtualMachineOption{Name: "delete", Value: bakeDevice + ",net0"})
	return waitTask(ctx, task, err, configTimeout)
}

func (b *Builder) exec(ctx context.Context, vm *proxmox.VirtualMachine, command []string, timeout time.Duration) error {
	status, err := b.run(ctx, vm, command, timeout)
	if err != nil {
		return fmt.Errorf("running %q: %w", strings.Join(command, " "), err)
	}
	if status.ExitCode != 0 {
		return fmt.Errorf("%q exited %d: %s", strings.Join(command, " "), status.ExitCode, status.output())
	}
	return nil
}

// output runs a command in the VM and returns what it printed, whatever its exit code.
func (b *Builder) output(ctx context.Context, vm *proxmox.VirtualMachine, command []string) (string, error) {
	status, err := b.run(ctx, vm, command, time.Minute)
	if err != nil {
		return "", err
	}
	return status.output(), nil
}

// execStatus is the guest agent's report on a command. go-proxmox's own type declares
// err-truncated a bool, which Proxmox VE sends as a number, so commands with long error output fail
// to decode there.
type execStatus struct {
	Exited       int    `json:"exited"`
	ExitCode     int    `json:"exitcode"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated any    `json:"out-truncated"`
	ErrTruncated any    `json:"err-truncated"`
}

func (s *execStatus) output() string {
	return strings.TrimSpace(s.OutData + "\n" + s.ErrData)
}

func (b *Builder) run(ctx context.Context, vm *proxmox.VirtualMachine, command []string, timeout time.Duration) (*execStatus, error) {
	pid, err := vm.AgentExec(ctx, command, "")
	if err != nil {
		return nil, fmt.Errorf("starting it through the guest agent: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for interval := pollFirst; ; interval = backoff(interval) {
		var status execStatus
		path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status?pid=%d", vm.Node, vm.VMID, pid)
		if err := b.Client.Get(ctx, path, &status); err != nil {
			return nil, fmt.Errorf("reading its status from the guest agent: %w", err)
		}
		if status.Exited != 0 {
			return &status, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("still running after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for it to end: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (b *Builder) isoStorage(ctx context.Context, node *proxmox.Node) (string, error) {
	if b.ISOStorage != "" {
		return b.ISOStorage, nil
	}
	s, err := node.StorageISO(ctx)
	if err != nil {
		return "", fmt.Errorf("no storage on %s takes ISO images: %w", node.Name, err)
	}
	return s.Name, nil
}

// deleteVolume removes a volume if it exists.
func deleteVolume(ctx context.Context, storage *proxmox.Storage, volume string) error {
	content, err := storage.GetContent(ctx)
	if err != nil {
		return fmt.Errorf("listing storage %s: %w", storage.Name, err)
	}
	if !slices.ContainsFunc(content, func(c *proxmox.StorageContent) bool { return c.Volid == volume }) {
		return nil
	}
	task, err := storage.DeleteContent(ctx, volume)
	return waitTask(ctx, task, err, shortTaskTimeout)
}

func (b *Builder) discard(ctx context.Context, node *proxmox.Node, vm *proxmox.VirtualMachine) {
	b.logf("removing VM %d", vm.VMID)
	if err := vm.Ping(ctx); err == nil && !vm.IsStopped() {
		task, stopErr := vm.Stop(ctx)
		if stopErr = waitTask(ctx, task, stopErr, shortTaskTimeout); stopErr != nil {
			b.logf("could not stop VM %d: %v", vm.VMID, stopErr)
		}
	}
	task, err := vm.Delete(ctx, &proxmox.VirtualMachineDeleteOptions{Purge: true, DestroyUnreferencedDisks: true})
	if err = waitTask(ctx, task, err, shortTaskTimeout); err != nil {
		b.logf("could not remove VM %d from %s: %v", vm.VMID, node.Name, err)
	}
}

func (b *Builder) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log(format, args...)
	}
}

// waitTask waits for a Proxmox VE task started by a call that returned (task, err), and fails if
// the task did; go-proxmox's own Wait reports only that the task ended.
func waitTask(ctx context.Context, task *proxmox.Task, err error, timeout time.Duration) error {
	if err != nil {
		return fmt.Errorf("starting a Proxmox VE task: %w", err)
	}
	if task == nil {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for interval := pollFirst; ; interval = backoff(interval) {
		if err := task.Ping(ctx); err != nil {
			return fmt.Errorf("task %s: %w", task.UPID, err)
		}
		if task.IsCompleted {
			if task.IsFailed {
				return fmt.Errorf("task %s failed: %s", task.UPID, task.ExitStatus)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("task %s: %w", task.UPID, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("task %s: still running after %s", task.UPID, timeout)
		case <-time.After(interval):
		}
	}
}

var macRE = regexp.MustCompile(`(?i)=([0-9a-f]{2}(:[0-9a-f]{2}){5})`)

func macAddress(net0 string) (string, error) {
	m := macRE.FindStringSubmatch(net0)
	if m == nil {
		return "", fmt.Errorf("no MAC address in net0 %q", net0)
	}
	return strings.ToLower(m[1]), nil
}

// NetworkConfig renders the NoCloud network-config (version 2) for a VM being baked.
func NetworkConfig(mac string, n *Network) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "version: 2\nethernets:\n  bake0:\n    match:\n      macaddress: %q\n", mac)
	if n == nil {
		sb.WriteString("    dhcp4: true\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "    addresses: [%q]\n    routes:\n      - to: default\n        via: %s\n", n.Address.String(), n.Gateway)
	if len(n.DNS) > 0 {
		dns := make([]string, len(n.DNS))
		for i, d := range n.DNS {
			dns[i] = d.String()
		}
		fmt.Fprintf(&sb, "    nameservers:\n      addresses: [%s]\n", strings.Join(dns, ", "))
	}
	return sb.String()
}
