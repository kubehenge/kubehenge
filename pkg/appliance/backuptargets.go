package appliance

import (
	"crypto/x509"
	"fmt"
	"regexp"
)

// BackupTarget is an S3 store clusters back up to, each under a prefix of its own beneath Prefix.
// The keys for it come with each cluster, never with the target.
type BackupTarget struct {
	Name string `json:"name"`
	// Endpoint is the store's https:// URL. It is unset for AWS S3 itself.
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	// CACertificate is the PEM CA of the store's certificate, when the system's do not cover it.
	CACertificate string `json:"caCertificate,omitempty"`
	// NoChecksums keeps the AWS SDK from sending the checksums some stores refuse.
	NoChecksums bool `json:"noChecksums,omitempty"`
}

// MaxBackupTargets is how many targets the chart's schema allows.
const MaxBackupTargets = 16

// The chart's schema's patterns for a target, which the module hands on unchanged.
var (
	targetName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	targetBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	targetRegion = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	targetPrefix = regexp.MustCompile(`^[A-Za-z0-9!_.*'()-]+(/[A-Za-z0-9!_.*'()-]+)*$`)
)

const (
	maxTargetName   = 63
	maxTargetPrefix = 256
)

// Validate checks a target as the chart's schema would.
func (t *BackupTarget) Validate() error {
	switch {
	case len(t.Name) > maxTargetName || !targetName.MatchString(t.Name):
		return fmt.Errorf("target %q: a name of lower-case letters, digits and dashes", t.Name)
	case !targetBucket.MatchString(t.Bucket):
		return fmt.Errorf("target %s: bucket %q is not an S3 bucket name", t.Name, t.Bucket)
	case t.Region != "" && !targetRegion.MatchString(t.Region):
		return fmt.Errorf("target %s: region %q is not an S3 region", t.Name, t.Region)
	case t.Prefix != "" && (len(t.Prefix) > maxTargetPrefix || !targetPrefix.MatchString(t.Prefix)):
		return fmt.Errorf("target %s: prefix %q: folders of letters, digits and !_.*'()- apart by /, without one at either end", t.Name, t.Prefix)
	}
	if t.Endpoint != "" && !plainHTTPS(t.Endpoint) {
		return fmt.Errorf("target %s: endpoint %q: an https:// URL without query, fragment or user", t.Name, t.Endpoint)
	}
	if t.CACertificate != "" {
		if ok := x509.NewCertPool().AppendCertsFromPEM([]byte(t.CACertificate)); !ok {
			return fmt.Errorf("target %s: the CA certificate holds no PEM certificate", t.Name)
		}
	}
	return nil
}

// ValidBackupTargets checks a list of targets: each valid, few enough, no name twice.
func ValidBackupTargets(targets []BackupTarget) error {
	if len(targets) > MaxBackupTargets {
		return fmt.Errorf("at most %d backup targets", MaxBackupTargets)
	}
	seen := map[string]bool{}
	for i := range targets {
		t := &targets[i]
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.Name] {
			return fmt.Errorf("target %s is listed twice", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// moduleBackupTargets names the targets as the module's backup_targets does. Null takes its default.
func moduleBackupTargets(targets []BackupTarget) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		out = append(out, map[string]any{
			"name":           t.Name,
			"endpoint":       nilIfEmpty(t.Endpoint),
			"bucket":         t.Bucket,
			"region":         nilIfEmpty(t.Region),
			"prefix":         nilIfEmpty(t.Prefix),
			"ca_certificate": nilIfEmpty(t.CACertificate),
			"checksums":      !t.NoChecksums,
		})
	}
	return out
}
