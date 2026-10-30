package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

// requestIDHeader carries the id of each request on its answer, which a user quotes as the
// reference an error shows, and which every line the interface logs for the request carries.
const requestIDHeader = "X-Request-ID"

// requestIDBytes make an id unique among an appliance's requests, and short enough to read out.
const requestIDBytes = 8

type requestIDKey struct{}

// withRequestID gives every request an id of its own. One a client sends is never taken: it could
// make one user's request look like another's in the journal.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	b := make([]byte, requestIDBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// requestID is the id of the request ctx belongs to, or "" outside one.
func requestID(ctx context.Context) string {
	id, ok := ctx.Value(requestIDKey{}).(string)
	if !ok {
		return ""
	}
	return id
}

// requestIDHandler adds the id of the request a line is logged for to the line.
type requestIDHandler struct {
	slog.Handler
}

// Handle implements slog.Handler.
func (h requestIDHandler) Handle(ctx context.Context, rec slog.Record) error { //nolint:gocritic // slog.Handler takes the record by value
	if id := requestID(ctx); id != "" {
		rec.AddAttrs(slog.String("requestId", id))
	}
	return h.Handler.Handle(ctx, rec) //nolint:wrapcheck // a handler's error is the logger's, passed on as it is
}

// WithAttrs implements slog.Handler.
func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// withRequestIDs is the logger, logging each line with the id of the request it is logged for.
func withRequestIDs(l *slog.Logger) *slog.Logger {
	if _, done := l.Handler().(requestIDHandler); done {
		return l
	}
	return slog.New(requestIDHandler{l.Handler()})
}
