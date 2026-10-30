package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// headlampActionsKey is where the chart says, in the interface's ConfigMap, that an administrator
// has given the shared Headlamp its actions: the Helm app catalog and the Prometheus charts that
// ship with Headlamp, and the Helm routes the catalog takes.
const headlampActionsKey = "headlampActions"

// staticPluginsPrefix is where Headlamp serves the plugins it ships, and how its plugin list
// names them.
const staticPluginsPrefix = "static-plugins/"

// headlampHelmRoutes are the Helm routes of a cluster, by method and path under its helm/, that
// act in the cluster with the user's own credentials there, and list the repositories and their
// charts.
var headlampHelmRoutes = map[string]bool{
	"GET releases/list": true, "GET releases": true, "GET release/history": true, "GET action/status": true,
	"PUT releases/rollback": true, "DELETE releases/uninstall": true,
	"POST release/install": true, "PUT releases/upgrade": true,
	"GET repositories": true, "GET charts": true,
}

// headlampRepositoryRoutes change Headlamp's Helm repositories. There is one list for the whole
// server, which every user's installs and upgrades on every cluster take charts from, so only
// those who may set the appliance up change it.
var headlampRepositoryRoutes = map[string]bool{
	"POST repositories": true, "PUT repositories/update": true, "DELETE repositories/remove": true,
}

// headlampChartRoutes name a chart for the Headlamp server to fetch.
var headlampChartRoutes = map[string]bool{"POST release/install": true, "PUT releases/upgrade": true}

// helmChartName is a chart of a repository, repository/chart. Helm also takes a path on the
// server, an address or an OCI reference there, which would have the appliance read its own
// files or fetch from anywhere it reaches.
var helmChartName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxHeadlampHelmBody bounds a Helm request's body: a release's values, which Helm keeps in one
// Secret of at most 1 MiB.
const maxHeadlampHelmBody = 1 << 20

// actionsOffered tells whether an administrator has given the shared Headlamp its actions.
func (h *Headlamp) actionsOffered() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.actions
}

// readHeadlampActions learns from the interface's ConfigMap whether the shared Headlamp offers its
// actions. Without a management cluster, or a ConfigMap to say so, it does not, and an error
// turns them off until the next read.
func (s *Server) readHeadlampActions(ctx context.Context, kube client.Client) error {
	on, err := headlampActionsOf(ctx, kube, s.ClassNamespace)
	s.Headlamp.mu.Lock()
	s.Headlamp.actions = on
	s.Headlamp.mu.Unlock()
	return err
}

func headlampActionsOf(ctx context.Context, kube client.Client, namespace string) (bool, error) {
	if kube == nil {
		return false, nil
	}
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: interfaceConfigMap}, cm)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading whether the shared Headlamp offers its actions: %w", err)
	}
	return cm.Data[headlampActionsKey] == "true", nil
}

// headlampHelm tells whether the shared Headlamp server may answer a request on a cluster's Helm
// routes, route being its path under the cluster's helm/, and answers it when not.
func (s *Server) headlampHelm(w http.ResponseWriter, r *http.Request, u user, cluster, route string) bool {
	route = r.Method + " " + route
	repositories := headlampRepositoryRoutes[route]
	if !s.Headlamp.actionsOffered() || !repositories && !headlampHelmRoutes[route] {
		http.Error(w, "not offered here", http.StatusForbidden)
		return false
	}
	if !s.headlampSees(w, r, u, cluster) {
		return false
	}
	switch {
	case repositories:
		if !s.require(w, r, u, "/", setupPrivileges...) {
			return false
		}
		return r.Method == http.MethodDelete || headlampBody(w, r, checkHelmRepository)
	case headlampChartRoutes[route]:
		return headlampBody(w, r, checkHelmChart)
	default:
		return true
	}
}

// headlampBody reads a request's body for check to judge, puts it back for Headlamp, and answers
// the request when check refuses it.
func headlampBody(w http.ResponseWriter, r *http.Request, check func([]byte) error) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxHeadlampHelmBody+1))
	_ = r.Body.Close()
	switch {
	case err != nil:
		http.Error(w, "reading the request failed", http.StatusBadRequest)
		return false
	case len(raw) > maxHeadlampHelmBody:
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if err = check(raw); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return true
}

// checkHelmChart lets an install or upgrade through with a chart of a repository alone, and
// without dependencies fetched from where the chart says.
func checkHelmChart(raw []byte) error {
	var req struct {
		Chart            string `json:"chart"`
		DependencyUpdate bool   `json:"dependencyUpdate"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errors.New("malformed request")
	}
	if !helmChartName.MatchString(req.Chart) {
		return errors.New("install a chart as repository/chart")
	}
	if req.DependencyUpdate {
		return errors.New("dependencies are not fetched here")
	}
	return nil
}

// checkHelmRepository lets a repository through on HTTPS, its certificate checked and any
// credentials sent to its own host alone.
func checkHelmRepository(raw []byte) error {
	var req struct {
		URL                string `json:"url"`
		Insecure           *bool  `json:"insecureSkipTLSverify"`
		PassCredentialsAll *bool  `json:"passCredentialsAll"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errors.New("malformed request")
	}
	if u, err := url.Parse(req.URL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("use an HTTPS repository address without credentials")
	}
	if req.Insecure != nil && *req.Insecure {
		return errors.New("repository certificates are always checked")
	}
	if req.PassCredentialsAll != nil && *req.PassCredentialsAll {
		return errors.New("repository credentials go to the repository's own host only")
	}
	return nil
}

// filterHeadlampPlugins leaves the plugins Headlamp ships out of its plugin list while the shared
// Headlamp does not offer its actions, so that the frontend loads none of them.
func (s *Server) filterHeadlampPlugins(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || s.Headlamp.actionsOffered() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHeadlampConfig))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading headlamp's plugins: %w", err)
	}
	var plugins []map[string]any
	if err = json.Unmarshal(raw, &plugins); err != nil {
		return fmt.Errorf("headlamp's plugins: %w", err)
	}
	kept := []map[string]any{}
	for _, p := range plugins {
		if path, ok := p["path"].(string); !ok || !strings.HasPrefix(path, staticPluginsPrefix) {
			kept = append(kept, p)
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("headlamp's plugins: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Set("Cache-Control", "no-store")
	return nil
}
