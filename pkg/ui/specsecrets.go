package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// createFromSpec creates a cluster from an exported spec, sent as {"spec": "<YAML>"}. It makes the
// wizard's request out of it, so that every check of a created cluster applies. The spec's Secrets
// are opened with the appliance's recovery key and made after the cluster. Where one cannot be
// opened the request is refused naming it, unless withoutSecrets says to go on without.
func (s *Server) createFromSpec(w http.ResponseWriter, r *http.Request, u user) {
	var in struct {
		Spec           string `json:"spec"`
		WithoutSecrets bool   `json:"withoutSecrets"`
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
	restore, lost := s.openSpecSecrets(secrets)
	if len(lost) > 0 && !in.WithoutSecrets {
		writeError(w, http.StatusUnprocessableEntity, "cannot restore "+strings.Join(lost, ", ")+". Send withoutSecrets to create the cluster without them")
		return
	}
	body, err := json.Marshal(createRequestOf(spec))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	got := &capturedResponse{header: http.Header{}}
	s.createCluster(got, r, u)
	if got.status == http.StatusCreated {
		if err := s.restoreSecrets(r, restore); err != nil {
			writeError(w, http.StatusInternalServerError, "the cluster was created, but its Secrets were not restored: "+err.Error())
			return
		}
	}
	maps.Copy(w.Header(), got.header)
	w.WriteHeader(got.status)
	_, _ = w.Write(got.body.Bytes())
}

// openSpecSecrets opens the Secrets of a spec, and names each one it cannot, with the reason.
func (s *Server) openSpecSecrets(in []provisioner.SpecSecret) ([]*corev1.Secret, []string) {
	var restore []*corev1.Secret
	var lost []string
	for i := range in {
		sp := &in[i]
		name := fmt.Sprintf("Secret %s/%s", sp.Namespace, sp.Name)
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
			Name: sp.Name, Namespace: sp.Namespace, Labels: sp.Labels, Type: sp.Type, Data: data,
		})
	}
	return restore, lost
}

// restoreSecrets makes the opened Secrets, or updates those that exist.
func (s *Server) restoreSecrets(r *http.Request, secrets []*corev1.Secret) error {
	if len(secrets) == 0 {
		return nil
	}
	kube := s.currentKube()
	if kube == nil {
		return errors.New("the management cluster does not answer")
	}
	for _, sec := range secrets {
		err := kube.Create(r.Context(), sec)
		if apierrors.IsAlreadyExists(err) {
			have := &corev1.Secret{}
			if err = kube.Get(r.Context(), client.ObjectKeyFromObject(sec), have); err == nil {
				have.Data, have.Labels = sec.Data, sec.Labels
				err = kube.Update(r.Context(), have)
			}
		}
		if err != nil {
			return fmt.Errorf("secret %s/%s: %w", sec.Namespace, sec.Name, err)
		}
	}
	return nil
}
