package appliance

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestBackupTargets(t *testing.T) {
	t.Parallel()
	good := BackupTarget{Name: "store", Endpoint: "https://s3.example.org:8333", Bucket: "backups", Prefix: "site/a"}
	s := settings("pve1")
	s.BackupTargets = []BackupTarget{good, {Name: "aws", Bucket: "aws-backups", Region: "eu-central-2"}}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid targets refused: %v", err)
	}
	for name, change := range map[string]func(*BackupTarget){
		"upper-case name":   func(t *BackupTarget) { t.Name = "Store" },
		"no bucket":         func(t *BackupTarget) { t.Bucket = "" },
		"bucket with _":     func(t *BackupTarget) { t.Bucket = "my_backups" },
		"http endpoint":     func(t *BackupTarget) { t.Endpoint = "http://s3.example.org" },
		"endpoint with key": func(t *BackupTarget) { t.Endpoint = "https://key:secret@s3.example.org" },
		"prefix with /":     func(t *BackupTarget) { t.Prefix = "/site" },
		"region":            func(t *BackupTarget) { t.Region = "EU Central" },
		"not a CA":          func(t *BackupTarget) { t.CACertificate = "nonsense" },
	} {
		bad := good
		change(&bad)
		s := settings("pve1")
		s.BackupTargets = []BackupTarget{bad}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Backup targets:") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, targets := range map[string][]BackupTarget{
		"twice":     {good, good},
		"seventeen": slices.Repeat([]BackupTarget{good}, MaxBackupTargets+1),
	} {
		if err := ValidBackupTargets(targets); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	raw, err := json.Marshal(moduleBackupTargets([]BackupTarget{good, {Name: "aws", Bucket: "aws-backups", NoChecksums: true}}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"bucket":"backups","ca_certificate":null,"checksums":true,"endpoint":"https://s3.example.org:8333","name":"store","prefix":"site/a","region":null},` +
		`{"bucket":"aws-backups","ca_certificate":null,"checksums":false,"endpoint":null,"name":"aws","prefix":null,"region":null}]`
	if string(raw) != want {
		t.Fatalf("module targets = %s", raw)
	}
	if raw, err = json.Marshal(moduleBackupTargets(nil)); err != nil || string(raw) != "[]" {
		t.Fatalf("no targets = %s, %v", raw, err)
	}
}
