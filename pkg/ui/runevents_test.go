package ui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// readRunEvents reads healthHarness's cluster's events since a time.
func readRunEvents(t *testing.T, b *browser, since time.Time) []eventView {
	t.Helper()
	rec := b.do("GET", "/api/clusters/default/one/events?since="+url.QueryEscape(since.Format(time.RFC3339)), "")
	expect(t, rec, http.StatusOK)
	var events []eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	return events
}

func messages(events []eventView) []string {
	out := make([]string, 0, len(events))
	for i := range events {
		out = append(out, events[i].Message)
	}
	return out
}

func TestRunEventsAreTheClustersSinceARunBegan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	restore := func(name, message string, ago time.Duration) *corev1.Event {
		return &corev1.Event{
			Namespace: "default", Name: name, Type: corev1.EventTypeNormal, Reason: "EtcdRestore", Message: message, Count: 1,
			InvolvedObject: corev1.ObjectReference{Kind: "Cluster", Name: "one"}, LastTimestamp: metav1.NewTime(now.Add(-ago)),
			Action: "RestoreEtcd",
		}
	}
	h, _ := healthHarness(t,
		restore("r1", "fetching the snapshot", 23*time.Hour),
		restore("r2", "older than a day", 25*time.Hour),
	)
	b := h.browser()
	b.signIn("viewer@pve")

	events := readRunEvents(t, b, now.Add(-48*time.Hour))
	if got, want := messages(events), []string{"fetching the snapshot", "Node one-md-0-a is not ready"}; !slices.Equal(got, want) {
		t.Errorf("events since two days ago %q, want %q: the cluster's own, oldest first, none older than a day", got, want)
	}
	if events[0].Action != "RestoreEtcd" || events[0].Object != "Cluster one" || events[0].Source != sourceManagement {
		t.Errorf("first event %+v, want the restore's action, its object and the management cluster as source", events[0])
	}
	if got := messages(readRunEvents(t, b, now.Add(-time.Hour))); !slices.Equal(got, []string{"Node one-md-0-a is not ready"}) {
		t.Errorf("events of the last hour %q, want only the one five minutes old", got)
	}
	if got := readRunEvents(t, b, now.Add(-time.Minute)); len(got) != 0 {
		t.Errorf("events of the last minute %q, want none", messages(got))
	}

	rec := b.do("GET", "/api/clusters/default/one/events?since=yesterday", "")
	expect(t, rec, http.StatusUnprocessableEntity)
}
