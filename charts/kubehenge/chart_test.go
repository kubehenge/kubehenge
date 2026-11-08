// Package kubehenge_test checks the templates in the chart that Cluster API, not Helm, renders.
package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"sigs.k8s.io/yaml"
)

// renderAfter renders a chart file as Cluster API renders a patch's valueFrom.template, behind the
// lines Helm writes in front of it, with the variables as they arrive there: decoded from JSON.
func renderAfter(t *testing.T, prefix, file, variables string) []byte {
	t.Helper()
	raw, err := os.ReadFile(file) //nolint:gosec // the chart's own files
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("tpl").Funcs(sprig.HermeticTxtFuncMap()).Parse(prefix + string(raw))
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

// bootstrapFile is the content of a rendered bootstrap file, which only root may read.
func bootstrapFile(t *testing.T, out []byte) map[string]any {
	t.Helper()
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
	var content map[string]any
	if err := yaml.Unmarshal([]byte(file.Content), &content); err != nil {
		t.Fatal(err)
	}
	return content
}

func TestAuthenticationConfiguration(t *testing.T) {
	t.Parallel()
	oidc := `{"oidc": [
		{"issuerURL": "https://idp.example/realms/k8s", "clientID": "kubernetes", "certificateAuthority": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"},
		{"issuerURL": "https://login.example", "clientID": "lab", "usernameClaim": "sub", "usernamePrefix": "", "groupsClaim": "roles", "groupsPrefix": "lab:"}
	]}`
	jwt := []any{
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
	}
	kubeadm := map[string]any{
		"enabled": true,
		"conditions": []any{
			map[string]any{"path": "/livez"},
			map[string]any{"path": "/readyz"},
			map[string]any{"path": "/healthz"},
			map[string]any{"path": "/api/v1/namespaces/kube-public/configmaps/cluster-info"},
		},
	}
	for _, c := range []struct {
		anonymous string
		variables string
		want      map[string]any
	}{
		// The API server's --anonymous-auth=false says it. It refuses the flag beside a section.
		{"false", oidc, map[string]any{"jwt": jwt}},
		{"true", oidc, map[string]any{"jwt": jwt, "anonymous": kubeadm}},
		// kubeadm's API server reads the file whether or not the cluster trusts a provider.
		{"true", `{}`, map[string]any{"jwt": []any{}, "anonymous": kubeadm}},
	} {
		want := map[string]any{"apiVersion": "apiserver.config.k8s.io/v1", "kind": "AuthenticationConfiguration"}
		maps.Copy(want, c.want)
		config := bootstrapFile(t, renderAfter(t, "{{- $anonymous := "+c.anonymous+" }}\n{{- $talos := false }}\n", "files/authentication-config.tpl", c.variables))
		if !reflect.DeepEqual(config, want) {
			t.Errorf("anonymous %s, %s: configuration =\n%v", c.anonymous, c.variables, config)
		}
	}
}

func TestProxmoxNetworkDevice(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		network string
		want    map[string]any
	}{
		// Unset, the device is what it was before the options existed.
		{`{"bridge": "vmbr0"}`, map[string]any{}},
		{`{"bridge": "vmbr0", "vlan": 30}`, map[string]any{"vlan": float64(30)}},
		// The guest's interface gets the MTU the Proxmox VE one does.
		{`{"bridge": "vmbr0", "mtu": 1450}`, map[string]any{"mtu": float64(1450), "linkMtu": float64(1450)}},
		{`{"bridge": "vmbr0", "mtu": 9000}`, map[string]any{"mtu": float64(9000), "linkMtu": float64(9000)}},
		{`{"bridge": "vmbr0", "queues": 4}`, map[string]any{"queues": float64(4)}},
		{
			`{"bridge": "vmbr0", "vlan": 10, "mtu": 1450, "queues": 2}`,
			map[string]any{"vlan": float64(10), "mtu": float64(1450), "linkMtu": float64(1450), "queues": float64(2)},
		},
	} {
		var spec struct {
			NetworkDevices []map[string]any `json:"networkDevices"`
		}
		out := renderAfter(t, "", "files/proxmox-network.tpl", `{"network": `+c.network+`}`)
		if err := yaml.UnmarshalStrict(out, &spec); err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
		want := map[string]any{"name": "net0", "model": "virtio", "bridge": "vmbr0"}
		maps.Copy(want, c.want)
		if len(spec.NetworkDevices) != 1 || !reflect.DeepEqual(spec.NetworkDevices[0], want) {
			t.Errorf("%s: devices = %v", c.network, spec.NetworkDevices)
		}
	}
	helpers, err := os.ReadFile("templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(helpers, []byte(`.Files.Get "files/proxmox-network.tpl"`)) {
		t.Error("the Proxmox VE patches do not use files/proxmox-network.tpl")
	}
}

func TestAdmissionConfiguration(t *testing.T) {
	t.Parallel()
	exempt := []any{"kube-system", "node-exporter"}
	level := func(enforce string) map[string]any {
		return map[string]any{
			"enforce": enforce, "enforce-version": "latest",
			"audit": "restricted", "audit-version": "latest", "warn": "restricted", "warn-version": "latest",
		}
	}
	for _, c := range []struct {
		variables string
		defaults  map[string]any
	}{
		{`{}`, level("baseline")},
		{`{"podSecurity": {"kyverno": "restricted"}}`, level("baseline")},
		{`{"podSecurity": {"admission": "privileged"}}`, level("privileged")},
		{`{"podSecurity": {"admission": "restricted"}}`, level("restricted")},
		{`{"cis": true}`, level("restricted")},
		{`{"cis": true, "podSecurity": {"admission": "baseline"}}`, level("restricted")},
		// The exemptions stay whatever the cluster runs, so that switching an add-on rolls no node.
		{`{"monitoring": {"enabled": false}, "rancher": {"register": false}}`, level("baseline")},
	} {
		config := bootstrapFile(t, renderAfter(t, `{{- $exempt := list "kube-system" "node-exporter" }}`+"\n{{- $talos := false }}\n", "files/admission.tpl", c.variables))
		want := map[string]any{
			"apiVersion": "apiserver.config.k8s.io/v1",
			"kind":       "AdmissionConfiguration",
			"plugins": []any{
				map[string]any{
					"name": "PodSecurity",
					"configuration": map[string]any{
						"apiVersion": "pod-security.admission.config.k8s.io/v1",
						"kind":       "PodSecurityConfiguration",
						"defaults":   c.defaults,
						"exemptions": map[string]any{"namespaces": exempt},
					},
				},
				map[string]any{
					"name": "EventRateLimit",
					"configuration": map[string]any{
						"apiVersion": "eventratelimit.admission.k8s.io/v1alpha1",
						"kind":       "Configuration",
						"limits": []any{
							map[string]any{"type": "Server", "qps": float64(500), "burst": float64(2000)},
							map[string]any{"type": "Namespace", "qps": float64(50), "burst": float64(500), "cacheSize": float64(4096)},
						},
					},
				},
			},
		}
		if !reflect.DeepEqual(config, want) {
			t.Errorf("%s: configuration =\n%v", c.variables, config)
		}
	}
}
