package provisioner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/csiproxy"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	// copiesPerHour is the most copies a cluster may have made within an hour. Each copy takes
	// room on the cluster's storage, which its token may take anyway, and the I/O of a node, which
	// one cluster's backups should not take from every other's for long.
	copiesPerHour = 60
	// concurrentCopies is the most copies under way at once, over every cluster.
	concurrentCopies = 4
	// finishedRequestTTL is how long a finished request is kept for a proxy that stopped waiting on
	// it before it is deleted.
	finishedRequestTTL = 10 * time.Minute
	// ticketRenewal is how old root@pam's ticket gets before it is renewed, well inside the two
	// hours Proxmox VE accepts it.
	ticketRenewal = time.Hour
	// copyTimeout bounds the wait for one copy, far beyond what copying a volume on a local disk
	// takes, so that a task Proxmox VE never ends does not hold up the cluster's other copies.
	copyTimeout = 24 * time.Hour
	// taskPoll is how often a copy's task is looked at.
	taskPoll = 2 * time.Second
)

// pluginVolume is a volume as the CSI plugin names it on a storage: vm-<owner>-<name>, below
// <owner>/ and with .raw on file-based storages.
var pluginVolume = regexp.MustCompile(`^(?:([0-9]+)/)?vm-([0-9]+)-[a-z0-9]+(?:-[a-z0-9]+)*(?:\.raw)?$`)

// nodeName is a Proxmox VE node's name, a DNS label.
var nodeName = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// RootAccounts signs root@pam in to Proxmox VE and copies volumes with its ticket; *pve.Accounts is
// the real one.
type RootAccounts interface {
	SignIn(ctx context.Context, username, password string) (*pve.Ticket, *pve.Challenge, error)
	Renew(ctx context.Context, t *pve.Ticket) (*pve.Ticket, error)
	StorageVolumes(ctx context.Context, t *pve.Ticket, node, storage string) ([]string, error)
	StartCopy(ctx context.Context, t *pve.Ticket, node, storage, volume, target, targetNode string) (string, error)
	TaskExit(ctx context.Context, t *pve.Ticket, node, upid string) (string, error)
}

// VolumeCopier makes the volume copies that the CSI plugins of clusters taking volume snapshots ask
// for through the proxy beside them (package csiproxy), signed in as root@pam, the one user Proxmox
// VE lets copy a volume, whose password never leaves the management cluster. It copies only one of
// the cluster's own volumes into a new one, both on the cluster's storage and named after its
// volume owner VM as the plugin names them; one at a time for each cluster, at most copiesPerHour
// an hour. It logs each copy, and records each refusal and failure on the Cluster.
type VolumeCopier struct {
	Client          client.Client
	ClassNamespace  string
	VolumeSnapshots VolumeSnapshots
	Accounts        RootAccounts
	// Remotes reaches the workload clusters, where the copy requests are.
	Remotes *Remotes
	// Events records refusals and failures on the Cluster; may be nil.
	Events events.EventRecorder
	// Interval is how often each cluster's requests are looked for.
	Interval time.Duration

	mu sync.Mutex
	// busy holds the clusters whose requests are being served.
	busy map[string]bool
	// started holds when each cluster's copies of the last hour started.
	started map[string][]time.Time
	slots   chan struct{}
}

// copyGrant is what a cluster's copies are checked against and made with; refused says why none may
// be made.
type copyGrant struct {
	owner    int
	storage  string
	password string
	refused  string
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*VolumeCopier) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (v *VolumeCopier) Start(ctx context.Context) error {
	t := time.NewTicker(v.Interval)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		v.copyAll(ctx, &wg)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// copyAll serves the requests of every cluster that takes volume snapshots, each cluster's apart
// from the others', so that one whose API does not answer holds up no other.
func (v *VolumeCopier) copyAll(ctx context.Context, wg *sync.WaitGroup) {
	log := ctrl.LoggerFrom(ctx).WithName("volume-copier")
	clusters := &clusterv1.ClusterList{}
	if err := v.Client.List(ctx, clusters); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		csi, err := csiOf(c)
		if err != nil || !csi.VolumeSnapshots || !Owns(c, v.ClassNamespace) || !c.DeletionTimestamp.IsZero() || !v.claim(c) {
			continue
		}
		wg.Go(func() {
			defer v.release(c)
			if err := v.serve(ctx, c); err != nil && ctx.Err() == nil {
				log.Error(err, "making the volume copies asked for", "cluster", client.ObjectKeyFromObject(c))
			}
		})
	}
}

