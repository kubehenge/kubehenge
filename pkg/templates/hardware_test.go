package templates

import (
	"slices"
	"strings"
	"testing"
)

func TestHardwareTagsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, hw := range []Hardware{
		DefaultHardware(DefaultCPUType),
		{CPUType: "host"},
		{CPUType: "Skylake-Server-noTSX-IBRS", SSD: true, IOThread: true},
		{CPUType: "EPYC-Rome-v2", Discard: true, NUMA: true},
		{CPUType: "custom-gold", SSD: true, Discard: true, IOThread: true, NUMA: true},
	} {
		tags := hw.Tags()
		for _, tag := range tags {
			if !tagRE.MatchString(tag) {
				t.Errorf("%+v: %q is no Proxmox VE tag", hw, tag)
			}
		}
		got, recorded := HardwareOf(tags)
		if !recorded || got != hw {
			t.Errorf("%+v came back as %+v (recorded %v) through %v", hw, got, recorded, tags)
		}
	}
	if _, recorded := HardwareOf([]string{"kubehenge", "rke2"}); recorded {
		t.Error("tags without hardware record some")
	}
}

func TestHardwareTagsAreNoPartOfTheLine(t *testing.T) {
	t.Parallel()
	line, _ := rke2(t)
	raw := strings.Join(line.Tags, ";") + ";" + strings.Join(DefaultHardware(DefaultCPUType).Tags(), ";")
	if got := ParseTags(raw); !slices.Equal(got, line.Tags) {
		t.Errorf("identity %v, want %v", got, line.Tags)
	}
	if got := ParseAllTags(raw); len(got) != len(line.Tags)+len(DefaultHardware(DefaultCPUType).Tags()) {
		t.Errorf("all tags %v", got)
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
		tags   []string
	}{
		{"the defaults", HardwareChoice{}, ",backup=0,discard=on,ssd=1", "virtio-scsi-pci", false, DefaultCPUType, []string{"hw-cpu-x86-64-v2-aes", "hw-discard", "hw-ssd"}},
		{"every option changed", HardwareChoice{CPUType: "Haswell", SSD: &no, Discard: &no, IOThread: &yes, NUMA: &yes}, ",backup=0,iothread=1", "virtio-scsi-single", true, "Haswell", []string{"hw-cpu-haswell", "hw-iothread", "hw-numa"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newBakeHost(t)
			b, tmpl, volume := talosBuilder(t, h)
			b.Hardware = tc.choice
			if _, err := b.Create(t.Context(), &tmpl); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			c := h.vms[101]
			if c["cpu"] != tc.cpu || c["scsihw"] != tc.scsihw || c["scsi0"] != "local-lvm:0,import-from="+volume+tc.disk {
				t.Errorf("the template is %v", c)
			}
			if _, numa := c["numa"]; numa != tc.numa {
				t.Errorf("numa set: %v, want %v (%v)", numa, tc.numa, c["numa"])
			}
			tags, isString := c["tags"].(string)
			if !isString {
				t.Fatalf("tags are %T, want a string", c["tags"])
			}
			for _, want := range tc.tags {
				if !slices.Contains(ParseAllTags(tags), want) {
					t.Errorf("tags %q lack %s", tags, want)
				}
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
