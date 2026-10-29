package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	// EtcdRestoreAnnotation asks for a cluster's etcd to be restored, as an EtcdRestoreRequest in
	// JSON. While it is there the cluster is being restored, and nothing else of Kubehenge's changes
	// it; the controller takes it away once the restore has ended.
	EtcdRestoreAnnotation = "kubehenge.dev/etcd-restore"
	// EtcdRestoreStatusAnnotation records where the cluster's last restore is, or how it ended, as an
	// EtcdRestoreStatus in JSON.
	EtcdRestoreStatusAnnotation = "kubehenge.dev/etcd-restore-status"
	// AutomaticRestoreAnnotation on a ClusterClass says whether the controller restores etcd
	// through the guest agent, "true" or "false"; else restores are done by hand.
	AutomaticRestoreAnnotation = "kubehenge.dev/etcd-restore-automatic"

	// etcdRestoreTimeout bounds a whole restore; each step on a node has guestStepTimeout.
	etcdRestoreTimeout = time.Hour
	guestStepTimeout   = 20 * time.Minute
	guestCheckTimeout  = time.Minute
	// restoreLinkLife is how long the node has to fetch the snapshot.
	restoreLinkLife = time.Hour
	// restoreObjectFolder holds, within a cluster's snapshot folder, the snapshot transcoded for the
	// node for the restore's duration.
	restoreObjectFolder = "restore"
	restoreObjectID     = 12
)

// RestorePhase is where a restore is.
type RestorePhase string

// The phases of a restore, in their order.
const (
	RestorePreparing RestorePhase = "Preparing"
	RestoreFetching  RestorePhase = "Fetching"
	RestoreStopping  RestorePhase = "Stopping"
	RestoreRestoring RestorePhase = "Restoring"
	RestoreStarting  RestorePhase = "Starting"
	RestoreReplacing RestorePhase = "Replacing"
	RestoreSucceeded RestorePhase = "Succeeded"
	RestoreFailed    RestorePhase = "Failed"
)

// EtcdRestoreRequest asks for a restore from one of a cluster's snapshots.
type EtcdRestoreRequest struct {
	Snapshot string `json:"snapshot"`
	// Target is the backup target the snapshot is in.
	Target string    `json:"target"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
}

// EtcdRestoreStatus is where a restore is, or how it ended.
type EtcdRestoreStatus struct {
	Snapshot    string       `json:"snapshot"`
	By          string       `json:"by,omitempty"`
	RequestedAt time.Time    `json:"requestedAt"`
	Phase       RestorePhase `json:"phase"`
	// FailedIn is the phase a failed restore had got to.
	FailedIn RestorePhase `json:"failedIn,omitempty"`
	// Machine and Node are the control-plane machine etcd is restored on, and its node.
	Machine string `json:"machine,omitempty"`
	Node    string `json:"node,omitempty"`
	// Replaced are the other control-plane machines, which Cluster API makes again.
	Replaced []string `json:"replaced,omitempty"`
	// Before is the snapshot of etcd as it was, taken into the restore's target before anything
	// of it was stopped or wiped; NoBefore says why none could be taken.
	Before   string     `json:"before,omitempty"`
	NoBefore string     `json:"noBefore,omitempty"`
	Message  string     `json:"message,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	// Paused is set when the restore paused the cluster, and so unpauses it.
	Paused bool `json:"paused,omitempty"`
}

// RestoreInProgress tells whether a cluster is being restored.
func RestoreInProgress(c *clusterv1.Cluster) bool {
	_, ok := c.Annotations[EtcdRestoreAnnotation]
	return ok
}

// EtcdRestoreRequestOf reads the restore a cluster is asked for.
func EtcdRestoreRequestOf(c *clusterv1.Cluster) (EtcdRestoreRequest, bool, error) {
	raw, ok := c.Annotations[EtcdRestoreAnnotation]
	if !ok {
		return EtcdRestoreRequest{}, false, nil
	}
	var r EtcdRestoreRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return EtcdRestoreRequest{}, true, fmt.Errorf("the restore request is not JSON: %w", err)
	}
	if !ValidSnapshotName(r.Snapshot) || r.Target == "" || r.At.IsZero() {
		return r, true, errors.New("the restore request names no snapshot, target and time")
	}
	return r, true, nil
}

