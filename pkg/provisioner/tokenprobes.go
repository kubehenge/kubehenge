package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrivRealmAllocateUser is what the controller's token needs on the realm to make cluster users.
const PrivRealmAllocateUser = "Realm.AllocateUser"

// The privileges and paths the tokens are tested for, as Proxmox VE names them.
const (
	privSysAudit       = "Sys.Audit"
	privVMAudit        = "VM.Audit"
	privAllocateSpace  = "Datastore.AllocateSpace"
	privDatastoreAudit = "Datastore.Audit"
	pathRoot           = "/"
	pathNodes          = "/nodes"
	pathPool           = "/pool/"
	pathStorage        = "/storage/"
)

// SecretProbe names a Secret that holds a token Kubehenge tests though it does not use it: Cluster
// API's provider and the template builder.
type SecretProbe struct {
	Name      string
	Namespace string
	// From is the Secret's name.
	From  string
	Needs []Need
}

// TokenProbes builds the tests of every Proxmox VE token Kubehenge relies on that exists.
type TokenProbes struct {
	Reader         client.Reader
	Client         client.Client
	ClassNamespace string
	Places         Places
	// Controller is the controller's own client.
	Controller *proxmox.Client
	// HA and Restore run requests in the Jobs that alone hold those tokens, nil where there are none.
	HA, Restore TokenRunner
	Secrets     []SecretProbe
	// Connect signs in with a token, for those read from Secrets.
	Connect func(token string) (*proxmox.Client, error)
}

// Places are the pools, group and storages the tokens hold their privileges on. The management
// cluster's chart sets them from the same values that create the roles, so that a privilege checked
// is a privilege granted.
type Places struct {
	// WorkloadPool is the pool the clusters' pools nest under, and TemplatePool the one the node
	// OS templates are in.
	WorkloadPool string `json:"workloadPool"`
	TemplatePool string `json:"templatePool"`
	// ClusterGroup is the group the clusters' users join.
	ClusterGroup string `json:"clusterGroup"`
	// Storages are those the clusters may use for their disks and volumes.
	Storages []string `json:"storages"`
	// BootLogs is whether the controller reads nodes' boot logs.
	BootLogs bool `json:"bootLogs"`
	// Builder is where the template builder works.
	Builder BuilderPlaces `json:"builder"`
}

// BuilderPlaces are the node and storages the template builder works on.
type BuilderPlaces struct {
	Node    string `json:"node"`
	Storage string `json:"storage"`
	Import  string `json:"import"`
	ISO     string `json:"iso"`
}

// needsOn lists privs on path, each as a Need.
func needsOn(path string, privs ...string) []Need {
	out := make([]Need, 0, len(privs))
	for _, p := range privs {
		out = append(out, Need{path, p})
	}
	return out
}

// ControllerNeeds are what the controller's token holds, as the management cluster's configuration
// grants them.
func ControllerNeeds(p *Places) []Need {
	pool := []string{"Pool.Allocate", "Pool.Audit", "Permissions.Modify", "VM.Allocate", privVMAudit, "VM.Config.Disk", "VM.Config.Options", "VM.Migrate"}
	if p.BootLogs {
		pool = append(pool, "VM.GuestAgent.FileRead")
	}
	needs := slices.Concat(
		needsOn(pathRoot, privSysAudit),
		needsOn(pathNodes, privSysAudit),
		needsOn(pathPool+p.WorkloadPool, pool...),
		needsOn("/access/groups/"+p.ClusterGroup, "User.Modify"),
		needsOn("/access/realm/pve", PrivRealmAllocateUser),
	)
	if p.TemplatePool != "" {
		needs = append(needs, needsOn(pathPool+p.TemplatePool, privVMAudit)...)
	}
	for _, s := range p.Storages {
		needs = append(needs, needsOn(pathStorage+s, privAllocateSpace, privDatastoreAudit)...)
	}
	return needs
}

// CapmoxNeeds are what Cluster API's provider holds. Its pool roles are Proxmox VE's own, whose
// privileges the module does not list, so these are the ones capmox uses.
func CapmoxNeeds(p *Places) []Need {
	needs := slices.Concat(
		needsOn(pathRoot, privSysAudit),
		needsOn(pathNodes, privSysAudit),
		needsOn(pathPool+p.WorkloadPool, "VM.Allocate", "VM.Clone", "VM.Config.Disk"),
	)
	for _, s := range p.Storages {
		needs = append(needs, needsOn(pathStorage+s, privAllocateSpace)...)
	}
	return needs
}

