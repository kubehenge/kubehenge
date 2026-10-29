package csiproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxAnswerBytes caps what is read of an answer of Proxmox VE's; a storage's content, a VM's
// configuration and the list of VMs stay far below it.
const maxAnswerBytes = 16 << 20

// credentialHeaders carry the plugin's credentials, which the proxy's own calls reuse.
var credentialHeaders = []string{"Authorization", "Cookie", "CSRFPreventionToken"}

// pve calls the Proxmox VE API with the credentials of the plugin's call it acts for.
type pve struct {
	base      *url.URL
	transport http.RoundTripper
	header    http.Header
}

func (p *Proxy) pveFor(r *http.Request) *pve {
	h := http.Header{}
	for _, k := range credentialHeaders {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	return &pve{base: p.Upstream, transport: p.Transport, header: h}
}

// apiError is a call Proxmox VE refused.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Proxmox VE answered %d: %s", e.status, e.message)
}

// call sends a call and decodes its data into out, which may be nil. Parameters go in the query
// for GET and DELETE and in the form otherwise.
func (c *pve) call(ctx context.Context, method, path string, params url.Values, out any) error {
	u := *c.base
	u.Path, u.RawPath = strings.TrimSuffix(u.Path, "/")+apiPrefix+path, ""
	var body io.Reader = http.NoBody
	if method == http.MethodGet || method == http.MethodDelete {
		u.RawQuery = params.Encode()
	} else if params != nil {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("the call to Proxmox VE: %w", err)
	}
	req.Header = c.header.Clone()
	if body != http.NoBody {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	var answer struct {
		Data   json.RawMessage   `json:"data"`
		Errors map[string]string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &answer)
	if resp.StatusCode != http.StatusOK {
		var msg strings.Builder
		msg.WriteString(strings.TrimSpace(resp.Status))
		for _, k := range slices.Sorted(maps.Keys(answer.Errors)) {
			msg.WriteString("; " + k + ": " + answer.Errors[k])
		}
		return fmt.Errorf("%s %s: %w", method, path, &apiError{status: resp.StatusCode, message: msg.String()})
	}
	if out == nil || len(answer.Data) == 0 || string(answer.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(answer.Data, out); err != nil {
		return fmt.Errorf("%s %s: decoding the answer: %w", method, path, err)
	}
	return nil
}

// vm is a VM as the list of resources shows it.
type vm struct {
	VMID int    `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	Pool string `json:"pool"`
}

func (c *pve) vms(ctx context.Context) ([]vm, error) {
	var list []vm
	if err := c.call(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// vmConfig is a VM's configuration, its keys with their values as text.
type vmConfig map[string]string

func (c *pve) config(ctx context.Context, node string, vmid int) (vmConfig, error) {
	raw := map[string]any{}
	if err := c.call(ctx, http.MethodGet, vmPath(node, vmid)+"/config", nil, &raw); err != nil {
		return nil, err
	}
	cfg := vmConfig{}
	for k, v := range raw {
		cfg[k] = fmt.Sprint(v)
	}
	return cfg, nil
}

func vmPath(node string, vmid int) string {
	return "/nodes/" + url.PathEscape(node) + "/qemu/" + strconv.Itoa(vmid)
}

// diskKeys are the configuration keys that name a volume.
var diskKeys = []string{"scsi", "virtio", "sata", "ide", "unused", "efidisk", "tpmstate"}

// volumes are the volume IDs the configuration names, by key.
func (cfg vmConfig) volumes() map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if !isDiskKey(k) {
			continue
		}
		volid, _, _ := strings.Cut(v, ",")
		if strings.Contains(volid, ":") {
			out[k] = volid
		}
	}
	return out
}

func isDiskKey(k string) bool {
	for _, prefix := range diskKeys {
		if n, ok := strings.CutPrefix(k, prefix); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			return true
		}
	}
	return false
}

// unused counts the configuration's unused disks.
func (cfg vmConfig) unused() int {
	n := 0
	for k := range cfg {
		if strings.HasPrefix(k, "unused") && isDiskKey(k) {
			n++
		}
	}
	return n
}

// keyOf is the key naming volid, or "".
func (cfg vmConfig) keyOf(volid string) string {
	for k, v := range cfg.volumes() {
		if v == volid {
			return k
		}
	}
	return ""
}

// taskStatus waits for a task to end and returns its exit status, OK when it succeeded.
func (c *pve) taskStatus(ctx context.Context, node, upid string, poll time.Duration) (string, error) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		err := c.call(ctx, http.MethodGet, "/nodes/"+url.PathEscape(node)+"/tasks/"+url.PathEscape(upid)+"/status", nil, &st)
		if err != nil && !isTransient(err) {
			return "", err
		}
		if err == nil && st.Status == "stopped" {
			if st.ExitStatus == "" {
				return "unknown", nil
			}
			return st.ExitStatus, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for task %s: %w", upid, ctx.Err())
		case <-t.C:
		}
	}
}

// isTransient reports whether a failed call may succeed when tried again: Proxmox VE or a proxy in
// front of it did not answer, or answered that it could not.
func isTransient(err error) bool {
	if api, ok := errors.AsType[*apiError](err); ok {
		return api.status >= http.StatusInternalServerError
	}
	return true
}
