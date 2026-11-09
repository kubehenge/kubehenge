//go:build e2e

package templates_test

import (
	"testing"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// TestMain hands the end-to-end test this build's Talos pins. It sits in the external test package
// because package templates cannot import pkg/provisioner.
func TestMain(m *testing.M) {
	templates.TalosPinsForE2E = provisioner.DefaultReleasePins().Talos()
	m.Run()
}
