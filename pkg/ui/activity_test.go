package ui

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The activity log takes the lines logged with a user, with their cluster and other attributes,
// marks those that carry an error as failed without its text, and takes none logged without a user
// or as an error.
func TestTheActivityLogKeepsWhatUsersDid(t *testing.T) {
	t.Parallel()
	l := newActivityLog(t.TempDir())
	log := withActivity(slog.New(slog.DiscardHandler), l).With("requestId", "abc")
	log.Info("changed cluster", "user", "admin@pve", "cluster", "default/one")
	log.Info("reading a cluster's backups", "cluster", "default/one", "error", "timeout")
	log.Error("a failure with a user", "user", "admin@pve")
	log.Warn("sign-in refused", "user", "mallory@pve", "address", "10.0.0.9")
	log.Warn("headlamp sign-in", "user", "bob@pve", "error", "secret detail")
	log.WithGroup("g").Info("grouped", "user", "admin@pve")

	got, err := l.since(time.Now().Add(-time.Hour), 10, func(*activityEntry) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	whats := make([]string, 0, len(got))
	for _, e := range got {
		whats = append(whats, e.User+" "+e.What)
	}
	if !slices.Equal(whats, []string{"bob@pve headlamp sign-in", "mallory@pve sign-in refused", "admin@pve changed cluster"}) {
		t.Fatalf("kept %q, newest first", whats)
	}
	if !got[0].Failed || got[1].Failed || got[0].Details["requestId"] != "abc" || len(got[0].Details) != 1 {
		t.Errorf("the failed action = %+v", got[0])
	}
	if got[1].Details["address"] != "10.0.0.9" || got[2].Cluster != "default/one" || got[2].Details["requestId"] != "abc" {
		t.Errorf("entries = %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(l.path), activityFile))
	if err != nil || strings.Count(string(raw), "\n") != 3 || strings.Contains(string(raw), "secret detail") {
		t.Errorf("the file holds %q, %v", raw, err)
	}
}

// Past its size the file keeps the newer half, and nothing older than activityKept.
func TestTheActivityLogStaysBounded(t *testing.T) {
	t.Parallel()
	l := newActivityLog(t.TempDir())
	l.maxBytes = 4 << 10
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 200 {
		if err := l.add(&activityEntry{At: start.Add(time.Duration(i) * time.Minute), User: "admin@pve", What: fmt.Sprintf("action %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(l.path); err != nil || fi.Size() > l.maxBytes {
		t.Fatalf("the file is %v bytes, %v", fi.Size(), err)
	}
	got, err := l.since(start, 1000, func(*activityEntry) bool { return true })
	if err != nil || len(got) == 0 || got[0].What != "action 199" {
		t.Fatalf("kept %d, newest %+v, %v", len(got), got, err)
	}
	if late := start.Add(activityKept + 2*time.Hour); l.add(&activityEntry{At: late, User: "a", What: strings.Repeat("x", 4<<10)}) != nil {
		t.Fatal("adding past the window")
	}
	got, _ = l.since(start, 1000, func(*activityEntry) bool { return true })
	if slices.ContainsFunc(got, func(e activityEntry) bool { return e.At.Before(start.Add(2 * time.Hour)) }) {
		t.Errorf("kept entries older than %v: %d left", activityKept, len(got))
	}
}

// The appliance's activity is for those who see all of Proxmox VE's configuration, a cluster's
// for those who see the cluster, with the management cluster's events of its objects.
func TestActivityShowsWhoDidWhat(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	event := func(name, object string, at time.Time) *corev1.Event {
		return &corev1.Event{
			Namespace: "default", Name: name, Type: corev1.EventTypeNormal, Reason: "Provisioned", Message: name, Count: 1,
			InvolvedObject: corev1.ObjectReference{Kind: "Cluster", Name: object}, LastTimestamp: metav1.NewTime(at),
		}
	}
	for _, e := range []*corev1.Event{
		event("recent", "one", h.clock.Add(-time.Hour)), event("other", "two", h.clock.Add(-time.Hour)),
		event("old", "one", h.clock.Add(-30*time.Hour)),
	} {
		if err := h.kube.Create(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	admin, viewer := h.browser(), h.browser()
	admin.signIn("admin@pve")
	viewer.signIn("viewer@pve")
	h.server.Log.Info("changed cluster", "user", "admin@pve", "cluster", "default/one")
	h.server.Log.Info("changed cluster", "user", "admin@pve", "cluster", "default/two")

	read := func(b *browser, path string) activityView {
		t.Helper()
		rec := b.do("GET", path, "")
		expect(t, rec, http.StatusOK)
		var v activityView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	messages := func(v activityView) []string {
		out := make([]string, 0, len(v.Events.Items))
		for _, e := range v.Events.Items {
			if e.Reason == "Provisioned" {
				out = append(out, e.Message)
			}
		}
		slices.Sort(out)
		return out
	}
	all := read(admin, "/api/activity")
	if len(all.Entries) != 4 || all.Entries[0].Cluster != "default/two" || all.Entries[3].What != "signed in" {
		t.Errorf("the appliance's entries = %+v", all.Entries)
	}
	if got := messages(all); !slices.Equal(got, []string{"other", "recent"}) {
		t.Errorf("the appliance's events = %q, want the last day's", got)
	}
	expect(t, viewer.do("GET", "/api/activity", ""), http.StatusForbidden)
	expect(t, admin.do("GET", "/api/activity?days=91", ""), http.StatusBadRequest)

	one := read(viewer, "/api/clusters/default/one/activity")
	if len(one.Entries) != 1 || one.Entries[0].Cluster != "default/one" {
		t.Errorf("cluster one's entries = %+v", one.Entries)
	}
	if got := messages(one); !slices.Equal(got, []string{"recent"}) {
		t.Errorf("cluster one's events = %q", got)
	}
}
