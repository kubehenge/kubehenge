package provisioner

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubehenge/kubehenge/pkg/redact"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// BootLogRequestAnnotation on a Cluster names the machines, comma-separated, whose boot logs the
// controller is to read into BootLogsConfigMap. It takes each name off once it has written that
// machine's log, so the interface and kubectl alike ask by adding a name and wait for it to go.
const BootLogRequestAnnotation = "kubehenge.dev/boot-log-request"

// BootLogsConfigMap names the ConfigMap in a cluster's namespace holding its machines' boot logs,
// one key per machine, each a BootLog in JSON. The Cluster owns it, so it goes with the cluster.
func BootLogsConfigMap(cluster string) string { return cluster + "-boot-logs" }

const (
	// bootLogLines is how much of each log is read: its end, where a boot stopped.
	bootLogLines = 200
	// bootLogSectionBytes bounds one log's lines, however long each is.
	bootLogSectionBytes = 64 << 10
	// bootLogsBytes keeps the ConfigMap well below the 1 MiB an object may take; the logs read
	// longest ago go first.
	bootLogsBytes = 768 << 10
	// bootLogTimeout bounds the reading of one machine's logs, through its guest agent or its Talos
	// API, so that a VM that does not answer holds up the others asked for no longer.
	bootLogTimeout = 2 * time.Minute
	// cloudInitOutputRead is how much of cloud-init's output is read. The guest agent reads a file
	// from its start, and the output grows by a few dozen KiB a boot, so this holds the end of
	// every log but a node's that rebooted hundreds of times.
	cloudInitOutputMiB  = 8
	cloudInitOutputRead = cloudInitOutputMiB << 20
	cloudInitOutputName = "cloud-init-output.log"
)

// talosServices are the Talos services whose logs say why a node did not join, and
// talosControlPlaneServices those of them that only a control-plane node runs.
var (
	talosServices             = []string{"machined", "apid", kubeletService}
	talosControlPlaneServices = []string{"trustd", "etcd"}
)

const kubeletService = "kubelet"

// BootLog is what a machine's node logged as it booted, as the controller last read it.
type BootLog struct {
	Read time.Time `json:"read"`
	// Error says why the logs could not be read at all.
	Error    string           `json:"error,omitempty"`
	Sections []BootLogSection `json:"sections,omitempty"`
}

// BootLogSection is the end of one log: cloud-init's output, a unit's journal, the kernel's ring
// buffer or a Talos service's log.
type BootLogSection struct {
	Name  string   `json:"name"`
	Lines []string `json:"lines,omitempty"`
	// Error says why this log could not be read.
	Error string `json:"error,omitempty"`
}

// TalosLogs reads a Talos node's kernel messages and its services' logs.
type TalosLogs interface {
	Dmesg(ctx context.Context) ([]string, error)
	ServiceLog(ctx context.Context, service string) ([]string, error)
	Close() error
}

// TalosLogDialer reaches one Talos node's API at its address with a cluster's talosconfig.
type TalosLogDialer func(ctx context.Context, clientConfig []byte, address string) (TalosLogs, error)

// DialTalosLogs is the TalosLogDialer of the Talos API itself.
func DialTalosLogs(ctx context.Context, raw []byte, address string) (TalosLogs, error) { //nolint:ireturn // a TalosLogDialer, which tests stand in for
	n, err := connectTalos(ctx, raw, address)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (n *talosNode) Dmesg(ctx context.Context) ([]string, error) {
	stream, err := n.c.Dmesg(ctx, false, false)
	if err != nil {
		return nil, fmt.Errorf("%s: reading the kernel messages: %w", n.address, err)
	}
	return readTalosStream(stream)
}

func (n *talosNode) ServiceLog(ctx context.Context, service string) ([]string, error) {
	stream, err := n.c.Logs(ctx, constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD, service, false, bootLogLines)
	if err != nil {
		return nil, fmt.Errorf("%s: reading %s's log: %w", n.address, service, err)
	}
	return readTalosStream(stream)
}

func readTalosStream(stream talosclient.MachineStream) ([]string, error) {
	r, err := talosclient.ReadStream(stream)
	if err != nil {
		return nil, fmt.Errorf("reading the stream: %w", err)
	}
	defer func() { _ = r.Close() }()
	return streamTail(r)
}

// streamTail reads the last bootLogLines lines of r that fit in bootLogSectionBytes, with their
// secrets taken out.
func streamTail(r io.Reader) ([]string, error) {
	var lines []string
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), bootLogSectionBytes)
	for scan.Scan() {
		lines = append(lines, logLine(scan.Text()))
		if len(lines) > bootLogLines {
			lines = lines[1:]
		}
	}
	return fitLines(lines), scan.Err()
}

