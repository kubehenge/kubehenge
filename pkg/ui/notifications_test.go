package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/notify"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// hook is a webhook target that keeps what it was sent.
type hook struct {
	*httptest.Server

	mu   sync.Mutex
	got  []notify.Notice
	fail bool
}

func newHook(t *testing.T) *hook {
	t.Helper()
	h := &hook{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var n notify.Notice
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			t.Error(err)
		}
		h.got = append(h.got, n)
	}))
	t.Cleanup(h.Close)
	return h
}

// taken returns what the hook was sent since it was last asked.
func (h *hook) taken() []notify.Notice {
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.got
	h.got = nil
	return got
}

func notifyTarget(t *testing.T, h *harness, purpose string, values map[string]string) {
	t.Helper()
	data := map[string][]byte{}
	for k, v := range values {
		data[k] = []byte(v)
	}
	secret := &corev1.Secret{
		Namespace: "default", Name: provisioner.CredentialName("", purpose), Labels: map[string]string{provisioner.CredentialLabel: purpose},
		Type: provisioner.CredentialType, Data: data,
	}
	if err := h.kube.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
}

func titles(list []notify.Notice) []string {
	out := make([]string, len(list))
	for i, n := range list {
		out[i] = n.Severity + " " + n.Title
	}
	return out
}

func TestAClusterProblemIsToldOnceItHasStoodAndAgainWhenItClears(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	h := newHarness(t, c)
	h.server.PublicURL = "https://kubehenge.example"
	target := newHook(t)
	notifyTarget(t, h, notify.PurposeWebhook, map[string]string{"url": target.URL})
	ctx := t.Context()
	setVMs := func(status metav1.ConditionStatus) {
		t.Helper()
		var live clusterv1.Cluster
		if err := h.kube.Get(ctx, client.ObjectKeyFromObject(c), &live); err != nil {
			t.Fatal(err)
		}
		meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{
			Type: provisioner.VMsCondition, Status: status, Reason: provisioner.VMsPaused, Message: "one-md-0-abc is paused.",
			LastTransitionTime: metav1.NewTime(h.clock),
		})
		// The harness's client has no status subresource, so an update writes the status too.
		if err := h.kube.Update(ctx, &live); err != nil {
			t.Fatal(err)
		}
	}
	setVMs(metav1.ConditionTrue)

	h.server.notifyOnce(ctx)
	if got := target.taken(); len(got) != 0 {
		t.Fatalf("told %v before the problem had stood %v", titles(got), notifyHold)
	}
	h.clock = h.clock.Add(notifyHold)
	h.server.notifyOnce(ctx)
	got := target.taken()
	if len(got) != 1 || got[0].Title != "one: VM paused" || got[0].Severity != notify.Warning ||
		got[0].Text != "one-md-0-abc is paused." || got[0].Link != "https://kubehenge.example/clusters/default/one/health" {
		t.Fatalf("told %+v, want the paused VM once", got)
	}
	h.clock = h.clock.Add(time.Minute)
	h.server.notifyOnce(ctx)
	if got := target.taken(); len(got) != 0 {
		t.Fatalf("told %v again", titles(got))
	}

	setVMs(metav1.ConditionFalse)
	h.clock = h.clock.Add(time.Minute)
	h.server.notifyOnce(ctx)
	if got := titles(target.taken()); len(got) != 1 || got[0] != "resolved Resolved: one: VM paused" {
		t.Fatalf("told %v, want it resolved", got)
	}
}

func TestATestNotificationGoesToEveryTargetAndSaysHowEachWent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.browser()
	a.signIn("admin@pve")
	expect(t, a.do("POST", "/api/notifications/test", ""), http.StatusConflict)

	good, bad := newHook(t), newHook(t)
	bad.fail = true
	notifyTarget(t, h, notify.PurposeWebhook, map[string]string{"url": good.URL})
	notifyTarget(t, h, notify.PurposeNtfy, map[string]string{"url": bad.URL})
	rec := a.do("POST", "/api/notifications/test", "")
	expect(t, rec, http.StatusOK)
	var views []notificationView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Kind != notify.PurposeWebhook || views[0].Error != "" ||
		views[1].Kind != notify.PurposeNtfy || !strings.Contains(views[1].Error, "503") {
		t.Fatalf("answered %+v", views)
	}
	if got := good.taken(); len(got) != 1 || got[0].Severity != notify.Info || !strings.Contains(got[0].Text, "admin@pve") {
		t.Fatalf("the webhook got %+v", got)
	}

	dev := h.browser()
	dev.signIn("dev@pve")
	expect(t, dev.do("POST", "/api/notifications/test", ""), http.StatusForbidden)
	expect(t, dev.do("GET", "/api/notifications", ""), http.StatusForbidden)
}

