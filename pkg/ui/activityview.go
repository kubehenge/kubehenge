package ui

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// activityDays is how far back the page looks unless asked otherwise.
	activityDays = 7
	// activityShown bounds the entries and the events listed, newest first.
	activityShown = 500
	// activityEventWindow is how long the management cluster keeps its events.
	activityEventWindow = day
)

// activityView is who did what through the interface, and what the management cluster's
// controllers did, newest first.
type activityView struct {
	Since   time.Time          `json:"since"`
	Entries []activityEntry    `json:"entries"`
	Error   string             `json:"error,omitempty"`
	Events  section[eventView] `json:"events"`
}

// activityFrom is the start of the window a request asks for, in whole days up to activityKept. It
// answers the request when the window is out of bounds.
func (s *Server) activityFrom(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	days := activityDays
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || time.Duration(n)*day > activityKept {
			writeError(w, http.StatusBadRequest, "days must be 1 to "+strconv.Itoa(int(activityKept/day)))
			return time.Time{}, false
		}
		days = n
	}
	return s.now().Add(-time.Duration(days) * day), true
}

// activity answers the appliance's activity, for those who may see all of Proxmox VE's
// configuration (Sys.Audit on /).
func (s *Server) activity(w http.ResponseWriter, r *http.Request, u user) {
	if !s.require(w, r, u, "/", "Sys.Audit") {
		return
	}
	from, ok := s.activityFrom(w, r)
	if !ok {
		return
	}
	v := activityView{Since: from}
	s.activityEntries(r.Context(), &v, from, func(*activityEntry) bool { return true })
	v.Events = activityEvents(r.Context(), u.kube, u.namespaces, s.now(), func(*corev1.Event) bool { return true })
	writeJSON(w, http.StatusOK, v)
}

// clusterActivity answers one cluster's activity, for anyone who sees the cluster.
func (s *Server) clusterActivity(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	from, ok := s.activityFrom(w, r)
	if !ok {
		return
	}
	key := c.Namespace + "/" + c.Name
	v := activityView{Since: from}
	s.activityEntries(r.Context(), &v, from, func(e *activityEntry) bool { return e.Cluster == key })
	m := readManagement(r.Context(), u.kube, c)
	ours := clusterObject(c, &m)
	v.Events = activityEvents(r.Context(), u.kube, managedNamespaces{c.Namespace}, s.now(), func(e *corev1.Event) bool {
		return ours(e.InvolvedObject)
	})
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) activityEntries(ctx context.Context, v *activityView, from time.Time, keep func(*activityEntry) bool) {
	entries, err := s.activityLog.since(from, activityShown, keep)
	if err != nil {
		s.Log.ErrorContext(ctx, "reading the activity log", "error", err)
		v.Error = "The activity log could not be read."
	}
	v.Entries = entries
	if v.Entries == nil {
		v.Entries = []activityEntry{}
	}
}

// activityEvents are the management cluster's events of the last day in the namespaces that keep
// says to, repeats counted once.
func activityEvents(ctx context.Context, kube client.Client, namespaces managedNamespaces, now time.Time, keep func(*corev1.Event) bool) section[eventView] {
	out := section[eventView]{Items: []eventView{}}
	merged := eventMerge{}
	var problems []string
	for _, ns := range namespaces {
		var events corev1.EventList
		lctx, cancel := context.WithTimeout(ctx, healthDeadline)
		err := kube.List(lctx, &events, client.InNamespace(ns), client.Limit(healthListLimit))
		cancel()
		if err != nil {
			problems = append(problems, ns+": "+problem(err))
			continue
		}
		for i := range events.Items {
			if keep(&events.Items[i]) {
				merged.add(sourceManagement, &events.Items[i], now.Add(-activityEventWindow))
			}
		}
	}
	out.Error = strings.Join(problems, ", ")
	for _, e := range merged {
		out.Items = append(out.Items, *e)
	}
	slices.SortFunc(out.Items, func(x, y eventView) int { return cmp.Or(y.Last.Compare(x.Last), strings.Compare(x.Object, y.Object)) })
	if len(out.Items) > activityShown {
		out.Items, out.Truncated = out.Items[:activityShown], true
	}
	return out
}
