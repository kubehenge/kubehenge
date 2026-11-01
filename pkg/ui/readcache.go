package ui

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// readCacheKeep is how long a read stays kept: longer than any caller takes one as fresh.
	readCacheKeep = time.Minute
	// clusterReads is how many clusters are read at once for a page about all of them.
	clusterReads = 4
)

// readCache keeps each cluster's last read of one kind, and makes one read at a time per cluster
// however many look, so ten viewers cost one round of reads.
type readCache[T any] struct {
	mu        sync.Mutex
	byCluster map[string]*cachedRead[T]
}

type cachedRead[T any] struct {
	done     chan struct{}
	inFlight bool
	view     T
	at       time.Time
}

// get answers with a read younger than fresh. It waits for one in flight or starts one. The read
// runs on past a viewer who leaves, for the others waiting on it.
func (rc *readCache[T]) get(ctx context.Context, key string, fresh time.Duration, now func() time.Time, read func(context.Context) T) (T, error) {
	rc.mu.Lock()
	r, ok := rc.byCluster[key]
	if !ok || !r.inFlight && now().Sub(r.at) >= fresh {
		r = &cachedRead[T]{done: make(chan struct{}), inFlight: true}
		if rc.byCluster == nil {
			rc.byCluster = map[string]*cachedRead[T]{}
		}
		for k, kept := range rc.byCluster {
			if !kept.inFlight && now().Sub(kept.at) > readCacheKeep {
				delete(rc.byCluster, k)
			}
		}
		rc.byCluster[key] = r
		detached := context.WithoutCancel(ctx)
		go func() {
			v := read(detached)
			rc.mu.Lock()
			r.view, r.at, r.inFlight = v, now(), false
			rc.mu.Unlock()
			close(r.done)
		}()
	}
	rc.mu.Unlock()
	select {
	case <-r.done:
		rc.mu.Lock()
		defer rc.mu.Unlock()
		return r.view, nil
	case <-ctx.Done():
		var none T
		return none, fmt.Errorf("waiting for the cluster's read: %w", ctx.Err())
	}
}
