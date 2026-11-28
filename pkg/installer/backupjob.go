package installer

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strconv"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

const stepBackupName = "backup"

// backupJob adds a Proxmox VE backup job for the appliance's container where no enabled job
// includes it, since the container holds the only copy of the management cluster's state. The job
// covers that container alone. It returns what it did, empty when a job included the container
// already, and a warning where it added none.
func (r *Runner) backupJob(ctx context.Context) (string, string) {
	n, id := r.S.Node, r.S.Cfg.CTID
	vmid, err := strconv.Atoi(id)
	if err != nil {
		return "", ""
	}
	node, pool := n.Hostname, ""
	if listed, listErr := n.run(ctx, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"); listErr == nil {
		for _, m := range jsonList(listed) {
			if str(m, "vmid") == id {
				node, pool = str(m, "node"), str(m, "pool")
			}
		}
	}
	manual := "Add one under Datacenter, Backup, Add in Proxmox VE, or run: sh install.sh --upgrade"
	var jobs []pve.BackupJob
	out, err := n.run(ctx, "pvesh", "get", "/cluster/backup", "--output-format", "json")
	if err == nil {
		err = json.Unmarshal([]byte(out), &jobs)
	}
	if err != nil {
		return "", "Proxmox VE's backup jobs could not be read, so none was added for container " + id + ". " + manual
	}
	if pve.JobsCover(jobs, vmid, node, pool) {
		return "", ""
	}
	var storages []pve.BackupStorage
	if out, err = n.run(ctx, "pvesh", "get", "/nodes/"+node+"/storage", "--content", "backup", "--enabled", "1", "--output-format", "json"); err == nil {
		_ = json.Unmarshal([]byte(out), &storages)
	}
	storage := pve.BestBackupStorage(storages)
	if storage == "" {
		return "", "No storage on node " + node + " takes backups, so no backup job includes container " + id +
			". Allow Backup content on one under Datacenter, Storage, Edit, then run: sh install.sh --upgrade"
	}
	job := pve.ApplianceBackupJob(vmid, storage)
	args := []string{"create", "/cluster/backup"}
	for _, k := range slices.Sorted(maps.Keys(job)) {
		args = append(args, "--"+k, job.Get(k))
	}
	if _, err = n.run(ctx, "pvesh", args...); err != nil {
		return "", "Adding a backup job for container " + id + " failed (" + err.Error() + "). " + manual
	}
	return "daily to " + storage, ""
}

// upgradeBackupJob is backupJob for an upgrade, which prints what it added.
func (r *Runner) upgradeBackupJob(ctx context.Context, out io.Writer) {
	done, warning := r.backupJob(ctx)
	if done != "" {
		r.sayf(out, "ok   Backup job %s", done)
	}
	if warning != "" {
		r.warnings = append(r.warnings, warning)
	}
}

// stepBackup is the install's backup job, a warning where it adds none.
func (r *Runner) stepBackup(ctx context.Context) error {
	done, warning := r.backupJob(ctx)
	switch {
	case warning != "":
		r.warnings = append(r.warnings, warning)
		r.emit(Event{Step: stepBackupName, State: StepSkipped, Detail: "none added"})
	case done == "":
		r.finish(stepBackupName, "already in a job")
	default:
		r.finish(stepBackupName, done)
	}
	return nil
}
