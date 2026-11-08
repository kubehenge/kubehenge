package kubehenge_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const customised = `{"builtin": {"cluster": {"name": "web"}}, "nodeCustomisation": {
	"files": [
		{"path": "/etc/audit/rules.d/50-co.rules", "permissions": "0640", "content": "-w /etc/passwd -p wa", "roles": ["controlPlane", "worker"]},
		{"path": "/etc/co/cp.conf", "content": "cp only", "roles": ["controlPlane"]},
		{"path": "/etc/co/worker.crt", "secret": {"name": "cacert", "key": "ca"}, "roles": ["worker"]}
	],
	"commands": [{"run": "augenrules --load"}, {"run": "cp-only", "roles": ["controlPlane"]}, {"run": "worker-only", "roles": ["worker"]}],
	"kubeletArgs": [{"name": "event-qps", "value": "50"}],
	"apiServerArgs": [{"name": "request-timeout", "value": "30s"}],
	"controllerManagerArgs": [{"name": "concurrent-gc-syncs", "value": "30"}],
	"schedulerArgs": [{"name": "kube-api-qps", "value": "60"}],
	"auditPolicy": {"configMap": "policy"},
	"auditWebhook": {"secret": "siem", "key": "kubeconfig"}
}}`

// talosCustomised is what Talos can carry: no Secret, no ConfigMap and no command.
const talosCustomised = `{"controlPlaneEndpoint": "192.0.2.10", "cni": "flannel", "nodeCustomisation": {
	"files": [
		{"path": "/var/co/all.conf", "permissions": "0640", "content": "everywhere"},
		{"path": "/var/co/cp.conf", "content": "cp only", "roles": ["controlPlane"]},
		{"path": "/var/co/worker.conf", "content": "worker only", "roles": ["worker"]}
	],
	"kubeletArgs": [{"name": "event-qps", "value": "50"}],
	"apiServerArgs": [{"name": "request-timeout", "value": "30s"}],
	"controllerManagerArgs": [{"name": "concurrent-gc-syncs", "value": "30"}],
	"schedulerArgs": [{"name": "kube-api-qps", "value": "60"}],
	"auditPolicy": {"policy": {"apiVersion": "audit.k8s.io/v1", "kind": "Policy", "rules": [{"level": "RequestResponse"}]}}
}}`

// fileOf is the file at path in a bootstrap template's files, and how often the path occurs.
func fileOf(t *testing.T, files any, path string) (map[string]any, int) {
	t.Helper()
	list, ok := files.([]any)
	if !ok {
		t.Fatalf("files %v", files)
	}
	var found map[string]any
	n := 0
	for _, f := range list {
		if m := asMap(t, f); m["path"] == path {
			found = m
			n++
		}
	}
	return found, n
}

func commandsOf(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("commands %v", v)
	}
	out := make([]string, 0, len(list))
	for _, c := range list {
		s, ok := c.(string)
		if !ok {
			t.Fatalf("command %v", c)
		}
		out = append(out, s)
	}
	return out
}

func argsOf(t *testing.T, v any) string {
	t.Helper()
	return asJSON(t, v)
}

func TestKubeadmNodesTakeTheirCustomisation(t *testing.T) {
	t.Parallel()
	class := renderClass(t, "kubeadm.yaml")
	vars := variablesOf(t, customised)
	cp := asMap(t, field(t, class.patched(t, "KubeadmControlPlaneTemplate", vars), "spec", "template", "spec", "kubeadmConfigSpec"))
	checkKubeadmControlPlaneFiles(t, cp)
	checkKubeadmControlPlaneArgs(t, cp)
	checkKubeadmWorker(t, asMap(t, field(t, class.patched(t, "KubeadmConfigTemplate", vars), "spec", "template", "spec")))
}