// terminalEscape is an escape sequence that colours or moves the cursor on a terminal, such as
// systemd and the distributions' installers write.
var terminalEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[\x40-\x5a\x5c-\x5f])`)

// logLine is a line of a boot log as the interface and a support bundle show it: its secrets taken
// out, and without terminal escapes and control characters, which mean nothing outside a terminal
// and which YAML refuses.
func logLine(line string) string {
	line = terminalEscape.ReplaceAllString(line, "")
	line = strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	return redact.Line(line)
}

// fitLines keeps the last of the lines that fit in bootLogSectionBytes.
func fitLines(lines []string) []string {
	size := 0
	for i, line := range slices.Backward(lines) {
		size += len(line) + 1
		if size > bootLogSectionBytes {
			return lines[i+1:]
		}
	}
	return lines
}

// BootLogReconciler reads the boot logs of the machines a Cluster asks for into its boot-log
// ConfigMap: through the guest agent of each VM, or through the Talos API of a Talos node.
type BootLogReconciler struct {
	Client client.Client
	// Reader reads the ConfigMap past the cache, which holds the class namespace's alone.
	Reader         client.Reader
	ClassNamespace string
	// Files reads the files nodes keep their boot logs in; nil when the management cluster's
	// setup switched that off.
	Files GuestFiles
	// Talos reaches Talos nodes' APIs.
	Talos TalosLogDialer
	Now   func() time.Time
}

// SetupWithManager registers the reconciler for the Clusters that ask for a boot log.
func (r *BootLogReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetAnnotations()[BootLogRequestAnnotation] != ""
		}))).
		Named("boot-logs").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the boot log reader: %w", err)
	}
	return nil
}

// BootLogsAsked are the machines a cluster asks for the boot logs of.
func BootLogsAsked(c *clusterv1.Cluster) []string {
	var names []string
	for n := range strings.SplitSeq(c.Annotations[BootLogRequestAnnotation], ",") {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// Reconcile implements reconcile.Reconciler.
func (r *BootLogReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	asked := BootLogsAsked(cluster)
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() || len(asked) == 0 {
		return ctrl.Result{}, nil
	}
	logs := make(map[string]BootLog, len(asked))
	for _, name := range asked {
		logs[name] = r.read(ctx, cluster, name)
	}
	if err := r.store(ctx, cluster, logs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.answered(ctx, cluster, asked)
}

// read reads one machine's boot log, or says why it cannot.
func (r *BootLogReconciler) read(ctx context.Context, cluster *clusterv1.Cluster, name string) BootLog {
	log := BootLog{Read: r.now()}
	m := &clusterv1.Machine{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, m)
	switch {
	case apierrors.IsNotFound(err) || err == nil && m.Labels[clusterv1.ClusterNameLabel] != cluster.Name:
		log.Error = "the cluster has no machine " + name
		return log
	case err != nil:
		log.Error = "reading the machine: " + err.Error()
		return log
	}
	ctx, cancel := context.WithTimeout(ctx, bootLogTimeout)
	defer cancel()
	if isTalos(cluster) {
		log.Sections, err = r.readTalos(ctx, cluster, m)
	} else {
		log.Sections, err = r.readGuest(ctx, m)
	}
	if err != nil {
		log.Error = redact.Line(err.Error())
	}
	ctrl.LoggerFrom(ctx).Info("read a machine's boot log", "machine", name, "error", log.Error)
	return log
}

// readGuest reads a VM's boot logs through its QEMU guest agent: cloud-init's output and the copy
// of its journals the node keeps (templates.BootLogFile), files only, which take no more than
// VM.GuestAgent.FileRead.
func (r *BootLogReconciler) readGuest(ctx context.Context, m *clusterv1.Machine) ([]BootLogSection, error) {
	if r.Files == nil {
		return nil, errors.New("reading nodes' boot logs through the guest agent is switched off in the management cluster's setup (node_boot_logs)")
	}
	_, vmid, err := proxmoxMachineOf(ctx, r.Client, m)
	if err != nil {
		return nil, err
	}
	if vmid == 0 {
		return nil, errors.New("the machine has no VM yet")
	}
	running, err := r.Files.Running(ctx, vmid)
	if err != nil {
		return nil, fmt.Errorf("asking whether VM %d runs: %w", vmid, err)
	}
	if !running {
		return nil, fmt.Errorf("VM %d is not running", vmid)
	}
	output, truncated, outputErr := r.Files.ReadFile(ctx, vmid, templates.CloudInitOutput, cloudInitOutputRead)
	journals, _, journalsErr := r.Files.ReadFile(ctx, vmid, templates.BootLogFile, templates.BootLogRead)
	if outputErr != nil && journalsErr != nil {
		if proxmox.IsNotAuthorized(outputErr) {
			return nil, fmt.Errorf("%w: the controller's token needs VM.GuestAgent.FileRead on the workload pool (node_boot_logs)", outputErr)
		}
		return nil, fmt.Errorf("reading the boot logs: %w", outputErr)
	}
	var cloudInit BootLogSection
	if outputErr != nil {
		cloudInit = section(cloudInitOutputName, nil, outputErr)
	} else {
		lines, err := streamTail(strings.NewReader(output))
		cloudInit = section(cloudInitOutputName, lines, err)
		if truncated && err == nil {
			cloudInit.Error = fmt.Sprintf("the file is longer than the %d MiB read; these are the last lines of those", cloudInitOutputMiB)
		}
	}
	sections := []BootLogSection{cloudInit}
	if journalsErr != nil {
		return append(sections, BootLogSection{Name: "journals", Error: redact.Line(journalsErr.Error())}), nil
	}
	return append(sections, parseBootLogs(journals)...), nil
}

// parseBootLogs splits the node's copy of its journals into one log each.
func parseBootLogs(out string) []BootLogSection {
	var sections []BootLogSection
	var lines []string
	flush := func() {
		if n := len(sections); n > 0 {
			sections[n-1].Lines = fitLines(lines)
		}
		lines = nil
	}
	for l := range strings.SplitSeq(out, "\n") {
		if name, ok := strings.CutPrefix(l, templates.BootLogMark); ok {
			flush()
			sections = append(sections, BootLogSection{Name: name})
			continue
		}
		if len(sections) > 0 {
			lines = append(lines, logLine(l))
		}
	}
	flush()
	return sections
}

// readTalos reads a Talos node's kernel messages and its services' logs through its API.
func (r *BootLogReconciler) readTalos(ctx context.Context, cluster *clusterv1.Cluster, m *clusterv1.Machine) ([]BootLogSection, error) {
	if r.Talos == nil {
		return nil, errors.New("this controller does not reach Talos nodes")
	}
	address := machineAddress(m)
	if address == "" {
		return nil, errors.New("the machine has no address yet")
	}
	raw, err := readTalosconfig(ctx, r.Client, cluster.Namespace, cluster.Name)
	if err != nil {
		return nil, err
	}
	node, err := r.Talos(ctx, raw, address)
	if err != nil {
		return nil, err
	}
	defer func() { _ = node.Close() }()
	dmesg, err := node.Dmesg(ctx)
	sections := []BootLogSection{section("kernel messages", dmesg, err)}
	services := talosServices
	if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
		services = append(slices.Clone(services), talosControlPlaneServices...)
	}
	for _, s := range services {
		lines, err := node.ServiceLog(ctx, s)
		sections = append(sections, section(s, lines, err))
	}
	return sections, nil
}

func section(name string, lines []string, err error) BootLogSection {
	s := BootLogSection{Name: name, Lines: fitLines(lines)}
	if err != nil {
		s.Error = redact.Line(err.Error())
	}
	return s
}

// store writes the logs read into the cluster's boot-log ConfigMap, beside those of other machines
// read before, the oldest of which go when they would not all fit.
func (r *BootLogReconciler) store(ctx context.Context, cluster *clusterv1.Cluster, logs map[string]BootLog) error {
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: BootLogsConfigMap(cluster.Name)}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		err := r.Reader.Get(ctx, key, cm)
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return fmt.Errorf("reading the boot logs: %w", err)
		}
		if create {
			cm = &corev1.ConfigMap{
				Namespace: key.Namespace, Name: key.Name,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: clusterv1.ClusterKind, Name: cluster.Name, UID: cluster.UID,
				}},
			}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		for name, log := range logs {
			raw, err := json.Marshal(log)
			if err != nil {
				return fmt.Errorf("recording the boot log of %s: %w", name, err)
			}
			cm.Data[name] = string(raw)
		}
		trimBootLogs(cm.Data)
		if create {
			return r.Client.Create(ctx, cm)
		}
		return r.Client.Update(ctx, cm)
	})
	if err != nil {
		return fmt.Errorf("writing the boot logs of cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

// trimBootLogs takes the logs read longest ago out until the rest fit in bootLogsBytes.
func trimBootLogs(data map[string]string) {
	size := 0
	read := map[string]time.Time{}
	for name, raw := range data {
		size += len(name) + len(raw)
		var log BootLog
		_ = json.Unmarshal([]byte(raw), &log)
		read[name] = log.Read
	}
	names := slices.SortedFunc(maps.Keys(data), func(a, b string) int {
		return cmp.Or(read[a].Compare(read[b]), strings.Compare(a, b))
	})
	for _, n := range names {
		if size <= bootLogsBytes || len(data) == 1 {
			return
		}
		size -= len(n) + len(data[n])
		delete(data, n)
	}
}

// answered takes the machines read off the cluster's request, leaving any asked for meanwhile.
func (r *BootLogReconciler) answered(ctx context.Context, cluster *clusterv1.Cluster, read []string) error {
	key := client.ObjectKeyFromObject(cluster)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &clusterv1.Cluster{}
		if err := r.Client.Get(ctx, key, c); err != nil {
			return client.IgnoreNotFound(err)
		}
		left := slices.DeleteFunc(BootLogsAsked(c), func(n string) bool { return slices.Contains(read, n) })
		patch := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if len(left) == 0 {
			delete(c.Annotations, BootLogRequestAnnotation)
		} else {
			c.Annotations[BootLogRequestAnnotation] = strings.Join(left, ",")
		}
		return r.Client.Patch(ctx, c, patch)
	})
	if err != nil {
		return fmt.Errorf("taking the boot log request off cluster %s: %w", key, err)
	}
	return nil
}

func (r *BootLogReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
