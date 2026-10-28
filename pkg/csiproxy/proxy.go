package csiproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// apiPrefix is where the Proxmox VE API's JSON calls are.
const apiPrefix = "/api2/json"

// maxListBytes caps what is read of a list the proxy filters; a storage's content and the storage
// configuration stay far below it.
const maxListBytes = 16 << 20

// errRemoved is a copy request deleted before the controller finished it.
var errRemoved = errors.New("the copy request was deleted before it finished")

// Proxy passes the CSI plugin's calls on to the Proxmox VE API with the cluster's own token, which
// they carry. Three it answers itself:
//
//   - A volume copy, which Proxmox VE lets only root@pam make, becomes a request the controller in
//     the management cluster carries out; the call returns once the copy has, as Proxmox VE's own
//     task would have been waited for.
//   - A storage's content leaves out the targets of copies still under way, so that the plugin,
//     asking again after its call timed out, never takes a half-written copy for a finished one.
//   - A storage's configuration, which Proxmox VE shows singly only to Datastore.Allocate, is read
//     from the list of storages, which shows it to a token that may use the storage.
type Proxy struct {
	// Upstream is the Proxmox VE API's address, such as https://pve.example.org:8006.
	Upstream *url.URL
	// Transport reaches it, verifying its certificate.
	Transport http.RoundTripper
	// Client reads and writes the copy requests in the workload cluster.
	Client client.Client
	// Poll is how often a copy's request is read again while the call waits on it.
	Poll time.Duration
	Log  *slog.Logger
}

// Handler routes the plugin's calls.
func (p *Proxy) Handler() http.Handler {
	forward := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(p.Upstream) },
		Transport: p.Transport,
		ErrorLog:  slog.NewLogLogger(p.Log.Handler(), slog.LevelWarn),
	}
	mux := http.NewServeMux()
	mux.Handle("/", forward)
	mux.HandleFunc("GET "+apiPrefix+"/storage/{storage}", func(w http.ResponseWriter, r *http.Request) {
		p.storageConfig(w, r, forward)
	})
	mux.HandleFunc("GET "+apiPrefix+"/nodes/{node}/storage/{storage}/content", p.content)
	mux.HandleFunc("POST "+apiPrefix+"/nodes/{node}/storage/{storage}/content/{volume...}", p.copyVolume)
	return mux
}

// storageConfig answers with the storage's entry of the storage list, or passes the call on when
// the list does not show it.
func (p *Proxy) storageConfig(w http.ResponseWriter, r *http.Request, forward http.Handler) {
	resp, err := p.upstream(r, apiPrefix+"/storage", "")
	if err != nil {
		p.Log.Warn("listing the storages", "error", err)
		forward.ServeHTTP(w, r)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&list) != nil {
		forward.ServeHTTP(w, r)
		return
	}
	for _, entry := range list.Data {
		var s struct {
			Storage string `json:"storage"`
		}
		if json.Unmarshal(entry, &s) == nil && s.Storage == r.PathValue("storage") {
			writeData(w, entry)
			return
		}
	}
	forward.ServeHTTP(w, r)
}

// content lists a storage's content on a node without the targets of copies still under way. It
// answers nothing rather than risk showing one, when the requests cannot be read.
func (p *Proxy) content(w http.ResponseWriter, r *http.Request) {
	node, storage := r.PathValue("node"), r.PathValue("storage")
	hidden, err := p.underWay(r.Context(), node, storage)
	if err != nil {
		p.Log.Error("reading the volume copies under way", "error", err)
		writeError(w, http.StatusServiceUnavailable, "the volume copies under way cannot be read")
		return
	}
	resp, err := p.upstream(r, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		p.Log.Error("listing a storage's content", "node", node, "storage", storage, "error", err)
		writeError(w, http.StatusBadGateway, "Proxmox VE cannot be reached")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "reading Proxmox VE's answer failed")
		return
	}
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || len(hidden) == 0 || json.Unmarshal(body, &list) != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}
	list.Data = slices.DeleteFunc(list.Data, func(entry json.RawMessage) bool {
		var v struct {
			VolID string `json:"volid"`
		}
		return json.Unmarshal(entry, &v) == nil && hidden[v.VolID]
	})
	writeData(w, list.Data)
}

// underWay are the volume IDs of the copies onto the node's storage that have not finished.
func (p *Proxy) underWay(ctx context.Context, node, storage string) (map[string]bool, error) {
	requests, err := p.requests(ctx)
	if err != nil {
		return nil, err
	}
	hidden := map[string]bool{}
	for i := range requests {
		req := &requests[i]
		if !req.Finished() && req.Storage == storage && req.Destination() == node {
			hidden[req.TargetID()] = true
		}
	}
	return hidden, nil
}