func checkKubeadmControlPlaneFiles(t *testing.T, cp map[string]any) {
	t.Helper()
	files := cp["files"]
	rules, _ := fileOf(t, files, "/etc/audit/rules.d/50-co.rules")
	if rules["content"] != "-w /etc/passwd -p wa" || rules["permissions"] != "0640" || rules["owner"] != "root:root" {
		t.Errorf("rules file %v", rules)
	}
	if only, _ := fileOf(t, files, "/etc/co/cp.conf"); only["content"] != "cp only" || only["permissions"] != "0644" {
		t.Errorf("control-plane file %v", only)
	}
	if other, _ := fileOf(t, files, "/etc/co/worker.crt"); other != nil {
		t.Errorf("a worker's file on the control plane: %v", other)
	}
	policy, policies := fileOf(t, files, "/etc/kubernetes/audit-policy.yaml")
	if policies != 2 || asJSON(t, policy["contentFrom"]) != `{"secret":{"key":"audit-policy.yaml","name":"web-audit-policy"}}` {
		t.Errorf("%d audit policies, the last %v", policies, policy)
	}
	if hook, _ := fileOf(t, files, "/etc/kubernetes/audit-webhook.yaml"); asJSON(t, hook["contentFrom"]) != `{"secret":{"key":"kubeconfig","name":"siem"}}` {
		t.Errorf("webhook file %v", hook)
	}
	commands := commandsOf(t, cp["preKubeadmCommands"])
	if i := slices.Index(commands, "sysctl --system"); i != 1 || commands[0] != waitClockCommand || !slices.Equal(commands[len(commands)-2:], []string{"augenrules --load", "cp-only"}) {
		t.Errorf("commands %v: Kubehenge's first, then the company's for the control plane", commands)
	}
}

func checkKubeadmControlPlaneArgs(t *testing.T, cp map[string]any) {
	t.Helper()
	api := asMap(t, field(t, cp, "clusterConfiguration", "apiServer"))
	if args := argsOf(t, api["extraArgs"]); !strings.Contains(args, `{"name":"request-timeout","value":"30s"}`) ||
		!strings.Contains(args, `{"name":"audit-webhook-config-file","value":"/etc/kubernetes/audit-webhook.yaml"}`) || !strings.Contains(args, `"profiling"`) {
		t.Errorf("API server arguments %s", args)
	}
	if !strings.Contains(argsOf(t, api["extraVolumes"]), `"name":"audit-webhook"`) {
		t.Errorf("the webhook file is not mounted into the API server: %s", argsOf(t, api["extraVolumes"]))
	}
	if args := argsOf(t, field(t, cp, "clusterConfiguration", "controllerManager", "extraArgs")); !strings.Contains(args, `{"name":"concurrent-gc-syncs","value":"30"}`) || !strings.Contains(args, `"profiling"`) {
		t.Errorf("controller manager arguments %s", args)
	}
	if args := argsOf(t, field(t, cp, "clusterConfiguration", "scheduler", "extraArgs")); !strings.Contains(args, `{"name":"kube-api-qps","value":"60"}`) {
		t.Errorf("scheduler arguments %s", args)
	}
	for _, config := range []string{"initConfiguration", "joinConfiguration"} {
		args := argsOf(t, field(t, cp, config, "nodeRegistration", "kubeletExtraArgs"))
		if !strings.Contains(args, `{"name":"event-qps","value":"50"}`) || !strings.Contains(args, "provider-id") {
			t.Errorf("%s kubelet arguments %s", config, args)
		}
	}
}

func checkKubeadmWorker(t *testing.T, worker map[string]any) {
	t.Helper()
	if f, _ := fileOf(t, worker["files"], "/etc/co/worker.crt"); asJSON(t, f["contentFrom"]) != `{"secret":{"key":"ca","name":"cacert"}}` {
		t.Errorf("worker secret file %v", f)
	}
	if f, _ := fileOf(t, worker["files"], "/etc/audit/rules.d/50-co.rules"); f == nil {
		t.Error("a file for both roles is missing on the workers")
	}
	for _, path := range []string{"/etc/co/cp.conf", "/etc/kubernetes/audit-policy.yaml", "/etc/kubernetes/audit-webhook.yaml"} {
		if f, _ := fileOf(t, worker["files"], path); f != nil {
			t.Errorf("%s on a worker", path)
		}
	}
	if got := commandsOf(t, worker["preKubeadmCommands"]); !slices.Equal(got[len(got)-2:], []string{"augenrules --load", "worker-only"}) {
		t.Errorf("worker commands %v", got)
	}
	if args := argsOf(t, field(t, worker, "joinConfiguration", "nodeRegistration", "kubeletExtraArgs")); !strings.Contains(args, `{"name":"event-qps","value":"50"}`) {
		t.Errorf("worker kubelet arguments %s", args)
	}
	if strings.Contains(asJSON(t, worker), "request-timeout") || strings.Contains(asJSON(t, worker), "concurrent-gc-syncs") {
		t.Error("the control plane's arguments on a worker")
	}
}

