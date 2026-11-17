package provisioner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	etcdVariable = "etcd"
	// DefaultEtcdQuotaGiB is the etcd variable's quotaGiB as the chart's schema defaults it.
	DefaultEtcdQuotaGiB = 8
	bytesPerGiB         = int64(1) << 30

	// EtcdStatusAnnotation on a Cluster records what the controller last read of its etcd members, as
	// an EtcdStatus in JSON.
	EtcdStatusAnnotation = "kubehenge.dev/etcd-status"
	// EtcdDefragRequestAnnotation asks for every member to be defragmented at once, as an
	// EtcdDefragRequest in JSON. The controller takes it away once it has tried.
	EtcdDefragRequestAnnotation = "kubehenge.dev/etcd-defrag-requested"
	// EtcdDefragAnnotation records the last defragmentation, as an EtcdDefrag in JSON.
	EtcdDefragAnnotation = "kubehenge.dev/etcd-defragmented"

	// EtcdWarnPercent is how full a member's database may be, of the quota, before the cluster warns.
	EtcdWarnPercent = 60
	// etcdDefragPercent is the share of a database that is free space, above which a member is
	// defragmented inside the window.
	etcdDefragPercent = 50
	// etcdDefragMinBytes keeps small databases alone: a defragmentation blocks the member's reads
	// and writes while it runs, and gives little back below this.
	etcdDefragMinBytes = int64(100) << 20
	// etcdReadInterval is how often a cluster's members are read.
	etcdReadInterval = 10 * time.Minute
	// etcdDefragTimeout bounds one defragmentation run over all members.
	etcdDefragTimeout = 30 * time.Minute
	// etcdMemberTimeout bounds one call to a member.
	etcdMemberTimeout = 2 * time.Minute
	// etcdReadTimeout bounds reading every member, which are asked in turn.
	etcdReadTimeout = 2 * etcdMemberTimeout
	// etcdDefragMemberTimeout bounds the defragmentation of one member, which takes far longer than
	// reading it.
	etcdDefragMemberTimeout = 5 * etcdMemberTimeout

	alarmNoSpace = "NOSPACE"
)

// defragTrigger says why a maintenance pass may defragment: on its schedule, inside the window, or
// because someone asked for it now.
type defragTrigger int

const (
	defragScheduled defragTrigger = iota
	defragRequested
)

// defragAllowance is what lets a pass defragment a member that is not full: nothing, the upgrade
// window, or a request.
type defragAllowance int

const (
	defragHeld defragAllowance = iota
	defragInWindow
	defragAsked
)

// EtcdSettings is a cluster's etcd variable.
type EtcdSettings struct {
	QuotaGiB int `json:"quotaGiB,omitempty"`
}

// EtcdSettingsOf reads a cluster's etcd variable with the chart's default.
func EtcdSettingsOf(c *clusterv1.Cluster) (EtcdSettings, error) {
	s, _, err := optionalVariable[EtcdSettings](c, etcdVariable)
	s.QuotaGiB = cmp.Or(s.QuotaGiB, DefaultEtcdQuotaGiB)
	return s, err
}

// QuotaBytes is the size a member's database may reach before etcd refuses writes.
func (s EtcdSettings) QuotaBytes() int64 { return int64(s.QuotaGiB) * bytesPerGiB }

// EtcdMember is what the controller reads of one etcd member.
type EtcdMember struct {
	// Name is the member's pod on kubeadm, RKE2 and k3s, and its Machine on Talos.
	Name string `json:"name"`
	// ID is etcd's own number for the member.
	ID      uint64 `json:"id,omitempty"`
	Leader  bool   `json:"leader,omitempty"`
	Learner bool   `json:"learner,omitempty"`
	// DBSize is the database file's size, and DBSizeInUse the part of it that holds data.
	DBSize      int64 `json:"dbSize"`
	DBSizeInUse int64 `json:"dbSizeInUse"`
	// QuotaPercent is DBSize as a share of the quota, filled in only where the status is shown.
	QuotaPercent int `json:"quotaPercent,omitempty"`
	// Alarms are the member's etcd alarms, NOSPACE once its database reached the quota.
	Alarms []string `json:"alarms,omitempty"`
	// Errors are what the member reports to be wrong with itself, such as having no leader.
	Errors []string `json:"errors,omitempty"`
}

// Fragmentation is the share of the database file that is free space, in percent.
func (m *EtcdMember) Fragmentation() int {
	if m.DBSize <= 0 || m.DBSizeInUse >= m.DBSize {
		return 0
	}
	return int((m.DBSize - m.DBSizeInUse) * 100 / m.DBSize)
}

// Alarmed tells whether the member refuses writes for want of space.
func (m *EtcdMember) Alarmed() bool { return slices.Contains(m.Alarms, alarmNoSpace) }

