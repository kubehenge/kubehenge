// Package kubehenge_test checks the templates in the chart that Cluster API, not Helm, renders.
package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"sigs.k8s.io/yaml"
)

// render renders a chart file as Cluster API renders a patch's valueFrom.template, with the
// variables as they arrive there: decoded from JSON.
func render(t *testing.T, file, variables string) []byte {
	t.Helper()
	raw, err := os.ReadFile(file) //nolint:gosec // the chart's own files
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("tpl").Funcs(sprig.HermeticTxtFuncMap()).Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(variables), &vars); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, vars); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestAuthenticationConfiguration(t *testing.T) {
	t.Parallel()
	out := render(t, "files/authentication-config.tpl", `{"oidc": [
		{"issuerURL": "https://idp.example/realms/k8s", "clientID": "kubernetes", "certificateAuthority": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"},
		{"issuerURL": "https://login.example", "clientID": "lab", "usernameClaim": "sub", "usernamePrefix": "", "groupsClaim": "roles", "groupsPrefix": "lab:"}
	]}`)
	var file struct {
		Owner       string `json:"owner"`
		Permissions string `json:"permissions"`
		Content     string `json:"content"`
	}
	if err := yaml.UnmarshalStrict(out, &file); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if file.Owner != "root:root" || file.Permissions != "0600" {
		t.Fatalf("file = %+v", file)
	}
	var config map[string]any
	if err := yaml.Unmarshal([]byte(file.Content), &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion": "apiserver.config.k8s.io/v1",
		"kind":       "AuthenticationConfiguration",
		"jwt": []any{
			map[string]any{
				"issuer": map[string]any{
					"url": "https://idp.example/realms/k8s", "audiences": []any{"kubernetes"},
					"certificateAuthority": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
				},
				"claimMappings": map[string]any{
					"username": map[string]any{"claim": "email", "prefix": "oidc:"},
					"groups":   map[string]any{"claim": "groups", "prefix": "oidc:"},
				},
			},
			map[string]any{
				"issuer": map[string]any{"url": "https://login.example", "audiences": []any{"lab"}},
				"claimMappings": map[string]any{
					"username": map[string]any{"claim": "sub", "prefix": ""},
					"groups":   map[string]any{"claim": "roles", "prefix": "lab:"},
				},
			},
		},
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("configuration =\n%s", file.Content)
	}
}
