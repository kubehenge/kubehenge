package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProxmoxReachName is the ConfigMap, beside the ClusterClasses, in which the controller records
// whether the Proxmox VE API answers from inside the management cluster. The VMs resolve its
// address with the DNS servers of the setup, which may know less than the node the installer ran on.
const ProxmoxReachName = "kubehenge-proxmox-reach"

// ProxmoxReachKey holds the outcome in ProxmoxReachName, as JSON.
const ProxmoxReachKey = "reach"

// Reachability is the outcome of the last ask.
type Reachability struct {
	// OK tells whether the API answered.
	OK bool `json:"ok"`
	// Error says why it did not, as the connection failed (no such host, refused, timed out).
	Error string `json:"error,omitempty"`
	// Blind names the privilege the token lacks to see which hosts are in maintenance, empty when it
	// has it. Without it Kubehenge cannot keep machines from replacement while a host is emptied.
	Blind string `json:"blind,omitempty"`
	// Tokens is the last test of every token Kubehenge relies on, which Tokens of ProxmoxReach makes.
	Tokens []TokenState `json:"tokens,omitempty"`
	// Unreachable are the nodes Proxmox VE lists as online that do not answer a call forwarded to
	// them, as the Placer last read.
	Unreachable []NodeFault `json:"unreachable,omitempty"`
	Checked     time.Time   `json:"checked"`
}

// hostBlindPrivilege is what reading the HA manager's status takes (`Status.pm`, manager_status).
const hostBlindPrivilege = "Sys.Audit on /"

// ParseReachability reads the outcome out of the ConfigMap.
func ParseReachability(cm *corev1.ConfigMap) (Reachability, error) {
	var r Reachability
	if err := json.Unmarshal([]byte(cm.Data[ProxmoxReachKey]), &r); err != nil {
		return Reachability{}, fmt.Errorf("%s: %w", ProxmoxReachName, err)
	}
	return r, nil
}

// ProxmoxReach keeps ProxmoxReachName current: the controller asks the API from its own pod, which
// is the view the clusters' controllers and plugins have.
type ProxmoxReach struct {
	Reader    client.Reader
	Client    client.Client
	Namespace string
	Proxmox   *proxmox.Client
	// Interval is how often the API is asked, so a fix of the DNS shows within about that long.
	Interval time.Duration
	// Now is the clock, time.Now unless a test sets it.
	Now func() time.Time
	// Tokens tests the tokens Kubehenge relies on every TokenInterval, or TokenCheckInterval where
	// that is zero. Nil leaves them untested.
	Tokens        *TokenProbes
	TokenInterval time.Duration
	// Unreachable, when set, names the online nodes that do not answer, as the Placer last found.
	// It reads what that look kept, so it adds no call.
	Unreachable func() []NodeFault

	mu         sync.Mutex
	lastStates []TokenState
	lastTested time.Time
	testing    bool
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*ProxmoxReach) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (r *ProxmoxReach) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("proxmox-reach")
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		if err := r.publish(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "recording whether Proxmox VE answers")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *ProxmoxReach) publish(ctx context.Context) error {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	outcome := Reachability{OK: true, Checked: now().UTC()}
	var version struct {
		Version string `json:"version"`
	}
	ask, cancel := context.WithTimeout(ctx, r.Interval)
	defer cancel()
	if err := r.Proxmox.Get(ask, "/version", &version); err != nil {
		outcome.OK, outcome.Error = false, err.Error()
	} else if _, err := haMaintenance(ask, r.Proxmox); errors.Is(err, proxmox.ErrNotAuthorized) {
		outcome.Blind = hostBlindPrivilege
	}
	outcome.Tokens = r.tokenStates(ctx, outcome.Checked)
	if r.Unreachable != nil {
		outcome.Unreachable = r.Unreachable()
	}
	raw, err := json.Marshal(outcome)
	if err != nil {
		return fmt.Errorf("encoding the outcome: %w", err)
	}
	cm := &corev1.ConfigMap{}
	err = r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: ProxmoxReachName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{Data: map[string]string{ProxmoxReachKey: string(raw)}}
		cm.Namespace, cm.Name = r.Namespace, ProxmoxReachName
		err = r.Client.Create(ctx, cm)
	case err != nil:
	default:
		cm.Data = map[string]string{ProxmoxReachKey: string(raw)}
		err = r.Client.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("recording the outcome in %s: %w", ProxmoxReachName, err)
	}
	return nil
}

// tokenStates returns the last test of the tokens, and starts the next when it is due. The tests
// run beside the reachability check, since a token only a Job holds takes a Job to test.
func (r *ProxmoxReach) tokenStates(ctx context.Context, now time.Time) []TokenState {
	if r.Tokens == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.testing && now.Sub(r.lastTested) >= cmp.Or(r.TokenInterval, TokenCheckInterval) {
		r.testing = true
		go r.testTokens(ctx, now)
	}
	return slices.Clone(r.lastStates)
}

// testTokens tests every token and puts the cluster tokens' outcome on their Clusters.
func (r *ProxmoxReach) testTokens(ctx context.Context, started time.Time) {
	states := CheckTokens(ctx, r.Tokens.Probes(ctx))
	if err := r.Tokens.MarkClusters(ctx, states); err != nil && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).WithName("proxmox-reach").Error(err, "recording a cluster's refused token")
	}
	r.mu.Lock()
	r.lastStates, r.lastTested, r.testing = states, started, false
	r.mu.Unlock()
}