func (v *VolumeCopier) claim(c *clusterv1.Cluster) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.busy == nil {
		v.busy = map[string]bool{}
	}
	if v.busy[remoteKey(c)] {
		return false
	}
	v.busy[remoteKey(c)] = true
	return true
}

func (v *VolumeCopier) release(c *clusterv1.Cluster) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.busy, remoteKey(c))
}

// serve handles the cluster's requests, oldest first.
func (v *VolumeCopier) serve(ctx context.Context, cluster *clusterv1.Cluster) error {
	workload, found, err := v.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return err
	}
	list := &corev1.ConfigMapList{}
	if err = workload.client.List(ctx, list, client.InNamespace(csiproxy.Namespace), client.HasLabels{csiproxy.RequestLabel}); err != nil {
		v.Remotes.forget(cluster)
		return fmt.Errorf("listing the copy requests: %w", err)
	}
	if len(list.Items) == 0 {
		return nil
	}
	slices.SortFunc(list.Items, func(a, b corev1.ConfigMap) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	grant, err := v.grant(ctx, cluster)
	if err != nil {
		return err
	}
	for i := range list.Items {
		r := &copyRequest{cluster: cluster, workload: workload.client, cm: &list.Items[i]}
		r.Request = csiproxy.ParseRequest(r.cm)
		if err := v.handle(ctx, r, grant); err != nil {
			return err
		}
	}
	return nil
}

// grant reads what the cluster's copies are made with, or why none may be.
func (v *VolumeCopier) grant(ctx context.Context, cluster *clusterv1.Cluster) (copyGrant, error) {
	refused, err := v.VolumeSnapshots.refusalNow(ctx, v.Client, v.ClassNamespace, cluster)
	if err != nil || refused != "" {
		return copyGrant{refused: refused}, err
	}
	stored := &corev1.Secret{}
	err = v.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, stored)
	if apierrors.IsNotFound(err) || err == nil && !isReady(stored) {
		return copyGrant{refused: "the cluster's Proxmox VE objects are not in place yet"}, nil
	}
	if err != nil {
		return copyGrant{}, fmt.Errorf("reading the cluster's credentials: %w", err)
	}
	creds, err := credentialsFrom(stored)
	if err != nil {
		return copyGrant{}, err
	}
	storage, err := variable[string](cluster, "storage")
	if err != nil {
		return copyGrant{}, err
	}
	password, refused, err := v.VolumeSnapshots.password(ctx, v.Client)
	return copyGrant{owner: creds.VolumeOwner, storage: storage, password: password, refused: refused}, err
}

// copyRequest is one of a cluster's requests, with what it is answered through.
type copyRequest struct {
	csiproxy.Request

	cluster  *clusterv1.Cluster
	workload client.Client
	cm       *corev1.ConfigMap
}

// handle answers a request: a finished one is deleted once no proxy waits on it any more, a copy
// under way is waited on, and a new one is checked and copied.
func (v *VolumeCopier) handle(ctx context.Context, r *copyRequest, grant copyGrant) error {
	switch {
	case r.Finished():
		if time.Since(r.Created) > finishedRequestTTL {
			if err := client.IgnoreNotFound(r.workload.Delete(ctx, r.cm)); err != nil {
				return fmt.Errorf("deleting the finished copy request %s: %w", r.Name, err)
			}
		}
		return nil
	case grant.refused != "":
		return v.fail(ctx, r, grant.refused)
	case r.State == csiproxy.StateCopying:
		return v.resume(ctx, r, grant)
	}
	if why := copyRefusal(&r.Copy, grant.owner, grant.storage); why != "" {
		return v.fail(ctx, r, why)
	}
	if !v.allow(r.cluster) {
		return v.fail(ctx, r, fmt.Sprintf("the cluster had %d volumes copied within the last hour, the most it may", copiesPerHour))
	}
	return v.copyVolume(ctx, r, grant)
}