// EtcdRestoreStatusOf reads where a cluster's last restore is.
func EtcdRestoreStatusOf(c *clusterv1.Cluster) (EtcdRestoreStatus, bool) {
	raw, ok := c.Annotations[EtcdRestoreStatusAnnotation]
	if !ok {
		return EtcdRestoreStatus{}, false
	}
	var s EtcdRestoreStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return EtcdRestoreStatus{}, false
	}
	return s, true
}

// EtcdRestorer restores clusters' etcd from their snapshots when asked through
// EtcdRestoreAnnotation, running the restore on the control-plane nodes through the guest agent.
//
// It pauses the cluster, so that Cluster API replaces no machine meanwhile; has the first
// control-plane node fetch the snapshot; stops etcd and the API server on every control-plane node;
// restores etcd on the first as a cluster of one; starts its control plane again; and deletes the
// other control-plane machines, whose etcd still holds the cluster from before, for Cluster API to
// make again once the cluster is unpaused. A restore that fails before etcd was restored starts the
// control plane again as it was.
//
// A Talos cluster is restored through the nodes' Talos API instead, as Talos recovers etcd: every
// control-plane node is reset with its EPHEMERAL partition wiped, the first is bootstrapped from the
// snapshot, and the others join it again by themselves.
type EtcdRestorer struct {
	Client client.Client
	// Reader reads the cluster past any cache: a reconcile queued while a restore ran can start
	// before the cache has the restore's end, and would take it for one the controller was stopped
	// in, running its recovery again and overwriting how it ended.
	Reader         client.Reader
	ClassNamespace string
	Targets        *ClusterTargets
	// Etcd snapshots the cluster's etcd as it is before a restore stops or wipes any of it.
	Etcd EtcdSource
	// Guests runs the restore on the nodes; nil when the management cluster has no restore token,
	// and every restore but a Talos cluster's fails, to be done by hand.
	Guests Guests
	// Talos reaches the nodes of Talos clusters.
	Talos TalosDialer
	// APIs asks the restored cluster's API server whether it answers on the cluster's address.
	APIs ClusterAPIs
	// poll is how often a restore looks at what it waits for; talosRestorePoll when 0.
	poll time.Duration
	// Events reports on a Cluster how its restore went; may be nil.
	Events events.EventRecorder
	Now    func() time.Time
}

// SetupWithManager registers the restorer. One restore runs at a time.
func (r *EtcdRestorer) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		Named("etcd-restores").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, ReconciliationTimeout: etcdRestoreTimeout + 5*time.Minute}).
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the etcd restorer: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (r *EtcdRestorer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Reader.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading the cluster: %w", err)
	}
	if !Owns(cluster, r.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	request, ok, err := EtcdRestoreRequestOf(cluster)
	if !ok {
		return ctrl.Result{}, nil
	}
	run := &restoreRun{r: r, key: client.ObjectKeyFromObject(cluster), cluster: cluster, request: request, talos: isTalos(cluster)}
	run.status = EtcdRestoreStatus{
		Snapshot: request.Snapshot, By: request.By, RequestedAt: request.At, Phase: RestorePreparing, Started: r.now(),
	}
	if err != nil {
		return ctrl.Result{}, run.finish(ctx, err)
	}
	if was, ok := EtcdRestoreStatusOf(cluster); ok && was.RequestedAt.Equal(request.At) && was.Snapshot == request.Snapshot {
		run.status = was
		if was.Finished != nil {
			return ctrl.Result{}, run.saveFinal(ctx)
		}
		// The controller stopped during this restore, so what it did is finished or undone now.
		err := run.recover(ctx, errors.New("the controller restarted during the restore"))
		return ctrl.Result{}, run.finish(ctx, err)
	}
	return ctrl.Result{}, run.do(ctx)
}

// restoreRun is one restore.
type restoreRun struct {
	r       *EtcdRestorer
	key     client.ObjectKey
	cluster *clusterv1.Cluster
	request EtcdRestoreRequest
	status  EtcdRestoreStatus

	scripts  RestoreScripts
	target   *ClusterTarget
	snapshot EtcdSnapshot
	// nodes are the control-plane nodes whose VMs run, the one restored on first.
	nodes []RestoreNode

	// talos restores through the Talos API, with the cluster's talosconfig.
	talos       bool
	talosconfig []byte
	// retried is why a Talos restore's first attempt failed, when a second restored etcd.
	retried error
}

