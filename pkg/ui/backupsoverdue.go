package ui

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// sectionBackups is the cluster's backups view, which an item about its backups leads to.
const sectionBackups = "backups"

// backupsAttention is what needs a look in a cluster's own backups: the last one that finished
// failed, or none has completed within twice the schedule's interval, the schedule having come
// round twice since the last that did, or since it was made. Velero's own schedule says when it
// runs, so what holds for the interface holds for a cluster set up with kubectl.
func backupsAttention(schedule *unstructured.Unstructured, backups []backupView, now time.Time) []attentionView {
	finished, completed := lastOwnBackups(backups)
	var out []attentionView
	if a, ok := lastBackupFailed(finished); ok {
		out = append(out, a)
	}
	if a, ok := backupsOverdue(schedule, completed, now); ok {
		out = append(out, a)
	}
	return out
}

// lastOwnBackups are the cluster's own backups that started last of those that finished and of
// those that completed, or nil where there is none.
func lastOwnBackups(backups []backupView) (*backupView, *backupView) {
	var finished, completed *backupView
	for i := range backups {
		b := &backups[i]
		if b.From != "" || b.location != "" && b.location != ownLocation || b.Started == nil {
			continue
		}
		if veleroFinished(b.Phase) && (finished == nil || b.Started.After(*finished.Started)) {
			finished = b
		}
		if b.Phase == veleroCompleted && (completed == nil || b.Started.After(*completed.Started)) {
			completed = b
		}
	}
	return finished, completed
}

// lastBackupFailed is the item for the last finished backup when it failed in whole or in part.
func lastBackupFailed(finished *backupView) (attentionView, bool) {
	if finished == nil {
		return attentionView{}, false
	}
	switch finished.Phase {
	case veleroFailed, veleroInvalid:
		return attentionView{
			Severity: severityError, Section: sectionBackups, What: "Last backup failed",
			Message: sentenceOf(finished.Failure, "Velero's log of backup "+finished.Name+" says why."), Since: *finished.Started,
		}, true
	case veleroPartiallyFailed:
		return attentionView{
			Severity: severityWarning, Section: sectionBackups, What: "Last backup partly failed",
			Message: fmt.Sprintf("Backup %s has %s.", finished.Name, counted(finished.Errors, "error")), Since: *finished.Started,
		}, true
	}
	return attentionView{}, false
}

// backupsOverdue is the item for a schedule that has come round twice since the last completed
// backup, or since it was made when none has completed.
func backupsOverdue(schedule *unstructured.Unstructured, completed *backupView, now time.Time) (attentionView, bool) {
	if schedule == nil {
		return attentionView{}, false
	}
	spec, _, _ := unstructured.NestedString(schedule.Object, "spec", "schedule")
	runs, err := cron.ParseStandard(spec)
	if err != nil {
		return attentionView{}, false
	}
	since := schedule.GetCreationTimestamp().Time
	message := "No backup has completed yet."
	if completed != nil {
		since = *completed.Started
		message = "None has completed since " + since.UTC().Format("2006-01-02 15:04") + " UTC."
	}
	if since.IsZero() || runs.Next(runs.Next(since)).After(now) {
		return attentionView{}, false
	}
	return attentionView{Severity: severityWarning, Section: sectionBackups, What: "Backups overdue", Message: message, Since: since}, true
}

// counted is how many of a thing there are, in words: "1 error", "3 errors".
func counted(n int64, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// sentenceOf is what Velero said, as a sentence, or the fallback when it said nothing.
func sentenceOf(said, fallback string) string {
	if said == "" {
		return fallback
	}
	if last := said[len(said)-1]; last != '.' && last != '!' && last != '?' {
		said += "."
	}
	return said
}
