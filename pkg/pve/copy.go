package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// StartCopy has Proxmox VE copy a volume of a storage into a new one on the same storage, which only
// root@pam may, and returns the copy's task. volume and target are the volumes' names on the
// storage; targetNode, when set, is the node the copy lands on, if not node.
func (a *Accounts) StartCopy(ctx context.Context, t *Ticket, node, storage, volume, target, targetNode string) (string, error) {
	form := url.Values{"target": {target}}
	if targetNode != "" {
		form.Set("target_node", targetNode)
	}
	// The volume keeps its slashes, as Proxmox VE takes the rest of the path for it.
	path := fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), escapeSegments(volume))
	var upid string
	if err := a.send(ctx, t, http.MethodPost, path, form, &upid); err != nil {
		return "", fmt.Errorf("copying %s:%s into %s: %w", storage, volume, target, err)
	}
	if upid == "" {
		return "", errors.New("proxmox VE started no task for the copy")
	}
	return upid, nil
}

// TaskExit is how a task ended: its exit status, OK when it succeeded, or "" while it runs.
func (a *Accounts) TaskExit(ctx context.Context, t *Ticket, node, upid string) (string, error) {
	var st struct {
		Status     string `json:"status"`
		ExitStatus string `json:"exitstatus"`
	}
	if err := a.get(ctx, t, fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), &st); err != nil {
		return "", err
	}
	if st.Status != "stopped" {
		return "", nil
	}
	if st.ExitStatus == "" {
		return "unknown", nil
	}
	return st.ExitStatus, nil
}

// StorageVolumes lists the volume IDs, storage:name, on a node's storage.
func (a *Accounts) StorageVolumes(ctx context.Context, t *Ticket, node, storage string) ([]string, error) {
	var content []struct {
		VolID string `json:"volid"`
	}
	if err := a.get(ctx, t, fmt.Sprintf("/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage)), &content); err != nil {
		return nil, err
	}
	volumes := make([]string, 0, len(content))
	for _, c := range content {
		volumes = append(volumes, c.VolID)
	}
	return volumes, nil
}

func escapeSegments(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