func (run *restoreRun) do(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, etcdRestoreTimeout)
	defer cancel()
	if err := run.save(ctx); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("restoring etcd", "snapshot", run.request.Snapshot, "by", run.request.By)
	err := run.prepare(ctx)
	for _, step := range []func(context.Context) error{run.pause, run.saveBefore, run.fetch, run.stop, run.restore, run.start, run.answer} {
		if err != nil {
			break
		}
		err = step(ctx)
	}
	if err != nil {
		err = run.recover(context.WithoutCancel(ctx), err)
	} else {
		err = errors.Join(run.replace(ctx), run.unpause(ctx))
	}
	return run.finish(context.WithoutCancel(ctx), err)
}

// prepare checks the snapshot and the nodes, and changes nothing.
func (run *restoreRun) prepare(ctx context.Context) error {
	if run.r.Guests == nil && !run.talos {
		return errors.New("automatic restore is off. Restore by hand as the steps say")
	}
	if !run.cluster.DeletionTimestamp.IsZero() {
		return errors.New("the cluster is being deleted")
	}
	if run.talos {
		return run.prepareTalos(ctx)
	}
	scripts, err := restoreScriptsFor(distributionOf(run.cluster))
	if err != nil {
		return err
	}
	run.scripts = scripts
	if err = run.readSnapshot(ctx); err != nil {
		return err
	}
	nodes, err := controlPlaneNodes(ctx, run.r.Client, run.cluster)
	if err != nil {
		return err
	}
	for i := range nodes {
		if err = run.reach(ctx, &nodes[i]); err != nil {
			return err
		}
	}
	if len(run.nodes) == 0 {
		return errors.New("no control-plane VM of the cluster runs")
	}
	run.status.Machine, run.status.Node = run.nodes[0].Machine, run.nodes[0].Node
	for _, n := range run.nodes[1:] {
		run.status.Replaced = append(run.status.Replaced, n.Machine)
	}
	return nil
}

// reach checks that a control-plane node's guest agent runs commands, so that its etcd can be
// stopped. A VM that is off runs no etcd to stop, and Cluster API makes its machine again.
func (run *restoreRun) reach(ctx context.Context, n *RestoreNode) error {
	if n.VMID == 0 {
		return fmt.Errorf("control-plane machine %s has no VM yet. Restore once all are running", n.Machine)
	}
	running, err := run.r.Guests.Running(ctx, n.VMID)
	if err != nil {
		return fmt.Errorf("control-plane machine %s: %w", n.Machine, err)
	}
	if !running {
		run.status.Replaced = append(run.status.Replaced, n.Machine)
		return nil
	}
	if _, err := run.r.Guests.Run(ctx, n.VMID, "true\n", guestCheckTimeout); err != nil {
		return fmt.Errorf("the guest agent of %s does not run commands, so its etcd cannot be stopped: %w", n.Machine, err)
	}
	run.nodes = append(run.nodes, *n)
	return nil
}

// readSnapshot opens the target and checks that the cluster can be restored from the snapshot.
func (run *restoreRun) readSnapshot(ctx context.Context) error {
	t, err := run.r.Targets.Open(ctx, run.cluster, run.request.Target, false)
	if err != nil {
		return err
	}
	o, err := t.Store.Stat(ctx, t.Folder+"/"+run.request.Snapshot)
	if err != nil {
		return fmt.Errorf("reading snapshot %s: %w", run.request.Snapshot, err)
	}
	s, ok := snapshotOf(o, t.Folder)
	if !ok || s.SHA256 == "" {
		return fmt.Errorf("%s is not a whole snapshot: it has no checksum", run.request.Snapshot)
	}
	if err := Restorable(run.cluster, &s); err != nil {
		return err
	}
	run.target, run.snapshot = t, s
	return nil
}

// pause pauses the cluster, unless it is paused already.
func (run *restoreRun) pause(ctx context.Context) error {
	if ptr.Deref(run.cluster.Spec.Paused, false) {
		return nil
	}
	if err := run.setPaused(ctx, new(true)); err != nil {
		return err
	}
	run.status.Paused = true
	return run.save(ctx)
}

// saveBefore snapshots etcd as it is into the restore's target, so that a restore from the wrong
// snapshot can be undone by another. A cluster whose etcd does not answer, often the reason it is
// restored, is restored without one, and its status says why.
func (run *restoreRun) saveBefore(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, etcdSnapshotTimeout)
	defer cancel()
	taken, err := takeEtcdSnapshot(sctx, run.r.Etcd, run.r.now(), run.cluster, run.target)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "snapshotting etcd as it is before the restore")
		run.status.NoBefore = err.Error()
	} else {
		ctrl.LoggerFrom(ctx).Info("snapshotted etcd as it is before the restore", "snapshot", taken.Name)
		run.status.Before = taken.Name
	}
	return run.save(ctx)
}