func TestAgentNodesTakeTheirCustomisation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, dist, cpKind, workerKind, commands string }{
		{"rke2.yaml", "rke2", "RKE2ControlPlaneTemplate", "RKE2ConfigTemplate", "preRKE2Commands"},
		{"k3s.yaml", "k3s", "KThreesControlPlaneTemplate", "KThreesConfigTemplate", "preK3sCommands"},
	} {
		class := renderClass(t, c.file)
		vars := variablesOf(t, customised)
		cp := asMap(t, field(t, class.patched(t, c.cpKind, vars), "spec", "template", "spec"))
		if c.dist == "k3s" {
			cp = asMap(t, cp["kthreesConfigSpec"])
		}
		dir := "/etc/rancher/" + c.dist
		dropIn := func(files any) map[string]any {
			t.Helper()
			f, n := fileOf(t, files, dir+"/config.yaml.d/70-kubehenge-customisation.yaml")
			if n != 1 {
				t.Fatalf("%s: %d customisation drop-ins", c.file, n)
			}
			var content map[string]any
			text, isString := f["content"].(string)
			if !isString {
				t.Fatalf("%s: drop-in %v", c.file, f)
			}
			if err := json.Unmarshal([]byte(text), &content); err != nil {
				t.Fatal(err)
			}
			return content
		}
		got := dropIn(cp["files"])
		want := map[string]string{
			"kubelet-arg+":                 `["event-qps=50"]`,
			"kube-apiserver-arg+":          `["request-timeout=30s","audit-webhook-config-file=` + dir + `/audit-webhook.yaml"]`,
			"kube-controller-manager-arg+": `["concurrent-gc-syncs=30"]`,
			"kube-scheduler-arg+":          `["kube-api-qps=60"]`,
		}
		for key, v := range want {
			if asJSON(t, got[key]) != v {
				t.Errorf("%s %s = %s, want %s", c.file, key, asJSON(t, got[key]), v)
			}
		}
		if f, n := fileOf(t, cp["files"], dir+"/audit-policy.yaml"); n != 2 || asJSON(t, f["contentFrom"]) != `{"secret":{"key":"audit-policy.yaml","name":"web-audit-policy"}}` {
			t.Errorf("%s: %d audit policies, the last %v", c.file, n, f)
		}
		if f, _ := fileOf(t, cp["files"], dir+"/audit-webhook.yaml"); asJSON(t, f["contentFrom"]) != `{"secret":{"key":"kubeconfig","name":"siem"}}` {
			t.Errorf("%s: webhook file %v", c.file, f)
		}
		if f, _ := fileOf(t, cp["files"], "/etc/co/cp.conf"); f == nil {
			t.Errorf("%s: control-plane file missing", c.file)
		}
		if cmds := commandsOf(t, cp[c.commands]); !slices.Equal(cmds[len(cmds)-2:], []string{"augenrules --load", "cp-only"}) || cmds[0] != waitClockCommand || cmds[1] != "sysctl --system" {
			t.Errorf("%s: commands %v", c.file, cmds)
		}

		worker := asMap(t, field(t, class.patched(t, c.workerKind, vars), "spec", "template", "spec"))
		if w := dropIn(worker["files"]); asJSON(t, w) != `{"kubelet-arg+":["event-qps=50"]}` {
			t.Errorf("%s: worker drop-in %s", c.file, asJSON(t, w))
		}
		if f, _ := fileOf(t, worker["files"], "/etc/co/worker.crt"); asJSON(t, f["contentFrom"]) != `{"secret":{"key":"ca","name":"cacert"}}` {
			t.Errorf("%s: worker secret file %v", c.file, f)
		}
		if f, _ := fileOf(t, worker["files"], "/etc/co/cp.conf"); f != nil {
			t.Errorf("%s: a control-plane file on a worker", c.file)
		}
		if cmds := commandsOf(t, worker[c.commands]); !slices.Equal(cmds[len(cmds)-2:], []string{"augenrules --load", "worker-only"}) {
			t.Errorf("%s: worker commands %v", c.file, cmds)
		}
	}
}

