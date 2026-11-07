package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// releasePinView is a release an administrator pinned.
type releasePinView struct {
	Version string     `json:"version"`
	File    string     `json:"file"`
	Script  string     `json:"script"`
	By      string     `json:"by,omitempty"`
	At      *time.Time `json:"at,omitempty"`
	// InUse names a cluster that runs the release, which keeps its pin from being removed.
	InUse string `json:"inUse,omitempty"`
}

type releasePinsView struct {
	Pins []releasePinView `json:"pins"`
	// Checks says whether this interface can read a release to pin it.
	Checks bool `json:"checks"`
}

type releasePinRequest struct {
	Version string `json:"version"`
	// File and Script are the digests the administrator was shown. Saving checks the release still
	// has them.
	File   string `json:"file,omitempty"`
	Script string `json:"script,omitempty"`
}

// ManagementReader reads from the management cluster once the appliance has made it, and finds
// nothing before.
type ManagementReader struct{ s *Server }

// ManagementReader is the interface's reader of the management cluster.
func (s *Server) ManagementReader() ManagementReader { return ManagementReader{s} }

// Get implements client.Reader.
func (m ManagementReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	kube := m.s.currentKube()
	if kube == nil {
		return fmt.Errorf("no management cluster yet: %w", apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name))
	}
	if err := kube.Get(ctx, key, obj, opts...); err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	return nil
}

// List implements client.Reader.
func (m ManagementReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kube := m.s.currentKube()
	if kube == nil {
		return nil
	}
	if err := kube.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	return nil
}

// pinsReady checks the user may look after the management cluster and that it exists, and returns
// the store of the pins.
func (s *Server) pinsReady(w http.ResponseWriter, r *http.Request, u user) (*provisioner.PinStore, bool) {
	kube := s.currentKube()
	if kube == nil {
		writeError(w, http.StatusConflict, "set up the management cluster first")
		return nil, false
	}
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return nil, false
	}
	return &provisioner.PinStore{Reader: kube, Writer: kube, Namespace: s.ClassNamespace}, true
}

func (s *Server) listPins(w http.ResponseWriter, r *http.Request, u user) {
	store, ok := s.pinsReady(w, r, u)
	if !ok {
		return
	}
	records, err := store.Records(r.Context())
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	view := releasePinsView{Pins: []releasePinView{}, Checks: s.PinChecks != nil}
	for _, rec := range records {
		v := releasePinView{Version: rec.Version, File: rec.File, Script: rec.Script, By: rec.By}
		if !rec.At.IsZero() {
			v.At = &rec.At
		}
		if v.InUse, err = store.RunningCluster(r.Context(), rec.Version); err != nil {
			s.kubeError(w, r, err)
			return
		}
		view.Pins = append(view.Pins, v)
	}
	writeJSON(w, http.StatusOK, view)
}

// checkPin reads a release's checksum file and install script for the administrator to see.
func (s *Server) checkPin(w http.ResponseWriter, r *http.Request, u user) {
	if _, ok := s.pinsReady(w, r, u); !ok {
		return
	}
	var req releasePinRequest
	if !readJSON(w, r, &req) {
		return
	}
	pin, ok := s.digests(w, r, req.Version)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, releasePinView{Version: req.Version, File: pin.File, Script: pin.Script})
}

func (s *Server) digests(w http.ResponseWriter, r *http.Request, version string) (provisioner.ReleasePin, bool) {
	if s.PinChecks == nil {
		writeError(w, http.StatusNotFound, "this interface does not read releases. Use kubehenge release pin")
		return provisioner.ReleasePin{}, false
	}
	pin, err := s.PinChecks.Digests(r.Context(), version)
	switch {
	case errors.Is(err, provisioner.ErrNotARelease):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		s.Log.WarnContext(r.Context(), "reading a release to pin", "version", version, "error", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		return pin, true
	}
	return provisioner.ReleasePin{}, false
}

// addPin saves a pin once the release still has the digests the administrator was shown.
func (s *Server) addPin(w http.ResponseWriter, r *http.Request, u user) {
	store, ok := s.pinsReady(w, r, u)
	if !ok {
		return
	}
	var req releasePinRequest
	if !readJSON(w, r, &req) {
		return
	}
	pin, ok := s.digests(w, r, req.Version)
	if !ok {
		return
	}
	if pin.File != req.File || pin.Script != req.Script {
		writeError(w, http.StatusConflict, req.Version+" changed since it was read. Check it again")
		return
	}
	err := store.Add(r.Context(), req.Version, pin, u.ticket.Username, s.now())
	switch {
	case errors.Is(err, provisioner.ErrNotARelease), errors.Is(err, provisioner.ErrPinEmbedded):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.kubeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "pinned a release", "user", u.ticket.Username, "version", req.Version, "checksums", pin.File, "script", pin.Script)
	writeJSON(w, http.StatusCreated, releasePinView{Version: req.Version, File: pin.File, Script: pin.Script, By: u.ticket.Username})
}

func (s *Server) removePin(w http.ResponseWriter, r *http.Request, u user) {
	store, ok := s.pinsReady(w, r, u)
	if !ok {
		return
	}
	version := r.PathValue("version")
	err := store.Remove(r.Context(), version)
	switch {
	case errors.Is(err, provisioner.ErrPinInUse):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, provisioner.ErrNoPin):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		s.kubeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "removed a release pin", "user", u.ticket.Username, "version", version)
	w.WriteHeader(http.StatusNoContent)
}
