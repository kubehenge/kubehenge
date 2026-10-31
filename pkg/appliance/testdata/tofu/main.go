// Command tofu stands in for OpenTofu in the appliance's tests: called with -chdir=<dir> it records
// its arguments and environment in <dir>/../fake-tofu.log and answers like OpenTofu. It is built
// without the race detector, whose runtime takes about a second to start on some systems.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
			fmt.Print("admin-kubeconfig")
		case "api_endpoint":
			fmt.Print("https://10.0.0.20:6443")
		case "talosconfig":
			fmt.Print("context: management\ncontexts:\n  management:\n    endpoints: [10.0.0.11, 10.0.0.12]\n")
		}
	}
	return 0
}