// Healthy tells whether the member votes and reports nothing wrong.
func (m *EtcdMember) Healthy() bool { return !m.Learner && len(m.Errors) == 0 }

// EtcdStatus is what the controller last read of a cluster's etcd members.
type EtcdStatus struct {
	At         time.Time    `json:"at"`
	QuotaBytes int64        `json:"quotaBytes"`
	Members    []EtcdMember `json:"members"`
	// Error is why some member could not be read.
	Error string `json:"error,omitempty"`
}

// LastEtcdStatus reads a cluster's recorded etcd status.
func LastEtcdStatus(c *clusterv1.Cluster) (EtcdStatus, bool) {
	var s EtcdStatus
	raw, ok := c.Annotations[EtcdStatusAnnotation]
	if !ok || json.Unmarshal([]byte(raw), &s) != nil {
		return EtcdStatus{}, false
	}
	return s, true
}

// Warning says what is wrong with the members' databases, or is "" when nothing is: a NOSPACE alarm,
// or a database above EtcdWarnPercent of the quota.
func (s *EtcdStatus) Warning() string {
	var parts []string
	for i := range s.Members {
		m := &s.Members[i]
		switch {
		case m.Alarmed():
			parts = append(parts, fmt.Sprintf("etcd member %s refuses writes, its database is full", m.Name))
		case s.QuotaBytes > 0 && m.DBSize*100 > s.QuotaBytes*EtcdWarnPercent:
			parts = append(parts, fmt.Sprintf("etcd member %s is at %d%% of its limit", m.Name, s.Percent(m)))
		}
	}
	return strings.Join(parts, ", ")
}

// Percent is the share of the quota a member's database takes, rounded to the nearest whole percent.
// Every place that shows the share takes it from here.
func (s *EtcdStatus) Percent(m *EtcdMember) int {
	if s.QuotaBytes <= 0 {
		return 0
	}
	return int((m.DBSize*100 + s.QuotaBytes/2) / s.QuotaBytes)
}

// EtcdDefragRequest asks for a defragmentation at once.
type EtcdDefragRequest struct {
	By string    `json:"by,omitempty"`
	At time.Time `json:"at"`
}

// EtcdDefrag is the last defragmentation.
type EtcdDefrag struct {
	At time.Time `json:"at"`
	// Members are those defragmented, in order.
	Members []string `json:"members"`
	Error   string   `json:"error,omitempty"`
}

// EtcdMaintenance reads and defragments a workload cluster's etcd members.
type EtcdMaintenance interface {
	// Members reads every control-plane member. Members it could not read are left out, with an
	// error that names them.
	Members(ctx context.Context, cluster *clusterv1.Cluster) ([]EtcdMember, error)
	// Defragment defragments the named member.
	Defragment(ctx context.Context, cluster *clusterv1.Cluster, member string) error
	// Disarm clears the NOSPACE alarms, which etcd keeps until told after space is freed.
	Disarm(ctx context.Context, cluster *clusterv1.Cluster) error
}

// defragOrder puts the members a defragmentation visits in order: followers by name, the leader
// last, so that the member that blocks its reads and writes is not the one clients are served by.
func defragOrder(members []EtcdMember) []EtcdMember {
	ordered := slices.Clone(members)
	slices.SortStableFunc(ordered, func(a, b EtcdMember) int {
		if a.Leader != b.Leader {
			if a.Leader {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ordered
}

// defragPlan names the members to defragment, in order. A request takes all of them. Otherwise a
// full database takes every member at once, and a large, fragmented one waits for the window.
func defragPlan(members []EtcdMember, allowance defragAllowance) []string {
	full := slices.ContainsFunc(members, func(m EtcdMember) bool { return m.Alarmed() })
	var plan []string
	for _, m := range defragOrder(members) {
		if allowance == defragAsked || full || (allowance == defragInWindow && m.DBSize >= etcdDefragMinBytes && m.Fragmentation() > etcdDefragPercent) {
			plan = append(plan, m.Name)
		}
	}
	return plan
}

// EtcdMaintainer reads every cluster's etcd members now and then, records their database sizes on
// the Cluster, and defragments them one at a time, followers first, inside the cluster's upgrade
// window or when asked. etcd gives file space back to the disk only when defragmented.
type EtcdMaintainer struct {
	Client         client.Client
	Reader         client.Reader
	ClassNamespace string
	Etcd           EtcdMaintenance
	// Events reports each defragmentation and a database that grows full. It may be nil.
	Events events.EventRecorder
	// Interval is how often members are read. Zero is etcdReadInterval.
	Interval time.Duration
	Now      func() time.Time
}

// SetupWithManager registers the maintainer. It looks at one cluster after another, since a
// defragmentation blocks a member.
func (m *EtcdMaintainer) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("etcd-maintenance").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, ReconciliationTimeout: etcdDefragTimeout + time.Minute}).
		Complete(m)
	if err != nil {
		return fmt.Errorf("setting up the etcd maintainer: %w", err)
	}
	return nil
}

// Reconcile implements reconcile.Reconciler.
func (m *EtcdMaintainer) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &clusterv1.Cluster{}
	if err := m.Reader.Get(ctx, req.NamespacedName, c); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading the cluster: %w", err)
	}
	if !ownedAndLive(c, m.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	if RestoreInProgress(c) || !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterControlPlaneInitializedCondition) {
		return ctrl.Result{RequeueAfter: etcdSnapshotWait}, nil
	}
	trigger := defragScheduled
	if _, asked := c.Annotations[EtcdDefragRequestAnnotation]; asked {
		trigger = defragRequested
	}
	now, interval := m.now(), cmp.Or(m.Interval, etcdReadInterval)
	last, recorded := LastEtcdStatus(c)
	if trigger == defragScheduled && recorded && now.Sub(last.At) < interval {
		return ctrl.Result{RequeueAfter: interval - now.Sub(last.At)}, nil
	}
	settings, err := EtcdSettingsOf(c)
	if err != nil {
		return ctrl.Result{}, err
	}
	return m.maintain(ctx, c, &settings, &last, trigger, interval)
}

