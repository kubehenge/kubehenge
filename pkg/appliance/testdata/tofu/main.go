// Command tofu stands in for OpenTofu in the appliance's tests: called with -chdir=<dir> it records
// its arguments and environment in <dir>/../fake-tofu.log and answers like OpenTofu. It is built
// without the race detector, whose runtime takes about a second to start on some systems.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

// raceFailures prints one error diagnostic for each letter of run, as a "race" apply reads them.
func raceFailures(run string) {
	const upid = "received an HTTP 500 response - Reason: got no worker upid - start worker failed"
	for _, c := range run {
		summary, address, detail := "Error downloading file from url", `module.management.proxmox_download_file.talos[\"pve\"]`, upid
		switch c {
		case 'v':
			summary, address = "Error creating VM", `module.management.proxmox_virtual_environment_vm.nodes[0]`
		case 'n':
			detail = "Could not download file, unexpected error: 404 Not Found"
		case 'w':
			summary, address = "Error upgrading chart", `module.management.helm_release.cluster_api_operator`
			detail = `Upgrade failed: cannot patch \"capi-operator-serving-cert\" with kind Certificate: Internal error occurred: failed calling webhook \"webhook.cert-manager.io\": failed to call webhook: Post \"https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s\": dial tcp 10.99.189.42:443: connect: no route to host`
		case 'r':
			summary, address = "Error upgrading chart", `module.management.helm_release.cluster_api_operator`
			detail = `Upgrade failed: cannot patch \"capi-operator-selfsigned-issuer\" with kind Issuer: admission webhook \"webhook.cert-manager.io\" denied the request: spec: Required value`
		}
		fmt.Printf(`{"@level":"error","@message":"Error: %s","type":"diagnostic","diagnostic":{"summary":"%s","detail":"%s","address":"%s"}}`+"\n", summary, summary, detail, address)
	}
}

// waitForInterrupt marks that it is applying, then writes its "state" once interrupted.
func waitForInterrupt(dir string) int {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	if err := os.WriteFile(filepath.Join(dir, "..", "applying"), nil, 0o600); err != nil {
		return 3
	}
	select {
	case <-interrupted:
		if err := os.WriteFile(filepath.Join(dir, "..", "state-flushed"), nil, 0o600); err != nil {
			return 3
		}
		return 1
	case <-time.After(30 * time.Second):
		return 4
	}
}

func main() {
	if len(os.Args) < 3 || !strings.HasPrefix(os.Args[1], "-chdir=") {
		os.Exit(2)
	}
	os.Exit(run(strings.TrimPrefix(os.Args[1], "-chdir="), os.Args[2:]))
}

