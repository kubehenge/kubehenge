package ui

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// veleroLogLines is how much of a log the run's page shows: its end, where a failure is.
	veleroLogLines = 200
	// maxVeleroLog bounds what is unpacked of a log, against a file that unpacks to far more than
	// any backup writes.
	maxVeleroLog = 256 << 20
	// maxVeleroLogLine bounds one line of it.
	maxVeleroLogLine = 1 << 20
)

// veleroLogView is the end of a backup's or restore's log.
type veleroLogView struct {
	Lines []string `json:"lines"`
	// More tells that the log has lines before these.
	More bool `json:"more,omitempty"`
}

// showBackupLog answers with the end of a finished backup's log.
func (s *Server) showBackupLog(w http.ResponseWriter, r *http.Request, u user) {
	s.showVeleroLog(w, r, u, "BackupLog", r.PathValue("backup"))
}

// showRestoreLog answers with the end of a finished restore's log.
func (s *Server) showRestoreLog(w http.ResponseWriter, r *http.Request, u user) {
	s.showVeleroLog(w, r, u, "RestoreLog", r.PathValue("restore"))
}

// showVeleroLog answers with the end of the log Velero keeps in the target beside a finished
// backup or restore, read through a DownloadRequest as its CLI does. A log may carry anything the
// cluster holds, so it goes to those its pods' logs go to.
func (s *Server) showVeleroLog(w http.ResponseWriter, r *http.Request, u user, kind, name string) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		writeError(w, http.StatusNotFound, "no such backup or restore")
		return
	}
	if !backupsOf(c).Enabled || c.Namespace != s.Namespace {
		writeError(w, http.StatusConflict, "the interface reads no backups of this cluster")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, ok := s.openVelero(ctx, w, r, u, c, provisioner.IdentityBackups)
	if !ok {
		return
	}
	backup, phase, err := veleroRunOf(ctx, v, kind, name)
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	switch {
	case phase == veleroInvalid:
		writeError(w, http.StatusConflict, "Velero refused it before it began, so it has no log")
		return
	case !veleroFinished(phase):
		writeError(w, http.StatusConflict, "the log comes once it has finished")
		return
	}
	of, err := restoredFrom(ctx, v, c, backup)
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	link, err := veleroDownloadLink(ctx, v, kind, name)
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	s.writeVeleroLog(ctx, w, u, c, name, of, link)
}

// veleroRunOf is the backup a backup or restore is of, and its phase.
func veleroRunOf(ctx context.Context, v dynamic.Interface, kind, name string) (string, string, error) {
	resource := veleroBackups
	if kind == "RestoreLog" {
		resource = veleroRestores
	}
	o, err := v.Resource(resource).Namespace(provisioner.VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", name, err)
	}
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	if kind == "RestoreLog" {
		backup, _, _ := unstructured.NestedString(o.Object, "spec", "backupName")
		return backup, phase, nil
	}
	return name, phase, nil
}

// writeVeleroLog answers with the end of the log behind Velero's link, read from the folder of the
// cluster of in the target.
func (s *Server) writeVeleroLog(ctx context.Context, w http.ResponseWriter, u user, c *clusterv1.Cluster, name, of, link string) {
	targets := &provisioner.ClusterTargets{Client: u.kube, ClassNamespace: s.ClassNamespace}
	body, err := targets.VeleroDownload(ctx, c, of, link)
	if err != nil {
		s.Log.InfoContext(ctx, "reading a Velero log", "cluster", c.Namespace+"/"+c.Name, "of", name, "error", err)
		if errors.Is(err, provisioner.ErrNoObject) {
			writeError(w, http.StatusNotFound, "Velero left no log of it in the target")
			return
		}
		writeError(w, http.StatusBadGateway, "the log could not be read from the target")
		return
	}
	defer func() { _ = body.Close() }()
	v, err := veleroLogOf(body, veleroLogLines)
	if err != nil {
		s.Log.InfoContext(ctx, "reading a Velero log", "cluster", c.Namespace+"/"+c.Name, "of", name, "error", err)
		writeError(w, http.StatusBadGateway, "the log in the target could not be read")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// veleroLogOf is the last n lines of a gzipped log.
func veleroLogOf(r io.Reader, n int) (veleroLogView, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return veleroLogView{}, fmt.Errorf("reading the log: %w", err)
	}
	lines := bufio.NewScanner(io.LimitReader(zr, maxVeleroLog))
	lines.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxVeleroLogLine)
	ring := make([]string, 0, n)
	seen := 0
	for lines.Scan() {
		if len(ring) < n {
			ring = append(ring, lines.Text())
		} else {
			ring[seen%n] = lines.Text()
		}
		seen++
	}
	if err := lines.Err(); err != nil {
		return veleroLogView{}, fmt.Errorf("reading the log: %w", err)
	}
	if seen > n {
		at := seen % n
		ring = slices.Concat(ring[at:], ring[:at])
	}
	return veleroLogView{Lines: ring, More: seen > n}, nil
}
