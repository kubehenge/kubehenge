package kubehenge_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/machine"
)

const (
	ntpNetwork = `{"controlPlaneEndpoint": "192.0.2.10", "cni": "flannel", "network": {"bridge": "vmbr0", "ntp": ["ntp1.example.org", "192.0.2.5"]}}`
	bareNet    = `{"controlPlaneEndpoint": "192.0.2.10", "cni": "flannel", "network": {"bridge": "vmbr0"}}`
)

func TestTalosNodesGetTimeSyncWithTheClusterServersAndABoundedWait(t *testing.T) {
	t.Parallel()
	class := renderTalosClass(t)
	for kind, role := range map[string]machine.Type{"TalosControlPlaneTemplate": machine.TypeControlPlane, "TalosConfigTemplate": machine.TypeWorker} {
		set := field(t, talosConfig(t, role, class.strategicPatches(t, kind, variablesOf(t, ntpNetwork)))["v1alpha1"], "machine", "time")
		if got := asJSON(t, set); got != `{"bootTimeout":"5m0s","servers":["ntp1.example.org","192.0.2.5","/dev/ptp0"]}` {
			t.Errorf("%s: machine.time is %s, want the cluster's servers, the hypervisor's PTP clock and a 5m boot timeout (Talos waits for ever by default)", kind, got)
		}
		bare := field(t, talosConfig(t, role, class.strategicPatches(t, kind, variablesOf(t, bareNet)))["v1alpha1"], "machine", "time")
		if got := asJSON(t, bare); got != `{"bootTimeout":"5m0s","servers":["time.cloudflare.com","/dev/ptp0"]}` {
			t.Errorf("%s: without servers machine.time is %s, want Talos's default server and the PTP clock", kind, got)
		}
	}
}

// clockCase is where a distribution's templates keep the files and the first commands of a node.
type clockCase struct {
	file, kind, commands string
	files                []string
}

func TestAgentAndKubeadmNodesWaitForTheClockAndTakeTheClusterServers(t *testing.T) {
	t.Parallel()
	cases := []clockCase{
		{"kubeadm.yaml", "KubeadmControlPlaneTemplate", "preKubeadmCommands", []string{"spec", "template", "spec", "kubeadmConfigSpec"}},
		{"kubeadm.yaml", "KubeadmConfigTemplate", "preKubeadmCommands", []string{"spec", "template", "spec"}},
		{"rke2.yaml", "RKE2ControlPlaneTemplate", "preRKE2Commands", []string{"spec", "template", "spec"}},
		{"rke2.yaml", "RKE2ConfigTemplate", "preRKE2Commands", []string{"spec", "template", "spec"}},
		{"k3s.yaml", "KThreesControlPlaneTemplate", "preK3sCommands", []string{"spec", "template", "spec", "kthreesConfigSpec"}},
		{"k3s.yaml", "KThreesConfigTemplate", "preK3sCommands", []string{"spec", "template", "spec"}},
	}
	for _, c := range cases {
		class := renderClass(t, c.file)
		for vars, servers := range map[string]string{ntpNetwork: "server ntp1.example.org iburst\nserver 192.0.2.5 iburst\n", bareNet: ""} {
			spec := asMap(t, field(t, class.patched(t, c.kind, variablesOf(t, vars)), c.files...))
			name := c.file + " " + c.kind
			commands, ok := spec[c.commands].([]any)
			if !ok || len(commands) == 0 || commands[0] != "/etc/kubehenge/wait-clock.sh || exit 1" {
				t.Errorf("%s: the first command must wait for the clock and end the bootstrap when it fails: %v", name, commands)
			}
			files, ok := spec["files"].([]any)
			if !ok {
				t.Fatalf("%s: no files", name)
			}
			var script, dropin string
			for _, f := range files {
				file := asMap(t, f)
				content, ok := file["content"].(string)
				if !ok {
					continue
				}
				switch file["path"] {
				case "/etc/kubehenge/wait-clock.sh":
					script = content
				case "/etc/chrony.d/50-kubehenge.conf":
					dropin = content
				}
			}
			if !strings.Contains(script, "chronyc waitsync 18") || !strings.Contains(script, "clock not synced") {
				t.Errorf("%s: wait-clock.sh is %q", name, script)
			}
			if want := "makestep 1.0 3\n" + servers; dropin != want {
				t.Errorf("%s: chrony drop-in %q, want %q", name, dropin, want)
			}
		}
	}
}

func TestEveryClassOffersTheTimeServersOfTheNetworkVariable(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml", "talos.yaml"} {
		schema := renderClass(t, file).variables["network"]
		network := func(ntp string) any {
			var v any
			raw := `{"bridge":"vmbr0","addresses":["10.0.0.5"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"],"ntp":` + ntp + `}`
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if errs := validateVariable(t, schema, network(`["a.example.org","b.example.org"]`), nil); len(errs) > 0 {
			t.Errorf("%s: %v", file, errs)
		}
		nine := `["a","b","c","d","e","f","g","h","i"]`
		if errs := validateVariable(t, schema, network(nine), nil); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "ntp") {
			t.Errorf("%s: nine servers: %v", file, errs)
		}
		if errs := validateVariable(t, schema, network(`[""]`), nil); len(errs) == 0 {
			t.Errorf("%s: an empty server was accepted", file)
		}
	}
}
