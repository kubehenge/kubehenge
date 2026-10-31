package csiproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// apiPrefix is where the Proxmox VE API's JSON calls are.
const apiPrefix = "/api2/json"

// holdersTTL is how long the list of holder VMs is taken as current for checking the records:
// Kubehenge's controller adds a holder at most every few minutes.
const holdersTTL = time.Minute

var (
	// contentPath is a call on one volume of a storage, whose name the rest of the path is, slashes
	// and all.
	contentPath = regexp.MustCompile(`^/nodes/([^/]+)/storage/([^/]+)/content/(.+)$`)
	// storagePath is one storage's configuration.
	storagePath = regexp.MustCompile(`^/storage/([^/]+)$`)
)

// Proxy passes the CSI plugin's calls on to the Proxmox VE API with the cluster's own token, which
// they carry, and answers three itself:
//
//   - A volume copy, which Proxmox VE lets only root@pam make, it makes with the token: it imports
//     the volume into a new disk of a holder VM of the cluster, on the source's node, detaches it as
//     an unused disk of the holder, and records it under the name the plugin asked for. The call
//     returns once the copy is done, as the plugin would have waited for Proxmox VE's task.
//   - Deleting a copy, which the token may not through the storage, it does through the holder.
//   - A storage's configuration, which Proxmox VE shows singly only to Datastore.Allocate, it reads
//     from the list of storages, which shows it to a token that may use the storage.
//
// Everything else goes on as it came, but for the copies' names: Proxmox VE chooses a copy's name
// itself, vm-<holder>-disk-<n>, so the proxy turns the plugin's name of each copy into the real
// one in the path, the query and the form, and back in every answer.
type Proxy struct {
	// Upstream is the Proxmox VE API's address, such as https://pve.example.org:8006.
	Upstream *url.URL
	// Transport reaches it. It verifies its certificate.
	Transport http.RoundTripper
	// Client keeps the copies' records in the workload cluster, and its events.
	Client client.Client
	Log    *slog.Logger
	// Owner is the VMID of the cluster's volume owner VM, which the plugin names volumes after.
	Owner int
	// Pool is the cluster's pool, and Holders the name of its holder VMs there.
	Pool, Holders string
	// Copies lets the proxy make copies. Without it, it only finds those made before.
	Copies bool
	// Pod is the proxy's own, which its events are about. When empty, it records none.
	Pod types.NamespacedName
	// Poll is how often a copy's task is looked at.
	Poll time.Duration

	// copying holds one copy or deletion of the cluster's at a time.
	copying sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[string]*job

	holdersMu   sync.Mutex
	holderIDs   []int
	holdersRead time.Time
}

// namesKey is the context key of the names a forwarded call is translated with.
type namesKey struct{}

// Handler serves the plugin's calls. It matches paths itself: a mux would answer a call on a
// storage's content with a redirect to a path of one of its volumes.
func (p *Proxy) Handler() http.Handler {
	forward := &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		ModifyResponse: modifyResponse,
		Transport:      p.Transport,
		ErrorLog:       slog.NewLogLogger(p.Log.Handler(), slog.LevelWarn),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, apiPrefix)
		if !ok {
			forward.ServeHTTP(w, r)
			return
		}
		if m := storagePath.FindStringSubmatch(path); m != nil && r.Method == http.MethodGet {
			p.storageConfig(w, r, m[1], forward)
			return
		}
		c := p.pveFor(r)
		copies, err := p.copies(r.Context(), c)
		if err != nil {
			p.Log.Error("reading the volume copies", "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "the volume copies cannot be read")
			return
		}
		n := newNames(copies)
		if m := contentPath.FindStringSubmatch(path); m != nil {
			switch r.Method {
			case http.MethodPost:
				p.copyVolume(w, r, n, m[1], m[2], m[3])
				return
			case http.MethodDelete:
				if p.deleteVolume(w, r, copies, m[2], m[3]) {
					return
				}
			}
		}
		forward.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), namesKey{}, n)))
	})
}

