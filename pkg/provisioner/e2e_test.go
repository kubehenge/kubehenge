//go:build e2e

package provisioner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func env(t *testing.T, name, fallback string) string {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		return v
	}
	if fallback == "" {
		t.Fatalf("%s must be set", name)
	}
	return fallback
}

func signIn(t *testing.T, token string) *proxmox.Client {
	t.Helper()
	c, err := pve.Connect(pve.Config{URL: env(t, "KUBEHENGE_PROXMOX_URL", ""), Token: token, CAFile: os.Getenv("KUBEHENGE_PROXMOX_CA")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fileStorages name a volume by its file, extension included.
var fileStorages = []string{"dir", "nfs", "cifs", "cephfs", "glusterfs", "btrfs"}

// allocate makes a volume under the owner VM's VMID as the CSI plugin does, with the cluster's token.
func allocate(t *testing.T, own *proxmox.Client, node, storage, kind string, owner int) string {
	t.Helper()
	name := fmt.Sprintf("vm-%d-e2e-%d", owner, time.Now().UnixNano()%1_000_000)
	if slices.Contains(fileStorages, kind) {
		name += ".raw"
	}
	var volid string
	err := own.Post(context.Background(), fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage),
		map[string]any{"vmid": owner, "filename": name, "size": "1M", "format": "raw"}, &volid)
	if err != nil {
		t.Fatalf("making a volume on node %s storage %s: %v", node, storage, err)
	}
	return volid
}

// removeLeftovers removes what a failed run left of its cluster.
func removeLeftovers(t *testing.T, p *Proxmox, n Names, held Credentials) {
	t.Helper()
	if err := p.Remove(context.Background(), n, held); err != nil {
		t.Errorf("removing %s: %v", n.Pool, err)
	}
}

func exists(t *testing.T, admin *proxmox.Client, node, storage, volid string) bool {
	t.Helper()
	var content []struct {
		VolID string `json:"volid"`
	}
	if err := admin.Get(context.Background(), fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage), &content); err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(content, func(c struct {
		VolID string `json:"volid"`
	},
	) bool {
		return c.VolID == volid
	})
}

// TestVolumesEndToEnd tests a cluster's own Proxmox VE objects on a Proxmox VE host, signed in as
// Kubehenge's controller: the cluster's pool, user, token and volume owner VM are created.
// Volumes are made under the owner on every node's local storage, as the CSI plugin makes them.
// One is deleted through the owner with the cluster's own token, and the owner moves to its node.
// Removing the cluster deletes the rest on every node and leaves nothing behind. With one node,
// nothing moves.
//
//	go test -tags e2e -count=1 -v -run TestVolumesEndToEnd ./pkg/provisioner/
//
// Settings come from the environment: KUBEHENGE_PROXMOX_URL and optionally KUBEHENGE_PROXMOX_CA.
// KUBEHENGE_PROXMOX_TOKEN, an administrator's, is only to check what is left.
// KUBEHENGE_E2E_CONTROLLER_TOKEN is the controller's token as management-cluster/ creates it.
// KUBEHENGE_E2E_WORKLOAD_POOL, _CLUSTER_GROUP, _CLUSTER_ROLE and _LOCAL_STORAGE are named as the
// management cluster names them, with _LOCAL_STORAGE one of the workload storages that is local to
// each node.
func TestVolumesEndToEnd(t *testing.T) { //nolint:paralleltest // owns the Proxmox VE host for its run
	ctx := context.Background()
	admin := signIn(t, env(t, "KUBEHENGE_PROXMOX_TOKEN", ""))
	p := &Proxmox{
		Client: signIn(t, env(t, "KUBEHENGE_E2E_CONTROLLER_TOKEN", "")),
		Settings: Settings{
			Group: env(t, "KUBEHENGE_E2E_CLUSTER_GROUP", "kubehenge-clusters"),
			Role:  env(t, "KUBEHENGE_E2E_CLUSTER_ROLE", "kubehenge-cluster-vms"),
		},
	}
	storage := env(t, "KUBEHENGE_E2E_LOCAL_STORAGE", "local")
	n, err := NamesFor(env(t, "KUBEHENGE_E2E_WORKLOAD_POOL", "kubehenge"), "e2e", fmt.Sprintf("volumes-%d", time.Now().Unix()%1_000_000))
	if err != nil {
		t.Fatal(err)
	}

	var all []storageConfig
	if err = admin.Get(ctx, "/storage", &all); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(s storageConfig) bool { return s.Storage == storage })
	if i < 0 || all[i].Shared != 0 {
		t.Fatalf("storage %s is not local to each node", storage)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err = admin.Get(ctx, "/storage/"+storage, &kind); err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err = admin.Get(ctx, "/nodes", &listed); err != nil {
		t.Fatal(err)
	}
	var nodes []string
	for _, l := range listed {
		if l.Status == "online" {
			nodes = append(nodes, l.Node)
		}
	}
	slices.Sort(nodes)
	t.Logf("nodes %v, cluster %s", nodes, n.Pool)

	var recorded Credentials
	t.Cleanup(func() { removeLeftovers(t, p, n, recorded) })
	creds, err := p.Ensure(ctx, n, Access{}, Credentials{}, func(c Credentials) error { recorded = c; return nil })
	if err != nil {
		t.Fatal(err)
	}
	own := signIn(t, creds.TokenID+"="+creds.TokenSecret)

	// Volumes on every node, the owner VM on the first.
	volumes := map[string]string{}
	for _, node := range nodes {
		volumes[node] = allocate(t, own, node, storage, kind.Type, creds.VolumeOwner)
	}

	last := nodes[len(nodes)-1]
	o := &ownerVM{pve: own, mover: p, vmid: creds.VolumeOwner, created: creds.VolumeOwnerNode}
	if err = o.deleteVolume(ctx, volume{node: last, storage: storage, volid: volumes[last], owner: creds.VolumeOwner}); err != nil {
		t.Fatalf("deleting %s through the owner VM: %v", volumes[last], err)
	}
	if exists(t, admin, last, storage, volumes[last]) {
		t.Fatalf("volume %s is still on node %s", volumes[last], last)
	}
	for _, node := range nodes[:len(nodes)-1] {
		if !exists(t, admin, node, storage, volumes[node]) {
			t.Fatalf("volume %s on node %s went too", volumes[node], node)
		}
	}
	at, err := locate(ctx, admin, creds.VolumeOwner, "")
	if err != nil || at != last {
		t.Fatalf("owner VM is on node %q (%v), not %s", at, err, last)
	}

	// The controller finds the owner VM where it moved to, rather than making another.
	again, err := p.Ensure(ctx, n, Access{}, creds, func(Credentials) error {
		t.Error("the controller chose a new volume owner")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.VolumeOwner != creds.VolumeOwner || again.VolumeOwnerNode != last {
		t.Errorf("volume owner %d on %s, want %d on %s", again.VolumeOwner, again.VolumeOwnerNode, creds.VolumeOwner, last)
	}

	volumes[last] = allocate(t, own, last, storage, kind.Type, creds.VolumeOwner)
	// Straight after creation, as a cluster that fails early is: Proxmox VE may not list the
	// owner VM in its pool yet.
	if err := p.Remove(ctx, n, again); err != nil {
		t.Fatal(err)
	}
	for node, v := range volumes {
		if exists(t, admin, node, storage, v) {
			t.Errorf("volume %s is still on node %s", v, node)
		}
	}
	if at, err := locate(ctx, admin, creds.VolumeOwner, ""); err != nil || at != "" {
		t.Errorf("owner VM %d is still on node %q (%v)", creds.VolumeOwner, at, err)
	}
	var pools []any
	if err := admin.Get(ctx, "/pools?"+url.Values{"poolid": {n.Pool}}.Encode(), &pools); err == nil && len(pools) > 0 {
		t.Errorf("pool %s is still there", n.Pool)
	}
	if err := admin.Get(ctx, "/access/users/"+url.PathEscape(n.User), &map[string]any{}); err == nil {
		t.Errorf("user %s is still there", n.User)
	}
}
