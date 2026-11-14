// Package backupstore keeps objects as files on a volume and serves them over HTTP to holders of a
// token. It runs in the management cluster on the backup volume. The appliance reaches it through
// the Kubernetes API's service proxy, and the controller through its Service.
package backupstore

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// TokenHeader carries the store's token. The API server's service proxy drops Authorization, so
// the token travels in a header of its own.
const TokenHeader = "X-Kubehenge-Store-Token" //nolint:gosec // a header's name

// MetadataHeader carries an object's metadata as a JSON object.
const MetadataHeader = "X-Kubehenge-Metadata"

// maxMetadata bounds the metadata header a Put may carry.
const maxMetadata = 8 << 10

// Server serves the objects under Dir to clients that send Token.
type Server struct {
	Dir   string
	Token string
}

// Entry is one object in a listing.
type Entry struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Handler serves the store.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /list", s.authorised(s.list))
	mux.HandleFunc("PUT /objects/{key...}", s.authorised(s.put))
	mux.HandleFunc("GET /objects/{key...}", s.authorised(s.get))
	mux.HandleFunc("HEAD /objects/{key...}", s.authorised(s.get))
	mux.HandleFunc("DELETE /objects/{key...}", s.authorised(s.remove))
	return mux
}

func (s *Server) authorised(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(s.Token)) != 1 {
			http.Error(w, "the store's token is missing or wrong: send it in "+TokenHeader, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// requestKey reads the object's key from the path, and refuses the request when it names none.
func requestKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if err := checkKey(key); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, provisioner.ErrNoObject) {
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	var meta map[string]string
	if raw := r.Header.Get(MetadataHeader); raw != "" {
		if len(raw) > maxMetadata || json.Unmarshal([]byte(raw), &meta) != nil {
			http.Error(w, MetadataHeader+" is not a JSON object of strings of at most 8 KiB", http.StatusBadRequest)
			return
		}
	}
	size, err := Dir{Root: s.Dir}.Put(r.Context(), key, r.Body, meta)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("X-Kubehenge-Size", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	d := Dir{Root: s.Dir}
	f, err := d.open(key)
	if err != nil {
		fail(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		fail(w, fmt.Errorf("reading %s: %w", key, err))
		return
	}
	if meta, err := os.ReadFile(d.metaPath(key)); err == nil {
		w.Header().Set(MetadataHeader, string(meta))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", info.ModTime(), io.ReadSeeker(f))
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if err := (Dir{Root: s.Dir}).Remove(r.Context(), key); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	list, err := Dir{Root: s.Dir}.List(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		fail(w, err)
		return
	}
	entries := make([]Entry, 0, len(list))
	for _, o := range list {
		entries = append(entries, Entry{Key: o.Key, Size: o.Size, Modified: o.Modified})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		fail(w, fmt.Errorf("listing: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