// The API server reaches the kubelets directly, as on kubeadm and Talos, never through the agents'
// tunnel, whose connection sync cuts off a log read or exec opened in the same moment.
func TestAgentServersReachKubeletsDirectly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, dist, cpKind string }{
		{"rke2.yaml", "rke2", "RKE2ControlPlaneTemplate"},
		{"k3s.yaml", "k3s", "KThreesControlPlaneTemplate"},
	} {
		class := renderClass(t, c.file)
		cp := asMap(t, field(t, class.templates[c.cpKind], "spec", "template", "spec"))
		if c.dist == "k3s" {
			cp = asMap(t, cp["kthreesConfigSpec"])
		}
		f, n := fileOf(t, cp["files"], "/etc/rancher/"+c.dist+"/config.yaml.d/40-kubehenge-server.yaml")
		if n != 1 {
			t.Fatalf("%s: %d server drop-ins", c.file, n)
		}
		text, isString := f["content"].(string)
		if !isString {
			t.Fatalf("%s: drop-in %v", c.file, f)
		}
		var content map[string]any
		if err := yaml.Unmarshal([]byte(text), &content); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if content["egress-selector-mode"] != "disabled" {
			t.Errorf("%s: egress-selector-mode %v, want disabled", c.file, content["egress-selector-mode"])
		}
	}
}

func TestTalosNodesTakeTheirCustomisation(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	vars := variablesOf(t, talosCustomised)
	cp := talosConfig(t, machine.TypeControlPlane, class.strategicPatches(t, "TalosControlPlaneTemplate", vars))
	files := asJSON(t, field(t, cp["v1alpha1"], "machine", "files"))
	for _, want := range []string{"/var/co/all.conf", "/var/co/cp.conf", "everywhere"} {
		if !strings.Contains(files, want) {
			t.Errorf("control-plane files %s lack %s", files, want)
		}
	}
	if strings.Contains(files, "/var/co/worker.conf") {
		t.Errorf("a worker's file on the control plane: %s", files)
	}
	for kind, want := range map[string]string{
		"KubeletConfig": `"event-qps":"50"`, "KubeAPIServerConfig": `"request-timeout":"30s"`,
		"KubeControllerManagerConfig": `"concurrent-gc-syncs":"30"`, "KubeSchedulerConfig": `"kube-api-qps":"60"`,
	} {
		if got := asJSON(t, cp[kind]["extraArgs"]); !strings.Contains(got, want) {
			t.Errorf("%s arguments %s lack %s", kind, got, want)
		}
	}
	if got := asJSON(t, cp["KubeAPIServerConfig"]["extraArgs"]); !strings.Contains(got, "kubelet-certificate-authority") {
		t.Errorf("Kubehenge's API server arguments were dropped: %s", got)
	}
	if got := asJSON(t, field(t, cp["KubeAuditPolicyConfig"], "configuration", "rules")); got != `[{"level":"RequestResponse"}]` {
		t.Errorf("the audit policy was merged, not replaced: %s", got)
	}

	worker := talosConfig(t, machine.TypeWorker, class.strategicPatches(t, "TalosConfigTemplate", vars))
	files = asJSON(t, field(t, worker["v1alpha1"], "machine", "files"))
	if !strings.Contains(files, "/var/co/all.conf") || !strings.Contains(files, "/var/co/worker.conf") || strings.Contains(files, "/var/co/cp.conf") {
		t.Errorf("worker files %s", files)
	}
	if got := asJSON(t, worker["KubeletConfig"]["extraArgs"]); !strings.Contains(got, `"event-qps":"50"`) {
		t.Errorf("worker kubelet arguments %s", got)
	}
	if _, ok := worker["KubeAPIServerConfig"]; ok {
		t.Error("a worker with an API server configuration")
	}
}

