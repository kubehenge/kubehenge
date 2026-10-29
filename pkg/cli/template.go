package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

func newTemplateCommand(conn *pve.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage the VM templates clusters are cloned from",
	}
	cmd.AddCommand(newTemplateCreateCommand(conn), newTemplateListCommand(conn), newTemplateDeleteCommand(conn), newTemplateRefreshCommand(conn))
	return cmd
}

func connect(conn *pve.Config) (*templates.Builder, error) {
	if conn.Token == "" {
		return nil, errors.New("set KUBEHENGE_PROXMOX_TOKEN to a Proxmox VE API token")
	}
	api, err := pve.Connect(*conn)
	if err != nil {
		return nil, fmt.Errorf("connecting to Proxmox VE: %w", err)
	}
	return &templates.Builder{Client: api, Sources: templates.DefaultSources()}, nil
}

// poolFlag is the pool a template command works in; clusters clone only from the templates in their
// management cluster's template pool.
func poolFlag(cmd *cobra.Command, pool *string, usage string) {
	cmd.Flags().StringVar(pool, "pool", "kubehenge-templates", usage)
}

func newTemplateListCommand(conn *pve.Config) *cobra.Command {
	var pool string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Kubehenge's templates in the template pool",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := connect(conn)
			if err != nil {
				return err
			}
			b.Pool = pool
			list, err := b.List(cmd.Context())
			if err != nil {
				return fmt.Errorf("listing the templates: %w", err)
			}
			for _, r := range list {
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%d %s %s %s\n", r.VMID, r.Node, r.Name, strings.Join(templates.ParseTags(r.Tags), ";")); err != nil {
					return fmt.Errorf("writing the list: %w", err)
				}
			}
			return nil
		},
	}
	poolFlag(cmd, &pool, "pool to list the templates of; empty lists every pool's")
	return cmd
}

// nodeOSValue is a node OS on the command line, as one word such as almalinux-10.
type nodeOSValue struct{ o *templates.NodeOS }

func (v nodeOSValue) String() string { return v.o.String() }

func (v nodeOSValue) Set(s string) error {
	o, err := templates.ParseNodeOS(s)
	if err != nil {
		return err //nolint:wrapcheck // the flag's own message
	}
	*v.o = o
	return nil
}

func (nodeOSValue) Type() string { return "node-os" }

// nodeOSFlag adds --node-os, which starts at the default node OS.
func nodeOSFlag(f *pflag.FlagSet, o *templates.NodeOS) {
	*o = templates.DefaultNodeOS
	f.Var(nodeOSValue{o}, "node-os", "operating system of the nodes (rke2, k3s, kubeadm): "+strings.Join(templates.AvailableNodeOSes(), ", "))
}

