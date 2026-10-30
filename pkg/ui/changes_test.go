package ui

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stream is a page's open stream of changes, read a line at a time.
type stream struct {
	t     *testing.T
	lines chan string
	ended chan error
}

func openStream(t *testing.T, h *harness, b *browser) *stream {
	t.Helper()
	srv := httptest.NewServer(h.handler)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", sessionCookie+"="+b.cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("the stream answered %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &stream{t: t, lines: make(chan string), ended: make(chan error, 1)}
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				s.ended <- err
				return
			}
			select {
			case s.lines <- strings.TrimSuffix(line, "\n"):
			case <-t.Context().Done():
				return
			}
		}
	}()
	return s
}

// next waits for a line of the stream that is not empty.
func (s *stream) next() string {
	s.t.Helper()
	for {
		select {
		case line := <-s.lines:
			if line != "" {
				return line
			}
		case err := <-s.ended:
			s.t.Fatalf("the stream ended: %v", err)
		case <-time.After(10 * time.Second):
			s.t.Fatal("nothing came on the stream")
		}
	}
}

// waitEnd waits for the server to end the stream.
func (s *stream) waitEnd() {
	s.t.Helper()
	for {
		select {
		case <-s.lines:
		case err := <-s.ended:
			if !errors.Is(err, io.EOF) {
				s.t.Fatalf("the stream broke rather than ending: %v", err)
			}
			return
		case <-time.After(10 * time.Second):
			s.t.Fatal("the stream did not end")
		}
	}
}

// watching starts telling the streams of changes, and has the browser's stream open on it.
func watching(t *testing.T, b *browser) (*harness, *stream, context.CancelFunc) {
	t.Helper()
	h := b.h
	b.signIn("viewer@pve")
	s := openStream(t, h, b)
	if line := s.next(); line != ": watching" {
		t.Fatalf("the stream began with %q", line)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go h.server.watchChanges(ctx)
	t.Cleanup(cancel)
	if line := s.next(); line != "event: clusters" {
		t.Fatalf("the watch's start was told as %q", line)
	}
	s.next()
	return h, s, cancel
}

func TestChangeStreamTellsOfEachChangeToTheClusters(t *testing.T) {
	t.Parallel()
	h, s, _ := watching(t, newHarness(t, cluster("default", "one")).browser())
	c := cluster("default", "two")
	if err := h.kube.Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if line := s.next(); line != "event: clusters" {
		t.Fatalf("a new cluster was told as %q", line)
	}
	if line := s.next(); line != "data:" {
		t.Fatalf("the event carried %q, not an empty line of data", line)
	}
}

func TestChangeStreamEndsWithTheSession(t *testing.T) {
	t.Parallel()
	b := newHarness(t, cluster("default", "one")).browser()
	h, s, _ := watching(t, b)
	expect(t, b.do("DELETE", "/api/session", ""), http.StatusNoContent)
	if err := h.kube.Create(t.Context(), cluster("default", "two")); err != nil {
		t.Fatal(err)
	}
	s.waitEnd()
}

func TestChangeStreamEndsWithTheServer(t *testing.T) {
	t.Parallel()
	_, s, stop := watching(t, newHarness(t, cluster("default", "one")).browser())
	stop()
	s.waitEnd()
}

func TestChangeStreamNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	expect(t, h.browser().do("GET", "/api/events", ""), http.StatusUnauthorized)
}

func TestChangesTellASlowStreamOnce(t *testing.T) {
	t.Parallel()
	var c changes
	told, stop := c.listen()
	defer stop()
	c.tell()
	c.tell()
	<-told
	select {
	case <-told:
		t.Fatal("two changes before the stream read were told twice")
	default:
	}
}