// TemplateBuilderNeeds are what the template refresh holds.
func TemplateBuilderNeeds(p *Places) []Need {
	b := p.Builder
	needs := slices.Concat(
		needsOn(pathRoot, privSysAudit),
		needsOn(pathNodes, privSysAudit),
		needsOn("/nodes/"+b.Node, "Sys.AccessNetwork", privSysAudit),
		needsOn(pathPool+p.TemplatePool, "Pool.Audit", "VM.Allocate", privVMAudit, "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Disk", "VM.Config.HWType",
			"VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.GuestAgent.Unrestricted", "VM.PowerMgmt"),
	)
	for _, s := range []string{b.Import, b.ISO} {
		needs = append(needs, needsOn(pathStorage+s, "Datastore.Allocate", "Datastore.AllocateTemplate", privDatastoreAudit)...)
	}
	return unique(append(needs, needsOn(pathStorage+b.Storage, privAllocateSpace, privDatastoreAudit)...))
}

// HANeeds are what the HA token holds.
func HANeeds() []Need { return needsOn(pathRoot, privSysAudit, "Sys.Console") }

// RestoreNeeds are what the etcd restore token holds on the workload pool.
func RestoreNeeds(p *Places) []Need {
	return needsOn(pathPool+p.WorkloadPool, privVMAudit, "VM.GuestAgent.Unrestricted")
}

// ClusterNeeds are what a cluster's user holds on its pool, and through the clusters' group on the
// nodes and storages. The cloud controller manager and the CSI plugin run as it.
func ClusterNeeds(pool string, p *Places) []Need {
	needs := slices.Concat(
		needsOn(pathNodes, privSysAudit),
		needsOn(pathPool+pool, privVMAudit, "VM.Clone", "VM.Config.Disk"),
	)
	for _, s := range p.Storages {
		needs = append(needs, needsOn(pathStorage+s, privAllocateSpace, privDatastoreAudit)...)
	}
	return needs
}

// Probes lists a test for each token that exists. A token whose Secret cannot be read tests as
// unreachable, so that a missing Secret shows as well.
func (t *TokenProbes) Probes(ctx context.Context) []TokenProbe {
	probes := []TokenProbe{{Name: "Controller", Needs: ControllerNeeds(&t.Places), Ask: AskPermissions(t.Controller)}}
	if t.HA != nil {
		probes = append(probes, TokenProbe{Name: "Proxmox VE HA", Needs: HANeeds(), Ask: AskThroughJob(t.HA)})
	}
	if t.Restore != nil {
		probes = append(probes, TokenProbe{Name: "etcd restore", Needs: RestoreNeeds(&t.Places), Ask: AskThroughJob(t.Restore)})
	}
	for _, s := range t.Secrets {
		probes = append(probes, t.secretProbe(s))
	}
	return append(probes, t.clusterProbes(ctx)...)
}

// MarkClusters sets ProxmoxAccessRefusedCondition on the clusters the states are of, and clears it
// where the token works.
func (t *TokenProbes) MarkClusters(ctx context.Context, states []TokenState) error {
	var errs []error
	for i := range states {
		if states[i].Cluster == "" {
			continue
		}
		ns, name, _ := strings.Cut(states[i].Cluster, "/")
		c := &clusterv1.Cluster{}
		if err := t.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, c); err != nil {
			continue
		}
		errs = append(errs, markClusterAccess(ctx, t.Client, t.Reader, c, &states[i]))
	}
	return errors.Join(errs...)
}

func (t *TokenProbes) secretProbe(s SecretProbe) TokenProbe {
	return TokenProbe{Name: s.Name, Needs: s.Needs, Ask: func(ctx context.Context, paths []string) (Permissions, error) {
		secret := &corev1.Secret{}
		if err := t.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.From}, secret); err != nil {
			return nil, fmt.Errorf("reading Secret %s/%s: %w", s.Namespace, s.From, err)
		}
		token, err := SecretToken(secret)
		if err != nil {
			return nil, err
		}
		c, err := t.Connect(token)
		if err != nil {
			return nil, fmt.Errorf("signing in: %w", err)
		}
		return AskPermissions(c)(ctx, paths)
	}}
}

// clusterProbes tests the token of each cluster whose credentials the controller has made.
func (t *TokenProbes) clusterProbes(ctx context.Context) []TokenProbe {
	var clusters clusterv1.ClusterList
	if err := t.Client.List(ctx, &clusters); err != nil {
		return nil
	}
	var probes []TokenProbe
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !Owns(c, t.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		names, err := NamesFor(t.Places.WorkloadPool, c.Namespace, c.Name)
		if err != nil {
			continue
		}
		secret := &corev1.Secret{}
		err = t.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: credentialsSecretName(c)}, secret)
		if err != nil || !isReady(secret) {
			continue
		}
		creds, err := credentialsFrom(secret)
		if err != nil {
			continue
		}
		probes = append(probes, TokenProbe{
			Name: "Cluster " + c.Namespace + "/" + c.Name, Cluster: c.Namespace + "/" + c.Name, Needs: ClusterNeeds(names.Pool, &t.Places),
			Ask: func(ctx context.Context, paths []string) (Permissions, error) {
				pc, err := t.Connect(creds.TokenID + "=" + creds.TokenSecret)
				if err != nil {
					return nil, fmt.Errorf("signing in: %w", err)
				}
				return AskPermissions(pc)(ctx, paths)
			},
		})
	}
	return probes
}
