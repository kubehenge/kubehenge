package provisioner

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// User credentials are the secrets people type into the interface for what reaches a service
// outside the clusters: a backup target's keys, a DNS provider's API token, a private Git
// repository's token, an identity provider's client secret. The interface keeps each in a Secret of
// its namespace, and the chart's admission policy (kubehenge-interface-credentials) holds the
// interface's own account to those Secrets alone, so it can neither write nor delete a cluster's
// kubeconfig, CA or Proxmox VE token.
const (
	// CredentialLabel marks a Secret as a user credential. Its value is the credential's purpose.
	CredentialLabel = "kubehenge.dev/user-credential" //nolint:gosec // a label's name, not a credential
	// CredentialClusterLabel names the cluster a credential belongs to, beside it in its namespace.
	// A credential without it serves every cluster of the appliance.
	CredentialClusterLabel = "kubehenge.dev/credential-cluster" //nolint:gosec // a label's name, not a credential
	// CredentialType is every user credential's Secret type. A type of its own keeps the interface
	// from making a Secret that Kubernetes or Cluster API acts on, such as a ServiceAccount token
	// or a ClusterResourceSet's resources.
	CredentialType corev1.SecretType = "kubehenge.dev/user-credential" //nolint:gosec // a Secret type, not a credential
	// CredentialSetBy and CredentialSetAt record the Proxmox VE user who last stored a credential,
	// and when (RFC 3339, UTC).
	CredentialSetBy = "kubehenge.dev/credential-set-by" //nolint:gosec // an annotation's name, not a credential
	CredentialSetAt = "kubehenge.dev/credential-set-at" //nolint:gosec // an annotation's name, not a credential
)

// CredentialPurpose is what a user credential is for, and the values it holds.
type CredentialPurpose struct {
	Name string
	// Keys are the Secret's data keys, every one of them required.
	Keys []string
	// Appliance tells whether an administrator may also store one for every cluster.
	Appliance bool
}

// credentialPurposes are those the chart's admission policy lets the interface store. The two lists
// change together.
var credentialPurposes = []CredentialPurpose{
	{Name: "backup", Keys: []string{"accessKeyID", "secretAccessKey"}, Appliance: true},
	{Name: "dns01", Keys: []string{"token"}, Appliance: true},
	{Name: "gitops", Keys: []string{"username", "password"}},
	{Name: "oidc-client", Keys: []string{"clientSecret"}},
}

// CredentialPurposes lists every purpose a user credential may have.
func CredentialPurposes() []CredentialPurpose {
	return slices.Clone(credentialPurposes)
}

// CredentialPurposeNamed finds a purpose by its name.
func CredentialPurposeNamed(name string) (CredentialPurpose, bool) {
	i := slices.IndexFunc(credentialPurposes, func(p CredentialPurpose) bool { return p.Name == name })
	if i < 0 {
		return CredentialPurpose{}, false
	}
	return credentialPurposes[i], true
}

// CredentialName is the Secret of a cluster's credential, or with no cluster, of the appliance-wide
// one. Only the first holds a dot: no cluster's credential can take an appliance-wide one's name,
// and since a purpose holds none, a name tells its cluster and purpose apart. Cluster API and the
// controller name a cluster's Secrets <cluster>-<suffix>, and the controller a namespace's own
// kubehenge-<name>; no purpose is one of those suffixes or names, so the interface cannot take the
// name of a Secret either of them is yet to make.
func CredentialName(cluster, purpose string) string {
	if cluster == "" {
		return "kubehenge-" + purpose
	}
	return cluster + "." + purpose
}
