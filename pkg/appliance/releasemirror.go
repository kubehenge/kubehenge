package appliance

import (
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// ReleaseMirror is where the nodes of RKE2 and k3s clusters download their release in place of
// GitHub, and where the interface reads a release's checksums before pinning it.
type ReleaseMirror struct {
	// URL serves the releases on GitHub's paths beneath it, such as https://mirror.example.org/github.
	URL string `json:"url,omitempty"`
	// CABundle is the PEM authority of a mirror the usual authorities do not sign.
	CABundle string `json:"caBundle,omitempty"`
}

// Mirror is the setting as the controller takes it, the zero mirror when unset.
func (m *ReleaseMirror) Mirror() provisioner.ReleaseMirror {
	if m == nil {
		return provisioner.ReleaseMirror{}
	}
	return provisioner.ReleaseMirror{URL: m.URL, CABundle: m.CABundle}
}

func (m *ReleaseMirror) check(p *problems, now time.Time) {
	if err := m.Mirror().Check(now); err != nil {
		p.addf("Release mirror: %s", err)
	}
}

// moduleReleaseMirror is the module's release_mirror variable.
func moduleReleaseMirror(m *ReleaseMirror) map[string]any {
	mirror := m.Mirror()
	return map[string]any{"url": mirror.URL, moduleCABundle: mirror.CABundle}
}
