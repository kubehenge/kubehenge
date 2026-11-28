package templates

import (
	"fmt"
	"strings"
	"testing"
)

func TestHardwareNotesRoundTrip(t *testing.T) {
	t.Parallel()
	facts := BuildFacts{Kernel: "6.12.0-124.8.1.el10_1.x86_64", Packages: strings.Repeat("c", 64)}
	for _, hw := range []Hardware{
		DefaultHardware(DefaultCPUType),
		{CPUType: "host"},
		{CPUType: "Skylake-Server-noTSX-IBRS", SSD: true, IOThread: true},
		{CPUType: "EPYC-Rome-v2", Discard: true, NUMA: true},
		{CPUType: "custom-gold", SSD: true, Discard: true, IOThread: true, NUMA: true},
	} {
		for _, recorded := range []*BuildFacts{nil, &facts} {
			notes := Notes(templateDescription, hw, recorded)
			got, ok := HardwareOfNotes(notes)
			if !ok || got != hw {
				t.Errorf("%+v came back as %+v (recorded %v) through %q", hw, got, ok, notes)
			}
			if back, ok := ParseBuildFacts(notes); ok != (recorded != nil) || (ok && back != facts) {
				t.Errorf("facts %+v, %v through %q", back, ok, notes)
			}
		}
	}
	if _, recorded := HardwareOfNotes(templateDescription); recorded {
		t.Error("notes without hardware record some")
	}
}

func TestValidateCPUType(t *testing.T) {
	t.Parallel()
	for cpu, ok := range map[string]bool{
		"": true, "host": true, "max": true, "x86-64-v2-AES": true, "x86-64-v3": true, "kvm64": true,
		"Haswell": true, "Haswell-noTSX-IBRS": true, "EPYC-Rome-v2": true, "Skylake-Server-v5": true, "custom-gold": true,
		"haswell": false, "x86-64-v5": false, "Haswel": false, "custom-Gold": false, "host,flags=+aes": false, "Ryzen": false,
	} {
		if err := ValidateCPUType(cpu); (err == nil) != ok {
			t.Errorf("ValidateCPUType(%q) = %v, want ok %v", cpu, err, ok)
		}
	}
}

func TestCPUTypeLevel(t *testing.T) {
	t.Parallel()
	for cpu, want := range map[string]int{
		"x86-64-v2-AES": 2, "x86-64-v3": 3, "x86-64-v4": 4, "host": 1, "kvm64": 1, "custom-gold": 1,
		"Nehalem": 2, "Haswell-noTSX": 3, "EPYC-Milan-v2": 3, "Icelake-Server-v7": 4, "EPYC-Genoa": 4,
	} {
		if got := CPUTypeLevel(cpu); got != want {
			t.Errorf("CPUTypeLevel(%q) = %d, want %d", cpu, got, want)
		}
	}
}

func TestCreateAppliesTheHardware(t *testing.T) {
	t.Parallel()
	no, yes := false, true
	for _, tc := range []struct {
		name   string
		choice HardwareChoice
		disk   string
		scsihw string
		numa   bool
		cpu    string
		notes  string
	}{
		{"the defaults", HardwareChoice{}, ",backup=0,discard=on,ssd=1", "virtio-scsi-pci", false, DefaultCPUType, "kubehenge-hardware: x86-64-v2-AES,ssd,discard"},
		{"every option changed", HardwareChoice{CPUType: "Haswell", SSD: &no, Discard: &no, IOThread: &yes, NUMA: &yes}, ",backup=0,iothread=1", "virtio-scsi-single", true, "Haswell", "kubehenge-hardware: Haswell,iothread,numa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newBakeHost(t)
			b, tmpl, _ := talosBuilder(t, h)
			b.Hardware = tc.choice
			if _, err := b.Create(t.Context(), &tmpl); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			c := h.vms[101]
			if c["cpu"] != tc.cpu || c["scsihw"] != tc.scsihw || c["scsi0"] != "local-lvm:5"+tc.disk {
				t.Errorf("the template is %v", c)
			}
			if _, numa := c["numa"]; numa != tc.numa {
				t.Errorf("numa set: %v, want %v (%v)", numa, tc.numa, c["numa"])
			}
			// The hardware is in the notes, the tags are the template's alone.
			if c["tags"] != strings.Join(tmpl.Tags, ";") {
				t.Errorf("tags %v, want %v", c["tags"], tmpl.Tags)
			}
			if notes := fmt.Sprint(c["description"]); !strings.Contains(notes, "\n"+tc.notes) {
				t.Errorf("notes %q lack %q", notes, tc.notes)
			}
		})
	}
}

func TestCreateRefusesAnUnknownCPUType(t *testing.T) {
	t.Parallel()
	h := newBakeHost(t)
	b, tmpl, _ := talosBuilder(t, h)
	b.Hardware.CPUType = "Pentium9"
	if _, err := b.Create(t.Context(), &tmpl); err == nil || !strings.Contains(err.Error(), "Pentium9") {
		t.Errorf("Create gave %v", err)
	}
}
