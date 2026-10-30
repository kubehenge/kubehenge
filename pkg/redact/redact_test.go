package redact

import (
	"strings"
	"testing"
)

func TestLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`{"msg":"x","password":"hunter2","cluster":"web"}`, `{"msg":"x","password":"[REDACTED]","cluster":"web"}`},
		{`rootPassword: hunter2`, `rootPassword: [REDACTED]`},
		{`kubeadm join 192.0.2.10:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:00`, `kubeadm join 192.0.2.10:6443 --token [REDACTED] --discovery-token-ca-cert-hash sha256:00`},
		{`using bootstrap token abcdef.0123456789abcdef now`, `using bootstrap token [REDACTED] now`},
		{`Authorization: Bearer eyJhbGciOi.abc.def`, `Authorization: Bearer [REDACTED]`},
		{`PVEAPIToken=kubehenge@pve!cluster=0f1e2d3c-aaaa-bbbb-cccc-0123456789ab`, `PVEAPIToken=[REDACTED]`},
		{`-----BEGIN RSA PRIVATE KEY-----`, Mark},
		{`machine web-cp-1 is ready`, `machine web-cp-1 is ready`},
	} {
		if got := Line(tc.in); got != tc.want {
			t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestObject(t *testing.T) {
	t.Parallel()
	o := map[string]any{
		"spec": map[string]any{ //nolint:gosec // the test's own secrets, looked for where they must not be
			"secretName": "web-kubeconfig",
			"token":      "abc",
			"values":     "rancher:\n  bootstrapPassword: s3cret\n  hostname: rancher.example\n",
			"list":       []any{map[string]any{"apiKey": "k", "name": "x"}},
		},
		"status": map[string]any{"talosConfig": "context: web\n", "ready": true},
	}
	Object(o)
	spec := as[map[string]any](t, o["spec"])
	status := as[map[string]any](t, o["status"])
	if spec["secretName"] != "web-kubeconfig" {
		t.Errorf("a reference to a secret was taken out: %v", spec["secretName"])
	}
	if spec["token"] != Mark || status["talosConfig"] != Mark {
		t.Errorf("a secret stayed: %v, %v", spec["token"], status["talosConfig"])
	}
	if v := as[string](t, spec["values"]); strings.Contains(v, "s3cret") || !strings.Contains(v, "rancher.example") {
		t.Errorf("values = %q", v)
	}
	item := as[map[string]any](t, as[[]any](t, spec["list"])[0])
	if item["apiKey"] != Mark || item["name"] != "x" {
		t.Errorf("list item = %v", item)
	}
	if !as[bool](t, status["ready"]) {
		t.Error("a field that is no secret changed")
	}
}

func as[T any](t *testing.T, v any) T {
	t.Helper()
	got, ok := v.(T)
	if !ok {
		t.Fatalf("%v is a %T", v, v)
	}
	return got
}