// copyRefusal says why a cluster may not have a volume copied: both volumes must be its own, on its
// storage, and named after its volume owner VM, as the CSI plugin names them; "" when they are.
func copyRefusal(c *csiproxy.Copy, owner int, storage string) string {
	switch {
	case c.Storage != storage:
		return fmt.Sprintf("storage %s is not the cluster's storage %s", c.Storage, storage)
	case !nodeName.MatchString(c.Node), c.TargetNode != "" && !nodeName.MatchString(c.TargetNode):
		return "the copy names a node that is not a Proxmox VE node's name"
	case !ownVolume(c.Volume, owner):
		return fmt.Sprintf("volume %s is not one of the cluster's own, named after its volume owner VM %d", c.Volume, owner)
	case !ownVolume(c.Target, owner):
		return fmt.Sprintf("target %s is not named after the cluster's volume owner VM %d", c.Target, owner)
	case c.Volume == c.Target:
		return "a volume cannot be copied onto itself"
	}
	return ""
}

func ownVolume(name string, owner int) bool {
	m := pluginVolume.FindStringSubmatch(name)
	id := strconv.Itoa(owner)
	return len(m) == 3 && m[2] == id && (m[1] == "" || m[1] == id)
}

// allow counts a copy of the cluster's, unless it had copiesPerHour within the last hour.
func (v *VolumeCopier) allow(c *clusterv1.Cluster) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started == nil {
		v.started = map[string][]time.Time{}
	}
	now := time.Now()
	key := remoteKey(c)
	recent := slices.DeleteFunc(v.started[key], func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(recent) >= copiesPerHour {
		v.started[key] = recent
		return false
	}
	v.started[key] = append(recent, now)
	return true
}

// copyVolume makes the copy, once the volume is there and the target is not.
func (v *VolumeCopier) copyVolume(ctx context.Context, r *copyRequest, grant copyGrant) error {
	release, err := v.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	t, why, err := v.signIn(ctx, grant.password)
	if err != nil || why != "" {
		return v.failOn(ctx, r, why, err)
	}
	on, err := v.Accounts.StorageVolumes(ctx, t, r.Node, r.Storage)
	if err != nil {
		return fmt.Errorf("listing storage %s on node %s: %w", r.Storage, r.Node, err)
	}
	if !slices.Contains(on, r.Storage+":"+r.Volume) {
		return v.fail(ctx, r, fmt.Sprintf("volume %s is not on storage %s on node %s", r.Volume, r.Storage, r.Node))
	}
	if r.Destination() != r.Node {
		if on, err = v.Accounts.StorageVolumes(ctx, t, r.Destination(), r.Storage); err != nil {
			return fmt.Errorf("listing storage %s on node %s: %w", r.Storage, r.Destination(), err)
		}
	}
	if slices.Contains(on, r.TargetID()) {
		return v.fail(ctx, r, fmt.Sprintf("volume %s is on storage %s on node %s already", r.Target, r.Storage, r.Destination()))
	}
	upid, err := v.Accounts.StartCopy(ctx, t, r.Node, r.Storage, r.Volume, r.Target, r.TargetNode)
	if err != nil {
		return v.fail(ctx, r, err.Error())
	}
	r.Task = upid
	audit(ctx, r, "copying a volume as root@pam")
	csiproxy.Record(r.cm, csiproxy.StateCopying, upid, "")
	if err := r.workload.Update(ctx, r.cm); err != nil && !apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Error(err, "recording the copy's task", "request", r.Name)
	}
	return v.finish(ctx, r, t, grant.password)
}

// resume waits for a copy started before the controller last stopped.
func (v *VolumeCopier) resume(ctx context.Context, r *copyRequest, grant copyGrant) error {
	// The task's node is its UPID's second field, UPID:<node>:...; a copy runs on the volume's node.
	fields := strings.Split(r.Task, ":")
	const nodeField = 1
	if len(fields) <= nodeField || fields[0] != "UPID" || fields[nodeField] != r.Node {
		return v.fail(ctx, r, "the request names no copy task of its volume's node")
	}
	release, err := v.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	t, why, err := v.signIn(ctx, grant.password)
	if err != nil || why != "" {
		return v.failOn(ctx, r, why, err)
	}
	return v.finish(ctx, r, t, grant.password)
}

