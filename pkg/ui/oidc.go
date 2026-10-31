package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

// oidcVariable is the variable naming the identity providers a cluster's API server trusts.
// Whoever sets it decides who can sign in to the cluster, and as whom: a provider one controls
// issues tokens for any name. So choosing a provider that neither the class's default nor the
// cluster trusts already needs what taking the cluster's admin kubeconfig needs.
const oidcVariable = "oidc"

// jsonNull is JSON's null, which as a variable's value gives a cluster the class's default again.
const jsonNull = "null"

// parseOIDC decodes and checks an oidc variable.
func parseOIDC(raw json.RawMessage) ([]appliance.OIDCProvider, error) {
	var providers []appliance.OIDCProvider
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&providers); err != nil {
		return nil, fmt.Errorf("oidc: a list of providers: %w", err)
	}
	if err := appliance.ValidOIDC(providers); err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	return providers, nil
}

// classDefault is a variable's default in the class, or nil.
func classDefault(class *clusterv1.ClusterClass, name string) *apiextensionsv1.JSON {
	for i := range class.Spec.Variables {
		if v := &class.Spec.Variables[i]; v.Name == name {
			return v.Schema.OpenAPIV3Schema.Default
		}
	}
	return nil
}

// classChoices is the list of strings a class variable, or a property of it, is limited to. Nil
// when it is not limited.
func classChoices(class *clusterv1.ClusterClass, name string, property ...string) []string {
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		if v.Name != name {
			continue
		}
		schema := v.Schema.OpenAPIV3Schema
		for _, p := range property {
			schema = schema.Properties[p]
		}
		if len(schema.Enum) == 0 {
			return nil
		}
		choices := []string{}
		for _, e := range schema.Enum {
			var s string
			if json.Unmarshal(e.Raw, &s) == nil {
				choices = append(choices, s)
			}
		}
		return choices
	}
	return nil
}

// sameJSON tells whether two JSON documents hold the same value.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, errX := json.Marshal(x)
	by, errY := json.Marshal(y)
	return errX == nil && errY == nil && bytes.Equal(ax, by)
}

// alreadyTrusted tells whether every provider in raw is one the class's default lists as it stands,
// or one the cluster trusts already (current, nil for a new cluster), so keeping or dropping them
// lets nobody in who was not let in before. null, which gives the cluster the default again, is
// within it.
func alreadyTrusted(class *clusterv1.ClusterClass, raw, current json.RawMessage) bool {
	if string(raw) == jsonNull {
		return true
	}
	var given, trusted, now []json.RawMessage
	if json.Unmarshal(raw, &given) != nil {
		return false
	}
	if def := classDefault(class, oidcVariable); def != nil && json.Unmarshal(def.Raw, &trusted) != nil {
		return false
	}
	if current != nil && string(current) != jsonNull && json.Unmarshal(current, &now) == nil {
		trusted = append(trusted, now...)
	}
	for _, g := range given {
		if !slices.ContainsFunc(trusted, func(t json.RawMessage) bool { return sameJSON(g, t) }) {
			return false
		}
	}
	return true
}

// checkOIDC checks a new oidc value for a cluster of the class, and answers the request when it
// is malformed or the user may not choose it. held are the user's privileges where the choice
// applies: the workload pool for a new cluster, the cluster's pool for a change, whose current
// value is current.
func checkOIDC(w http.ResponseWriter, u user, class *clusterv1.ClusterClass, path string, held map[string]bool, raw, current json.RawMessage) bool {
	if string(raw) != jsonNull {
		if _, err := parseOIDC(raw); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return false
		}
	}
	if !alreadyTrusted(class, raw, current) && !held[privKubeconfig] {
		writeError(w, http.StatusForbidden, fmt.Sprintf("%s needs %s on %s to add identity providers", u.ticket.Username, privKubeconfig, path))
		return false
	}
	return true
}