// maintain reads the members, defragments those due, and records both.
func (m *EtcdMaintainer) maintain(ctx context.Context, c *clusterv1.Cluster, settings *EtcdSettings, previous *EtcdStatus, trigger defragTrigger, interval time.Duration) (ctrl.Result, error) {
	rctx, cancel := context.WithTimeout(ctx, etcdReadTimeout)
	members, readErr := m.Etcd.Members(rctx, c)
	cancel()
	status := EtcdStatus{At: m.now().UTC(), QuotaBytes: settings.QuotaBytes(), Members: members}
	if readErr != nil {
		status.Error = readErr.Error()
	}
	defrag := m.defragment(ctx, c, &status, trigger)
	if err := m.record(ctx, c, &status, defrag, trigger); err != nil {
		return ctrl.Result{}, err
	}
	if w := status.Warning(); w != "" && w != previous.Warning() {
		m.eventf(c, corev1.EventTypeWarning, "EtcdDatabaseLarge", "%s", w)
	}
	if readErr != nil {
		m.eventf(c, corev1.EventTypeWarning, "EtcdUnreadable", "reading the etcd members: %v", readErr)
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// defragment visits the members due, one at a time and the leader last, and returns the record of it,
// or nil when none was due. The status is read again afterwards.
func (m *EtcdMaintainer) defragment(ctx context.Context, c *clusterv1.Cluster, status *EtcdStatus, trigger defragTrigger) *EtcdDefrag {
	v := m.defragGate(c, status, trigger)
	if !v.proceed {
		return v.refusal
	}
	plan := defragPlan(status.Members, v.allowance)
	if len(plan) == 0 {
		return nil
	}
	done := m.visit(ctx, c, status, plan, &v.policy, trigger)
	m.afterDefrag(ctx, c, status, done)
	return done
}

// defragVerdict is whether a defragmentation may start, and what lets it.
type defragVerdict struct {
	policy    UpgradePolicy
	allowance defragAllowance
	// refusal says why a request cannot run. A scheduled run that cannot start stays quiet.
	refusal *EtcdDefrag
	proceed bool
}

// defragGate decides whether a defragmentation may start at all. A defragmentation blocks a member,
// which the others must carry: with one unread or unwell, none is started.
func (m *EtcdMaintainer) defragGate(c *clusterv1.Cluster, status *EtcdStatus, trigger defragTrigger) defragVerdict {
	var v defragVerdict
	refuse := func(reason string) defragVerdict {
		if trigger == defragRequested {
			v.refusal = &EtcdDefrag{At: m.now().UTC(), Error: reason}
		}
		return v
	}
	if status.Error != "" {
		return refuse("not all etcd members could be read: " + status.Error)
	}
	policy, _, err := optionalVariable[UpgradePolicy](c, "upgrades")
	if err != nil {
		return v
	}
	v.policy = policy
	for i := range status.Members {
		if !status.Members[i].Healthy() {
			return refuse(fmt.Sprintf("etcd member %s is not healthy", status.Members[i].Name))
		}
	}
	v.proceed = true
	switch open, _ := policy.Window.Open(m.now()); {
	case trigger == defragRequested:
		v.allowance = defragAsked
	case open && settled(c):
		v.allowance = defragInWindow
	}
	return v
}

// visit defragments the planned members one at a time and stops at the first failure. A scheduled
// run also stops when the window closes, unless a full database makes it urgent.
func (m *EtcdMaintainer) visit(ctx context.Context, c *clusterv1.Cluster, status *EtcdStatus, plan []string, policy *UpgradePolicy, trigger defragTrigger) *EtcdDefrag {
	done := &EtcdDefrag{At: m.now().UTC(), Members: []string{}}
	byName := map[string]EtcdMember{}
	for _, member := range status.Members {
		byName[member.Name] = member
	}
	urgent := trigger == defragRequested || slices.ContainsFunc(status.Members, func(x EtcdMember) bool { return x.Alarmed() })
	dctx, cancel := context.WithTimeout(ctx, etcdDefragTimeout)
	defer cancel()
	for _, name := range plan {
		if stillOpen, _ := policy.Window.Open(m.now()); !urgent && !stillOpen {
			break
		}
		before := byName[name]
		mctx, stop := context.WithTimeout(dctx, etcdDefragMemberTimeout)
		err := m.Etcd.Defragment(mctx, c, name)
		stop()
		if err != nil {
			done.Error = fmt.Sprintf("defragmenting etcd member %s: %v", name, err)
			m.eventf(c, corev1.EventTypeWarning, "EtcdDefragFailed", "%s", done.Error)
			break
		}
		done.Members = append(done.Members, name)
		m.eventf(c, corev1.EventTypeNormal, "EtcdDefragmented", "defragmented etcd member %s, which held %s of a %s database",
			name, formatBytes(before.DBSizeInUse), formatBytes(before.DBSize))
	}
	return done
}

// afterDefrag reads the members again, and clears the NOSPACE alarm once every database is under the
// quota: etcd keeps refusing writes until the alarm is cleared, whatever space was freed.
func (m *EtcdMaintainer) afterDefrag(ctx context.Context, c *clusterv1.Cluster, status *EtcdStatus, done *EtcdDefrag) {
	if len(done.Members) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, etcdReadTimeout)
	defer cancel()
	members, err := m.Etcd.Members(rctx, c)
	if err != nil {
		return
	}
	status.Members = members
	alarmed := slices.ContainsFunc(members, func(x EtcdMember) bool { return x.Alarmed() })
	full := slices.ContainsFunc(members, func(x EtcdMember) bool { return x.DBSize >= status.QuotaBytes })
	if !alarmed || full {
		return
	}
	if err := m.Etcd.Disarm(rctx, c); err != nil {
		m.eventf(c, corev1.EventTypeWarning, "EtcdAlarmNotCleared", "clearing etcd's NOSPACE alarm: %v", err)
		return
	}
	m.eventf(c, corev1.EventTypeNormal, "EtcdAlarmCleared", "cleared etcd's NOSPACE alarm, the databases are under the quota again")
	for i := range status.Members {
		status.Members[i].Alarms = slices.DeleteFunc(status.Members[i].Alarms, func(a string) bool { return a == alarmNoSpace })
	}
}

// record writes the status, the defragmentation if there was one, and takes a request away.
func (m *EtcdMaintainer) record(ctx context.Context, c *clusterv1.Cluster, status *EtcdStatus, defrag *EtcdDefrag, trigger defragTrigger) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("recording the etcd status: %w", err)
	}
	patch := client.MergeFrom(c.DeepCopy())
	metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdStatusAnnotation, string(raw))
	if defrag != nil {
		rawDefrag, err := json.Marshal(defrag)
		if err != nil {
			return fmt.Errorf("recording the defragmentation: %w", err)
		}
		metav1.SetMetaDataAnnotation(&c.ObjectMeta, EtcdDefragAnnotation, string(rawDefrag))
	}
	if trigger == defragRequested {
		delete(c.Annotations, EtcdDefragRequestAnnotation)
	}
	if err := m.Client.Patch(ctx, c, patch); err != nil {
		return fmt.Errorf("recording the etcd status: %w", err)
	}
	return nil
}

func (m *EtcdMaintainer) eventf(c *clusterv1.Cluster, kind, reason, format string, args ...any) {
	if m.Events != nil {
		m.Events.Eventf(c, nil, kind, reason, "MaintainEtcd", format, args...)
	}
}

func (m *EtcdMaintainer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// formatBytes writes a size in the binary unit that fits.
func formatBytes(n int64) string {
	const unit = 1024
	value := float64(n)
	for _, suffix := range []string{"B", "KiB", mebibytes, "GiB"} {
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
		value /= unit
	}
	return fmt.Sprintf("%.1f TiB", value)
}

var errNoEtcdMembers = errors.New("no etcd member of the cluster is running and ready")