// copies are the copies in place whose records hold: named after the volume owner as the plugin
// names volumes, on a holder VM of the cluster. A record anyone else wrote turns no volume into
// another.
func (p *Proxy) copies(ctx context.Context, c *pve) (map[string]*Copy, error) {
	all, err := Copies(ctx, p.Client)
	if err != nil || len(all) == 0 {
		return all, err
	}
	holders, err := p.holderVMIDs(ctx, c, all)
	if err != nil {
		return nil, err
	}
	for id, cp := range all {
		storage, name, _ := splitVolID(cp.Real)
		if storage != cp.Storage || volumeVMID(cp.Volume) != p.Owner || volumeVMID(name) != cp.Holder || !slices.Contains(holders, cp.Holder) {
			p.Log.Warn("ignoring a volume copy's record that names no volume of the cluster's", "record", cp.Name, "volume", id, "copy", cp.Real)
			delete(all, id)
		}
	}
	return all, nil
}

// holderVMIDs are the VMIDs of the cluster's holder VMs, read again when older than holdersTTL or
// when a record names one not among them.
func (p *Proxy) holderVMIDs(ctx context.Context, c *pve, copies map[string]*Copy) ([]int, error) {
	p.holdersMu.Lock()
	defer p.holdersMu.Unlock()
	known := time.Since(p.holdersRead) < holdersTTL
	for _, cp := range copies {
		if !slices.Contains(p.holderIDs, cp.Holder) {
			known = false
		}
	}
	if known {
		return p.holderIDs, nil
	}
	holders, err := p.holders(ctx, c)
	if err != nil {
		return nil, err
	}
	p.holderIDs = p.holderIDs[:0]
	for _, h := range holders {
		p.holderIDs = append(p.holderIDs, h.VMID)
	}
	p.holdersRead = time.Now()
	return p.holderIDs, nil
}

// copyVolume makes the copy the plugin asks for, or finds it made, and answers once it is done.
func (p *Proxy) copyVolume(w http.ResponseWriter, r *http.Request, n names, node, storage, volume string) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "the copy's parameters cannot be read")
		return
	}
	target := r.Form.Get("target")
	if target == "" {
		writeError(w, http.StatusBadRequest, "a copy needs a target")
		return
	}
	source := volume
	if !strings.HasPrefix(source, storage+":") {
		source = storage + ":" + source
	}
	if realID, ok := n.toReal[source]; ok {
		source = realID
	}
	ask := &copyAsk{Storage: storage, Source: source, Target: target, Node: node, TargetNode: r.Form.Get("target_node")}
	j := p.startCopy(r.Context(), p.pveFor(r), ask)
	select {
	case <-j.done:
	case <-r.Context().Done():
		// The plugin gave up. The copy goes on, and its next call waits on it or finds it done.
		return
	}
	switch {
	case errors.Is(j.err, errRefused):
		writeErrors(w, http.StatusForbidden, map[string]string{"copy": j.err.Error()})
	case j.err != nil:
		writeErrors(w, http.StatusInternalServerError, map[string]string{"copy": j.err.Error()})
	default:
		// No task: the plugin then waits for none, as the copy is done.
		writeData(w, nil)
	}
}

