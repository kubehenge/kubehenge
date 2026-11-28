package pve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// BackupJob is a backup job of the Proxmox VE cluster, as /cluster/backup lists it.
type BackupJob struct {
	// Enabled is unset for a job enabled by default.
	Enabled *flag  `json:"enabled"`
	Node    string `json:"node"`
	All     flag   `json:"all"`
	VMID    string `json:"vmid"`
	Exclude string `json:"exclude"`
	Pool    string `json:"pool"`
}

// JobsCover tells whether an enabled job includes the guest on the node in the pool: by its ID, by
// all guests without excluding it, or by its pool.
func JobsCover(jobs []BackupJob, vmid int, node, pool string) bool {
	id := strconv.Itoa(vmid)
	for _, j := range jobs {
		// A job limited to one node backs up only the guests on it.
		if j.Enabled != nil && !*j.Enabled || j.Node != "" && j.Node != node {
			continue
		}
		switch {
		case bool(j.All) && !slices.Contains(strings.Split(j.Exclude, ","), id):
			return true
		case slices.Contains(strings.Split(j.VMID, ","), id):
			return true
		case j.Pool != "" && j.Pool == pool:
			return true
		}
	}
	return false
}

// BackupStorage is a storage of a node, as /nodes/{node}/storage lists it.
type BackupStorage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Shared  flag   `json:"shared"`
	Active  flag   `json:"active"`
	Avail   number `json:"avail"`
}

// BestBackupStorage picks the storage an appliance backup goes to among a node's storages that take
// backups: Proxmox Backup Server, then a shared storage, then a local one, and within each the most
// free. Empty when none is active.
func BestBackupStorage(list []BackupStorage) string {
	const (
		rankPBS = iota
		rankShared
		rankLocal
	)
	rank := func(s BackupStorage) int {
		switch {
		case s.Type == "pbs":
			return rankPBS
		case bool(s.Shared):
			return rankShared
		default:
			return rankLocal
		}
	}
	var active []BackupStorage
	for _, s := range list {
		if s.Active {
			active = append(active, s)
		}
	}
	if len(active) == 0 {
		return ""
	}
	return slices.MinFunc(active, func(a, b BackupStorage) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(b.Avail, a.Avail))
	}).Storage
}

// ApplianceBackupComment names the backup job Kubehenge adds for its appliance.
const ApplianceBackupComment = "Kubehenge appliance"

// ApplianceBackupJob is the job that backs up the appliance's container alone: daily at 02:30 as a
// snapshot, keeping a week of dailies and a month of weeklies.
func ApplianceBackupJob(vmid int, storage string) url.Values {
	return url.Values{
		"vmid": {strconv.Itoa(vmid)}, "storage": {storage}, "schedule": {"02:30"}, "mode": {"snapshot"},
		"prune-backups": {"keep-daily=7,keep-weekly=4"}, "comment": {ApplianceBackupComment}, "enabled": {"1"},
	}
}

// ErrNoBackupStorage is that no active storage of the guest's node takes backups.
var ErrNoBackupStorage = errors.New("no storage of the node takes backups")

// guestPlace is the node and pool of a guest the user sees.
func (a *Accounts) guestPlace(ctx context.Context, t *Ticket, vmid int) (string, string, error) {
	var guests []listedGuest
	if err := a.get(ctx, t, "/cluster/resources?type=vm", &guests); err != nil {
		return "", "", err
	}
	i := slices.IndexFunc(guests, func(g listedGuest) bool { return g.VMID == vmid })
	if i < 0 {
		return "", "", fmt.Errorf("guest %d is not among those %s sees", vmid, t.Username)
	}
	return guests[i].Node, guests[i].Pool, nil
}

// AddApplianceBackup adds ApplianceBackupJob for the guest with the user's rights, to the best
// storage of its node, unless a job covers it already. It returns the storage, empty when nothing
// was added.
func (a *Accounts) AddApplianceBackup(ctx context.Context, t *Ticket, vmid int) (string, error) {
	node, pool, err := a.guestPlace(ctx, t, vmid)
	if err != nil {
		return "", err
	}
	var jobs []BackupJob
	if err = a.get(ctx, t, "/cluster/backup", &jobs); err != nil {
		return "", err
	}
	if JobsCover(jobs, vmid, node, pool) {
		return "", nil
	}
	var storages []BackupStorage
	if err = a.get(ctx, t, "/nodes/"+url.PathEscape(node)+"/storage?content=backup&enabled=1", &storages); err != nil {
		return "", err
	}
	storage := BestBackupStorage(storages)
	if storage == "" {
		return "", ErrNoBackupStorage
	}
	if err = a.send(ctx, t, http.MethodPost, "/cluster/backup", ApplianceBackupJob(vmid, storage), nil); err != nil {
		return "", fmt.Errorf("adding the backup job: %w", err)
	}
	return storage, nil
}
