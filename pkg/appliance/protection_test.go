package appliance

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// protectCall is one call to set or lift a VM's protection, with the tofu command that ran last
// before it.
type protectCall struct {
	vm    int
	on    bool
	after string
}

// protections records the protection calls of a, failing those fail picks.
type protections struct {
	mu    sync.Mutex
	calls []protectCall
}

func (p *protections) list() []protectCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func recordProtections(a *Appliance, fail func(vm managementVM, on bool) error) *protections {
	p := &protections{}
	a.protect = func(_ context.Context, _ *pve.Ticket, vm managementVM, value protection) error {
		p.mu.Lock()
		p.calls = append(p.calls, protectCall{vm.ID, value == protected, lastTofuCommand(a)})
		p.mu.Unlock()
		if fail != nil {
			return fail(vm, value == protected)
		}
		return nil
	}
	return p
}

// lastTofuCommand is the first argument of the last command the fake OpenTofu ran.
func lastTofuCommand(a *Appliance) string {
	raw, _ := os.ReadFile(filepath.Join(a.Dir, "fake-tofu.log"))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var rec struct {
		Args []string `json:"args"`
	}
	if json.Unmarshal([]byte(lines[len(lines)-1]), &rec) != nil || len(rec.Args) == 0 {
		return ""
	}
	return rec.Args[0]
}

// change is one entry of a plan as `tofu show -json` prints it.
type change struct {
	address string
	actions []string
	id      int
}

// writePlan leaves the plan the fake OpenTofu shows.
func writePlan(t *testing.T, a *Appliance, changes ...change) {
	t.Helper()
	rcs := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		rcs = append(rcs, map[string]any{
			"address": c.address,
			"change": map[string]any{
				"actions": c.actions,
				"before":  map[string]any{"vm_id": c.id, "name": fmt.Sprintf("vm-%d", c.id), "protection": true},
			},
		})
	}
	raw, err := json.Marshal(map[string]any{"resource_changes": rcs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "plan.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func applyWith(t *testing.T, a *Appliance, node string) *JobView {
	t.Helper()
	if err := a.Apply(settings(node), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return waitFor(t, a)
}

// A replace and a scale-down lift protection on exactly the VMs they delete, between reading the
// plan and applying it. The backups VM and an in-place change keep theirs.
func TestApplyLiftsProtectionOnTheVMsThePlanDeletes(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	p := recordProtections(a, nil)
	writePlan(t, a,
		change{nodeVMAddress + "[0]", []string{"update"}, 1001},
		change{nodeVMAddress + "[1]", []string{"delete", "create"}, 1002},
		change{nodeVMAddress + "[2]", []string{"delete"}, 1003},
		change{backupVolumeAddress, []string{"delete"}, 1004},
	)
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	want := []protectCall{{1002, false, "show"}, {1003, false, "show"}}
	if got := p.list(); !slices.Equal(got, want) {
		t.Fatalf("protection calls %+v, want %+v", got, want)
	}
	var applies [][]string
	for _, args := range tofuCalls(t, a) {
		if args[0] == "apply" {
			applies = append(applies, args)
		}
	}
	if len(applies) != 1 || !slices.Equal(applies[0], []string{"apply", "-auto-approve", "-input=false", "-json", planFile}) {
		t.Fatalf("applies %q, want one of the plan read", applies)
	}
	if _, err := os.Stat(filepath.Join(a.workdir(), planFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the plan is left in the working directory: %v", err)
	}
}

// An apply that fails on etcd timing out a request plans and applies again, up to applyAttempts
// runs in all. Each run lifts and sets again the protection it needs.
func TestApplyRunsAgainWhileEtcdTimesOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stalls string
		state  string
	}{
		{"x", StateSucceeded},
		{"xx", StateSucceeded},
		{"xxx", StateFailed},
	} {
		t.Run(fmt.Sprintf("%d stalls", len(tc.stalls)), func(t *testing.T) {
			t.Parallel()
			a, _ := newAppliance(t)
			a.stallPause = time.Millisecond
			p := recordProtections(a, nil)
			writePlan(t, a, change{nodeVMAddress + "[0]", []string{"delete", "create"}, 1001})
			if err := os.WriteFile(filepath.Join(a.Dir, "stalls"), []byte(tc.stalls), 0o600); err != nil {
				t.Fatal(err)
			}
			j := applyWith(t, a, "stall")
			if j.State != tc.state {
				t.Fatalf("apply: %+v", j)
			}
			if tc.state == StateFailed && !strings.Contains(j.Error, "etcdserver: request timed out") {
				t.Fatalf("the job's error lacks etcd's: %s", j.Error)
			}
			runs := min(len(tc.stalls)+1, applyAttempts)
			var plans, applies int
			for _, args := range tofuCalls(t, a) {
				switch args[0] {
				case "plan":
					plans++
				case "apply":
					applies++
				}
			}
			if plans != runs || applies != runs {
				t.Fatalf("%d plans and %d applies, want %d of each", plans, applies, runs)
			}
			lifts := 0
			for _, c := range p.list() {
				if !c.on {
					lifts++
				}
			}
			if lifts != runs {
				t.Fatalf("protection lifted %d times, want once a run: %+v", lifts, p.list())
			}
		})
	}
}

// Proxmox VE fails to start a task's worker when another task ends at that moment. A run whose
// every error is a download it did not start runs again, as those made nothing. A VM's create,
// which leaves its configuration locked, fails the job at once, as does any other error beside a
// raced download. Each line of races is one run: d a raced download, v a raced VM create, n a
// download that found no file.
func TestApplyRunsAgainWhenADownloadDidNotStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, races string
		state       string
		applies     int
	}{
		{"a download raced", "d\n", StateSucceeded, 2},
		{"two downloads raced", "dd\n", StateSucceeded, 2},
		{"a VM create raced", "v\n", StateFailed, 1},
		{"a VM create raced and a download found no file", "vn\n", StateFailed, 1},
		{"a download raced and a VM create raced", "dv\n", StateFailed, 1},
		{"a download raced, then found no file", "d\nn\n", StateFailed, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppliance(t)
			a.stallPause = time.Millisecond
			writePlan(t, a, change{nodeVMAddress + "[0]", []string{"update"}, 1001})
			if err := os.WriteFile(filepath.Join(a.Dir, "races"), []byte(tc.races), 0o600); err != nil {
				t.Fatal(err)
			}
			j := applyWith(t, a, "race")
			if j.State != tc.state {
				t.Fatalf("apply: %+v", j)
			}
			if tc.state == StateFailed && !strings.Contains(j.Error, "Error ") {
				t.Fatalf("the job's error lacks OpenTofu's: %s", j.Error)
			}
			applies := 0
			for _, args := range tofuCalls(t, a) {
				if args[0] == "apply" {
					applies++
				}
			}
			if applies != tc.applies {
				t.Fatalf("%d applies, want %d", applies, tc.applies)
			}
		})
	}
}