func TestNothingChangesWithoutCustomisation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, cp, worker string }{
		{"kubeadm.yaml", "KubeadmControlPlaneTemplate", "KubeadmConfigTemplate"},
		{"rke2.yaml", "RKE2ControlPlaneTemplate", "RKE2ConfigTemplate"},
		{"k3s.yaml", "KThreesControlPlaneTemplate", "KThreesConfigTemplate"},
	} {
		class := renderClass(t, c.file)
		for _, kind := range []string{c.cp, c.worker} {
			bare := asJSON(t, class.patched(t, kind, variablesOf(t, `{}`)))
			for _, vars := range []string{`{"nodeCustomisation": {}}`, `{"nodeCustomisation": {"files": [], "commands": [], "kubeletArgs": []}}`} {
				if got := asJSON(t, class.patched(t, kind, variablesOf(t, vars))); got != bare {
					t.Errorf("%s %s with %s changed", c.file, kind, vars)
				}
			}
			if strings.Contains(bare, "customisation") || strings.Contains(bare, "audit-webhook") {
				t.Errorf("%s %s: %s", c.file, kind, bare)
			}
		}
	}
	talos := renderTalosClass(t)
	base := talos.strategicPatches(t, "TalosControlPlaneTemplate", variablesOf(t, `{}`))
	if got := talos.strategicPatches(t, "TalosControlPlaneTemplate", variablesOf(t, `{"nodeCustomisation": {}}`)); !slices.Equal(got, base) {
		t.Error("Talos patches changed without customisation")
	}
}

func TestNodeCustomisationIsValidatedByTheClass(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 64)
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		schema, ok := renderClass(t, file).variables["nodeCustomisation"]
		if !ok {
			t.Fatalf("%s has no nodeCustomisation", file)
		}
		for name, c := range map[string]struct{ value, want string }{
			"fine":                    {customisedValue(t), ""},
			"empty":                   {`{}`, ""},
			"permissions":             {`{"files":[{"path":"/etc/x","content":"x","permissions":"0777"}]}`, "permissions"},
			"permissions not octal":   {`{"files":[{"path":"/etc/x","content":"x","permissions":"rw"}]}`, "permissions"},
			"a path with a space":     {`{"files":[{"path":"/etc/a b","content":"x"}]}`, "path"},
			"a relative path":         {`{"files":[{"path":"etc/x","content":"x"}]}`, "path"},
			"a path with ..":          {`{"files":[{"path":"/etc/../x","content":"x"}]}`, "no . or .. part"},
			"a path with //":          {`{"files":[{"path":"/etc//x","content":"x"}]}`, "names a file"},
			"a directory":             {`{"files":[{"path":"/etc/x/","content":"x"}]}`, "names a file"},
			"content and secret":      {`{"files":[{"path":"/etc/x","content":"x","secret":{"name":"s","key":"k"}}]}`, "content or a secret, not both"},
			"neither":                 {`{"files":[{"path":"/etc/x"}]}`, "content or a secret, not both"},
			"a path twice":            {`{"files":[{"path":"/etc/x","content":"x"},{"path":"/etc/x","content":"y"}]}`, "Each path once"},
			"too many files":          {`{"files":[` + strings.TrimSuffix(strings.Repeat(`{"path":"/etc/x","content":"x"},`, 11), ",") + `]}`, "Too many"},
			"a secret name":           {`{"files":[{"path":"/etc/x","secret":{"name":"A_B","key":"k"}}]}`, "name"},
			"a secret without a key":  {`{"files":[{"path":"/etc/x","secret":{"name":"a"}}]}`, "key"},
			"an unknown role":         {`{"files":[{"path":"/etc/x","content":"x","roles":["etcd"]}]}`, "roles"},
			"no roles":                {`{"files":[{"path":"/etc/x","content":"x","roles":[]}]}`, "roles"},
			"an empty command":        {`{"commands":[{"run":""}]}`, "run"},
			"a long command":          {`{"commands":[{"run":"` + strings.Repeat("a", 1025) + `"}]}`, "run"},
			"an argument with dashes": {`{"kubeletArgs":[{"name":"--event-qps","value":"1"}]}`, "name"},
			"an argument twice":       {`{"kubeletArgs":[{"name":"a","value":"1"},{"name":"a","value":"2"}]}`, "Each argument once"},
			"a long argument":         {`{"kubeletArgs":[{"name":"` + long + `","value":"1"}]}`, "name"},
			"policy and configmap":    {`{"auditPolicy":{"configMap":"p","policy":{"rules":[]}}}`, "ConfigMap or the policy, not both"},
			"no policy":               {`{"auditPolicy":{}}`, "ConfigMap or the policy, not both"},
			"a webhook without one":   {`{"auditWebhook":{}}`, "secret"},
		} {
			var value any
			if err := json.Unmarshal([]byte(c.value), &value); err != nil {
				t.Fatal(err)
			}
			errs := validateVariable(t, schema, value, nil)
			switch {
			case c.want == "" && len(errs) > 0:
				t.Errorf("%s, %s: %v", file, name, errs)
			case c.want != "" && (len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), c.want)):
				t.Errorf("%s, %s: %v, want %q", file, name, errs, c.want)
			}
		}
	}
}

