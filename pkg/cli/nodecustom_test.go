package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const customToken = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

func lastSent(t *testing.T, fake *fakeClusterAPI) string {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) == 0 {
		t.Fatal("nothing was sent")
	}
	return fake.sent[len(fake.sent)-1]
}

func TestClusterUpdateSendsNodeCustomisation(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	rules := filepath.Join(t.TempDir(), "50-x.rules")
	if err := os.WriteFile(rules, []byte("-w /etc/passwd\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{
			"inline file",
			[]string{"--node-file", "/etc/audit/rules.d/50-x.rules=" + rules + ",mode=0640,roles=control-plane"},
			`{"variables":{"nodeCustomisation":{"files":[{"content":"-w /etc/passwd\n","path":"/etc/audit/rules.d/50-x.rules","permissions":"0640","roles":["controlPlane"]}]}}}`,
		},
		{
			"both roles",
			[]string{"--node-file", "/etc/a=" + rules + ",roles=control-plane|worker"},
			`"roles":["controlPlane","worker"]`,
		},
		{
			"secret file",
			[]string{"--node-file", "/etc/a=secret:creds/token"},
			`{"variables":{"nodeCustomisation":{"files":[{"path":"/etc/a","secret":{"key":"token","name":"creds"}}]}}}`,
		},
		{
			"command",
			[]string{"--node-command", "augenrules --load"},
			`{"variables":{"nodeCustomisation":{"commands":[{"run":"augenrules --load"}]}}}`,
		},
		{
			"command control plane",
			[]string{"--node-command", "control-plane:augenrules --load"},
			`{"variables":{"nodeCustomisation":{"commands":[{"roles":["controlPlane"],"run":"augenrules --load"}]}}}`,
		},
		{
			"command worker",
			[]string{"--node-command", "worker:sysctl -p"},
			`"roles":["worker"],"run":"sysctl -p"`,
		},
		{
			"kubelet arg",
			[]string{"--kubelet-arg", "event-qps=50", "--kubelet-arg", "x="},
			`{"variables":{"nodeCustomisation":{"kubeletArgs":[{"name":"event-qps","value":"50"},{"name":"x","value":""}]}}}`,
		},
		{
			"apiserver arg",
			[]string{"--apiserver-arg", "audit-log-maxage=30"},
			`{"variables":{"nodeCustomisation":{"apiServerArgs":[{"name":"audit-log-maxage","value":"30"}]}}}`,
		},
		{
			"controller manager arg",
			[]string{"--controller-manager-arg", "a=b"},
			`{"variables":{"nodeCustomisation":{"controllerManagerArgs":[{"name":"a","value":"b"}]}}}`,
		},
		{
			"scheduler arg",
			[]string{"--scheduler-arg", "a=b"},
			`{"variables":{"nodeCustomisation":{"schedulerArgs":[{"name":"a","value":"b"}]}}}`,
		},
		{
			"audit policy",
			[]string{"--audit-policy-configmap", "audit"},
			`{"variables":{"nodeCustomisation":{"auditPolicy":{"configMap":"audit"}}}}`,
		},
		{
			"audit webhook",
			[]string{"--audit-webhook-secret", "hook"},
			`{"variables":{"nodeCustomisation":{"auditWebhook":{"secret":"hook"}}}}`,
		},
		{
			"audit webhook key",
			[]string{"--audit-webhook-secret", "hook/conf"},
			`{"variables":{"nodeCustomisation":{"auditWebhook":{"key":"conf","secret":"hook"}}}}`,
		},
		{
			"with cis",
			[]string{"--cis", "--kubelet-arg", "a=b"},
			`{"variables":{"cis":true,"nodeCustomisation":{"kubeletArgs":[{"name":"a","value":"b"}]}}}`,
		},
	} {
		out, err := runCluster(t, srv.URL, customToken, append([]string{"update", "web"}, c.args...)...)
		if err != nil || out != "cluster web changed\n" {
			t.Fatalf("%s: %q, %v", c.name, out, err)
		}
		if got := lastSent(t, fake); !strings.Contains(got, c.want) {
			t.Errorf("%s: sent %s, want it to hold %s", c.name, got, c.want)
		}
	}
}

