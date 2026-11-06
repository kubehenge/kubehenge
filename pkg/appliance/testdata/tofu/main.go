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
		if strings.Contains(string(vars), `"fail"`) {
			fmt.Println(`{"@level":"error","@message":"Error: no such node","type":"diagnostic","diagnostic":{"summary":"Error","detail":"node fail does not exist"}}`)
			return 1
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
