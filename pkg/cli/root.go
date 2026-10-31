// Package cli is the kubehenge command line.
package cli

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// NewRoot returns the kubehenge command.
func NewRoot() *cobra.Command {
	var conn pve.Config
	root := &cobra.Command{
		Use:           "kubehenge",
		Short:         "Kubernetes for Proxmox VE",
		Version:       release.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	flags := root.PersistentFlags()
	flags.StringVar(&conn.URL, "proxmox-url", os.Getenv("KUBEHENGE_PROXMOX_URL"),
		"Proxmox VE API, such as https://pve.example.org:8006 (env KUBEHENGE_PROXMOX_URL)")
	flags.StringVar(&conn.CAFile, "proxmox-ca", os.Getenv("KUBEHENGE_PROXMOX_CA"),
		"PEM file with the CA of the API's certificate; default the system trust store (env KUBEHENGE_PROXMOX_CA)")
	insecure, _ := strconv.ParseBool(os.Getenv("KUBEHENGE_PROXMOX_INSECURE"))
	flags.BoolVar(&conn.Insecure, "proxmox-insecure", insecure,
		"do not verify the API's certificate (env KUBEHENGE_PROXMOX_INSECURE)")
	// The token comes only from the environment, so it never shows in process listings.
	conn.Token = os.Getenv("KUBEHENGE_PROXMOX_TOKEN")

	root.AddCommand(newTemplateCommand(&conn), newControllerCommand(&conn), newServeCommand(&conn),
		newUpdateCommand(), newReleaseCommand(), newWaitCommand(), newManifestsCommand(), newEtcdSnapshotCommand(),
		newApplianceCommand(), newDiagnoseCommand(), newTokenJobCommand(&conn), newCSIProxyCommand(&conn))
	return root
}
