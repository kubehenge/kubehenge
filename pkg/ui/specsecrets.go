package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// capturedResponse holds a handler's answer, so that work can follow a success before it is sent.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p) //nolint:wrapcheck // a buffer's write
}

func (c *capturedResponse) WriteHeader(code int) { c.status = code }

func (c *capturedResponse) relay(w http.ResponseWriter) {
	maps.Copy(w.Header(), c.header)
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}

// createFromSpec creates a cluster from an exported spec, sent as {"spec": "<YAML>"}. It makes the
// wizard's request out of it, so that every check of a created cluster applies. The spec's Secrets
// are opened with the appliance's recovery key and made before the cluster, so that the controller
// finds the keys the cluster was made with instead of making new ones. Where one cannot be opened
// the request is refused naming it, unless withoutSecrets says to go on without.
func (s *Server) createFromSpec(w http.ResponseWriter, r *http.Request, u user) {
	var in struct {
		Spec           string `json:"spec"`
		WithoutSecrets bool   `json:"withoutSecrets"`
		// Opened holds Secrets the client opened with an identity it keeps offline, so that the
		// private key never reaches the appliance.
		Opened []openedSecret `json:"opened"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	spec, err := provisioner.ParseClusterSpec([]byte(in.Spec))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	secrets, err := provisioner.ParseSpecSecrets([]byte(in.Spec))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	for i := range secrets {
		if !provisioner.SpecSecretBelongs(spec, &secrets[i]) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("secret %s/%s does not belong to cluster %s/%s", secrets[i].Namespace, secrets[i].Name, spec.Namespace, spec.Name))
			return
		}
	}
	restore, lost := s.openSpecSecrets(secrets, in.Opened)
	if len(lost) > 0 && !in.WithoutSecrets {
		writeError(w, http.StatusUnprocessableEntity, "cannot restore "+strings.Join(lost, ", ")+". Send withoutSecrets to create the cluster without them")
		return
	}
	body, err := json.Marshal(createRequestOf(spec))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A dry run applies every check and the user's rights, so that nothing is written for a request
	// the real one refuses.
	dry := s.createWith(r, u, body, url.Values{"dryRun": {metav1.DryRunAll}})
	if dry.status >= http.StatusMultipleChoices {
		dry.relay(w)
		return
	}
	made, err := s.restoreSecrets(r.Context(), restore)
	if err != nil {
		s.removeSecrets(r.Context(), made)
		writeError(w, http.StatusInternalServerError, "no cluster was created, because its Secrets were not restored: "+err.Error())
		return
	}
	got := s.createWith(r, u, body, nil)
	if got.status != http.StatusCreated {
		s.removeSecrets(r.Context(), made)
	}
	got.relay(w)
}

// createWith runs the create handler on a request body of its own.
func (s *Server) createWith(r *http.Request, u user, body []byte, query url.Values) *capturedResponse {
	req := r.Clone(r.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if len(query) > 0 {
		q := req.URL.Query()
		for k, v := range query {
			q[k] = v
		}
		req.URL.RawQuery = q.Encode()
	}
	got := &capturedResponse{header: http.Header{}}
	s.createCluster(got, req, u)
	return got
}

// openedSecret is a spec's Secret that the client opened itself.
type openedSecret struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Data      map[string][]byte `json:"data"`
}

// openSpecSecrets opens the Secrets of a spec, and names each one it cannot, with the reason. A
// Secret the client opened is taken as it is, and the appliance's key is tried for the rest.
func (s *Server) openSpecSecrets(in []provisioner.SpecSecret, opened []openedSecret) ([]*corev1.Secret, []string) {
	var restore []*corev1.Secret
	var lost []string
	for i := range in {
		sp := &in[i]
		name := fmt.Sprintf("Secret %s/%s", sp.Namespace, sp.Name)
		if j := slices.IndexFunc(opened, func(o openedSecret) bool { return o.Namespace == sp.Namespace && o.Name == sp.Name }); j >= 0 {
			restore = append(restore, &corev1.Secret{
				Name: sp.Name, Namespace: sp.Namespace, Labels: sp.Labels, Annotations: sp.Annotations, Type: sp.Type, Data: opened[j].Data,
			})
			continue
		}
		if sp.Sealed == "" {
			lost = append(lost, name+" (the file holds no values for it)")
			continue
		}
		if s.Appliance == nil {
			lost = append(lost, name+" ("+appliance.ErrNoSpecKey.Error()+")")
			continue
		}
		plain, err := s.Appliance.OpenSpecSecret(sp.Sealed)
		if err != nil {
			lost = append(lost, name+" ("+err.Error()+")")
			continue
		}
		data := map[string][]byte{}
		if err = json.Unmarshal(plain, &data); err != nil {
			lost = append(lost, name+" (the sealed values are damaged)")
			continue
		}
		restore = append(restore, &corev1.Secret{
			Name: sp.Name, Namespace: sp.Namespace, Labels: sp.Labels, Annotations: sp.Annotations, Type: sp.Type, Data: data,
		})
	}
	return restore, lost
}

// restoreSecrets makes the opened Secrets and returns those it made. A Secret that exists is left as
// it is: it is another cluster's, or a leftover that the controller already uses.
func (s *Server) restoreSecrets(ctx context.Context, secrets []*corev1.Secret) ([]*corev1.Secret, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	kube := s.currentKube()
	if kube == nil {
		return nil, errors.New("the management cluster does not answer")
	}
	var made []*corev1.Secret
	for _, sec := range secrets {
		err := kube.Create(ctx, sec)
		switch {
		case apierrors.IsAlreadyExists(err):
			continue
		case err != nil:
			return made, fmt.Errorf("secret %s/%s: %w", sec.Namespace, sec.Name, err)
		}
		made = append(made, sec)
	}
	return made, nil
}

// removeSecrets deletes Secrets restoreSecrets made for a cluster that was not created.
func (s *Server) removeSecrets(ctx context.Context, made []*corev1.Secret) {
	kube := s.currentKube()
	if kube == nil {
		return
	}
	for _, sec := range made {
		if err := client.IgnoreNotFound(kube.Delete(ctx, sec)); err != nil {
			s.Log.WarnContext(ctx, "removing a restored Secret of a cluster that was not created", "secret", sec.Namespace+"/"+sec.Name, "error", err)
		}
	}
}
