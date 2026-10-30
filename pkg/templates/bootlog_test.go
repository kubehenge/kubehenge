package templates

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// The units, the script and the policy agree on where the boot log is and how its journals are
// marked, which the controller reads by BootLogFile and BootLogMark.
func TestBootLogUnits(t *testing.T) {
	t.Parallel()
	read := func(name string) string {
		t.Helper()
		b, err := recipeFiles.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	script, service, timer := read("node/boot-log.sh"), read("node/kubehenge-boot-log.service"), read("node/kubehenge-boot-log.timer")
	dir := path.Dir(BootLogFile)
	for _, want := range []string{"dir=${1:-" + dir + "}", "mark='" + BootLogMark + "'", `"${dir}/` + path.Base(BootLogFile) + `"`, "journal cloud-init ", "umask 077"} {
		if !strings.Contains(script, want) {
			t.Errorf("node/boot-log.sh does not say %s", want)
		}
	}
	for _, want := range []string{"Type=oneshot\n", "ExecStart=/usr/libexec/kubehenge/boot-log\n", "LogsDirectory=" + path.Base(dir) + "\n", "LogsDirectoryMode=0700\n", "ProtectSystem=strict\n"} {
		if !strings.Contains(service, want) {
			t.Errorf("the service does not say %s", strings.TrimSpace(want))
		}
	}
	for _, want := range []string{"OnBootSec=", "OnUnitActiveSec=", "WantedBy=timers.target\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("the timer does not say %s", strings.TrimSpace(want))
		}
	}
	if cil := read("node/kubehenge-qga.cil"); !strings.Contains(cil, `(filecon "`+dir+`(/.*)?" any (system_u object_r virt_qemu_ga_log_t`) ||
		!strings.Contains(cil, "(allow virt_qemu_ga_t cloud_log_t (file (getattr open read)))") {
		t.Error("the policy does not let the guest agent read the boot log and cloud-init's output")
	}
	for _, family := range []string{"el", "leap"} {
		node := read(family + "/node.sh")
		for _, want := range []string{
			`"${files}/node/boot-log.sh" /usr/libexec/kubehenge/boot-log`, `"${files}/node/kubehenge-boot-log.timer"`,
			`enable ` + bootLogUnit + `\n`, "systemctl preset qemu-guest-agent " + bootLogUnit, "guest-file-open,guest-file-read,guest-file-close",
		} {
			if !strings.Contains(node, want) {
				t.Errorf("%s/node.sh does not say %s", family, want)
			}
		}
	}
}

// fileReads answers the guest agent's file reads of VM 120 on pve1 from files, and refuses those
// of anything else as Proxmox VE does a file it cannot open.
func fileReads(t *testing.T, files map[string]string) *proxmox.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, ok := files[r.URL.Query().Get("file")]
		if r.URL.Path != "/api2/json/nodes/pve1/qemu/120/agent/file-read" || r.URL.Query().Get("count") == "" || !ok {
			http.Error(w, `{"data":null,"message":"guest-file-open failed"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"content": content, "truncated": 0}})
	}))
	t.Cleanup(srv.Close)
	return proxmox.NewClient(srv.URL + "/api2/json")
}

func TestCheckBootLogReadsWhatTheControllerReads(t *testing.T) {
	t.Parallel()
	vm := &proxmox.VirtualMachine{Node: "pve1", VMID: 120}
	log := BootLogMark + "journal of cloud-init\n2026-10-28T12:00:00+0000 node cloud-init[1]: done\n"
	for name, c := range map[string]struct {
		files map[string]string
		fails string
	}{
		"both":                     {files: map[string]string{BootLogFile: log, CloudInitOutput: "Cloud-init finished"}},
		"no journal of cloud-init": {files: map[string]string{BootLogFile: "", CloudInitOutput: "x"}, fails: "no journal of cloud-init"},
		"no copy":                  {files: map[string]string{CloudInitOutput: "x"}, fails: "the node's boot log"},
		"no cloud-init output":     {files: map[string]string{BootLogFile: log}, fails: "cloud-init's output"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &Builder{Client: fileReads(t, c.files)}
			err := b.checkBootLog(context.Background(), vm)
			if c.fails == "" && err != nil || c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)) {
				t.Errorf("error %v, want one saying %q", err, c.fails)
			}
		})
	}
}