func run(dir string, args []string) int {
	f, err := os.OpenFile(filepath.Join(dir, "..", "fake-tofu.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 3
	}
	rec, _ := json.Marshal(map[string]any{"args": args, "env": os.Environ()})
	_, _ = f.Write(append(rec, '\n'))
	_ = f.Close()
	vars, _ := os.ReadFile(filepath.Join(dir, "terraform.tfvars.json"))
	switch args[0] {
	case "init":
		fmt.Println("OpenTofu has been initialised")
	case "apply":
		// A "slow" apply runs until it is interrupted, then writes <dir>/../state-written as
		// OpenTofu writes its state, and fails.
		if strings.Contains(string(vars), `"slow"`) {
			interrupt := make(chan os.Signal, 1)
			signal.Notify(interrupt, os.Interrupt)
			// Said once it listens, so that a test interrupts it only from then on.
			fmt.Println(`{"@level":"info","@message":"Applying.","type":"log"}`)
			select {
			case <-interrupt:
				fmt.Println(`{"@level":"info","@message":"Interrupt received.","type":"log"}`)
				_ = os.WriteFile(filepath.Join(dir, "..", "state-written"), nil, 0o600)
			case <-time.After(30 * time.Second):
			}
			return 1
		}
		// A "held" apply runs until the test creates <dir>/../release.
		if strings.Contains(string(vars), `"held"`) {
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				if _, err := os.Stat(filepath.Join(dir, "..", "release")); err == nil || time.Now().After(deadline) {
					break
				}
			}
		}
		fmt.Println(`{"@level":"info","@message":"proxmox_virtual_environment_vm.node[0]: Creation complete","type":"apply_complete"}`)
		// A line longer than the appliance reads, then a failure.
		if strings.Contains(string(vars), `"long"`) {
			fmt.Println(strings.Repeat("x", 5<<20))
			fmt.Println(`{"@level":"error","@message":"Error: no such node","type":"diagnostic"}`)
			return 1
		}
		if strings.Contains(string(vars), `"twofail"`) {
			fmt.Println(`{"@level":"error","@message":"Error: pool","type":"diagnostic","diagnostic":{"summary":"Unable to create pool","detail":"pool kubehenge exists"}}`)
			fmt.Println(`{"@level":"warning","@message":"Warning: x","type":"diagnostic","diagnostic":{"summary":"Careful","detail":"not an error"}}`)
			fmt.Println(`{"@level":"error","@message":"Error: image","type":"diagnostic","diagnostic":{"summary":"Unable to download","detail":"storage ceph lacks import"}}`)
			return 1
		}
		// An "interrupt" apply stands for a long one: it writes its state only when interrupted, as
		// OpenTofu does.
		if strings.Contains(string(vars), `"interrupt"`) {
			return waitForInterrupt(dir)
		}
		// A "stall" apply fails on etcd timing out a request while <dir>/../stalls holds stalls left,
		// one fewer each time.
		if strings.Contains(string(vars), `"stall"`) {
			left, _ := os.ReadFile(filepath.Join(dir, "..", "stalls"))
			if n := len(left); n > 0 {
				_ = os.WriteFile(filepath.Join(dir, "..", "stalls"), left[1:], 0o600)
				fmt.Println(`{"@level":"error","@message":"Error: Helm release error","type":"diagnostic","diagnostic":{"summary":"Helm release error","detail":"1 error occurred:\n\t* etcdserver: request timed out","address":"module.management.helm_release.kubehenge"}}`)
				return 1
			}
		}
		// A "race" apply fails with the errors of the first line left in <dir>/../races, which it
		// takes away: d for a download Proxmox VE did not start a task for, v for a VM's create it
		// did not, and n for a download that found no file.
		if strings.Contains(string(vars), `"race"`) {
			left, _ := os.ReadFile(filepath.Join(dir, "..", "races"))
			if run, rest, _ := strings.Cut(string(left), "\n"); run != "" {
				_ = os.WriteFile(filepath.Join(dir, "..", "races"), []byte(rest), 0o600)
				raceFailures(run)
				return 1
			}
		}
		if strings.Contains(string(vars), `"fail"`) {
			fmt.Println(`{"@level":"error","@message":"Error: no such node","type":"diagnostic","diagnostic":{"summary":"Error","detail":"node fail does not exist"}}`)
			return 1
		}
	case "plan":
		for _, arg := range args {
			if out, ok := strings.CutPrefix(arg, "-out="); ok {
				if err := os.WriteFile(filepath.Join(dir, out), []byte("plan"), 0o600); err != nil {
					return 3
				}
			}
		}
	case "show":
		// A test leaves the plan as `tofu show -json` prints it in <dir>/../plan.json.
		if _, err := os.Stat(filepath.Join(dir, args[len(args)-1])); err != nil {
			fmt.Fprintln(os.Stderr, "Error: no saved plan")
			return 1
		}
		shown, err := os.ReadFile(filepath.Join(dir, "..", "plan.json"))
		if err != nil {
			shown = []byte(`{"resource_changes":[]}`)
		}
		fmt.Print(string(shown))
	case "state":
		// A test lists what the state holds in <dir>/../state-list.
		if len(args) > 1 && args[1] == "list" {
			held, _ := os.ReadFile(filepath.Join(dir, "..", "state-list"))
			fmt.Print(string(held))
		}
	case "output":
		// The state opens only with the passphrase the tests use.
		if !strings.Contains(os.Getenv("TF_ENCRYPTION"), "correct horse battery staple") {
			fmt.Fprintln(os.Stderr, "Error: decryption failed")
			return 1
		}
		switch args[len(args)-1] {
		case "kubeconfig":
			// A test that serves the cluster's API leaves the kubeconfig for it in
			// <dir>/../admin.kubeconfig.
			if admin, err := os.ReadFile(filepath.Join(dir, "..", "admin.kubeconfig")); err == nil {
				fmt.Print(string(admin))
				break
			}
			fmt.Print("admin-kubeconfig")
		case "api_endpoint":
			fmt.Print("https://10.0.0.20:6443")
		case "talosconfig":
			fmt.Print("context: management\ncontexts:\n  management:\n    endpoints: [10.0.0.11, 10.0.0.12]\n")
		}
	}
	return 0
}
