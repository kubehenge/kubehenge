package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// changeBeat is how often a quiet stream says it is still there, well inside the minute after
	// which proxies close a connection that sends nothing.
	changeBeat = 25 * time.Second
	// changeGap is the least time between two tellings on one stream: a cluster coming up changes
	// its objects many times a second, and each telling has every open page read again.
	changeGap = time.Second
	// rewatchAfter is how long a watch that ended waits before starting again, and how often a
	// running one checks that its management cluster is still the current one.
	rewatchAfter = 5 * time.Second
)

// changes tells the pages' streams that the management cluster's clusters or machines changed. A
// stream holds at most one telling, so a slow one is told once rather than falling behind. Its
// zero value is ready to use.
type changes struct {
	mu      sync.Mutex
	streams map[chan struct{}]struct{}
	closed  bool
}

// listen returns the channel a stream is told on, closed once the server stops, and the function
// that stops telling it.
func (c *changes) listen() (<-chan struct{}, func()) {
	told := make(chan struct{}, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		close(told)
		return told, func() {}
	}
	if c.streams == nil {
		c.streams = map[chan struct{}]struct{}{}
	}
	c.streams[told] = struct{}{}
	return told, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.streams, told)
	}
}

func (c *changes) tell() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for told := range c.streams {
		select {
		case told <- struct{}{}:
		default:
		}
	}
}

// close ends every stream, so that the server's shutdown does not wait on them.
func (c *changes) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for told := range c.streams {
		close(told)
	}
	c.streams = nil
}

// watchedLists are the objects whose changes the pages are told of: a cluster's own status
// carries its control plane's and workers' counts, and its machines are listed on their own.
var watchedLists = []func() client.ObjectList{
	func() client.ObjectList { return &clusterv1.ClusterList{} },
	func() client.ObjectList { return &clusterv1.MachineList{} },
}

// watchChanges tells the streams of every change to the management cluster's clusters and
// machines until ctx ends, watching again whenever a watch ends or another management cluster
// becomes the current one.
func (s *Server) watchChanges(ctx context.Context) {
	defer s.changes.close()
	failed := ""
	for {
		if kube, ok := s.currentKube().(client.WithWatch); ok {
			err := s.watchRound(ctx, kube)
			// A watch refused once is refused every round. It is logged when it starts failing.
			if text := fmt.Sprint(err); err != nil && text != failed {
				s.Log.Warn("watching the clusters for the interface's pages failed; they poll meanwhile", "error", err)
				failed = text
			} else if err == nil {
				failed = ""
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchAfter):
		}
	}
}

// watchRound watches from the current state on until a watch ends or kube is no longer the
// current management cluster. Whatever changed before it started is told as one change.
func (s *Server) watchRound(ctx context.Context, kube client.WithWatch) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan error, len(watchedLists))
	watches := make([]watch.Interface, 0, len(watchedLists))
	defer func() {
		for _, w := range watches {
			w.Stop()
		}
	}()
	for _, newList := range watchedLists {
		w, err := watchFromNow(ctx, kube, newList)
		if err != nil {
			return err
		}
		watches = append(watches, w)
		go func() {
			for ev := range w.ResultChan() {
				if ev.Type == watch.Error {
					ended <- fmt.Errorf("the watch ended: %v", ev.Object)
					return
				}
				s.changes.tell()
			}
			ended <- nil
		}()
	}
	s.changes.tell()
	check := time.NewTicker(rewatchAfter)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-ended:
			return err
		case <-check.C:
			if s.currentKube() != kube {
				return nil
			}
		}
	}
}

// watchFromNow watches a kind from its current resource version, which a list of one item reads,
// so that the watch does not start by sending every object again.
func watchFromNow(ctx context.Context, kube client.WithWatch, newList func() client.ObjectList) (watch.Interface, error) { //nolint:ireturn // the client's own watch, of whichever kind
	list := newList()
	if err := kube.List(ctx, list, client.Limit(1)); err != nil {
		return nil, fmt.Errorf("listing to watch: %w", err)
	}
	w, err := kube.Watch(ctx, newList(), &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: list.GetResourceVersion()}})
	if err != nil {
		return nil, fmt.Errorf("watching: %w", err)
	}
	return w, nil
}

// changeStream tells the page, as Server-Sent Events, each time the clusters or machines change,
// until the page leaves, its session ends or the server stops. An event names no cluster: the page
// reads again through the API, which shows each user only what they may see.
func (s *Server) changeStream(w http.ResponseWriter, r *http.Request, _ user) {
	rc := http.NewResponseController(w)
	// The interface's write timeout bounds answers, and this one lasts as long as the page.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// Proxies such as nginx otherwise hold the events back until a buffer fills.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(text string) bool {
		if _, err := io.WriteString(w, text); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	told, stop := s.changes.listen()
	defer stop()
	beat := time.NewTicker(changeBeat)
	defer beat.Stop()
	if !send(": watching\n\n") {
		return
	}
	for {
		text, change := ": still watching\n\n", false
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
		case _, open := <-told:
			if !open {
				return
			}
			text, change = "event: clusters\ndata:\n\n", true
		}
		if s.sessions.lookup(r) == nil || !send(text) {
			return
		}
		if !change {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(changeGap):
		}
	}
}