// copyVolume asks the controller for the copy and waits until it is done. A call for a copy already
// asked for waits on that request, so that the plugin's retries never start a second copy.
func (p *Proxy) copyVolume(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "the copy's parameters cannot be read")
		return
	}
	c := Copy{
		Node: r.PathValue("node"), Storage: r.PathValue("storage"), Volume: r.PathValue("volume"),
		Target: r.Form.Get("target"), TargetNode: r.Form.Get("target_node"),
	}
	c.Volume = strings.TrimPrefix(c.Volume, c.Storage+":")
	if c.Target == "" {
		writeError(w, http.StatusBadRequest, "a copy needs a target")
		return
	}
	name, done, err := p.ask(r.Context(), &c)
	if err == nil && !done {
		var req Request
		if req, err = p.wait(r.Context(), name); err == nil && req.State == StateFailed {
			p.Log.Warn("the volume copy was refused or failed", "request", name, "volume", c.Volume, "target", c.Target, "reason", req.Message)
			writeErrors(w, http.StatusBadRequest, map[string]string{"copy": req.Message})
			return
		}
	}
	switch {
	case r.Context().Err() != nil:
		// The plugin gave up; the request stays, so that its next call waits on the same copy.
		return
	case err != nil:
		p.Log.Error("asking for a volume copy", "volume", c.Volume, "target", c.Target, "error", err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	p.Log.Info("copied a volume", "request", name, "node", c.Node, "storage", c.Storage, "volume", c.Volume, "target", c.Target, "targetNode", c.TargetNode)
	// No task: the plugin then waits for none, as the copy is done.
	writeData(w, nil)
}

// ask finds the request for the copy, or makes one; done says a request for it has finished
// already. A finished request for it is deleted, so that a failed copy is asked for afresh.
func (p *Proxy) ask(ctx context.Context, c *Copy) (string, bool, error) {
	requests, err := p.requests(ctx)
	if err != nil {
		return "", false, err
	}
	for i := range requests {
		req := &requests[i]
		if req.Copy != *c {
			continue
		}
		if !req.Finished() {
			return req.Name, false, nil
		}
		if err := p.remove(ctx, req.Name); err != nil {
			return "", false, err
		}
		if req.State == StateDone {
			return req.Name, true, nil
		}
	}
	cm := NewRequest(c)
	if err := p.Client.Create(ctx, cm); err != nil {
		return "", false, fmt.Errorf("asking for the copy: %w", err)
	}
	p.Log.Info("asked for a volume copy", "request", cm.Name, "node", c.Node, "storage", c.Storage, "volume", c.Volume, "target", c.Target, "targetNode", c.TargetNode)
	return cm.Name, false, nil
}

// wait reads the request until the controller has finished it, and then deletes it.
func (p *Proxy) wait(ctx context.Context, name string) (Request, error) {
	t := time.NewTicker(p.Poll)
	defer t.Stop()
	for {
		cm := &corev1.ConfigMap{}
		err := p.Client.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, cm)
		if apierrors.IsNotFound(err) {
			return Request{}, errRemoved
		}
		if err != nil && ctx.Err() == nil {
			p.Log.Warn("reading the copy request", "request", name, "error", err)
		}
		if err == nil {
			if req := ParseRequest(cm); req.Finished() {
				return req, p.remove(ctx, name)
			}
		}
		select {
		case <-ctx.Done():
			return Request{}, fmt.Errorf("waiting for the copy: %w", ctx.Err())
		case <-t.C:
		}
	}
}

func (p *Proxy) requests(ctx context.Context) ([]Request, error) {
	list := &corev1.ConfigMapList{}
	if err := p.Client.List(ctx, list, client.InNamespace(Namespace), client.HasLabels{RequestLabel}); err != nil {
		return nil, fmt.Errorf("listing the copy requests: %w", err)
	}
	requests := make([]Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, ParseRequest(&list.Items[i]))
	}
	return requests, nil
}

func (p *Proxy) remove(ctx context.Context, name string) error {
	cm := &corev1.ConfigMap{Namespace: Namespace, Name: name}
	if err := client.IgnoreNotFound(p.Client.Delete(ctx, cm)); err != nil {
		return fmt.Errorf("deleting the copy request %s: %w", name, err)
	}
	return nil
}

// upstream sends a GET to the API with the credentials the plugin's call carries.
func (p *Proxy) upstream(r *http.Request, path, query string) (*http.Response, error) {
	u := *p.Upstream
	u.Path, u.RawPath, u.RawQuery = strings.TrimSuffix(u.Path, "/")+path, "", query
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("the request to Proxmox VE: %w", err)
	}
	for _, h := range []string{"Authorization", "Cookie", "CSRFPreventionToken"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := p.Transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("asking Proxmox VE: %w", err)
	}
	return resp, nil
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
