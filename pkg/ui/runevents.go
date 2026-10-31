package ui

import (
	"net/http"
	"slices"
	"time"
)

// runEventsKept is how far back a run's events go: as long as a failed run is listed, which is as
// long as the management cluster's API server keeps events.
const runEventsKept = runsFailedShown

// runEvents answers with the management cluster's events about the cluster's objects since the
// time the query's since names, oldest first, repeats counted once. They are the log of the runs
// that keep none of their own: etcd snapshots and restores, and changes.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "since: a time such as 2026-10-31T09:00:00Z")
		return
	}
	since = later(since, s.now().Add(-runEventsKept))
	m := readManagement(r.Context(), u.kube, c)
	if m.eventsErr != nil {
		s.Log.ErrorContext(r.Context(), "reading a cluster's events", "cluster", c.Namespace+"/"+c.Name, "error", m.eventsErr)
		writeError(w, http.StatusBadGateway, "the events could not be read: "+problem(m.eventsErr))
		return
	}
	ours := clusterObject(c, &m)
	merged := eventMerge{}
	for i := range m.events {
		if ours(m.events[i].InvolvedObject) {
			merged.add(sourceManagement, &m.events[i], since)
		}
	}
	list := make([]eventView, 0, len(merged))
	for _, e := range merged {
		list = append(list, *e)
	}
	slices.SortFunc(list, func(x, y eventView) int { return x.Last.Compare(y.Last) })
	if len(list) > eventsShown {
		list = list[len(list)-eventsShown:]
	}
	writeJSON(w, http.StatusOK, list)
}

// later is the later of two times.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
