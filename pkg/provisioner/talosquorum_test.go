package provisioner

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/tools/events"
)

// threeControlPlanes is a Talos cluster of three control planes on v1.14.3, upgrading to v1.14.4.
func threeControlPlanes(t *testing.T) (*talosRig, *events.FakeRecorder) {
	t.Helper()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"),
		talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"),
		talosMachine("web-cp-1", "192.0.2.11", "v1.14.3"),
		talosMachine("web-cp-2", "192.0.2.12", "v1.14.3"))
	rec := events.NewFakeRecorder(20)
	r.u.Events = rec
	return r, rec
}

func TestTalosUpgradeWaitsForAnUnhealthyEtcdMember(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		etcd   map[string]EtcdMember
		listed []EtcdListed
		down   map[string]bool
		want   string
	}{
		{"an error", map[string]EtcdMember{"192.0.2.11": {ID: 11, Errors: []string{"etcdserver: no leader"}}}, nil, nil, "etcd member web-cp-1 is unhealthy: etcdserver: no leader"},
		{"a learner", map[string]EtcdMember{"192.0.2.12": {ID: 12, Learner: true}}, nil, nil, "etcd member web-cp-2 is still joining as a learner"},
		{"a full database", map[string]EtcdMember{"192.0.2.12": {ID: 12, Alarms: []string{"NOSPACE"}}}, nil, nil, "etcd member web-cp-2 refuses writes"},
		{"an unreachable node", nil, nil, map[string]bool{"192.0.2.11": true}, "etcd on web-cp-1 cannot be read"},
		{
			"a learner in the list", nil,
			[]EtcdListed{{ID: 1, Name: "web-cp-0"}, {ID: 2, Name: "web-cp-1"}, {ID: 3, Name: "web-cp-2"}, {ID: 4, Name: "web-cp-3", Learner: true}},
			nil, "etcd member web-cp-3 is a learner",
		},
		{
			"fewer voters than control planes", nil,
			[]EtcdListed{{ID: fakeMemberID("192.0.2.11"), Name: "web-cp-1"}, {ID: fakeMemberID("192.0.2.12"), Name: "web-cp-2"}},
			nil, "etcd has 2 voting members for 3 control planes",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, rec := threeControlPlanes(t)
			r.talos.etcd, r.talos.listed, r.talos.down = c.etcd, c.listed, c.down
			cl := r.run(t)
			if len(r.talos.installed) != 0 || len(r.talos.rebooted) != 0 || cl.Annotations[TalosUpgradingAnnotation] != "" {
				t.Fatalf("started: installed %v, rebooted %v, %v", r.talos.installed, r.talos.rebooted, cl.Annotations)
			}
			if note := cl.Annotations[TalosUpgradeWaitingAnnotation]; !strings.Contains(note, "node web-cp-0 waits") || !strings.Contains(note, c.want) {
				t.Fatalf("waiting note %q, want %q", note, c.want)
			}
			if event := <-rec.Events; !strings.Contains(event, "TalosUpgradeWaiting") || !strings.Contains(event, c.want) {
				t.Fatalf("event %q", event)
			}
			// The same wait is not reported again.
			r.run(t)
			select {
			case event := <-rec.Events:
				t.Fatalf("a second event: %q", event)
			default:
			}
		})
	}
}

func TestTalosUpgradeGoesOnOnceEtcdIsHealthy(t *testing.T) {
	t.Parallel()
	r, _ := threeControlPlanes(t)
	r.talos.etcd = map[string]EtcdMember{"192.0.2.11": {ID: 11, Errors: []string{"no leader"}}}
	if c := r.run(t); c.Annotations[TalosUpgradeWaitingAnnotation] == "" || len(r.talos.installed) != 0 {
		t.Fatalf("did not wait: %v", c.Annotations)
	}
	r.talos.mu.Lock()
	r.talos.etcd = nil
	r.talos.mu.Unlock()
	c := r.run(t)
	if _, waiting := c.Annotations[TalosUpgradeWaitingAnnotation]; waiting {
		t.Fatalf("still waiting: %v", c.Annotations)
	}
	if r.talos.installed["192.0.2.10"] == "" || len(r.talos.rebooted) != 1 || r.talos.rebooted[0] != "192.0.2.10" {
		t.Fatalf("installed %v, rebooted %v", r.talos.installed, r.talos.rebooted)
	}
}

func TestTalosUpgradeOfTheOnlyControlPlaneDoesNotWait(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.talos.etcd = map[string]EtcdMember{"192.0.2.10": {ID: 10, Errors: []string{"no leader"}}}
	r.run(t)
	if len(r.talos.rebooted) != 1 {
		t.Fatalf("rebooted %v", r.talos.rebooted)
	}
}

func TestTalosUpgradeChecksEtcdAgainBeforeTheReboot(t *testing.T) {
	t.Parallel()
	r, _ := threeControlPlanes(t)
	// A member falls ill while the installer is written.
	r.talos.installing = func(context.Context) error {
		r.talos.mu.Lock()
		defer r.talos.mu.Unlock()
		r.talos.etcd = map[string]EtcdMember{"192.0.2.12": {ID: 12, Errors: []string{"no leader"}}}
		return nil
	}
	c := r.run(t)
	if len(r.talos.rebooted) != 0 || len(r.drainer.drained) != 0 {
		t.Fatalf("rebooted %v, drained %v", r.talos.rebooted, r.drainer.drained)
	}
	if !strings.Contains(c.Annotations[TalosUpgradeWaitingAnnotation], "web-cp-2") {
		t.Fatalf("annotations %v", c.Annotations)
	}
}
