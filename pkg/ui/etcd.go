package ui

import (
	"encoding/json"
	"net/http"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// etcdVariable is the variable that sets how large a cluster's etcd database may grow.
const etcdVariable = "etcd"

// etcdView is what the controller last read of a cluster's etcd members.
type etcdView struct {
	Read       time.Time               `json:"read"`
	QuotaBytes int64                   `json:"quotaBytes"`
	Members    []provisioner.EtcdMember `json:"members"`
	// Error is why some member could not be read.
	Error string `json:"error,omitempty"`
	// Warning says which database is full or near it.
	Warning string `json:"warning,omitempty"`
	// Defrag is the last defragmentation.
	Defrag *provisioner.EtcdDefrag `json:"defrag,omitempty"`
	// Requested tells that a defragmentation asked for waits for the controller.
	Requested bool `json:"requested,omitempty"`
}

// etcdStatusView is nil for a cluster whose members the controller has not read.
func etcdStatusView(c *clusterv1.Cluster) *etcdView {
	status, ok := provisioner.LastEtcdStatus(c)
	if !ok {
		return nil
	}
	v := &etcdView{
		Read: status.At, QuotaBytes: status.QuotaBytes, Members: status.Members, Error: status.Error, Warning: status.Warning(),
	}
	if v.Members == nil {
		v.Members = []provisioner.EtcdMember{}
	}
	_, v.Requested = c.Annotations[provisioner.EtcdDefragRequestAnnotation]
	var last provisioner.EtcdDefrag
	if raw, ok := c.Annotations[provisioner.EtcdDefragAnnotation]; ok && json.Unmarshal([]byte(raw), &last) == nil {
		v.Defrag = &last
	}
	return v
}

// etcdAttention warns of a database that is full or near it.
func etcdAttention(c *clusterv1.Cluster, a *attention) {
	status, ok := provisioner.LastEtcdStatus(c)
	if !ok {
		return
	}
	if w := status.Warning(); w != "" {
		a.add(severityWarning, sectionControlPlane, "etcd database filling", w, status.At)
	}
}

// defragEtcd asks the controller to defragment every etcd member at once.
func (s *Server) defragEtcd(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	if provisioner.RestoreInProgress(c) {
		writeError(w, http.StatusConflict, "an etcd restore is running")
		return
	}
	req := provisioner.EtcdDefragRequest{By: u.ticket.Username, At: time.Now().UTC()}
	if !s.annotate(w, r, u, c, provisioner.EtcdDefragRequestAnnotation, req) {
		return
	}
	s.Log.InfoContext(r.Context(), "asked for an etcd defragmentation", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	writeJSON(w, http.StatusAccepted, req)
}