// deleteVolume deletes a copy through its holder. It reports false for a volume that is no copy,
// whose call goes on to Proxmox VE. Only a copy among copies, whose records hold, is deleted.
func (p *Proxy) deleteVolume(w http.ResponseWriter, r *http.Request, copies map[string]*Copy, storage, volume string) bool {
	volume = strings.TrimPrefix(volume, storage+":")
	rec, found, err := p.record(r.Context(), storage, volume)
	switch {
	case err != nil:
		p.Log.Error("reading a volume copy's record", "volume", storage+":"+volume, "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "the volume copies cannot be read")
		return true
	case !found:
		return false
	case rec.State != stateDone:
		writeError(w, http.StatusConflict, "the volume is still being copied")
		return true
	}
	if _, ok := copies[rec.PluginID()]; !ok {
		writeError(w, http.StatusForbidden, "the record of "+rec.PluginID()+" names no copy of the cluster's")
		return true
	}
	if err := p.deleteCopy(r.Context(), p.pveFor(r), &rec); err != nil {
		p.Log.Error("deleting a volume copy", "volume", rec.PluginID(), "copy", rec.Real, "error", err.Error())
		writeErrors(w, http.StatusInternalServerError, map[string]string{"delete": err.Error()})
		return true
	}
	writeData(w, nil)
	return true
}

// rewrite sends a call on to Proxmox VE with the copies' real names.
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	r.SetURL(p.Upstream)
	n, ok := r.In.Context().Value(namesKey{}).(names)
	if !ok || n.empty() {
		return
	}
	// Proxmox VE's own encoding, which the translation needs to see through.
	r.Out.Header.Del("Accept-Encoding")
	if path, ok := strings.CutPrefix(r.In.URL.Path, apiPrefix); ok {
		if m := contentPath.FindStringSubmatch(path); m != nil {
			path = "/nodes/" + m[1] + "/storage/" + m[2] + "/content/" + n.volumePath(m[2], m[3])
		}
		r.Out.URL.Path, r.Out.URL.RawPath = strings.TrimSuffix(p.Upstream.Path, "/")+apiPrefix+path, ""
	}
	r.Out.URL.RawQuery = values(r.In.URL.Query(), n.toReal).Encode()
	if !isForm(r.In.Header) || r.In.Body == nil || r.In.Body == http.NoBody {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.In.Body, maxAnswerBytes))
	form, perr := url.ParseQuery(string(raw))
	if err == nil && perr == nil {
		raw = []byte(values(form, n.toReal).Encode())
	}
	r.Out.Body, r.Out.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	r.Out.Header.Set("Content-Length", strconv.Itoa(len(raw)))
}

// modifyResponse gives the plugin its own names of the copies back.
func modifyResponse(resp *http.Response) error {
	n, ok := resp.Request.Context().Value(namesKey{}).(names)
	if !ok || n.empty() || !isJSON(resp.Header) {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	_ = resp.Body.Close()
	if err != nil {
		return err //nolint:wrapcheck // the ReverseProxy logs it and answers 502
	}
	raw = translateJSON(raw, n.toPlugin)
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return nil
}

func isForm(h http.Header) bool {
	t, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && t == "application/x-www-form-urlencoded"
}

func isJSON(h http.Header) bool {
	t, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && (t == "application/json" || strings.HasSuffix(t, "+json"))
}

// storageConfig answers with the storage's entry of the storage list, or passes the call on when
// the list does not show it.
func (p *Proxy) storageConfig(w http.ResponseWriter, r *http.Request, storage string, forward http.Handler) {
	var list []json.RawMessage
	if err := p.pveFor(r).call(r.Context(), http.MethodGet, "/storage", nil, &list); err != nil {
		p.Log.Warn("listing the storages", "error", err.Error())
		forward.ServeHTTP(w, r)
		return
	}
	for _, entry := range list {
		var s struct {
			Storage string `json:"storage"`
		}
		if json.Unmarshal(entry, &s) == nil && s.Storage == storage {
			writeData(w, entry)
			return
		}
	}
	forward.ServeHTTP(w, r)
}

// writeData answers as Proxmox VE does, with the data in an envelope.
func writeData(w http.ResponseWriter, data any) {
	write(w, http.StatusOK, map[string]any{"data": data})
}

// writeErrors answers as Proxmox VE refuses a call, which the plugin's client shows with its
// status.
func writeErrors(w http.ResponseWriter, status int, errs map[string]string) {
	write(w, status, map[string]any{"data": nil, "errors": errs})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeErrors(w, status, map[string]string{"proxy": message})
}

func write(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "the answer could not be encoded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}
