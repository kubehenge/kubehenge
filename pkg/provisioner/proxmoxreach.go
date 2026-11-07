package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Blind   string    `json:"blind,omitempty"`
	Checked time.Time `json:"checked"`
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
