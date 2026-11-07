package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The privileges and paths the tokens are tested for, as Proxmox VE names them.
const (
	privSysAudit = "Sys.Audit"
	privVMAudit  = "VM.Audit"
	pathRoot     = "/"
	pathPool     = "/pool/"
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
	WorkloadPool   string
	// Controller is the controller's own client.
	Controller *proxmox.Client
	// HA and Restore run requests in the Jobs that alone hold those tokens, nil where there are none.
	HA, Restore TokenRunner
	Secrets     []SecretProbe
	// Connect signs in with a token, for those read from Secrets.
	Connect func(token string) (*proxmox.Client, error)
}

// ControllerNeeds are what the controller's token holds, as the management cluster's configuration
// grants them. pool is the workload pool.
func ControllerNeeds(pool string) []Need {
	return []Need{
		{pathRoot, privSysAudit},
		{pathPool + pool, "Pool.Allocate"},
		{pathPool + pool, "Permissions.Modify"},
		{pathPool + pool, "VM.Allocate"},
		{pathPool + pool, privVMAudit},
		{"/access/realm/pve", "Realm.AllocateUser"},
	}
}

// CapmoxNeeds are what Cluster API's provider holds.
func CapmoxNeeds(pool string) []Need {
	return []Need{
		{pathRoot, privSysAudit},
		{pathPool + pool, "VM.Allocate"},
		{pathPool + pool, "VM.Clone"},
		{pathPool + pool, "VM.Config.Disk"},
	}
}

// TemplateBuilderNeeds are what the template refresh holds on the whole of Proxmox VE.
func TemplateBuilderNeeds() []Need { return []Need{{pathRoot, privSysAudit}} }

// HANeeds are what the HA token holds.
func HANeeds() []Need { return []Need{{pathRoot, privSysAudit}, {pathRoot, "Sys.Console"}} }

// RestoreNeeds are what the etcd restore token holds on the workload pool.
func RestoreNeeds(pool string) []Need {
	return []Need{{pathPool + pool, privVMAudit}, {pathPool + pool, "VM.GuestAgent.Unrestricted"}}
}

// ClusterNeeds are what a cluster's cloud controller manager and CSI plugin hold on its pool.
func ClusterNeeds(pool string) []Need {
	return []Need{{pathPool + pool, privVMAudit}, {pathPool + pool, "VM.Config.Disk"}}
}

// Probes lists a test for each token that exists. A token whose Secret cannot be read tests as
// unreachable, so that a missing Secret shows as well.
func (t *TokenProbes) Probes(ctx context.Context) []TokenProbe {
	probes := []TokenProbe{{Name: "Controller", Needs: ControllerNeeds(t.WorkloadPool), Ask: AskPermissions(t.Controller)}}
	if t.HA != nil {
		probes = append(probes, TokenProbe{Name: "Proxmox VE HA", Needs: HANeeds(), Ask: AskThroughJob(t.HA)})
	}
	if t.Restore != nil {
		probes = append(probes, TokenProbe{Name: "etcd restore", Needs: RestoreNeeds(t.WorkloadPool), Ask: AskThroughJob(t.Restore)})
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
		names, err := NamesFor(t.WorkloadPool, c.Namespace, c.Name)
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
			Name: "Cluster " + c.Namespace + "/" + c.Name, Cluster: c.Namespace + "/" + c.Name, Needs: ClusterNeeds(names.Pool),
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
