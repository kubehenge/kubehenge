package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

var (
	errNoAppliance     = errors.New("this interface does not manage its management cluster")
	errSetupNotAllowed = errors.New("the user may not change the management settings")
	errNotSetUp        = errors.New("the management cluster is not set up yet")
	errNothingToSet    = errors.New("give --site-ca-file, --time-zone or --vm-id-range")
)

// settingsCommand shows and changes the management settings the interface's Setup and
// Administration pages hold for every cluster: the site CA, the site time zone and the VM ID range.
func (o *applianceOptions) settingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show the site CA, time zone and VM ID range every cluster inherits",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				saved, raw, err := savedSettings(ctx, s)
				if err != nil {
					return err
				}
				if o.json {
					return printJSON(cmd, raw)
				}
				if err := say(cmd, "Site CA     "+siteCASummary(saved.SiteCA)); err != nil {
					return err
				}
				if err := say(cmd, "Time zone   "+orNone(saved.TimeZone)); err != nil {
					return err
				}
				return say(cmd, "VM ID range "+vmIDRangeSummary(saved.WorkloadVMIDRange))
			})
		},
	}
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	cmd.AddCommand(o.settingsSetCommand())
	return cmd
}

func vmIDRangeSummary(r *provisioner.VMIDRange) string {
	if r == nil {
		return "None"
	}
	return r.String()
}

func siteCASummary(pem string) string {
	if pem == "" {
		return "None"
	}
	return fmt.Sprintf("%d bytes of PEM", len(pem))
}

// savedSettings reads the settings of the last apply as the interface shows them.
func savedSettings(ctx context.Context, s *applianceSession) (*appliance.Settings, []byte, error) {
	var view struct {
		Appliance bool                `json:"appliance"`
		Allowed   bool                `json:"allowed"`
		Settings  *appliance.Settings `json:"settings"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/setup", nil, &view); err != nil {
		return nil, nil, err
	}
	switch {
	case !view.Appliance:
		return nil, nil, errNoAppliance
	case !view.Allowed:
		return nil, nil, errSetupNotAllowed
	case view.Settings == nil:
		return nil, nil, errNotSetUp
	}
	settings, err := json.Marshal(view.Settings)
	if err != nil {
		return nil, nil, fmt.Errorf("the interface's settings: %w", err)
	}
	return view.Settings, settings, nil
}

// settingsChange is what settings set changes in the saved settings. A setting it does not set keeps
// its value.
type settingsChange struct {
	setCA, setZone, setRange bool
	siteCA, zone             string
	vmIDRange                *provisioner.VMIDRange
	passphrase               string
}

// apply sends the saved settings with the change to the route Setup saves through, which checks them.
func (c *settingsChange) apply(ctx context.Context, s *applianceSession) error {
	settings, _, err := savedSettings(ctx, s)
	if err != nil {
		return err
	}
	if c.setCA {
		settings.SiteCA = c.siteCA
	}
	if c.setZone {
		settings.TimeZone = c.zone
	}
	if c.setRange {
		settings.WorkloadVMIDRange = c.vmIDRange
	}
	_, err = s.call(ctx, http.MethodPost, "/api/setup", map[string]any{"settings": settings, "passphrase": c.passphrase}, nil)
	return err
}

func (o *applianceOptions) settingsSetCommand() *cobra.Command {
	var caFile, passphraseFile, zone, vmIDs string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change the site CA, time zone or VM ID range and apply it to the management cluster",
		Long: `Change the site CA, the site time zone or the site VM ID range and apply the settings to the
management cluster, as Setup does. A setting left out keeps its value. --site-ca-file names a PEM file of
certificates, and an empty file, --time-zone "" or --vm-id-range "" clears it. --vm-id-range is start-end,
both included, for the VMs of new clusters and the VM templates. The interface refuses what it refuses on
its pages, and a new site CA replaces the nodes of the clusters that inherit it, a few at a time and inside
their upgrade windows. The state passphrase comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := cmd.Flags()
			change := settingsChange{setCA: f.Changed("site-ca-file"), setZone: f.Changed("time-zone"), setRange: f.Changed("vm-id-range"), zone: zone}
			if !change.setCA && !change.setZone && !change.setRange {
				return errNothingToSet
			}
			if change.setRange && vmIDs != "" {
				r, err := provisioner.ParseVMIDRange(vmIDs)
				if err != nil {
					return fmt.Errorf("--vm-id-range: %w", err)
				}
				change.vmIDRange = &r
			}
			var err error
			if change.passphrase, err = secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE"); err != nil {
				return err
			}
			if change.setCA && caFile != "" {
				raw, err := os.ReadFile(caFile) //nolint:gosec // the file the person running the command named
				if err != nil {
					return fmt.Errorf("reading %s: %w", caFile, err)
				}
				change.siteCA = string(raw)
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				if err := change.apply(ctx, s); err != nil {
					return err
				}
				return say(cmd, "Applying the settings")
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&caFile, "site-ca-file", "", "file of PEM certificates every cluster, mirror, backup target and identity provider trusts, empty to clear")
	f.StringVar(&zone, "time-zone", "", "IANA time zone schedules use unless they name their own, empty to clear")
	f.StringVar(&vmIDs, "vm-id-range", "", "VM IDs of new clusters' VMs and the VM templates, as start-end, empty to clear")
	f.StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	o.signInFlags(cmd)
	return cmd
}