// beforeMessage says where etcd as it was before the restore is, or why it is nowhere.
func (run *restoreRun) beforeMessage() string {
	switch {
	case run.status.Before != "":
		return " etcd as it was before is in snapshot " + run.status.Before + "."
	case run.status.NoBefore != "":
		return " No snapshot of etcd as it was could be taken first: " + run.status.NoBefore + "."
	}
	return ""
}

// unpause gives the cluster back to Cluster API, if the restore paused it.
func (run *restoreRun) unpause(ctx context.Context) error {
	if !run.status.Paused {
		return nil
	}
	return run.setPaused(ctx, nil)
}

// setPaused sets the cluster's spec.paused, or takes it away when nil.
func (run *restoreRun) setPaused(ctx context.Context, paused *bool) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &clusterv1.Cluster{}
		if err := run.r.Client.Get(ctx, run.key, c); err != nil {
			return fmt.Errorf("reading the cluster: %w", err)
		}
		before := c.DeepCopy()
		c.Spec.Paused = paused
		return run.r.Client.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return fmt.Errorf("pausing or unpausing the cluster: %w", err)
	}
	return nil
}

// fetch has the node restored on download the snapshot, transcoded to what openssl decrypts, from
// a link that expires, and check it. The transcoded copy is deleted once fetched.
func (run *restoreRun) fetch(ctx context.Context) error {
	if err := run.phase(ctx, RestoreFetching); err != nil {
		return err
	}
	if run.talos {
		return run.checkSnapshot(ctx)
	}
	password, err := newRestorePassword()
	if err != nil {
		return err
	}
	id := make([]byte, restoreObjectID)
	if _, err = rand.Read(id); err != nil {
		return fmt.Errorf("naming the transcoded snapshot: %w", err)
	}
	key := run.target.Folder + "/" + restoreObjectFolder + "/" + hex.EncodeToString(id) + ".enc"
	if err = run.transcode(ctx, key, password); err != nil {
		return err
	}
	defer func() { _ = run.target.Store.Remove(context.WithoutCancel(ctx), key) }()
	link, err := run.target.Store.PresignGet(ctx, key, restoreLinkLife)
	if err != nil {
		return fmt.Errorf("signing a link to the transcoded snapshot: %w", err)
	}
	return run.on(ctx, run.nodes[0], "fetching the snapshot", fetchScriptFor(link, password, run.snapshot.SHA256, run.target.CACertificate))
}

// transcode stores the snapshot under key encrypted with password, as it is read.
func (run *restoreRun) transcode(ctx context.Context, key, password string) error {
	plain, err := run.target.OpenEtcdSnapshot(ctx, run.snapshot.Name)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close() }()
	body, err := opensslSeal(plain, password)
	if err != nil {
		return err
	}
	if _, err := run.target.Store.Put(ctx, key, body, nil); err != nil {
		return fmt.Errorf("storing the transcoded snapshot: %w", err)
	}
	return nil
}

// stop stops etcd and the API server on every control-plane node.
func (run *restoreRun) stop(ctx context.Context) error {
	if err := run.phase(ctx, RestoreStopping); err != nil {
		return err
	}
	if run.talos {
		return run.wipeTalos(ctx)
	}
	for _, n := range run.nodes {
		if err := run.on(ctx, n, "stopping the control plane", run.scripts.Stop); err != nil {
			return err
		}
	}
	return nil
}

func (run *restoreRun) restore(ctx context.Context) error {
	if err := run.phase(ctx, RestoreRestoring); err != nil {
		return err
	}
	if run.talos {
		return run.recoverTalosEtcd(ctx)
	}
	return run.on(ctx, run.nodes[0], "restoring etcd", run.scripts.Restore)
}

func (run *restoreRun) start(ctx context.Context) error {
	if err := run.phase(ctx, RestoreStarting); err != nil {
		return err
	}
	if run.talos {
		return run.waitTalosEtcd(ctx)
	}
	return run.on(ctx, run.nodes[0], "starting the control plane", run.scripts.Start)
}