func customisedValue(t *testing.T) string {
	t.Helper()
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(customised), &all); err != nil {
		t.Fatal(err)
	}
	return string(all["nodeCustomisation"])
}

// flagsOf collects the arguments a rendered class sets for the control plane and the kubelet.
func flagsOf(t *testing.T, file string) map[string]bool {
	t.Helper()
	class := renderClass(t, file)
	found := map[string]bool{}
	arg := func(s string) {
		name, _, _ := strings.Cut(strings.TrimSpace(s), "=")
		found[strings.TrimSuffix(name, "+")] = true
	}
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				walk(k, child)
			}
			if name, ok := x["name"].(string); ok && x["value"] != nil && (key == "extraArgs" || key == "kubeletExtraArgs") {
				found[name] = true
			}
		case []any:
			for _, child := range x {
				if s, ok := child.(string); ok && strings.HasSuffix(strings.ToLower(key), "args") {
					arg(s)
				}
				walk(key, child)
			}
		case string:
			if key == "content" {
				dropInArgs(x, arg)
			}
		}
	}
	for _, tpl := range class.templates {
		walk("", tpl)
	}
	for _, p := range class.patches {
		if !strings.Contains(p.Name, "-node-") {
			patchArgs(p, arg, found)
		}
	}
	return found
}

// dropInArgs reads the arguments a configuration drop-in's text lists under its -arg keys.
func dropInArgs(text string, arg func(string)) {
	var drop map[string]any
	if yaml.Unmarshal([]byte(text), &drop) != nil {
		return
	}
	for k, list := range drop {
		items, ok := list.([]any)
		if !ok || !strings.Contains(k, "-arg") {
			continue
		}
		for _, item := range items {
			if s, isString := item.(string); isString {
				arg(s)
			}
		}
	}
}

// patchArgs reads the arguments a class patch adds to an argument list.
func patchArgs(p classPatch, arg func(string), found map[string]bool) {
	for _, d := range p.Definitions {
		for _, jp := range d.JSONPatches {
			if !strings.HasSuffix(jp.Path, "Args/-") && !strings.HasSuffix(jp.Path, "ExtraArgs/-") {
				continue
			}
			if s, ok := jp.Value.(string); ok {
				arg(s)
			}
			if jp.ValueFrom == nil {
				continue
			}
			for line := range strings.SplitSeq(jp.ValueFrom.Template, "\n") {
				if name, ok := strings.CutPrefix(line, "name: "); ok {
					found[name] = true
				}
			}
		}
	}
}

func TestEveryArgumentKubehengeSetsIsRefused(t *testing.T) {
	t.Parallel()
	components := []string{"kubelet", "apiServer", "controllerManager", "scheduler"}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml"} {
		found := flagsOf(t, file)
		if len(found) < 10 {
			t.Fatalf("%s: only %d arguments found: %v", file, len(found), found)
		}
		for name := range found {
			if !slices.ContainsFunc(components, func(c string) bool { return provisioner.ProtectedNodeFlag(c, name) }) {
				t.Errorf("%s sets %s and the variable does not refuse it", file, name)
			}
		}
	}
}