func newTemplateDeleteCommand(conn *pve.Config) *cobra.Command {
	var (
		req            templates.Request
		pool           string
		build          string
		classNamespace string
		roll           bool
	)
	cmd := &cobra.Command{
		Use:   "delete {talos|rke2|k3s|kubeadm}",
		Short: "Delete the template for a distribution, if it exists",
		Long: `Delete the template for a distribution, if it exists: every build of a node OS template, or
with --build the one build with that tag, as kubehenge template list shows it.

A build a cluster's machines are cloned from is refused, as in the interface, naming the clusters:
deleting it would replace their nodes at once, outside their maintenance windows. Which clusters
those are is read from the management cluster that KUBECONFIG points at. For a build found broken,
--roll-clusters deletes it anyway, and those clusters' nodes are replaced now with clones of the
build before it; the last build of a template is never deleted while a cluster runs it.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"talos", "rke2", "k3s", "kubeadm"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Distribution = templates.Distribution(args[0])
			t, err := templates.Plan(&req)
			if err != nil {
				return fmt.Errorf("the template: %w", err)
			}
			b, err := connect(conn)
			if err != nil {
				return err
			}
			b.Pool = pool
			users := func(list []*proxmox.ClusterResource) (map[string][]string, error) {
				return buildUsers(cmd.Context(), classNamespace, list)
			}
			deleted, err := deleteTemplate(cmd, b, &t, build, deleteChecks{users: users, roll: roll})
			if err != nil {
				return fmt.Errorf("deleting template %s: %w", t.LineName, err)
			}
			if deleted == 0 {
				if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "no template carries the tags %s\n", strings.Join(t.Tags, ";")); err != nil {
					return fmt.Errorf("writing the result: %w", err)
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.TalosVersion, "talos-version", templates.DefaultTalosVersion, "Talos release (talos)")
	nodeOSFlag(f, &req.NodeOS)
	f.StringVar(&req.KubernetesVersion, "kubernetes-version", "", "Kubernetes release (kubeadm)")
	f.StringVar(&build, "build", "", "the build to delete, such as build-202610181200; default every build")
	f.StringVar(&classNamespace, "class-namespace", "kubehenge-system", "namespace of Kubehenge's ClusterClasses in the management cluster")
	f.BoolVar(&roll, "roll-clusters", false, "delete builds clusters run too: their nodes are replaced now, outside their maintenance windows, with clones of the build before")
	poolFlag(cmd, &pool, "pool the template is in; empty looks in every pool")
	return cmd
}

// deleteChecks are what deleteTemplate checks a build against before it deletes it: who clones
// from each build of the pool (provisioner.BuildUsers), and whether builds in use may go.
type deleteChecks struct {
	users func(list []*proxmox.ClusterResource) (map[string][]string, error)
	roll  bool
}

// buildUsers reads who clones from the builds from the management cluster, with the soak its
// refresh is set to.
func buildUsers(ctx context.Context, classNamespace string, list []*proxmox.ClusterResource) (map[string][]string, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("finding which clusters run the builds needs the management cluster's API (KUBECONFIG): %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err = add(scheme); err != nil {
			return nil, fmt.Errorf("the management cluster's scheme: %w", err)
		}
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("the management cluster's API: %w", err)
	}
	soak := provisioner.DefaultNodeImageSoak
	cm := &corev1.ConfigMap{}
	err = kube.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: provisioner.TemplateRefreshName}, cm)
	switch {
	case err == nil:
		schedule, perr := provisioner.ParseRefreshSchedule(cm.Data)
		if perr != nil {
			return nil, fmt.Errorf("reading the template refresh's soak: %w", perr)
		}
		soak = schedule.Soak
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading the template refresh's soak: %w", err)
	}
	users, err := provisioner.BuildUsers(ctx, kube, classNamespace, list, soak)
	if err != nil {
		return nil, fmt.Errorf("finding which clusters run the builds: %w", err)
	}
	return users, nil
}

// refusedBuilds are the builds of a line that may not be deleted, and why: those clusters run,
// unless checks.roll allows it, and with it those that would leave the clusters no build at all.
func refusedBuilds(builds, doomed []templates.Built, checks deleteChecks) ([]string, error) {
	list := make([]*proxmox.ClusterResource, 0, len(builds))
	for _, b := range builds {
		list = append(list, b.Resource)
	}
	users, err := checks.users(list)
	if err != nil {
		return nil, err
	}
	var refused []string
	for _, b := range doomed {
		who := users[strings.Join(templates.NormaliseTags(templates.ParseTags(b.Resource.Tags)), ";")]
		switch {
		case len(who) == 0:
		case len(doomed) == len(builds):
			refused = append(refused, fmt.Sprintf("%s: %s clone from it, and no build of the template would be left for them", b.Resource.Name, strings.Join(who, ", ")))
		case !checks.roll:
			refused = append(refused, fmt.Sprintf("%s: %s clone from it; --roll-clusters deletes it and replaces their nodes now, outside their maintenance windows", b.Resource.Name, strings.Join(who, ", ")))
		}
	}
	return refused, nil
}

// deleteTemplate deletes the templates of a line, or one build of it, and says how many it deleted.
// It deletes nothing when a build it would delete is refused.
func deleteTemplate(cmd *cobra.Command, b *templates.Builder, t *templates.Template, build string, checks deleteChecks) (int, error) {
	if !t.Refreshed() {
		if build != "" {
			return 0, fmt.Errorf("%s has no builds", t.LineName)
		}
		vmid, err := b.Delete(cmd.Context(), t)
		if err != nil {
			return 0, fmt.Errorf("deleting %s: %w", t.Name, err)
		}
		if vmid == 0 {
			return 0, nil
		}
		return 1, reportDeleted(cmd, vmid, t.Name)
	}
	builds, err := b.Builds(cmd.Context(), t)
	if err != nil {
		return 0, fmt.Errorf("listing the builds of %s: %w", t.LineName, err)
	}
	var doomed []templates.Built
	for _, built := range builds {
		if build == "" || templates.BuildTag(built.At) == build {
			doomed = append(doomed, built)
		}
	}
	if len(doomed) == 0 {
		return 0, nil
	}
	refused, err := refusedBuilds(builds, doomed, checks)
	if err != nil {
		return 0, err
	}
	if len(refused) > 0 {
		return 0, fmt.Errorf("refused, nothing deleted:\n  %s", strings.Join(refused, "\n  "))
	}
	deleted := 0
	for _, built := range doomed {
		if err = b.DeleteBuild(cmd.Context(), built.Resource); err != nil {
			return deleted, fmt.Errorf("deleting %s: %w", built.Resource.Name, err)
		}
		deleted++
		if err = reportDeleted(cmd, built.Resource.VMID, built.Resource.Name); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func reportDeleted(cmd *cobra.Command, id any, name string) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "deleted %v %s\n", id, name); err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return nil
}

// builderFlags are the flags that say where and how templates are built.
type builderFlags struct {
	builder templates.Builder
	address string
	gateway string
	dns     []string
}

func (b *builderFlags) add(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&b.builder.Node, "node", "", "Proxmox VE node to create the template on (required)")
	f.StringVar(&b.builder.ImportStorage, "import-storage", "local", "file storage with the import content type, for the downloaded image")
	f.StringVar(&b.builder.Storage, "storage", "local-lvm", "storage for the template's disk; shared storage lets clones start on any node")
	f.StringVar(&b.builder.ISOStorage, "iso-storage", "", "storage for the cloud-init ISO while preparing an image; default the node's first ISO storage")
	poolFlag(cmd, &b.builder.Pool, "pool the template is put in and looked for in; empty looks in every pool")
	f.StringVar(&b.builder.Bridge, "bridge", "vmbr0", "bridge for the VM that prepares an image")
	f.IntVar(&b.builder.VLAN, "vlan", 0, "VLAN tag for the VM that prepares an image")
	f.StringVar(&b.address, "build-address", "", "static address with prefix for the VM that prepares an image, such as 192.0.2.50/24; default DHCP")
	f.StringVar(&b.gateway, "build-gateway", "", "gateway for --build-address")
	f.StringSliceVar(&b.dns, "build-dns", nil, "DNS servers for --build-address")
	f.BoolVar(&b.builder.KeepOnFailure, "keep-on-failure", false, "leave the VM in place when preparing an image fails")
	f.StringVar(&b.builder.CPUType, "cpu-type", "", "QEMU CPU type of the template, which its clones keep; host shows them the node's own CPU (default: that of the newest build of the template, else the least its node OS runs on, "+templates.DefaultCPUType+" for most)")
	_ = cmd.MarkFlagRequired("node")
}

// connect signs the builder in, with its log on the command's standard error.
func (b *builderFlags) connect(cmd *cobra.Command, conn *pve.Config) (*templates.Builder, error) {
	// Cobra's required check passes --node "", which would reach the API as /nodes//.
	if strings.TrimSpace(b.builder.Node) == "" {
		return nil, errors.New("--node needs the Proxmox VE node to build on, such as --node pve1")
	}
	var err error
	switch {
	case b.address != "":
		if b.builder.Network, err = parseNetwork(b.address, b.gateway, b.dns); err != nil {
			return nil, err
		}
	case b.gateway != "" || len(b.dns) > 0:
		return nil, errors.New("--build-gateway and --build-dns need --build-address")
	}
	c, err := connect(conn)
	if err != nil {
		return nil, err
	}
	b.builder.Client, b.builder.Sources = c.Client, c.Sources
	// Progress is best effort; a closed stderr must not fail the build.
	b.builder.Log = func(format string, a ...any) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...) }
	return &b.builder, nil
}

func newTemplateCreateCommand(conn *pve.Config) *cobra.Command {
	var (
		req      templates.Request
		flags    builderFlags
		newBuild bool
	)
	cmd := &cobra.Command{
		Use:   "create {talos|rke2|k3s|kubeadm}",
		Short: "Create the template for a distribution on a Proxmox VE node, unless it exists",
		Long: `Create the template for a distribution on a Proxmox VE node, unless it exists.

Talos templates are the Talos Image Factory's image with the QEMU guest agent. RKE2, k3s and kubeadm
templates are of a node OS (--node-os): its packages alone, with cloud-init, the QEMU guest agent,
SELinux enforcing with the distribution's policy, and for kubeadm containerd, the kubelet and kubeadm
at one Kubernetes version. Every image and package is checked against its publisher's signature.

Node OS templates are built again for their updates, each build a template of its own tagged with
when it was made; clusters move to a new build inside their maintenance windows. Without --new-build
the newest build is kept when there is one.

A node OS template is made by a VM that boots the family's cloud image and builds the node from the
family's packages; that VM needs to reach the package repositories, through DHCP or the
--build-address network.

The API token is read from KUBEHENGE_PROXMOX_TOKEN.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"talos", "rke2", "k3s", "kubeadm"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Distribution = templates.Distribution(args[0])
			t, err := templates.Plan(&req)
			if err != nil {
				return fmt.Errorf("the template: %w", err)
			}
			builder, err := flags.connect(cmd, conn)
			if err != nil {
				return err
			}
			now := time.Now()
			var vmid int
			if newBuild && t.Refreshed() {
				build, buildErr := t.Build(now)
				if buildErr != nil {
					return fmt.Errorf("naming the build of %s: %w", t.LineName, buildErr)
				}
				t = build
				vmid, err = builder.Create(cmd.Context(), &t)
			} else {
				vmid, err = builder.Ensure(cmd.Context(), &t, now)
			}
			if err != nil {
				return fmt.Errorf("creating template %s: %w", t.LineName, err)
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%d %s\n", vmid, t.LineName); err != nil {
				return fmt.Errorf("writing the result: %w", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.TalosVersion, "talos-version", templates.DefaultTalosVersion, "Talos release (talos)")
	nodeOSFlag(f, &req.NodeOS)
	f.StringVar(&req.KubernetesVersion, "kubernetes-version", "", "Kubernetes release, such as v1.36.5 (kubeadm)")
	f.BoolVar(&newBuild, "new-build", false, "build a node OS template again even if a build of it exists")
	flags.add(cmd)
	cmd.PreRunE = func(_ *cobra.Command, args []string) error {
		if args[0] == string(templates.Kubeadm) && req.KubernetesVersion == "" {
			return errors.New("kubeadm templates need --kubernetes-version")
		}
		return nil
	}
	return cmd
}

// parseNetwork is the static network of the VM that prepares an image.
func parseNetwork(address, gateway string, dns []string) (*templates.Network, error) {
	n := &templates.Network{}
	var err error
	if n.Address, err = netip.ParsePrefix(address); err != nil {
		return nil, fmt.Errorf("--build-address: %w", err)
	}
	if n.Gateway, err = netip.ParseAddr(gateway); err != nil {
		return nil, fmt.Errorf("--build-gateway: %w", err)
	}
	if !n.Address.Contains(n.Gateway) {
		return nil, fmt.Errorf("gateway %s is outside %s", n.Gateway, n.Address)
	}
	for _, d := range dns {
		a, parseErr := netip.ParseAddr(d)
		if parseErr != nil {
			return nil, fmt.Errorf("--build-dns: %w", parseErr)
		}
		n.DNS = append(n.DNS, a)
	}
	return n, nil
}