// A service stop in the pause before the next run ends the job as a stop mid-run does.
func TestStopDuringTheEtcdPauseSaysRunItAgain(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	a.stallPause = time.Hour
	writePlan(t, a, change{nodeVMAddress + "[0]", []string{"update"}, 1001})
	if err := os.WriteFile(filepath.Join(a.Dir, "stalls"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(settings("stall"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for j := a.Current(); j == nil || !slices.ContainsFunc(j.Log, func(l string) bool { return strings.Contains(l, "Running again in") }); j = a.Current() {
		if ctx.Err() != nil {
			t.Fatalf("the job never paused: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if j := waitFor(t, a); j.State != StateFailed || !strings.Contains(j.Error, "interrupted as the appliance's service stopped") || !strings.Contains(j.Error, "so run it again") {
		t.Fatalf("the job once stopped = %+v", j)
	}
}

func TestInPlaceChangeAppliesWithProtectionOn(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	p := recordProtections(a, nil)
	writePlan(t, a, change{nodeVMAddress + "[0]", []string{"update"}, 1001}, change{nodeVMAddress + "[1]", []string{"no-op"}, 1002})
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	if got := p.list(); len(got) != 0 {
		t.Fatalf("an in-place change touched protection: %+v", got)
	}
}

// The module leaves protection to the appliance, so a run first sets it again on a node VM its
// refresh found without it, except one the CSI plugin lifted for a detach and marked.
func TestApplyProtectsNodeVMsFoundWithoutIt(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	p := recordProtections(a, nil)
	before := func(id int, protection bool, description string) map[string]any {
		return map[string]any{"vm_id": id, "name": fmt.Sprintf("vm-%d", id), "protection": protection, "description": description}
	}
	raw, err := json.Marshal(map[string]any{"resource_changes": []map[string]any{
		{"address": nodeVMAddress + "[0]", "change": map[string]any{"actions": []string{"no-op"}, "before": before(1001, false, "Node")}},
		{"address": nodeVMAddress + "[1]", "change": map[string]any{"actions": []string{"no-op"}, "before": before(1002, false, "Node\n"+csiLiftedMarker)}},
		{"address": nodeVMAddress + "[2]", "change": map[string]any{"actions": []string{"no-op"}, "before": before(1003, true, "Node")}},
		{"address": nodeVMAddress + "[3]", "change": map[string]any{"actions": []string{"delete"}, "before": before(1004, false, "Node")}},
		{"address": backupVolumeAddress, "change": map[string]any{"actions": []string{"no-op"}, "before": before(1005, false, "")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Dir, "plan.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	want := []protectCall{{1001, true, "show"}, {1004, false, "show"}}
	if got := p.list(); !slices.Equal(got, want) {
		t.Fatalf("protection calls %+v, want %+v", got, want)
	}
}

// The module leaves the node VMs' protection and description alone after creating them, so that an
// apply in the middle of a detach neither sets the protection the plugin lifted nor takes its marker
// off. Mocked OpenTofu tests cannot drift a configured attribute, so the module's own text is read.
func TestModuleLeavesNodeVMProtectionToTheAppliance(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "management-cluster", "proxmox.tf"))
	if err != nil {
		t.Fatal(err)
	}
	module := string(raw)
	start := strings.Index(module, `resource "proxmox_virtual_environment_vm" "node" {`)
	if start < 0 {
		t.Fatal("the node VMs' resource is not in proxmox.tf")
	}
	node := module[start:]
	if end := strings.Index(node[1:], "\nresource "); end >= 0 {
		node = node[:end+1]
	}
	ignored := ""
	for line := range strings.SplitSeq(node, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ignore_changes") {
			ignored = line
		}
	}
	fields := strings.FieldsFunc(ignored, func(r rune) bool { return strings.ContainsRune("[], =", r) })
	for _, field := range []string{"description", "protection"} {
		if !slices.Contains(fields, field) {
			t.Errorf("the node VMs' ignore_changes (%q) lacks %s", strings.TrimSpace(ignored), field)
		}
	}
}

// The appliance and the patched CSI plugin name the plugin's marker alike.
func TestCSIMarkerMatchesThePluginPatch(t *testing.T) {
	t.Parallel()
	patch, err := os.ReadFile(filepath.Join("..", "..", "scripts", "proxmox-csi-patches", "lift-protection-to-detach.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "+const liftedMarker = \""+csiLiftedMarker+"\"") {
		t.Fatal("the plugin patch's liftedMarker is not csiLiftedMarker")
	}
}

// A lift takes the CSI plugin's marker off in the same change, against the configuration it read,
// and reads it again when the plugin changed it meanwhile.
func TestLiftTakesTheCSIMarkerOff(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var puts []map[string]any
	digest := 1
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":1001,"name":"mgmt-cp-0","node":"pve2","type":"qemu"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/qemu/1001/config"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"digest": strconv.Itoa(digest), "description": "Node\n" + csiLiftedMarker}})
		case r.Method == http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			puts = append(puts, body)
			if len(puts) == 1 {
				// The plugin changed the configuration since it was read.
				digest++
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Appliance{ProxmoxURL: srv.URL, ProxmoxCAFile: ca}
	if err := a.setProtection(context.Background(), ticket(), managementVM{1001, "mgmt-cp-0"}, unprotected); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 2 {
		t.Fatalf("puts %v, want a refused one and one on the configuration read again", puts)
	}
	last := puts[1]
	if last["protection"] != float64(0) || last["description"] != "Node" || last["digest"] != "2" {
		t.Fatalf("lift %v, want protection 0, the description without the marker, digest 2", last)
	}
}

// A failed apply sets protection again on what it lifted, and says how to set it by hand where
// that fails too.
func TestFailedApplyProtectsTheLiftedVMsAgain(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]bool{"set again": false, "refused": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppliance(t)
			p := recordProtections(a, func(_ managementVM, on bool) error {
				if on && refuse {
					return errors.New("permission denied")
				}
				return nil
			})
			writePlan(t, a, change{nodeVMAddress + "[0]", []string{"delete", "create"}, 1001})
			j := applyWith(t, a, "fail")
			if j.State != StateFailed || !strings.Contains(j.Error, "node fail does not exist") {
				t.Fatalf("apply: %+v", j)
			}
			want := []protectCall{{1001, false, "show"}, {1001, true, "apply"}}
			if got := p.list(); !slices.Equal(got, want) {
				t.Fatalf("protection calls %+v, want %+v", got, want)
			}
			if told := strings.Contains(j.Error, "qm set 1001 --protection 1"); told != refuse {
				t.Fatalf("the error tells how to set protection by hand %v, with setting it refused %v: %s", told, refuse, j.Error)
			}
		})
	}
}

// A lift Proxmox VE refuses stops the run before the apply, with the VMs lifted before it
// protected again.
func TestRefusedLiftProtectsTheOthersAgain(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	p := recordProtections(a, func(vm managementVM, on bool) error {
		if vm.ID == 1002 && !on {
			return errors.New("permission denied")
		}
		return nil
	})
	writePlan(t, a, change{nodeVMAddress + "[1]", []string{"delete"}, 1001}, change{nodeVMAddress + "[2]", []string{"delete"}, 1002})
	j := applyWith(t, a, "pve1")
	if j.State != StateFailed || !strings.Contains(j.Error, "lifting the protection of VM 1002") {
		t.Fatalf("apply: %+v", j)
	}
	want := []protectCall{{1001, false, "show"}, {1002, false, "show"}, {1001, true, "show"}}
	if got := p.list(); !slices.Equal(got, want) {
		t.Fatalf("protection calls %+v, want %+v", got, want)
	}
}

func TestRemoveLiftsTheNodeVMsProtectionBeforeTheDestroy(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if j := applyWith(t, a, "pve1"); j.State != StateSucceeded {
		t.Fatalf("apply: %+v", j)
	}
	if err := os.WriteFile(filepath.Join(a.workdir(), "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := recordProtections(a, nil)
	writePlan(t, a, change{nodeVMAddress + "[0]", []string{"delete"}, 1001}, change{nodeVMAddress + "[1]", []string{"delete"}, 1002})
	ran := len(tofuCalls(t, a))
	if err := a.Remove("correct horse battery staple", KeepBackups, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("remove: %+v", j)
	}
	want := []protectCall{{1001, false, "show"}, {1002, false, "show"}}
	if got := p.list(); !slices.Equal(got, want) {
		t.Fatalf("protection calls %+v, want %+v", got, want)
	}
	// Each lift ran after the plan was shown, and the destroy's apply after them.
	destroyApplied(t, tofuCalls(t, a)[ran:])
}

// Recreating the VMs of a lost node lifts protection on a VM the targeted plan replaces.
func TestRecreatingTheVMsLiftsProtectionOnThoseReplaced(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	p := recordProtections(a, nil)
	writePlan(t, a, change{nodeVMAddress + "[0]", []string{"create"}, 0}, change{nodeVMAddress + "[1]", []string{"delete", "create"}, 1002})
	r := &recreation{passphrase: "correct horse battery staple", ticket: ticket(), settings: settings("pve1"), targets: recreateTargets(1)}
	if err := a.applyVMs(context.Background(), &Job{}, r); err != nil {
		t.Fatal(err)
	}
	want := []protectCall{{1002, false, "show"}}
	if got := p.list(); !slices.Equal(got, want) {
		t.Fatalf("protection calls %+v, want %+v", got, want)
	}
	if plans := appliedPlans(tofuCalls(t, a)); len(plans) != 1 || !slices.Equal(plans[0][len(plans[0])-2:], r.targets) {
		t.Fatalf("applied plans %q, want the targeted one", plans)
	}
}

// Proxmox VE is asked on the node the VM runs on now, which HA may have changed, and a VM that is
// gone or whose ID another VM holds is passed by.
func TestSetProtectionFollowsTheVM(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[` +
				`{"vmid":1001,"name":"mgmt-cp-0","node":"pve2","type":"qemu"},` +
				`{"vmid":1002,"name":"someone-else","node":"pve1","type":"qemu"},` +
				`{"vmid":1003,"name":"mgmt-cp-2","node":"pve3","type":"lxc"}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(`{"data":{"digest":"d"}}`))
		case r.Method == http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			puts = append(puts, fmt.Sprintf("%s protection=%v csrf=%s", r.URL.Path, body["protection"], r.Header.Get("Csrfpreventiontoken")))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Appliance{ProxmoxURL: srv.URL, ProxmoxCAFile: ca}
	ctx := context.Background()
	for _, c := range []struct {
		vm    managementVM
		value protection
	}{
		{managementVM{1001, "mgmt-cp-0"}, unprotected},
		{managementVM{1001, "mgmt-cp-0"}, protected},
		{managementVM{1002, "mgmt-cp-1"}, unprotected},
		{managementVM{1003, "mgmt-cp-2"}, unprotected},
		{managementVM{1009, "mgmt-cp-3"}, protected},
	} {
		if err := a.setProtection(ctx, ticket(), c.vm, c.value); err != nil {
			t.Fatalf("VM %d: %v", c.vm.ID, err)
		}
	}
	want := []string{"/api2/json/nodes/pve2/qemu/1001/config protection=0 csrf=csrf", "/api2/json/nodes/pve2/qemu/1001/config protection=1 csrf=csrf"}
	if !slices.Equal(puts, want) {
		t.Fatalf("puts %q, want %q", puts, want)
	}
}