// finish waits for the copy's task and records how it ended.
func (v *VolumeCopier) finish(ctx context.Context, r *copyRequest, t *pve.Ticket, password string) error {
	began := time.Now()
	exit, err := v.waitTask(ctx, t, password, r.Node, r.Task)
	switch {
	case ctx.Err() != nil:
		// Waited for again when the controller next runs.
		return fmt.Errorf("waiting for the copy: %w", ctx.Err())
	case err != nil:
		return v.fail(ctx, r, "waiting for the copy: "+err.Error())
	case exit != "OK":
		return v.fail(ctx, r, "Proxmox VE's copy failed: "+exit)
	}
	csiproxy.Record(r.cm, csiproxy.StateDone, r.Task, "")
	if err := client.IgnoreNotFound(r.workload.Update(ctx, r.cm)); err != nil {
		return fmt.Errorf("recording the copy done: %w", err)
	}
	audit(ctx, r, "copied a volume as root@pam", "waited", time.Since(began).Round(time.Second).String())
	return nil
}

// waitTask waits for the task to end, renewing root@pam's ticket as it ages.
func (v *VolumeCopier) waitTask(ctx context.Context, t *pve.Ticket, password, node, upid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()
	tick := time.NewTicker(taskPoll)
	defer tick.Stop()
	for {
		if time.Since(t.Issued) > ticketRenewal {
			renewed, err := v.renew(ctx, t, password)
			if err != nil {
				return "", err
			}
			t = renewed
		}
		exit, err := v.Accounts.TaskExit(ctx, t, node, upid)
		if err != nil {
			return "", fmt.Errorf("reading task %s: %w", upid, err)
		}
		if exit != "" {
			return exit, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for task %s: %w", upid, ctx.Err())
		case <-tick.C:
		}
	}
}

// renew renews root@pam's ticket, or signs it in again once Proxmox VE no longer renews it.
func (v *VolumeCopier) renew(ctx context.Context, t *pve.Ticket, password string) (*pve.Ticket, error) {
	if renewed, err := v.Accounts.Renew(ctx, t); err == nil {
		return renewed, nil
	}
	renewed, why, err := v.signIn(ctx, password)
	if err == nil && why != "" {
		err = errors.New(why)
	}
	return renewed, err
}

// signIn signs root@pam in, or says why it cannot be.
func (v *VolumeCopier) signIn(ctx context.Context, password string) (*pve.Ticket, string, error) {
	t, challenge, err := v.Accounts.SignIn(ctx, rootUser, password)
	switch {
	case errors.Is(err, pve.ErrAuthentication):
		return nil, "Proxmox VE refuses the root@pam password stored in the management cluster", nil
	case err != nil:
		return nil, "", fmt.Errorf("signing root@pam in: %w", err)
	case challenge != nil:
		return nil, "root@pam asks for a second factor, so Kubehenge cannot sign it in", nil
	}
	return t, "", nil
}

// slot waits until fewer than concurrentCopies copies are under way, and returns what ends this one.
func (v *VolumeCopier) slot(ctx context.Context) (func(), error) {
	v.mu.Lock()
	if v.slots == nil {
		v.slots = make(chan struct{}, concurrentCopies)
	}
	slots := v.slots
	v.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a copy to end: %w", ctx.Err())
	}
}

// failOn fails the request with why, unless err says it could not be tried yet.
func (v *VolumeCopier) failOn(ctx context.Context, r *copyRequest, why string, err error) error {
	if err != nil {
		return err
	}
	return v.fail(ctx, r, why)
}

// fail records why the request was refused or its copy failed, on the request, where the CSI
// plugin reports it, and on the Cluster.
func (v *VolumeCopier) fail(ctx context.Context, r *copyRequest, why string) error {
	csiproxy.Record(r.cm, csiproxy.StateFailed, r.Task, why)
	if err := client.IgnoreNotFound(r.workload.Update(ctx, r.cm)); err != nil {
		return fmt.Errorf("recording the copy request %s failed: %w", r.Name, err)
	}
	audit(ctx, r, "refused or failed a volume copy", "reason", why)
	if v.Events != nil {
		v.Events.Eventf(r.cluster, nil, corev1.EventTypeWarning, "VolumeCopyFailed", "CopyVolume",
			"copying volume %s into %s on storage %s: %s", r.Volume, r.Target, r.Storage, why)
	}
	return nil
}

// audit logs what was done as root@pam, and for which cluster.
func audit(ctx context.Context, r *copyRequest, msg string, more ...any) {
	ctrl.LoggerFrom(ctx).WithName("volume-copier").Info(msg, append([]any{
		"cluster", client.ObjectKeyFromObject(r.cluster), "request", r.Name, "node", r.Node, "storage", r.Storage,
		"volume", r.Volume, "targetVolume", r.Target, "targetNode", r.TargetNode, "task", r.Task,
	}, more...)...)
}
