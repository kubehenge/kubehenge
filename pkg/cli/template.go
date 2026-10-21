package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

func newTemplateCommand(conn *pve.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage the VM templates clusters are cloned from",
	}
	cmd.AddCommand(newTemplateCreateCommand(conn), newTemplateListCommand(conn), newTemplateDeleteCommand(conn))
	return cmd
}

func connect(conn *pve.Config) (*templates.Builder, error) {
	if conn.Token == "" {
		return nil, errors.New("set KUBEHENGE_PROXMOX_TOKEN to a Proxmox VE API token")
	}
	client, err := pve.Connect(*conn)
	if err != nil {
		return nil, fmt.Errorf("connecting to Proxmox VE: %w", err)
	}
	return &templates.Builder{Client: client, Sources: templates.DefaultSources()}, nil
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

func newTemplateDeleteCommand(conn *pve.Config) *cobra.Command {
	var (
		req  templates.Request
		pool string
	)
	cmd := &cobra.Command{
		Use:       "delete {talos|rke2|k3s|kubeadm}",
		Short:     "Delete the template for a distribution, if it exists",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"talos", "rke2", "k3s", "kubeadm"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Distribution = templates.Distribution(args[0])
			t, err := templates.Plan(req)
			if err != nil {
				return fmt.Errorf("the template: %w", err)
			}
			b, err := connect(conn)
			if err != nil {
				return err
			}
			b.Pool = pool
			vmid, err := b.Delete(cmd.Context(), &t)
			if err != nil {
				return fmt.Errorf("deleting template %s: %w", t.Name, err)
			}
			if vmid == 0 {
				if _, err = fmt.Fprintf(cmd.ErrOrStderr(), "no template carries the tags %s\n", strings.Join(t.Tags, ";")); err != nil {
					return fmt.Errorf("writing the result: %w", err)
				}
				return nil
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted %d %s\n", vmid, t.Name); err != nil {
				return fmt.Errorf("writing the result: %w", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.TalosVersion, "talos-version", templates.DefaultTalosVersion, "Talos release (talos)")
	f.StringVar(&req.OS, "os", templates.DefaultOS, "cloud image (rke2, k3s, kubeadm)")
	f.StringVar(&req.KubernetesVersion, "kubernetes-version", "", "Kubernetes release (kubeadm)")
	poolFlag(cmd, &pool, "pool the template is in; empty looks in every pool")
	return cmd
}

func newTemplateCreateCommand(conn *pve.Config) *cobra.Command {
	var (
		req     templates.Request
		builder templates.Builder
		address string
		gateway string
		dns     []string
	)
	cmd := &cobra.Command{
		Use:   "create {talos|rke2|k3s|kubeadm}",
		Short: "Create the template for a distribution on a Proxmox VE node, unless it exists",
		Long: `Create the template for a distribution on a Proxmox VE node, unless it exists.

Talos templates are the Talos Image Factory's image with the QEMU guest agent. RKE2 and k3s share one
template per operating system: Ubuntu's cloud image, updated and with the QEMU guest agent. kubeadm
templates add containerd, the kubelet and kubeadm at one Kubernetes version. Ubuntu images are checked
against Ubuntu's signed checksums.

Images that are not ready to use are booted once to prepare them; that VM needs to reach Ubuntu's and
Kubernetes' package mirrors, through DHCP or the --build-address network.

The API token is read from KUBEHENGE_PROXMOX_TOKEN.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"talos", "rke2", "k3s", "kubeadm"},
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Distribution = templates.Distribution(args[0])
			t, err := templates.Plan(req)
			if err != nil {
				return fmt.Errorf("the template: %w", err)
			}
			switch {
			case address != "":
				if builder.Network, err = parseNetwork(address, gateway, dns); err != nil {
					return err
				}
			case gateway != "" || len(dns) > 0:
				return errors.New("--build-gateway and --build-dns need --build-address")
			}
			c, err := connect(conn)
			if err != nil {
				return err
			}
			builder.Client, builder.Sources = c.Client, c.Sources
			// Progress is best effort; a closed stderr must not fail the build.
			builder.Log = func(format string, a ...any) { _, _ = fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...) }
			vmid, err := builder.Create(cmd.Context(), &t)
			if err != nil {
				return fmt.Errorf("creating template %s: %w", t.Name, err)
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%d %s %s\n", vmid, t.Name, strings.Join(t.Tags, ";")); err != nil {
				return fmt.Errorf("writing the result: %w", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.TalosVersion, "talos-version", templates.DefaultTalosVersion, "Talos release (talos)")
	f.StringVar(&req.OS, "os", templates.DefaultOS, "cloud image (rke2, k3s, kubeadm): ubuntu-26.04 or ubuntu-24.04")
	f.StringVar(&req.KubernetesVersion, "kubernetes-version", "", "Kubernetes release, such as v1.37.1 (kubeadm)")
	f.StringVar(&builder.Node, "node", "", "Proxmox VE node to create the template on")
	f.StringVar(&builder.ImportStorage, "import-storage", "local", "file storage with the import content type, for the downloaded image")
	f.StringVar(&builder.Storage, "storage", "local-lvm", "storage for the template's disk; shared storage lets clones start on any node")
	f.StringVar(&builder.ISOStorage, "iso-storage", "", "storage for the cloud-init ISO while preparing an image; default the node's first ISO storage")
	poolFlag(cmd, &builder.Pool, "pool the template is put in and looked for in; empty looks in every pool")
	f.StringVar(&builder.Bridge, "bridge", "vmbr0", "bridge for the VM that prepares an image")
	f.IntVar(&builder.VLAN, "vlan", 0, "VLAN tag for the VM that prepares an image")
	f.StringVar(&address, "build-address", "", "static address with prefix for the VM that prepares an image, such as 192.0.2.50/24; default DHCP")
	f.StringVar(&gateway, "build-gateway", "", "gateway for --build-address")
	f.StringSliceVar(&dns, "build-dns", nil, "DNS servers for --build-address")
	f.BoolVar(&builder.KeepOnFailure, "keep-on-failure", false, "leave the VM in place when preparing an image fails")
	f.StringVar(&builder.CPUType, "cpu-type", templates.DefaultCPUType, "QEMU CPU type of the template, which its clones keep; host shows them the node's own CPU")
	_ = cmd.MarkFlagRequired("node")
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