func problemNotice(cluster, what string, since time.Time) notify.Notice {
	return notify.Notice{Key: cluster + "/" + what, Severity: notify.Error, Title: cluster + ": " + what, Cluster: cluster, At: since}
}

func TestTheStateKeepsWhatWasNotReadAndDropsWhatIsGone(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var st notifyState
	both := &notifyFound{
		problems: []notify.Notice{problemNotice("default/a", "Unreachable", start), problemNotice("default/b", "VM paused", start)},
		read:     map[string]bool{"default/a": true, "default/b": true}, listed: true,
	}
	if got := st.step(start, both); len(got) != 0 {
		t.Fatalf("told %v at once", titles(got))
	}
	if got := st.step(start.Add(notifyHold), both); len(got) != 2 {
		t.Fatalf("told %v, want both", titles(got))
	}
	// a is read only in part and b's list failed: neither problem is taken for resolved.
	partial := &notifyFound{read: map[string]bool{"default/a": false}}
	if got := st.step(start.Add(notifyHold+time.Minute), partial); len(got) != 0 || len(st.Problems) != 2 {
		t.Fatalf("told %v, kept %d problems", titles(got), len(st.Problems))
	}
	// b is gone from a list that was read: dropped without a word. a was read and is fine.
	fine := &notifyFound{read: map[string]bool{"default/a": true}, listed: true}
	got := st.step(start.Add(notifyHold+2*time.Minute), fine)
	if len(got) != 1 || got[0].Title != "Resolved: default/a: Unreachable" || len(st.Problems) != 0 {
		t.Fatalf("told %v, kept %v", titles(got), st.Problems)
	}
}

func TestAFailedRunIsToldOnceAndOnlyIfItFailedAfterTheFirstLook(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	old := notify.Notice{Key: "run/backup/old", Title: "Back up failed", At: start.Add(-time.Hour)}
	st := notifyState{}
	if got := st.step(start, &notifyFound{runs: []notify.Notice{old}}); len(got) != 0 {
		t.Fatalf("told %v, a run that failed before the first look", titles(got))
	}
	fresh := notify.Notice{Key: "run/backup/new", Title: "Back up failed", At: start.Add(time.Minute)}
	found := &notifyFound{runs: []notify.Notice{old, fresh}}
	if got := st.step(start.Add(2*time.Minute), found); len(got) != 1 || got[0].Key != fresh.Key {
		t.Fatalf("told %v, want the new failure", titles(got))
	}
	if got := st.step(start.Add(3*time.Minute), found); len(got) != 0 {
		t.Fatalf("told %v again", titles(got))
	}
	st.step(start.Add(notifyRunsKept+2*time.Hour), &notifyFound{})
	if len(st.Runs) != 0 {
		t.Errorf("kept %v past %v", st.Runs, notifyRunsKept)
	}
}

func TestANoticeNoTargetTookIsToldAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	target := newHook(t)
	target.fail = true
	n := &h.server.notifier
	n.targets = []notify.Target{&notify.Webhook{URL: target.URL}}
	start := h.clock
	run := notify.Notice{Key: "run/template/x", Severity: notify.Error, Title: "Build template failed", At: start.Add(time.Minute)}
	n.state.Since = start
	tell := n.state.step(start.Add(2*time.Minute), &notifyFound{runs: []notify.Notice{run}})
	h.server.send(t.Context(), n, tell)
	if _, told := n.state.Runs[run.Key]; told || n.state.Sent[notify.PurposeWebhook].Error == "" {
		t.Fatalf("runs %v, sent %v: want the run kept to tell and the failure noted", n.state.Runs, n.state.Sent)
	}
	target.mu.Lock()
	target.fail = false
	target.mu.Unlock()
	tell = n.state.step(start.Add(3*time.Minute), &notifyFound{runs: []notify.Notice{run}})
	h.server.send(t.Context(), n, tell)
	if got := target.taken(); len(got) != 1 || got[0].Key != run.Key {
		t.Fatalf("told %v, want the run again", titles(got))
	}
}

func TestManyNoticesGoAsOneWithTheWorstFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	target := newHook(t)
	n := &h.server.notifier
	n.targets = []notify.Target{&notify.Webhook{URL: target.URL}}
	tell := make([]notify.Notice, 0, notifyBatch+1)
	for i := range notifyBatch + 1 {
		severity := notify.Warning
		if i == notifyBatch {
			severity = notify.Error
		}
		tell = append(tell, notify.Notice{Key: string(rune('a' + i)), Severity: severity, Title: "cluster " + string(rune('a'+i))})
	}
	h.server.send(t.Context(), n, tell)
	got := target.taken()
	if len(got) != 1 || got[0].Severity != notify.Error || !strings.HasPrefix(got[0].Text, "cluster f\n") {
		t.Fatalf("told %+v, want one notice, the error first", got)
	}
}