func TestNodeCustomisationFlagsRefuseBadInput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&fakeClusterAPI{})
	t.Cleanup(srv.Close)
	rules := filepath.Join(t.TempDir(), "r")
	if err := os.WriteFile(rules, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(t.TempDir(), "plain.yaml")
	if err := os.WriteFile(plain, []byte("name: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--node-file", "/etc/a"}, "give PATH=SOURCE"},
		{[]string{"--node-file", "=" + rules}, "give PATH=SOURCE"},
		{[]string{"--node-file", "/etc/a="}, "give PATH=SOURCE"},
		{[]string{"--node-file", "/etc/a=" + rules + ",mode"}, "give PATH=SOURCE"},
		{[]string{"--node-file", "/etc/a=" + rules + ",mode=0777"}, `the mode "0777" is not one of`},
		{[]string{"--node-file", "/etc/a=" + rules + ",roles=master"}, `the role "master"`},
		{[]string{"--node-file", "/etc/a=" + rules + ",owner=root"}, `the option "owner"`},
		{[]string{"--node-file", "/etc/a=secret:creds"}, "a Secret source is secret:NAME/KEY"},
		{[]string{"--node-file", "/etc/a=secret:/key"}, "a Secret source is secret:NAME/KEY"},
		{[]string{"--node-file", "/etc/a=secret:n/"}, "a Secret source is secret:NAME/KEY"},
		{[]string{"--node-file", "/etc/a=" + rules + ".missing"}, "reading the source"},
		{[]string{"--node-command", "worker:  "}, "give the command to run"},
		{[]string{"--kubelet-arg", "event-qps"}, `--kubelet-arg "event-qps": give NAME=VALUE`},
		{[]string{"--apiserver-arg", "=1"}, `--apiserver-arg "=1": give NAME=VALUE`},
		{[]string{"--controller-manager-arg", "x"}, "--controller-manager-arg"},
		{[]string{"--scheduler-arg", "x"}, "--scheduler-arg"},
		{[]string{"--audit-webhook-secret", "/key"}, "give NAME or NAME/KEY"},
		{[]string{"--audit-webhook-secret", "a/b/c"}, "give NAME or NAME/KEY"},
	} {
		for _, verb := range [][]string{{"update", "web"}, {"create", "-f", plain}} {
			_, err := runCluster(t, srv.URL, customToken, slices.Concat(verb, c.args)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%v: %v, want %q", c.args, err, c.want)
			}
		}
	}
}

func TestClusterCreateMergesNodeCustomisation(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	file := filepath.Join(dir, "new.yaml")
	request := `name: new
class: talos
variables:
  nodeCustomisation:
    kubeletArgs:
      - {name: a, value: "1"}
    auditPolicy:
      configMap: old
`
	if err := os.WriteFile(file, []byte(request), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runCluster(t, srv.URL, customToken, "create", "-f", file,
		"--kubelet-arg", "b=2", "--audit-policy-configmap", "new", "--node-command", "worker:true")
	if err != nil || out != "cluster clusters/new created\n" {
		t.Fatalf("create = %q, %v", out, err)
	}
	want := `{"class":"talos","name":"new","variables":{"nodeCustomisation":{"auditPolicy":{"configMap":"new"},` +
		`"commands":[{"roles":["worker"],"run":"true"}],"kubeletArgs":[{"name":"a","value":"1"},{"name":"b","value":"2"}]}}}`
	if got := lastSent(t, fake); !strings.HasSuffix(got, want) {
		t.Errorf("sent %s, want %s", got, want)
	}

	plain := filepath.Join(dir, "plain.yaml")
	if err = os.WriteFile(plain, []byte("name: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = runCluster(t, srv.URL, customToken, "create", "-f", plain, "--apiserver-arg", "a=b"); err != nil {
		t.Fatal(err)
	}
	if got := lastSent(t, fake); !strings.HasSuffix(got, `{"name":"p","variables":{"nodeCustomisation":{"apiServerArgs":[{"name":"a","value":"b"}]}}}`) {
		t.Errorf("sent %s", got)
	}
	if _, err = runCluster(t, srv.URL, customToken, "create", "-f", plain, "--kubelet-arg", "bad"); err == nil ||
		!strings.Contains(err.Error(), "give NAME=VALUE") {
		t.Errorf("a bad flag on create: %v", err)
	}
}
