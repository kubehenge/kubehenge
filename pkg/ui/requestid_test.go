package ui

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// syncBuffer collects log lines that handlers may write from more than one goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRequestIDsReachTheAnswerAndTheLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var logged syncBuffer
	h.server.Log = withRequestIDs(slog.New(slog.NewJSONHandler(&logged, nil)))
	b := h.browser()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://kubehenge.example/api/session",
		strings.NewReader(`{"username":"admin@pve","password":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "forged")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	expect(t, rec, http.StatusOK)
	id := rec.Header().Get(requestIDHeader)
	if id == "" || id == "forged" {
		t.Fatalf("request id %q: want a fresh one of the interface's own", id)
	}
	var line struct {
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(logged.String(), "\n", 2)[0]), &line); err != nil {
		t.Fatalf("log %q: %v", logged.String(), err)
	}
	if line.Msg != "signed in" || line.RequestID != id {
		t.Fatalf("logged %+v, want the sign-in with request id %s", line, id)
	}

	rec = b.do(http.MethodGet, "/api/no-such-thing", "")
	expect(t, rec, http.StatusNotFound)
	var answer struct {
		Error     string `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.RequestID == "" || answer.RequestID != rec.Header().Get(requestIDHeader) || answer.RequestID == id {
		t.Fatalf("error answer %+v with header %q: want this request's own id", answer, rec.Header().Get(requestIDHeader))
	}
}

func TestRequestIDsWrapTheLoggerOnce(t *testing.T) {
	t.Parallel()
	l := withRequestIDs(slog.New(slog.DiscardHandler))
	if withRequestIDs(l) != l {
		t.Fatal("a logger already carrying request ids is wrapped again")
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := h.browser().do(http.MethodGet, "/healthz", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != "ok\n" {
		t.Fatalf("healthz = %q, want ok and nothing more", rec.Body)
	}
}