// answer waits, for at most guestStepTimeout, until the cluster's API server answers on the
// cluster's address, where Cluster API, the interface and the cluster's users reach it. The
// restored node's own API server answering is not enough: the address moves by a leader election
// that lapsed while etcd was stopped, and comes back only once the restored API server lets its
// holder take it again.
func (run *restoreRun) answer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, guestStepTimeout)
	defer cancel()
	poll := run.r.poll
	if poll == 0 {
		poll = talosRestorePoll
	}
	for {
		err := run.r.APIs.Ready(ctx, run.cluster)
		if err == nil {
			ctrl.LoggerFrom(ctx).Info("the cluster answers on its address")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the cluster's API server does not answer on its address: %w", err)
		case <-time.After(poll):
		}
	}
}

// replace deletes the control-plane machines etcd was not restored on, for Cluster API to make
// again: their etcd holds the cluster from before, which the restored one does not take back.
func (run *restoreRun) replace(ctx context.Context) error {
	if err := run.phase(ctx, RestoreReplacing); err != nil {
		return err
	}
	var errs []error
	for _, name := range run.status.Replaced {
		m := &clusterv1.Machine{Namespace: run.key.Namespace, Name: name}
		if err := run.r.Client.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting control-plane machine %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// recover finishes or undoes what a restore that failed did, by the phase it got to: before etcd
// was restored, it starts every control-plane node again as it was; after, it finishes the
// restore, since the other nodes' etcd would take back the cluster from before.
func (run *restoreRun) recover(ctx context.Context, cause error) error {
	if run.talos {
		return run.recoverTalos(ctx, cause)
	}
	errs := []error{cause, run.removeTranscoded(ctx)}
	if run.status.Phase == RestorePreparing {
		return errors.Join(append(errs, run.unpause(ctx))...)
	}
	if err := run.knowNodes(ctx); err != nil {
		return errors.Join(append(errs, err, errors.New("control-plane nodes left unchanged. Restore by hand as the steps say"))...)
	}
	restored := false
	switch run.status.Phase {
	case RestoreFetching:
		errs = append(errs, run.on(ctx, run.nodes[0], "removing the fetched snapshot", run.scripts.Clean))
		return errors.Join(append(errs, run.unpause(ctx))...)
	case RestoreRestoring:
		_, err := run.r.Guests.Run(ctx, run.nodes[0].VMID, run.scripts.Restored, guestCheckTimeout)
		restored = err == nil
	case RestoreStarting, RestoreReplacing:
		restored = true
	case RestorePreparing, RestoreStopping, RestoreSucceeded, RestoreFailed:
	}
	if restored {
		errs = append(errs, run.finishRestored(ctx)...)
	} else {
		for _, n := range run.nodes {
			errs = append(errs, run.on(ctx, n, "starting the control plane as it was", run.scripts.StartNoWait))
		}
	}
	return errors.Join(append(errs, run.unpause(ctx))...)
}

// finishRestored starts the restored node's control plane, if it may not be running, and replaces
// the other control-plane machines.
func (run *restoreRun) finishRestored(ctx context.Context) []error {
	var errs []error
	if run.status.Phase != RestoreReplacing {
		errs = append(errs, run.on(ctx, run.nodes[0], "starting the control plane", run.scripts.Start))
	}
	return append(errs, run.replace(ctx))
}

// removeTranscoded deletes what is left of the snapshots transcoded for restores of the cluster.
func (run *restoreRun) removeTranscoded(ctx context.Context) error {
	t := run.target
	if t == nil {
		var err error
		if t, err = run.r.Targets.Open(ctx, run.cluster, run.request.Target, false); err != nil {
			return nil //nolint:nilerr // without its target, the restore stored nothing there either
		}
	}
	objects, err := t.Store.List(ctx, t.Folder+"/"+restoreObjectFolder+"/")
	if err != nil {
		return fmt.Errorf("listing transcoded snapshots: %w", err)
	}
	var errs []error
	for _, o := range objects {
		errs = append(errs, t.Store.Remove(ctx, o.Key))
	}
	return errors.Join(errs...)
}

// knowNodes reads the nodes again for a restore the controller restarted in.
func (run *restoreRun) knowNodes(ctx context.Context) error {
	if run.nodes != nil {
		return nil
	}
	if run.r.Guests == nil {
		return errors.New("this management cluster does not restore through the guest agent")
	}
	scripts, err := restoreScriptsFor(distributionOf(run.cluster))
	if err != nil {
		return err
	}
	run.scripts = scripts
	nodes, err := controlPlaneNodes(ctx, run.r.Client, run.cluster)
	if err != nil {
		return err
	}
	replaced := map[string]bool{}
	for _, m := range run.status.Replaced {
		replaced[m] = true
	}
	for _, n := range nodes {
		switch {
		case n.Machine == run.status.Machine:
			run.nodes = append([]RestoreNode{n}, run.nodes...)
		case replaced[n.Machine]:
			if running, err := run.r.Guests.Running(ctx, n.VMID); err == nil && running {
				run.nodes = append(run.nodes, n)
			}
		}
	}
	if len(run.nodes) == 0 || run.nodes[0].Machine != run.status.Machine {
		return fmt.Errorf("control-plane machine %s, which etcd was restored on, is gone", run.status.Machine)
	}
	return nil
}

// on runs a script on a node, logging what it printed.
func (run *restoreRun) on(ctx context.Context, n RestoreNode, what, s string) error {
	out, err := run.r.Guests.Run(ctx, n.VMID, s, guestStepTimeout)
	ctrl.LoggerFrom(ctx).Info(what, "machine", n.Machine, "output", tail(out, outputTail), "failed", err != nil)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", what, n.Machine, err)
	}
	return nil
}

func (run *restoreRun) phase(ctx context.Context, p RestorePhase) error {
	run.status.Phase = p
	return run.save(ctx)
}

// finish records how the restore ended and takes the request away.
func (run *restoreRun) finish(ctx context.Context, err error) error {
	at := run.r.now()
	run.status.Finished = &at
	kind, reason := corev1.EventTypeNormal, "EtcdRestored"
	if err != nil {
		run.status.FailedIn = run.status.Phase
		run.status.Phase, run.status.Message = RestoreFailed, err.Error()
		kind, reason = corev1.EventTypeWarning, "EtcdRestoreFailed"
	} else {
		run.status.Phase, run.status.Message = RestoreSucceeded, run.succeededMessage()
	}
	ctrl.LoggerFrom(ctx).Info("etcd restore ended", "phase", run.status.Phase, "message", run.status.Message,
		"before", run.status.Before, "noBefore", run.status.NoBefore)
	if run.r.Events != nil {
		note := run.status.Message
		if before := run.beforeMessage(); before != "" {
			note = strings.TrimSuffix(note, ".") + "." + before
		}
		run.r.Events.Eventf(run.cluster, nil, kind, reason, "RestoreEtcd", "restore from %s: %s", run.status.Snapshot, note)
	}
	return run.saveFinal(ctx)
}

func (run *restoreRun) succeededMessage() string {
	s := &run.status
	m := "Restored etcd from " + s.Snapshot + " on " + s.Machine + "."
	if run.retried != nil {
		m += " The first attempt failed, " + run.retried.Error() + ", and a second restored it."
	}
	if run.talos && len(run.nodes) > 1 {
		m += fmt.Sprintf(" The other %d control-plane nodes join it again by themselves.", len(run.nodes)-1)
	}
	switch {
	case len(s.Replaced) == 0:
	case run.talos:
		m += fmt.Sprintf(" Cluster API makes the %d control-plane machines whose Talos API did not answer again.", len(s.Replaced))
	default:
		m += fmt.Sprintf(" Cluster API makes the other %d control-plane machines again.", len(s.Replaced))
	}
	if ptr.Deref(run.cluster.Spec.Paused, false) && !s.Paused {
		m += " The cluster was paused before the restore and stays paused until it is unpaused."
	}
	return m
}

// save records the status on the cluster.
func (run *restoreRun) save(ctx context.Context) error {
	return run.patch(ctx, func(*clusterv1.Cluster) {})
}

// saveFinal records the status on the cluster and takes the request away, unless another has
// taken its place.
func (run *restoreRun) saveFinal(ctx context.Context) error {
	return run.patch(ctx, func(c *clusterv1.Cluster) {
		if still, ok, _ := EtcdRestoreRequestOf(c); ok && still.At.Equal(run.request.At) {
			delete(c.Annotations, EtcdRestoreAnnotation)
		}
	})
}

func (run *restoreRun) patch(ctx context.Context, change func(*clusterv1.Cluster)) error {
	raw, merr := json.Marshal(run.status)
	if merr != nil {
		return fmt.Errorf("recording the restore: %w", merr)
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &clusterv1.Cluster{}
		if err := run.r.Client.Get(ctx, run.key, c); err != nil {
			return fmt.Errorf("reading the cluster: %w", err)
		}
		before := c.DeepCopy()
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdRestoreStatusAnnotation, string(raw))
		change(c)
		return run.r.Client.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return fmt.Errorf("recording the restore: %w", err)
	}
	return nil
}

func (r *EtcdRestorer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
